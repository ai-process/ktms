package nodes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/rediskeys"
	"github.com/ai-process/ktms/internal/registry"
)

// With Redis gone the pool's state is unknown: the request must move on to the
// next model in the chain (a cloud fallback), and not trip the vendor breaker.
func TestPoolFallsThroughWhenRedisIsDown(t *testing.T) {
	ctx := context.Background()
	mgr, mr := setupTestManager(t)
	pa := NewPoolAdapter(NewSelector(mgr), nil, &proxydb.Model{ID: "local-gemma", Vendor: registry.VendorLocal}, 0, nil)
	mr.Close()

	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	_, err := pa.GenerateText(ctx, chat)
	if err == nil {
		t.Fatal("expected an error with Redis down")
	}
	if !llm.ShouldSkipToNextModel(err) || !llm.IsCapacityExhausted(err) {
		t.Fatalf("err = %v: a Redis failure must skip to the next model without counting against the vendor", err)
	}
}

// failMetaGets makes reads of node meta fail, as a flaky Redis would.
type failMetaGets struct{ prefix string }

func (failMetaGets) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failMetaGets) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h failMetaGets) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 {
			if key, _ := cmd.Args()[1].(string); strings.HasPrefix(key, h.prefix) {
				err := errors.New("LOADING Redis is loading the dataset in memory")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

// The sweep must not evict a live node because one read of it failed.
func TestSweepKeepsNodesOnReadErrors(t *testing.T) {
	ctx := context.Background()
	mgr, mr := setupTestManager(t)
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID: "n1", Name: "n", Endpoint: "http://127.0.0.1:1", Engine: "vllm", BaseWeight: 1, MaxConcurrent: 1,
		Models: []*pb.NodeModelSpec{{ModelId: "m"}},
	}); err != nil {
		t.Fatal(err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	keys := rediskeys.New("test")
	rdb.AddHook(failMetaGets{prefix: keys.NodeMeta("")})
	flaky := NewManager(NewStore(rdb, keys), NewLeaseManager(rdb, mgr.store), mgr.circuit, mgr.telemetry, mgr.cfg)
	flaky.sweep(ctx)

	ids, err := mgr.store.GetAllNodeIDs(ctx)
	if err != nil || len(ids) != 1 {
		t.Fatalf("nodes after sweep = %v, %v; a read error must not evict a node", ids, err)
	}
}
