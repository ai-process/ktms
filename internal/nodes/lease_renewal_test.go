package nodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// A unary generation that outlasts the lease TTL must keep its slot, or the
// pool admits a request the node has no room for.
func TestUnaryGenerationKeepsItsLease(t *testing.T) {
	ctx := context.Background()
	const ttl = 300 * time.Millisecond
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gemma","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before srv.Close, so a failed check cannot hang the test

	mgr, mr := setupTestManager(t)
	nodeID, modelID := "node-long", "local-gemma"
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID: nodeID, Name: "n", Endpoint: srv.URL, Engine: "ollama", BaseWeight: 10, MaxConcurrent: 1,
		Models: []*pb.NodeModelSpec{{ModelId: modelID, RemoteModelName: "gemma", IsWarm: true}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: nodeID}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	key := mgr.Slots().store.keys.NodeInflight(nodeID)
	liveLeases := func() int64 {
		n, err := rdb.ZCount(ctx, key, strconv.FormatInt(time.Now().UnixMilli(), 10), "+inf").Result()
		if err != nil {
			t.Fatalf("ZCount: %v", err)
		}
		return n
	}

	pa := NewPoolAdapter(NewSelector(mgr), nil, &proxydb.Model{ID: modelID, Vendor: registry.VendorLocal}, ttl, nil)
	done := make(chan error, 1)
	go func() {
		chat := llm.NewChatContext()
		chat.AddUserMessage("go")
		_, err := pa.GenerateText(ctx, chat)
		done <- err
	}()

	time.Sleep(3 * ttl) // well past the TTL the lease was taken with
	if n := liveLeases(); n != 1 {
		t.Fatalf("live leases mid-call = %d, want 1: the slot lapsed while the node was busy", n)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if n, _ := rdb.ZCard(ctx, key).Result(); n != 0 {
		t.Fatalf("leases after the call = %d, want 0", n)
	}
}
