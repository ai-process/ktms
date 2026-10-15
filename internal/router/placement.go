package router

import (
	"fmt"
	"strings"

	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// Placement policy constants defining infrastructure locality preferences.
const (
	PlacementNodesFirst = "nodes_first"
	PlacementNodesOnly  = "nodes_only"
	PlacementCloudFirst = "cloud_first"
	PlacementCloudOnly  = "cloud_only"

	// Routing attributes passed in GenerateTextRequest.Attributes.
	PlacementAttribute       = "placement"
	AutoRouteAttribute       = "auto_route"
	EffortSpecifiedAttribute = "effort_specified"

	// Route modes returned in GenerateTextResponse.RouteMode.
	RouteModeDirect      = "direct"
	RouteModeOverride    = "override"
	RouteModeAutoJudge   = "auto-judge"
	RouteModeAutoDefault = "auto-default"
	RouteModeAutoEffort  = "auto-effort"
)

// NormalizePlacement standardizes placement aliases into canonical names.
// Supports aliases: nodes, node, private, private_only, self, self_hosted.
func NormalizePlacement(p string) string {
	cleaned := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "-", "_"))
	switch cleaned {
	case "nodes", "node", "nodes_only", "private", "private_only", "self", "self_hosted":
		return PlacementNodesOnly
	case "nodes_first", "private_first", "self_first":
		return PlacementNodesFirst
	case "cloud", "cloud_only":
		return PlacementCloudOnly
	case "cloud_first":
		return PlacementCloudFirst
	default:
		return ""
	}
}

// IsNodeModel reports whether the model runs on self-hosted or local compute nodes.
func IsNodeModel(m *proxydb.Model) bool {
	if m == nil {
		return false
	}
	return m.Vendor == registry.VendorLocal || m.Vendor == registry.VendorOllama
}

// ApplyPlacement reorders or prunes candidates according to the placement policy.
func ApplyPlacement(chain []*proxydb.Model, placement string) ([]*proxydb.Model, error) {
	norm := NormalizePlacement(placement)
	if norm == "" {
		return chain, nil
	}

	var nodeModels, cloudModels []*proxydb.Model
	for _, m := range chain {
		if IsNodeModel(m) {
			nodeModels = append(nodeModels, m)
		} else {
			cloudModels = append(cloudModels, m)
		}
	}

	switch norm {
	case PlacementNodesOnly:
		if len(nodeModels) == 0 {
			return nil, fmt.Errorf("%w: no nodes available for placement %s", ErrNoCapableModel, norm)
		}
		return nodeModels, nil

	case PlacementCloudOnly:
		if len(cloudModels) == 0 {
			return nil, fmt.Errorf("%w: no cloud models available for placement %s", ErrNoCapableModel, norm)
		}
		return cloudModels, nil

	case PlacementNodesFirst:
		out := make([]*proxydb.Model, 0, len(chain))
		out = append(out, nodeModels...)
		out = append(out, cloudModels...)
		return out, nil

	case PlacementCloudFirst:
		out := make([]*proxydb.Model, 0, len(chain))
		out = append(out, cloudModels...)
		out = append(out, nodeModels...)
		return out, nil

	default:
		return chain, nil
	}
}
