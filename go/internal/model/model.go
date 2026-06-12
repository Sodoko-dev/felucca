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
// TenantID is deliberately excluded from JSON: the v2 wire shape is frozen by
// the conformance goldens; tenancy lives in the store and in auth scoping.
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
	TenantID  string       `json:"-"`
	// Ingress (v4 P3). omitempty keeps the frozen pre-P3 wire shape for
	// sandboxes that never used ingress (the conformance goldens).
	Exposes           []Expose `json:"exposes,omitempty"`
	AllowDynamicPorts bool     `json:"allow_dynamic_ports,omitempty"`
	// Templates & disk (v4 P4). omitempty again: sandboxes created without a
	// template or custom disk keep the frozen wire shape. DiskGB == 0 means
	// "the image's own size" (the 2 GiB base image) — quota accounting uses
	// EffectiveDiskGB, never the raw field.
	Template string `json:"template,omitempty"`
	DiskGB   uint32 `json:"disk_gb,omitempty"`
}

// BaseImageDiskGB is the size of the stock base rootfs (ubuntu-base.ext4,
// built as a 2 GiB ext4 by deploy/firecracker-assets.sh). Sandboxes with
// DiskGB == 0 run on an unresized copy of their image, so this is what they
// count against disk quotas.
const BaseImageDiskGB = 2

// EffectiveDiskGB is the disk footprint used for quota accounting.
func (sb *Sandbox) EffectiveDiskGB() uint32 {
	if sb.DiskGB > 0 {
		return sb.DiskGB
	}
	return BaseImageDiskGB
}

// Expose is one published service port on a sandbox (v4 P3 ingress): the
// hostname label "<name>--<id>" routes to the owning node's NodePort, which
// the worker DNATs to the guest's GuestPort. Names are single-label,
// lowercase, and never contain "--" (keeps the separator unambiguous);
// all-digit names are reserved for dynamic port-in-hostname routing.
type Expose struct {
	Name      string `json:"name"`
	GuestPort uint16 `json:"guest_port"`
	NodePort  uint16 `json:"node_port"`
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
