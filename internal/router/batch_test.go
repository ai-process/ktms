package router

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

type mockBudgetThrottle struct {
	NoopThrottle
	trippedKind string
}

func (m mockBudgetThrottle) CheckDailyBudget(ctx context.Context, model *proxydb.Model, keyName, userID string) string {
	return m.trippedKind
}

func TestResolveBatchModel_NoCapableModel(t *testing.T) {
	snap := registry.NewSnapshotForTest(
		[]*proxydb.Model{
			{ID: "m1", Vendor: registry.VendorGoogle, Enabled: true}, // no batch capability
			{ID: "m2", Vendor: registry.VendorOpenAI, Enabled: true}, // no batch capability
		},
		nil,
		map[string]map[string]string{
			"key1": {registry.VendorGoogle: "g-key", registry.VendorOpenAI: "o-key"},
		},
		nil,
	)

	_, err := ResolveBatchModel(context.Background(), snap, NoopThrottle{}, []string{"m1", "m2"}, "key1", "gen", "user1")
	if !errors.Is(err, ErrNoCapableModel) {
		t.Fatalf("expected ErrNoCapableModel, got %v", err)
	}
}

func TestResolveBatchModel_NoVendorKey(t *testing.T) {
	snap := registry.NewSnapshotForTest(
		[]*proxydb.Model{
			{
				ID:           "m-batch",
				Vendor:       registry.VendorGoogle,
				Capabilities: []string{registry.CapabilityBatch},
				Enabled:      true,
			},
		},
		nil,
		map[string]map[string]string{
			"key1": {registry.VendorOpenAI: "o-key"}, // no google key
		},
		nil,
	)

	_, err := ResolveBatchModel(context.Background(), snap, NoopThrottle{}, []string{"m-batch"}, "key1", "gen", "user1")
	if !errors.Is(err, ErrNoCapableModel) {
		t.Fatalf("expected ErrNoCapableModel when vendor key is missing, got %v", err)
	}
}

func TestResolveBatchModel_BudgetExhausted(t *testing.T) {
	snap := registry.NewSnapshotForTest(
		[]*proxydb.Model{
			{
				ID:           "m-batch",
				Vendor:       registry.VendorGoogle,
				Capabilities: []string{registry.CapabilityBatch},
				Enabled:      true,
			},
		},
		nil,
		map[string]map[string]string{
			"key1": {registry.VendorGoogle: "g-key"},
		},
		nil,
	)

	th := mockBudgetThrottle{trippedKind: "budget_key"}
	_, err := ResolveBatchModel(context.Background(), snap, th, []string{"m-batch"}, "key1", "gen", "user1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected ChainExhausted error, got %v", err)
	}
	if !exhausted.AllThrottled || exhausted.ThrottleKind != "budget_key" {
		t.Fatalf("expected throttled:budget_key, got %+v", exhausted)
	}
}

func TestResolveBatchModel_SuccessFirstCapableWithKeyAndBudget(t *testing.T) {
	snap := registry.NewSnapshotForTest(
		[]*proxydb.Model{
			{
				ID:      "m-sync-only",
				Vendor:  registry.VendorGoogle,
				Enabled: true,
			},
			{
				ID:           "m-batch-no-key",
				Vendor:       registry.VendorDeepSeek,
				Capabilities: []string{registry.CapabilityBatch},
				Enabled:      true,
			},
			{
				ID:           "m-batch-ok",
				Vendor:       registry.VendorGoogle,
				Capabilities: []string{registry.CapabilityBatch},
				Enabled:      true,
			},
		},
		nil,
		map[string]map[string]string{
			"key1": {registry.VendorGoogle: "g-key"},
		},
		nil,
	)

	m, err := ResolveBatchModel(context.Background(), snap, NoopThrottle{}, []string{"m-sync-only", "m-batch-no-key", "m-batch-ok"}, "key1", "gen", "user1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ID != "m-batch-ok" {
		t.Fatalf("expected m-batch-ok, got %s", m.ID)
	}
}
