package nodeagent

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

type AgentConfig struct {
	ProxyAddr     string
	TLS           bool // dial ProxyAddr over TLS with system roots, e.g. a load balancer on :443
	NodeKey       string
	NodeID        string
	NodeName      string
	Endpoint      string
	Engine        string
	NodeAuthToken string
	BaseWeight    float64
	MaxConcurrent uint32
}

type Agent struct {
	cfg       AgentConfig
	client    pb.NodeRegistryServiceClient
	inspector EngineInspector
	drainChan chan struct{}
	stopChan  chan struct{}
	draining  bool
	mu        sync.Mutex
}

func NewAgent(cfg AgentConfig) *Agent {
	return &Agent{
		cfg:       cfg,
		inspector: GetInspector(cfg.Engine),
		drainChan: make(chan struct{}, 1),
		stopChan:  make(chan struct{}),
	}
}

func (a *Agent) Run(ctx context.Context) error {
	creds := insecure.NewCredentials()
	if a.cfg.TLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpclib.DialContext(ctx, a.cfg.ProxyAddr, grpclib.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("connect to proxy %s: %w", a.cfg.ProxyAddr, err)
	}
	defer conn.Close()

	a.client = pb.NewNodeRegistryServiceClient(conn)

	// Context carrying Bearer token
	authCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.NodeKey)

	// Handle SIGTERM/SIGINT for graceful drain
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			log.Info().Str("signal", sig.String()).Msg("received shutdown signal, initiating graceful drain")
			a.mu.Lock()
			a.draining = true
			a.mu.Unlock()
			select {
			case a.drainChan <- struct{}{}:
			default:
			}
		case <-a.stopChan:
			return
		}
	}()

	// 1. Initial Inspection & Registration
	models, err := a.inspector.Inspect(authCtx, a.cfg.Endpoint, a.cfg.NodeAuthToken)
	if err != nil {
		log.Warn().Err(err).Msg("failed initial engine inspection, registering with empty models")
	}

	gpu, _ := ReadNvidiaSMI(authCtx)
	res := &pb.NodeResources{
		BaseWeight:    a.cfg.BaseWeight,
		MaxConcurrent: a.cfg.MaxConcurrent,
	}
	if gpu != nil {
		res.VramTotalMb = gpu.TotalVRAMMB
		res.VramFreeMb = gpu.FreeVRAMMB
		res.GpuUtilPercent = gpu.UtilPercent
		res.GpuTempC = gpu.TempC
		res.GpuModel = gpu.ModelName
	}

	regResp, err := a.client.RegisterNode(authCtx, &pb.RegisterNodeRequest{
		NodeId:        a.cfg.NodeID,
		Name:          a.cfg.NodeName,
		Endpoint:      a.cfg.Endpoint,
		Engine:        a.cfg.Engine,
		NodeAuthToken: a.cfg.NodeAuthToken,
		Resources:     res,
		Models:        models,
	})
	if err != nil {
		return fmt.Errorf("register node: %w", err)
	}

	interval := 5 * time.Second
	if regResp.HeartbeatIntervalSeconds > 0 {
		interval = time.Duration(regResp.HeartbeatIntervalSeconds) * time.Second
	}

	log.Info().Str("node_id", a.cfg.NodeID).Dur("interval", interval).Msg("node registered successfully")

	// 2. Heartbeat loop
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return a.unregister(authCtx)
		case <-a.drainChan:
			return a.drainAndExit(authCtx)
		case <-ticker.C:
			// Inspect current models and GPU
			currModels, _ := a.inspector.Inspect(authCtx, a.cfg.Endpoint, a.cfg.NodeAuthToken)
			currGPU, _ := ReadNvidiaSMI(authCtx)
			currRes := &pb.NodeResources{
				BaseWeight:    a.cfg.BaseWeight,
				MaxConcurrent: a.cfg.MaxConcurrent,
			}
			if currGPU != nil {
				currRes.VramTotalMb = currGPU.TotalVRAMMB
				currRes.VramFreeMb = currGPU.FreeVRAMMB
				currRes.GpuUtilPercent = currGPU.UtilPercent
				currRes.GpuTempC = currGPU.TempC
				currRes.GpuModel = currGPU.ModelName
			}

			a.mu.Lock()
			draining := a.draining
			a.mu.Unlock()

			hbResp, err := a.client.Heartbeat(authCtx, &pb.HeartbeatRequest{
				NodeId:    a.cfg.NodeID,
				Draining:  draining,
				Resources: currRes,
				Models:    currModels,
			})
			if err != nil {
				log.Warn().Err(err).Msg("heartbeat failed")
				continue
			}

			if hbResp.ReRegisterRequired {
				log.Info().Msg("proxy requested re-registration")
				_, _ = a.client.RegisterNode(authCtx, &pb.RegisterNodeRequest{
					NodeId:        a.cfg.NodeID,
					Name:          a.cfg.NodeName,
					Endpoint:      a.cfg.Endpoint,
					Engine:        a.cfg.Engine,
					NodeAuthToken: a.cfg.NodeAuthToken,
					Resources:     currRes,
					Models:        currModels,
				})
			}

			if hbResp.DrainRequested && !draining {
				log.Info().Msg("proxy requested drain")
				a.mu.Lock()
				a.draining = true
				a.mu.Unlock()
				select {
				case a.drainChan <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (a *Agent) drainAndExit(ctx context.Context) error {
	log.Info().Msg("waiting for active proxy requests to finish before stopping")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		hbResp, err := a.client.Heartbeat(ctx, &pb.HeartbeatRequest{
			NodeId:   a.cfg.NodeID,
			Draining: true,
		})
		if err == nil {
			if hbResp.ActiveProxyRequests <= 0 {
				log.Info().Msg("active requests reached 0, exiting cleanly")
				return a.unregister(ctx)
			}
			log.Info().Int32("active_requests", hbResp.ActiveProxyRequests).Msg("still draining...")
		}
		select {
		case <-ctx.Done():
			return a.unregister(ctx)
		case <-ticker.C:
		}
	}
}

func (a *Agent) unregister(ctx context.Context) error {
	close(a.stopChan)
	log.Info().Str("node_id", a.cfg.NodeID).Msg("unregistering node from proxy")
	_, err := a.client.UnregisterNode(ctx, &pb.UnregisterNodeRequest{
		NodeId: a.cfg.NodeID,
		Reason: "agent shutdown",
	})
	return err
}
