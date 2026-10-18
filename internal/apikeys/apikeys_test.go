package apikeys

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/proxydb"
)

type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]*proxydb.APIKey
	nodeRows map[string]*proxydb.NodeKey
	lookups  int
	err      error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:     map[string]*proxydb.APIKey{},
		nodeRows: map[string]*proxydb.NodeKey{},
	}
}

func (f *fakeStore) GetAPIKeyByLookup(_ context.Context, lookup string) (*proxydb.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	row, ok := f.rows[lookup]
	if !ok {
		return nil, proxydb.ErrNotFound
	}
	return row, nil
}

func (f *fakeStore) GetNodeKeyByLookup(_ context.Context, lookup string) (*proxydb.NodeKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	row, ok := f.nodeRows[lookup]
	if !ok {
		return nil, proxydb.ErrNotFound
	}
	return row, nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// mint generates a key and registers it in the store, returning the plain value.
func mint(t *testing.T, store *fakeStore, name string, scopes ...string) string {
	t.Helper()
	plain, row, err := Generate(name, scopes)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store.rows[row.KeyLookup] = row
	return plain
}

func TestGenerateProducesPrefixedUnguessableKey(t *testing.T) {
	plain, row, err := Generate("generation", []string{ScopeGenerate})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(plain, KeyPrefix) {
		t.Errorf("key %q lacks prefix %q", plain, KeyPrefix)
	}
	if len(plain) != len(KeyPrefix)+64 {
		t.Errorf("key length = %d, want %d", len(plain), len(KeyPrefix)+64)
	}
	if row.KeyHash == plain || strings.Contains(row.KeyHash, plain) {
		t.Error("stored hash contains the plain key")
	}
	if row.KeyLookup != LookupHash(plain) {
		t.Error("lookup column is not the sha256 of the plain key")
	}
	if row.ID == "" || !row.Active() {
		t.Errorf("row = %+v", row)
	}

	other, _, _ := Generate("generation", []string{ScopeGenerate})
	if other == plain {
		t.Error("two generated keys collided")
	}
}

func TestVerifyAcceptsMintedKey(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)

	id, err := NewVerifier(store, "").Verify(context.Background(), plain)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Name != "generation" {
		t.Errorf("name = %q", id.Name)
	}
	if !id.HasScope(ScopeGenerate) {
		t.Errorf("scopes = %v", id.Scopes)
	}
	if id.HasScope(ScopeAdmin) {
		t.Error("generate key must not carry admin")
	}
}

func TestVerifyRejectsUnknownAndTamperedKeys(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v := NewVerifier(store, "")

	for name, key := range map[string]string{
		"empty":     "",
		"unknown":   KeyPrefix + strings.Repeat("a", 64),
		"truncated": plain[:len(plain)-1],
		"garbage":   "not-a-key",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), key); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// A forged row whose lookup matches but whose hash belongs to another secret
// must not authenticate: the hash column is the authentication, not the lookup.
func TestVerifyRejectsKeyWithMatchingLookupButWrongSecret(t *testing.T) {
	store := newFakeStore()
	real := mint(t, store, "generation", ScopeGenerate)

	forged := KeyPrefix + strings.Repeat("f", 64)
	row := store.rows[LookupHash(real)]
	store.rows[LookupHash(forged)] = &proxydb.APIKey{
		ID: row.ID, Name: row.Name, KeyLookup: LookupHash(forged),
		KeyHash: row.KeyHash, Scopes: row.Scopes,
	}

	if _, err := NewVerifier(store, "").Verify(context.Background(), forged); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestVerifyRejectsDisabledKey(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "old", ScopeGenerate)
	now := time.Now()
	store.rows[LookupHash(plain)].DisabledAt = &now

	if _, err := NewVerifier(store, "").Verify(context.Background(), plain); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestVerifyPropagatesStoreErrors(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	boom := errors.New("connection refused")
	store.err = boom

	_, err := NewVerifier(store, "").Verify(context.Background(), plain)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store error (never a silent allow)", err)
	}
}

// Nothing is cached, so a key revoked in the database stops working on the
// very next request — in every replica.
func TestRevocationTakesEffectImmediately(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v := NewVerifier(store, "")

	if _, err := v.Verify(context.Background(), plain); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	now := time.Now()
	store.rows[LookupHash(plain)].DisabledAt = &now

	if _, err := v.Verify(context.Background(), plain); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated right after revocation", err)
	}
	if store.count() != 2 {
		t.Errorf("lookups = %d, want 2 — every verification must read the row", store.count())
	}
}

func TestBootstrapKey(t *testing.T) {
	store := newFakeStore()
	v := NewVerifier(store, "s3cret-bootstrap")

	id, err := v.Verify(context.Background(), "s3cret-bootstrap")
	if err != nil {
		t.Fatalf("Verify bootstrap: %v", err)
	}
	if !id.HasScope(ScopeAdmin) || !id.HasScope(ScopeGenerate) {
		t.Errorf("bootstrap identity lacks admin power: %v", id.Scopes)
	}
	if store.count() != 0 {
		t.Error("bootstrap key hit the database")
	}

	if _, err := v.Verify(context.Background(), "s3cret-bootstrap-wrong"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// An unset bootstrap key must not make the empty string (or anything) an admin.
func TestEmptyBootstrapKeyIsNotAWildcard(t *testing.T) {
	v := NewVerifier(newFakeStore(), "")
	for _, key := range []string{"", " ", "bootstrap"} {
		if _, err := v.Verify(context.Background(), key); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Verify(%q) err = %v, want ErrUnauthenticated", key, err)
		}
	}
}

func TestVerifyScope(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v := NewVerifier(store, "")

	if _, err := v.VerifyScope(context.Background(), plain, ScopeGenerate); err != nil {
		t.Fatalf("VerifyScope(generate): %v", err)
	}
	_, err := v.VerifyScope(context.Background(), plain, ScopeAdmin)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}

	// An admin key satisfies every scope without listing them.
	adminKey := mint(t, store, "admin", ScopeAdmin)
	if _, err := v.VerifyScope(context.Background(), adminKey, ScopeGenerate); err != nil {
		t.Fatalf("admin key denied generate: %v", err)
	}
}

func TestParseBearer(t *testing.T) {
	ok := map[string]string{
		"Bearer ktms_abc": "ktms_abc",
		"bearer ktms_abc": "ktms_abc",
		"BEARER ktms_abc": "ktms_abc",
		"Bearer llm_abc":  "llm_abc",
	}
	for header, want := range ok {
		got, err := ParseBearer(header)
		if err != nil || got != want {
			t.Errorf("ParseBearer(%q) = %q, %v", header, got, err)
		}
	}
	for _, header := range []string{"", "ktms_abc", "Basic ktms_abc", "Bearer", "Bearer   "} {
		if _, err := ParseBearer(header); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("ParseBearer(%q) err = %v, want ErrUnauthenticated", header, err)
		}
	}
}

func TestNormalizeScopes(t *testing.T) {
	got, err := NormalizeScopes([]string{ScopeGenerate, " " + ScopeAdmin, ScopeGenerate})
	if err != nil {
		t.Fatalf("NormalizeScopes: %v", err)
	}
	if len(got) != 2 || got[0] != ScopeGenerate || got[1] != ScopeAdmin {
		t.Fatalf("got %v", got)
	}
	if _, err := NormalizeScopes(nil); err == nil {
		t.Error("empty scope list accepted")
	}
	if _, err := NormalizeScopes([]string{"root"}); err == nil {
		t.Error("unknown scope accepted")
	}
}

func TestVerifyIsRaceFree(t *testing.T) {
	store := newFakeStore()
	plain := mint(t, store, "generation", ScopeGenerate)
	v := NewVerifier(store, "")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), plain); err != nil {
				t.Errorf("Verify: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestNodeKeyAndLegacyNodeKeyVerification(t *testing.T) {
	store := newFakeStore()
	v := NewVerifier(store, "")

	// 1. Current prefix (ktms_node_)
	plainNodeKey, row, err := GenerateNodeKey("node-1", "node-1-id", nil, nil, 1.0, 10)
	if err != nil {
		t.Fatalf("GenerateNodeKey: %v", err)
	}
	if !strings.HasPrefix(plainNodeKey, NodeKeyPrefix) {
		t.Fatalf("plainNodeKey %q lacks prefix %q", plainNodeKey, NodeKeyPrefix)
	}
	store.nodeRows[row.KeyLookup] = row

	id, err := v.Verify(context.Background(), plainNodeKey)
	if err != nil {
		t.Fatalf("Verify ktms_node key failed: %v", err)
	}
	if id.NodeID != "node-1-id" || !id.HasScope(ScopeNode) {
		t.Errorf("unexpected identity: %+v", id)
	}

	// 2. Legacy prefix (llm_node_)
	legacyPlain := LegacyNodeKeyPrefix + strings.Repeat("e", 64)
	legacyRow := &proxydb.NodeKey{
		ID:             99,
		NodeID:         "node-legacy-id",
		Name:           "legacy-node",
		KeyLookup:      LookupHash(legacyPlain),
		KeyHash:        VerifyHash(legacyPlain),
		MaxBaseWeight:  1.0,
		MaxConcurrency: 5,
	}
	store.nodeRows[legacyRow.KeyLookup] = legacyRow

	idLegacy, err := v.Verify(context.Background(), legacyPlain)
	if err != nil {
		t.Fatalf("Verify legacy llm_node key failed: %v", err)
	}
	if idLegacy.NodeID != "node-legacy-id" || !idLegacy.HasScope(ScopeNode) {
		t.Errorf("unexpected legacy node identity: %+v", idLegacy)
	}
}
