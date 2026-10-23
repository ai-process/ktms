package nodes

import "time"

// maxCooldown bounds the slot hold after one long request, so a node at a
// partial level never sits out for longer than this.
const maxCooldown = 15 * time.Minute

// capacityOf is the node's schedule level from its heartbeat; an agent that
// does not report one runs at 100%.
func capacityOf(hb *NodeHeartbeatData) uint32 {
	if hb == nil || hb.CapacityPercent == nil {
		return 100
	}
	return min(*hb.CapacityPercent, 100)
}

// Capacity is the share of time, 0-100, the node may spend on proxy requests.
func (n *NodeSnapshot) Capacity() uint32 {
	return capacityOf(n.Heartbeat)
}

// Slots is how many requests may run at once at the node's level.
func (n *NodeSnapshot) Slots() int32 {
	p := int64(n.Capacity())
	if p == 0 || n.Meta.MaxConcurrent <= 0 {
		return 0
	}
	return int32((int64(n.Meta.MaxConcurrent)*p + 99) / 100)
}

// share is the slots' worth of time the level allows, before rounding up.
func (n *NodeSnapshot) share() float64 {
	if n.Meta.MaxConcurrent <= 0 {
		return 0
	}
	return float64(n.Meta.MaxConcurrent) * float64(n.Capacity()) / 100
}

// Cooldown is how long a slot stays taken after a request that ran for d.
// Rounding slots up overshoots the level (one slot of one is 100%, not 50%),
// so the slot idles for the difference: a 20 s request at 1 slot, 50% -> 20 s.
func (n *NodeSnapshot) Cooldown(d time.Duration) time.Duration {
	p := n.Capacity()
	slots := n.Slots()
	if p >= 100 || slots == 0 || d <= 0 {
		return 0
	}
	cd := float64(d) * (float64(slots)/n.share() - 1)
	if cd >= float64(maxCooldown) {
		return maxCooldown
	}
	return time.Duration(cd)
}
