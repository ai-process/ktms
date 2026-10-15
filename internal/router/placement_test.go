package router

import (
	"errors"
	"testing"

	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

func TestNormalizePlacement(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nodes", PlacementNodesOnly},
		{"nodes-only", PlacementNodesOnly},
		{"nodes_only", PlacementNodesOnly},
		{"node", PlacementNodesOnly},
		{"private", PlacementNodesOnly},
		{"private-only", PlacementNodesOnly},
		{"self", PlacementNodesOnly},
		{"self-hosted", PlacementNodesOnly},
		{"nodes-first", PlacementNodesFirst},
		{"nodes_first", PlacementNodesFirst},
		{"private-first", PlacementNodesFirst},
		{"cloud", PlacementCloudOnly},
		{"cloud-only", PlacementCloudOnly},
		{"cloud_only", PlacementCloudOnly},
		{"cloud-first", PlacementCloudFirst},
		{"cloud_first", PlacementCloudFirst},
		{"unknown", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := NormalizePlacement(tt.input)
		if got != tt.want {
			t.Errorf("NormalizePlacement(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestApplyPlacement(t *testing.T) {
	node1 := &proxydb.Model{ID: "local-gemma", Vendor: registry.VendorLocal}
	node2 := &proxydb.Model{ID: "ollama-qwen", Vendor: registry.VendorOllama}
	cloud1 := &proxydb.Model{ID: "openai-gpt4o", Vendor: registry.VendorOpenAI}
	cloud2 := &proxydb.Model{ID: "anthropic-sonnet", Vendor: registry.VendorAnthropic}

	chain := []*proxydb.Model{cloud1, node1, cloud2, node2}

	t.Run("nodes_only", func(t *testing.T) {
		got, err := ApplyPlacement(chain, PlacementNodesOnly)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].ID != "local-gemma" || got[1].ID != "ollama-qwen" {
			t.Errorf("got %+v, want only nodes", got)
		}
	})

	t.Run("nodes_only_fails_when_none", func(t *testing.T) {
		_, err := ApplyPlacement([]*proxydb.Model{cloud1, cloud2}, PlacementNodesOnly)
		if !errors.Is(err, ErrNoCapableModel) {
			t.Errorf("expected ErrNoCapableModel, got %v", err)
		}
	})

	t.Run("cloud_only", func(t *testing.T) {
		got, err := ApplyPlacement(chain, PlacementCloudOnly)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].ID != "openai-gpt4o" || got[1].ID != "anthropic-sonnet" {
			t.Errorf("got %+v, want only cloud", got)
		}
	})

	t.Run("cloud_only_fails_when_none", func(t *testing.T) {
		_, err := ApplyPlacement([]*proxydb.Model{node1, node2}, PlacementCloudOnly)
		if !errors.Is(err, ErrNoCapableModel) {
			t.Errorf("expected ErrNoCapableModel, got %v", err)
		}
	})

	t.Run("nodes_first", func(t *testing.T) {
		got, err := ApplyPlacement(chain, PlacementNodesFirst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 4 || got[0].ID != "local-gemma" || got[1].ID != "ollama-qwen" || got[2].ID != "openai-gpt4o" || got[3].ID != "anthropic-sonnet" {
			t.Errorf("nodes_first ordering incorrect: %+v", got)
		}
	})

	t.Run("cloud_first", func(t *testing.T) {
		got, err := ApplyPlacement([]*proxydb.Model{node1, cloud1, node2, cloud2}, PlacementCloudFirst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 4 || got[0].ID != "openai-gpt4o" || got[1].ID != "anthropic-sonnet" || got[2].ID != "local-gemma" || got[3].ID != "ollama-qwen" {
			t.Errorf("cloud_first ordering incorrect: %+v", got)
		}
	})

	t.Run("empty_placement_returns_unmodified", func(t *testing.T) {
		got, err := ApplyPlacement(chain, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != len(chain) {
			t.Errorf("got %d models, want %d", len(got), len(chain))
		}
	})
}
