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

// JoinTokenTTL is how long (seconds) an unused join token stays redeemable.
// Tokens are operational credentials handed to provisioning scripts — a day
// bounds the window a leaked/forgotten one can be replayed in.
const JoinTokenTTL int64 = 86400

// JoinToken is a one-time credential for enrolling a worker node.
type JoinToken struct {
	ID        string `json:"id"`
	TokenHash string `json:"-"` // sha256 hex of the secret; full token shown once at creation
	CreatedAt int64  `json:"created_at"`
	UsedAt    *int64 `json:"used_at"`
	NodeHint  string `json:"node_hint"` // optional operator label, e.g. "hetzner-1"
}

// WgPeer is an enrolled worker's WireGuard identity + overlay address.
type WgPeer struct {
	PubKey    string `json:"pub_key"`    // base64 wg public key, unique
	OverlayIP string `json:"overlay_ip"` // allocated overlay address, unique, e.g. "10.100.0.2"
	Hostname  string `json:"hostname"`
	CreatedAt int64  `json:"created_at"`
}

// Template is a reusable sandbox recipe (v4 P4): a rootfs image plus the
// default shape sandboxes created from it get. Image is an image name — the
// file images/<image>.ext4 in hearthd's images dir. Captured templates have
// Image == Name; registered ones may reference a pre-provisioned image like
// "ubuntu-base". PoolSize is the per-node warm-pool target for this template
// (0 = no warm pool).
type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	ImageSHA256 string `json:"image_sha256"`
	Vcpus       uint32 `json:"vcpus"`
	MemMiB      uint64 `json:"mem_mib"`
	// DiskGB is the default sandbox disk; always >= ImageSizeGB (enforced at
	// template creation — the agent cannot shrink an image, and disk quota
	// accounting relies on DiskGB being the real footprint).
	DiskGB uint32 `json:"disk_gb"`
	// ImageSizeGB is the image file's size rounded up to whole GiB — the
	// floor for any per-sandbox disk_gb override.
	ImageSizeGB uint32 `json:"image_size_gb"`
	PoolSize    uint32 `json:"pool_size"`
	CreatedAt   int64  `json:"created_at"`
}

// UsageEvent is one append-only row per sandbox lifecycle transition —
// the raw material for any future metering/billing aggregation.
type UsageEvent struct {
	TenantID  string
	SandboxID string
	Event     string
	Vcpus     uint32
	MemMiB    uint64
	DiskGB    uint32
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

	// Join tokens (one-time, JoinTokenTTL lifetime).
	CreateJoinToken(t *JoinToken) error
	// CheckJoinToken reports whether the token is currently redeemable
	// (unused, unexpired) without consuming it.
	CheckJoinToken(hash string, now int64) (bool, error)
	// ConsumeJoinToken atomically marks the token used and returns true iff
	// it was valid, unused, and unexpired (single conditional UPDATE).
	ConsumeJoinToken(hash string, now int64) (bool, error)

	// WireGuard peers.
	// CreateWgPeer upserts by pubkey: a re-join with the same key keeps its
	// overlay IP and only refreshes the hostname.
	CreateWgPeer(p *WgPeer) error
	// GetWgPeerByPubKey returns nil, nil when the peer is absent.
	GetWgPeerByPubKey(pub string) (*WgPeer, error)
	ListWgPeers() ([]*WgPeer, error)

	// Templates (v4 P4).
	CreateTemplate(t *Template) error
	// GetTemplateByName returns nil, nil when the template is absent.
	GetTemplateByName(name string) (*Template, error)
	ListTemplates() ([]*Template, error)
	// DeleteTemplate returns false when the template was absent.
	DeleteTemplate(name string) (bool, error)

	// Usage events (append-only).
	AppendUsage(e UsageEvent) error

	Close() error
}
