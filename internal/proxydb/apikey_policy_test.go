package proxydb

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestAPIKeyPolicyRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	name := "policy-" + uuid.NewString()[:8]
	lookup := uuid.NewString()
	if err := d.InsertAPIKey(ctx, &APIKey{ID: uuid.NewString(), Name: name, KeyLookup: lookup, KeyHash: "h",
		Scopes: []string{"generate"}, AllowedModels: []string{"m1"}, RPMLimit: 60, TPMLimit: 100000}); err != nil {
		t.Fatal(err)
	}
	k, err := d.GetAPIKeyByLookup(ctx, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.AllowedModels, []string{"m1"}) || k.RPMLimit != 60 || k.TPMLimit != 100000 {
		t.Fatalf("minted policy = %v %d %d", k.AllowedModels, k.RPMLimit, k.TPMLimit)
	}
	if err := d.SetAPIKeyPolicy(ctx, name, nil, 0, 5, nil); err != nil {
		t.Fatal(err)
	}
	k, err = d.GetAPIKeyByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.AllowedModels) != 0 || k.RPMLimit != 0 || k.TPMLimit != 5 {
		t.Fatalf("replaced policy = %v %d %d", k.AllowedModels, k.RPMLimit, k.TPMLimit)
	}
	if err := d.SetAPIKeyPolicy(ctx, "missing-"+name, nil, 0, 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v, want ErrNotFound", err)
	}
}
