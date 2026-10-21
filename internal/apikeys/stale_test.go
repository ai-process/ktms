package apikeys

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/proxydb"
)

func staleVerifier(store *fakeStore, maxAge time.Duration) (*Verifier, *time.Time) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := NewVerifier(store, "").WithStaleIfError(maxAge)
	v.stale.now = func() time.Time { return now }
	return v, &now
}

// While Postgres is unreachable, a key that verified recently keeps working,
// so an outage of the key store does not become an outage of the gateway.
func TestStaleIfErrorKeepsRecentKeysWorking(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	other := mint(t, store, "never-seen", ScopeGenerate)
	v, now := staleVerifier(store, 5*time.Minute)
	ctx := context.Background()

	if _, err := v.Verify(ctx, plain); err != nil {
		t.Fatalf("healthy Verify: %v", err)
	}
	store.err = errors.New("connection refused")

	id, err := v.Verify(ctx, plain)
	if err != nil || id.Name != "generation" {
		t.Fatalf("during outage: id=%v err=%v, want the recently verified key", id, err)
	}
	if _, err := v.Verify(ctx, other); err == nil {
		t.Fatal("a key not verified before the outage must be refused")
	}
	if _, err := v.Verify(ctx, plain+"x"); err == nil {
		t.Fatal("a wrong secret must be refused, cache or not")
	}

	*now = now.Add(6 * time.Minute)
	if _, err := v.Verify(ctx, plain); err == nil {
		t.Fatal("past AUTH_STALE_IF_ERROR the key must be refused")
	}
}

// A revocation the store reports must also clear the cache, so the key does
// not come back to life if the store goes down right after.
func TestStaleIfErrorForgetsRevokedKeys(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v, _ := staleVerifier(store, 5*time.Minute)
	ctx := context.Background()

	if _, err := v.Verify(ctx, plain); err != nil {
		t.Fatal(err)
	}
	store.rows = map[string]*proxydb.APIKey{} // revoked: the store no longer knows it
	if _, err := v.Verify(ctx, plain); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked key: err = %v", err)
	}
	store.err = errors.New("connection refused")
	if _, err := v.Verify(ctx, plain); err == nil {
		t.Fatal("a key revoked before the outage must stay refused during it")
	}
}

func TestStaleIfErrorDisabledRefusesDuringOutage(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v := NewVerifier(store, "").WithStaleIfError(0)
	ctx := context.Background()
	if _, err := v.Verify(ctx, plain); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("connection refused")
	if _, err := v.Verify(ctx, plain); err == nil {
		t.Fatal("with AUTH_STALE_IF_ERROR=0 an outage must refuse every call")
	}
}

// slowStore stands in for a blackholed database: lookups wait for their context.
type slowStore struct{ fakeStore }

func (s *slowStore) GetAPIKeyByLookup(ctx context.Context, _ string) (*proxydb.APIKey, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestQueryTimeoutBoundsLookups(t *testing.T) {
	v := NewVerifier(&slowStore{}, "").WithQueryTimeout(50 * time.Millisecond)
	plain, _, err := Generate("g", []string{ScopeGenerate})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := v.Verify(context.Background(), plain); err == nil {
		t.Fatal("a hung store must fail the call")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("lookup took %v, want about the query timeout", elapsed)
	}
}
