package nodeagent

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

const (
	retryInitial = 15 * time.Second
	retryMax     = 5 * time.Minute
)

// errStopped means shutdown was requested while the agent was waiting to retry.
var errStopped = errors.New("stopped while waiting for the proxy to accept this node")

// backoff doubles from initial up to max.
type backoff struct{ initial, max, cur time.Duration }

func (b *backoff) next() time.Duration {
	switch {
	case b.cur == 0:
		b.cur = b.initial
	case b.cur < b.max:
		b.cur = min(b.cur*2, b.max)
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }

func (a *Agent) newBackoff() *backoff { return &backoff{initial: a.retryInitial, max: a.retryMax} }

// explainRegistrationError turns a refusal into what the operator should do about it.
func explainRegistrationError(err error) string {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.Unauthenticated:
		return "the proxy rejected the node key (wrong, revoked or expired); ask for a new key. Retrying with a pause, not restarting"
	case codes.PermissionDenied:
		return "the proxy refuses this node: " + st.Message() + ". Fix the key's restrictions or this node's --node-id / --only-model; retrying with a pause, not restarting"
	case codes.InvalidArgument:
		return "the proxy rejected the registration: " + st.Message() + ". Retrying with a pause, not restarting"
	}
	return "the proxy is not accepting registrations right now: " + err.Error()
}

// wait sleeps for d. It returns false when shutdown was requested, and true early when a
// new tunnel address arrives so the caller retries with it.
func (a *Agent) wait(ctx context.Context, d time.Duration, urls <-chan string) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case u := <-urls: // nil channel blocks forever when nothing is exposed
		a.setEndpoint(u)
	case <-a.drainChan:
		return false
	case <-ctx.Done():
		return false
	}
	return true
}

// preflight asks the proxy whether this node would be accepted, before any tunnel exists, so a
// permanent refusal never costs a Cloudflare tunnel per restart. Old proxies without
// ValidateNode skip the check.
func (a *Agent) preflight(ctx context.Context) error {
	bo := a.newBackoff()
	for {
		models, _ := a.snapshot(ctx)
		resp, err := a.client.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: a.cfg.NodeID, Models: models})
		if err == nil {
			if len(resp.GetDroppedModels()) > 0 {
				log.Warn().Strs("ignored_models", resp.GetDroppedModels()).Msg("the node key does not allow these models; the proxy will ignore them (use --only-model to stop reporting them)")
			}
			return nil
		}
		if status.Code(err) == codes.Unimplemented {
			log.Info().Msg("proxy has no pre-flight check; registering directly")
			return nil
		}
		delay := bo.next()
		log.Error().Err(err).Dur("retry_in", delay).Msg(explainRegistrationError(err))
		if !a.wait(ctx, delay, nil) {
			return errStopped
		}
	}
}

// registerUntilAccepted registers, pausing between failures instead of exiting, so a
// supervisor restarting the process never opens a fresh tunnel for a refusal.
func (a *Agent) registerUntilAccepted(ctx context.Context, urls <-chan string) (*pb.RegisterNodeResponse, error) {
	bo := a.newBackoff()
	for {
		models, res := a.snapshot(ctx)
		resp, err := a.register(ctx, models, res)
		if err == nil {
			return resp, nil
		}
		delay := bo.next()
		log.Error().Err(err).Dur("retry_in", delay).Msg(explainRegistrationError(err))
		if !a.wait(ctx, delay, urls) {
			return nil, errStopped
		}
	}
}
