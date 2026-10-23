package nodes

import (
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

type NodeMeta struct {
	NodeID        string              `json:"node_id"`
	Name          string              `json:"name"`
	Endpoint      string              `json:"endpoint"`
	Engine        string              `json:"engine"`
	AuthToken     string              `json:"auth_token,omitempty"`
	BaseWeight    float64             `json:"base_weight"`
	MaxConcurrent int32               `json:"max_concurrent"`
	Models        []*pb.NodeModelSpec `json:"models"`
	Tags          map[string]string   `json:"tags,omitempty"`
	RegisteredAt  time.Time           `json:"registered_at"`
}

type NodeHeartbeatData struct {
	NodeID          string              `json:"node_id"`
	LastHeartbeat   time.Time           `json:"last_heartbeat"`
	Draining        bool                `json:"draining"`
	ActiveRequests  int32               `json:"active_requests"`
	QueueDepth      int32               `json:"queue_depth"`
	VRAMTotalMB     uint64              `json:"vram_total_mb"`
	VRAMFreeMB      uint64              `json:"vram_free_mb"`
	GPUUtilPercent  float64             `json:"gpu_util_percent"`
	GPUTempC        uint32              `json:"gpu_temp_c"`
	GPUModel        string              `json:"gpu_model"`
	WarmModels      []string            `json:"warm_models"`
	ModelSpecs      []*pb.NodeModelSpec `json:"model_specs,omitempty"`
	CapacityPercent *uint32             `json:"capacity_percent,omitempty"` // nil = 100
}

type NodePerfData struct {
	NodeID    string    `json:"node_id"`
	ModelID   string    `json:"model_id"`
	TPSEMA    float64   `json:"tps_ema"`
	TTFTEMAMs float64   `json:"ttft_ema_ms"`
	UpdatedAt time.Time `json:"updated_at"`
}

type NodeSnapshot struct {
	Meta      *NodeMeta
	Heartbeat *NodeHeartbeatData
	Perf      map[string]*NodePerfData // modelID -> perf
	Inflight  int32
	State     pb.NodeState
}
