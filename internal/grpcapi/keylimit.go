package grpcapi

import (
	"context"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
)

// KeyLimiter enforces a client key's own request and token rates over all
// models (throttle.Redis).
type KeyLimiter interface {
	AdmitKey(ctx context.Context, keyName string, rpm int32, tpm, estimate int64) (settle func(actual int64), kind string, retry time.Duration)
}

// WithKeyLimiter enforces each key's rpm_limit and tpm_limit.
func (s *ProxyServer) WithKeyLimiter(l KeyLimiter) *ProxyServer {
	s.keyLimiter = l
	return s
}

// admitKey books a call on its key's rates; the returned func settles it with
// the tokens the call used, so tokens are booked by estimate and then by fact.
func (s *ProxyServer) admitKey(ctx context.Context, id *apikeys.Identity, estimate int64) (func(llm.TokensUsage), error) {
	if s.keyLimiter == nil || id == nil || (id.RPMLimit <= 0 && id.TPMLimit <= 0) {
		return func(llm.TokensUsage) {}, nil
	}
	settle, kind, retry := s.keyLimiter.AdmitKey(ctx, id.Name, id.RPMLimit, id.TPMLimit, estimate)
	if kind != "" {
		observability.ThrottledTotal.WithLabelValues(kind).Inc()
		return nil, keyLimitStatus(kind, retry)
	}
	return func(u llm.TokensUsage) { settle(int64(u.Input + u.Output)) }, nil
}

// keyLimitStatus carries RetryInfo (HTTP: Retry-After) unless the call can
// never fit, being larger than a minute of the key's tokens.
func keyLimitStatus(kind string, retry time.Duration) error {
	st := status.New(codes.ResourceExhausted, "throttled:"+kind)
	if retry <= 0 {
		return st.Err()
	}
	if withInfo, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retry)}); err == nil {
		return withInfo.Err()
	}
	return st.Err()
}
