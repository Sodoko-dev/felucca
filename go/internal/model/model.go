// Package model defines the shared data types for the Hearth control plane.
// JSON field order and null handling must match the Zig implementation exactly.
package model

import "encoding/json"

// SandboxState is the lifecycle state of a sandbox.
type SandboxState string

const (
	StateCreating SandboxState = "creating"
	StateRunning  SandboxState = "running"
	StatePaused   SandboxState = "paused"
	StateStopped  SandboxState = "stopped"
	StateSleeping SandboxState = "sleeping"
	StateError    SandboxState = "error"
)

// AllStates is the canonical order matching the Zig enum declaration order
// (used for metrics iteration).
var AllStates = []SandboxState{
	StateCreating,
	StateRunning,
	StatePaused,
	StateStopped,
	StateSleeping,
	StateError,
}

// ParseSandboxState returns the SandboxState for s, or StateStopped if unknown.
func ParseSandboxState(s string) SandboxState {
	switch SandboxState(s) {
	case StateCreating, StateRunning, StatePaused, StateStopped, StateSleeping, StateError:
		return SandboxState(s)
	}
	return StateStopped
}

// Sandbox is the in-memory representation. Pointer fields serialize as explicit
// null when nil (no omitempty). Field order matches the Zig struct declaration.
type Sandbox struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Namespace string       `json:"namespace"`
	NodeID    *string      `json:"node_id"`
	State     SandboxState `json:"state"`
	VCPUs     uint32       `json:"vcpus"`
	MemMiB    uint64       `json:"mem_mib"`
	IP        *string      `json:"ip"`
	CreatedAt int64        `json:"created_at"`
	ParentID  *string      `json:"parent_id"`
}

// Node is the in-memory representation of an agent node.
// Status is computed at marshal time via a view struct.
type Node struct {
	ID           string `json:"id"`
	Hostname     string `json:"hostname"`
	Addr         string `json:"addr"`
	CPUs         uint32 `json:"cpus"`
	MemTotalMiB  uint64 `json:"mem_total_mib"`
	MemFreeMiB   uint64 `json:"mem_free_mib"`
	VMCount      uint32 `json:"vm_count"`
	PoolSize     uint32 `json:"pool_size"`
	LastHB       int64  `json:"last_heartbeat"`
}

// NodeStatus computes whether the node is ready based on the supplied now (unix secs).
// A node is down if now-last_heartbeat > 15.
func (n *Node) NodeStatus(now int64) string {
	if now-n.LastHB > 15 {
		return "down"
	}
	return "ready"
}

// nodeView is the wire shape for a Node. It adds the computed "status" field
// in the exact key order the Zig implementation produces.
type nodeView struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	Addr        string `json:"addr"`
	CPUs        uint32 `json:"cpus"`
	MemTotalMiB uint64 `json:"mem_total_mib"`
	MemFreeMiB  uint64 `json:"mem_free_mib"`
	VMCount     uint32 `json:"vm_count"`
	PoolSize    uint32 `json:"pool_size"`
	Status      string `json:"status"`
	LastHB      int64  `json:"last_heartbeat"`
}

// MarshalWithNow marshals the node using `now` to compute status.
func (n *Node) MarshalWithNow(now int64) ([]byte, error) {
	v := nodeView{
		ID:          n.ID,
		Hostname:    n.Hostname,
		Addr:        n.Addr,
		CPUs:        n.CPUs,
		MemTotalMiB: n.MemTotalMiB,
		MemFreeMiB:  n.MemFreeMiB,
		VMCount:     n.VMCount,
		PoolSize:    n.PoolSize,
		Status:      n.NodeStatus(now),
		LastHB:      n.LastHB,
	}
	return json.Marshal(v)
}

// MarshalJSON is intentionally NOT defined on Node — always use MarshalWithNow.
// This prevents accidental marshaling without a now timestamp.
