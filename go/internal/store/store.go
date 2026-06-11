// Package store is the durable storage layer for hearthd. The control plane
// keeps its working set in memory (internal/state) and writes through to a
// Store; tenants, API keys, and usage events live only here.
package store

import (
	"github.com/alpham/infra-saas/hearth/internal/state"
)

// Tenant is an isolation + quota boundary. Zero quota values mean unlimited.
type Tenant struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MaxSandboxes int64  `json:"max_sandboxes"`
	MaxVcpus     int64  `json:"max_vcpus"`
	MaxMemMiB    int64  `json:"max_mem_mib"`
	MaxDiskGb    int64  `json:"max_disk_gb"`
	CreatedAt    int64  `json:"created_at"`
}

// APIKey carries only the sha256 of the secret; the full key is shown once at
// creation and never stored.
type APIKey struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	KeyHash   string `json:"-"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	RevokedAt *int64 `json:"revoked_at"`
}

// UsageEvent is one append-only row per sandbox lifecycle transition —
// the raw material for any future metering/billing aggregation.
type UsageEvent struct {
	TenantID  string
	SandboxID string
	Event     string
	Vcpus     uint32
	MemMiB    uint64
	TS        int64
}

// Store is hearthd's durable storage boundary. SaveSnapshot/LoadInto move the
// whole in-memory working set (sandboxes, nodes, counters) in one transaction —
// the same granularity the JSON file had, just transactional. Entity methods
// are row-level.
type Store interface {
	// Snapshot persistence (replaces state.json).
	SaveSnapshot(st *state.State) error
	LoadInto(st *state.State) error
	// Empty reports whether no snapshot has ever been saved (drives the
	// one-time state.json migration).
	Empty() (bool, error)

	// Tenants.
	CreateTenant(t *Tenant) error
	GetTenant(id string) (*Tenant, error)
	ListTenants() ([]*Tenant, error)

	// API keys.
	CreateKey(k *APIKey) error
	// LookupKeyByHash returns the owning tenant id for an active (non-revoked)
	// key hash, or "" when unknown/revoked.
	LookupKeyByHash(hash string) (string, error)
	RevokeKey(id string, now int64) (bool, error)

	// Usage events (append-only).
	AppendUsage(e UsageEvent) error

	Close() error
}
