// Package rediskeys builds every Redis key this service uses, so one Redis
// can host several deployments behind REDIS_KEY_PREFIX.
package rediskeys

import "fmt"

type Keys struct {
	prefix string
}

func New(prefix string) Keys {
	if prefix != "" {
		prefix += ":"
	}
	return Keys{prefix: prefix}
}

// RPM is the fixed-window request counter per client key and model.
func (k Keys) RPM(keyName, modelID string, unixMinute int64) string {
	return fmt.Sprintf("%sllmproxy:rpm:%s:%s:%d", k.prefix, keyName, modelID, unixMinute)
}

// GCRAKey is the GCRA Theoretical Arrival Time key per client key and model.
func (k Keys) GCRAKey(keyName, modelID string) string {
	return fmt.Sprintf("%sllmproxy:gcra:k:%s:%s", k.prefix, keyName, modelID)
}

// GCRAUser is the GCRA Theoretical Arrival Time key per end user and model.
func (k Keys) GCRAUser(userID, modelID string) string {
	return fmt.Sprintf("%sllmproxy:gcra:u:%s:%s", k.prefix, userID, modelID)
}

// BudgetKey is the daily token budget per client key and model (UTC day).
func (k Keys) BudgetKey(keyName, modelID, day string) string {
	return fmt.Sprintf("%sllmproxy:bud:k:%s:%s:%s", k.prefix, keyName, modelID, day)
}

// BudgetUser is the daily token budget per end user and model (UTC day).
func (k Keys) BudgetUser(userID, modelID, day string) string {
	return fmt.Sprintf("%sllmproxy:bud:u:%s:%s:%s", k.prefix, userID, modelID, day)
}

// NodeMeta holds the static & semi-static registration data of a node.
func (k Keys) NodeMeta(nodeID string) string {
	return fmt.Sprintf("%snode:meta:%s", k.prefix, nodeID)
}

// NodeHeartbeat holds the ephemeral heartbeat timestamp and dynamic telemetry.
func (k Keys) NodeHeartbeat(nodeID string) string {
	return fmt.Sprintf("%snode:heartbeat:%s", k.prefix, nodeID)
}

// NodeInflight is the ZSET of active in-flight requests on the node.
func (k Keys) NodeInflight(nodeID string) string {
	return fmt.Sprintf("%snode:inflight:%s", k.prefix, nodeID)
}

// NodePerf holds EWMA metrics (tps_ema, ttft_ema_ms) for a specific node and model.
func (k Keys) NodePerf(nodeID, modelID string) string {
	return fmt.Sprintf("%snode:perf:%s:%s", k.prefix, nodeID, modelID)
}

// NodeDrain is set when an operator requests a graceful drain of the node.
func (k Keys) NodeDrain(nodeID string) string {
	return fmt.Sprintf("%snode:drain:%s", k.prefix, nodeID)
}

// NodesSet is the SET of all known node IDs.
func (k Keys) NodesSet() string {
	return fmt.Sprintf("%snodes:all", k.prefix)
}

// NodeQueue is the model-level in-flight queue counter for predictive admission.
func (k Keys) NodeQueue(modelID string) string {
	return fmt.Sprintf("%snode:queue:%s", k.prefix, modelID)
}

