// Package store is the durable storage layer for feluccad. The control plane
// keeps its working set in memory (internal/state) and writes through to a
// Store; tenants, API keys, and usage events live only here.
package store

import (
	"github.com/alpham/infra-saas/felucca/internal/state"
)

// Tenant is an isolation + quota boundary. Zero quota values mean unlimited.
// The lifecycle defaults (v4 P5.2) are seconds; 0 disables the policy for the
// tenant (sandboxes can still opt in per-sandbox).
type Tenant struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MaxSandboxes int64  `json:"max_sandboxes"`
	MaxVcpus     int64  `json:"max_vcpus"`
	MaxMemMiB    int64  `json:"max_mem_mib"`
	MaxDiskGb    int64  `json:"max_disk_gb"`
	// DefaultIdleSleepS auto-sleeps a running sandbox after this many seconds
	// without exec or ingress activity.
	DefaultIdleSleepS int64 `json:"default_idle_sleep_s"`
	// DefaultAsleepDeleteS auto-deletes a sandbox that has been sleeping this
	// many seconds.
	DefaultAsleepDeleteS int64 `json:"default_asleep_delete_s"`
	CreatedAt            int64 `json:"created_at"`
}

// APIKey carries only the sha256 of the secret; the full key is shown once at
// creation and never stored.
type APIKey struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	KeyHash   string `json:"-"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	// ExpiresAt bounds how long a leaked key stays useful: unix seconds, 0
	// means never. Enforced in LookupKeyByHash's WHERE clause so an expired
	// key stops authenticating without any caller having to check.
	ExpiresAt int64  `json:"expires_at"`
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

// NodeCred is the credential feluccad presents when it dials ONE worker agent.
//
// It is the only credential that leaves the control plane outbound, and it is
// deliberately NOT the admin API token: an agent address is attacker-supplied
// (POST /api/v1/agents/register), so every dial is a decision about where to
// deliver a bearer secret. Per-node tokens make a harvested one worth exactly
// one worker instead of the whole control plane.
//
// Host is the credential KEY, an opaque string the server layer derives from a
// node's dial target. It is "host:port" (see server.nodeCredKey): the identity a
// credential stands for is an agent endpoint, and two agents on one host are two
// nodes with two credentials. Rows written before that — keyed on the bare host —
// are still readable and are migrated to the wider key on first use.
//
// Unlike APIKey/JoinToken this stores the secret itself, not a hash: feluccad is
// the CLIENT here and has to be able to present it. A Legacy row carries no
// token and means "this address was enrolled before per-node credentials
// existed — keep using the shared token until it re-enrolls".
type NodeCred struct {
	Host      string `json:"host"`
	Token     string `json:"-"`
	Legacy    bool   `json:"legacy"`
	CreatedAt int64  `json:"created_at"`
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
// file images/<image>.ext4 in feluccad's images dir. Captured templates have
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
	// TenantID scopes catalog visibility and create-by-template (v4 P5.4,
	// ADR-0008 deferral): "" = public (every tenant sees and may use it),
	// otherwise only the owning tenant (and the admin) can.
	TenantID  string `json:"tenant_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
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

// Store is feluccad's durable storage boundary. SaveSnapshot/LoadInto move the
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
	// LookupKeyByHash returns the owning tenant id for an active (non-revoked,
	// unexpired) key hash, or "" when unknown/revoked/expired.
	LookupKeyByHash(hash string) (string, error)
	// ListKeys returns a tenant's keys newest first, never the hash. Without
	// it a key id lost from the one-shot creation response is unrecoverable
	// and the key therefore unrevokable short of raw SQL.
	ListKeys(tenantID string) ([]*APIKey, error)
	RevokeKey(id string, now int64) (bool, error)
	// RevokeKeyByHash revokes the key matching a presented secret's hash and
	// reports its id — the remediation path for a leak where the operator
	// holds the secret but not the id.
	RevokeKeyByHash(hash string, now int64) (string, bool, error)

	// Join tokens (one-time, JoinTokenTTL lifetime).
	CreateJoinToken(t *JoinToken) error
	// CheckJoinToken reports whether the token is currently redeemable
	// (unused, unexpired) without consuming it.
	CheckJoinToken(hash string, now int64) (bool, error)
	// ConsumeJoinToken atomically marks the token used and returns true iff
	// it was valid, unused, and unexpired (single conditional UPDATE).
	ConsumeJoinToken(hash string, now int64) (bool, error)

	// Node credentials (the per-node feluccad→agent bearer token).
	// GetNodeCred returns nil, nil when the host has none — which means
	// feluccad has no credential to offer that address and must dial it
	// without one.
	GetNodeCred(host string) (*NodeCred, error)
	// PutNodeCred upserts by host. A re-enrollment ROTATES the node's
	// credential: the join token that authorizes it is one-time and
	// operator-issued, so re-enrolling is also the recovery path for a worker
	// that lost its copy.
	PutNodeCred(c *NodeCred) error
	// DeleteNodeCred removes one credential row, reporting whether it existed.
	// Used to retire a row keyed on the bare host once it has been rewritten
	// under the "host:port" key — leaving it would keep a SECOND agent on the
	// same host resolving to the first one's credential, which is the whole
	// point of widening the key.
	DeleteNodeCred(host string) (bool, error)
	// ListNodeCreds returns every credential row. feluccad loads them once at
	// startup to build the in-memory token→node index that authenticates
	// INBOUND agent calls, so a heartbeat never costs a database read.
	ListNodeCreds() ([]*NodeCred, error)
	// SeedLegacyNodeCreds grandfathers a pre-existing fleet onto the shared
	// token, exactly ONCE per database, and reports how many rows it wrote
	// (0 on every later call). Callers pass the hosts of the nodes the store
	// loaded at startup — the fleet as of the previous shutdown.
	//
	// The once-only bound is the security property: feluccad offers the shared
	// admin token only to a host with a Legacy row, so if "no credential yet"
	// could mint one, registering any address would still hand the
	// control-plane key to it.
	SeedLegacyNodeCreds(hosts []string, now int64) (int, error)

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
	// ListUsage returns, in ts order, the events the usage fold needs: every
	// lifecycle TRANSITION with ts <= upTo (the aggregation needs the full
	// transition history up to the window's end to know each sandbox's state
	// at the window's start), plus the high-volume 'exec' counter rows only
	// within [from, upTo] (pre-window exec rows are never counted or folded,
	// so loading them would be pure waste). v4 P5.3.
	ListUsage(tenantID string, from, upTo int64) ([]UsageEvent, error)
	// PruneUsage deletes events older than `before`, returning the count
	// (retention, v4 P5.3).
	PruneUsage(before int64) (int64, error)

	Close() error
}
