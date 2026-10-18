package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/panicsafe"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

// GRPCServer hosts the proxy data-plane and admin services.
type GRPCServer struct {
	server *grpclib.Server
	addr   string
}

// NewGRPCServer wires interceptors (logging first, auth second) and registers
// the services.
func NewGRPCServer(proxy pb.GatewayServiceServer, admin pb.AdminServiceServer,
	nodeReg pb.NodeRegistryServiceServer, verifier *apikeys.Verifier, addr string, maxRecvMsgSize int) *GRPCServer {
	opts := []grpclib.ServerOption{
		grpclib.StatsHandler(otelgrpc.NewServerHandler()),
		grpclib.ChainUnaryInterceptor(unaryLoggingInterceptor, authUnaryInterceptor(verifier)),
		grpclib.ChainStreamInterceptor(streamLoggingInterceptor, authStreamInterceptor(verifier)),
	}
	if maxRecvMsgSize > 0 {
		opts = append(opts, grpclib.MaxRecvMsgSize(maxRecvMsgSize))
	}
	srv := grpclib.NewServer(opts...)

	pb.RegisterGatewayServiceServer(srv, proxy)
	pb.RegisterAdminServiceServer(srv, admin)
	if nodeReg != nil {
		pb.RegisterNodeRegistryServiceServer(srv, nodeReg)
	}
	reflection.Register(srv)

	return &GRPCServer{server: srv, addr: addr}
}

func (s *GRPCServer) Start() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	log.Info().Str("addr", s.addr).Msg("starting gRPC server")
	return s.server.Serve(lis)
}

func (s *GRPCServer) Stop() {
	log.Info().Msg("stopping gRPC server")
	s.server.GracefulStop()
}

func unaryLoggingInterceptor(
	ctx context.Context,
	req any,
	info *grpclib.UnaryServerInfo,
	handler grpclib.UnaryHandler,
) (resp any, err error) {
	start := time.Now()
	defer recoverToStatus(info.FullMethod, &err)()
	resp, err = handler(ctx, req)
	evt := rpcLogEvent(err)
	if info.FullMethod == method(pb.NodeRegistryService_Heartbeat_FullMethodName) && err == nil {
		evt = log.Debug()
	}
	evt.Str("method", info.FullMethod).Dur("duration", time.Since(start)).Msg("grpc request")
	return resp, err
}

func streamLoggingInterceptor(
	srv any,
	ss grpclib.ServerStream,
	info *grpclib.StreamServerInfo,
	handler grpclib.StreamHandler,
) (err error) {
	start := time.Now()
	defer recoverToStatus(info.FullMethod, &err)()
	err = handler(srv, ss)
	rpcLogEvent(err).Str("method", info.FullMethod).Dur("duration", time.Since(start)).Msg("grpc request")
	return err
}

// rpcLogEvent picks the log level for a finished RPC. Client-caused and
// expected-control-flow statuses (throttled, bad key, unconfigured chain)
// would ship to Sentry as noise at error level.
func rpcLogEvent(err error) *zerolog.Event {
	if err == nil {
		return log.Info()
	}
	switch status.Code(err) {
	case codes.NotFound, codes.InvalidArgument, codes.AlreadyExists,
		codes.FailedPrecondition, codes.Canceled, codes.ResourceExhausted,
		codes.Unauthenticated, codes.PermissionDenied:
		return log.Warn().Err(err)
	default:
		return log.Error().Err(err)
	}
}

// recoverToStatus turns a panic in the handler goroutine into an Internal
// status instead of crashing the whole service, reporting it to Sentry.
// Returns a func to defer so the recover runs in the interceptor's own frame.
func recoverToStatus(method string, err *error) func() {
	return func() {
		if r := recover(); r != nil {
			panicsafe.Report("grpc handler "+method, r)
			*err = status.Errorf(codes.Internal, "internal error")
		}
	}
}
