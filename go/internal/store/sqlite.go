package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"

	_ "modernc.org/sqlite" // pure-Go driver: hearthd stays CGO_ENABLED=0
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
  id             TEXT PRIMARY KEY,
  hostname       TEXT NOT NULL,
  addr           TEXT NOT NULL,
  cpus           INTEGER NOT NULL,
  mem_total_mib  INTEGER NOT NULL,
  mem_free_mib   INTEGER NOT NULL,
  vm_count       INTEGER NOT NULL,
  pool_size      INTEGER NOT NULL,
  last_heartbeat INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sandboxes (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  namespace  TEXT NOT NULL,
  node_id    TEXT,
  state      TEXT NOT NULL,
  vcpus      INTEGER NOT NULL,
  mem_mib    INTEGER NOT NULL,
  ip         TEXT,
  created_at INTEGER NOT NULL,
  parent_id  TEXT,
  tenant_id  TEXT NOT NULL DEFAULT '',
  exposes    TEXT NOT NULL DEFAULT '',
  allow_dynamic_ports INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sandboxes_tenant ON sandboxes(tenant_id);
CREATE TABLE IF NOT EXISTS tenants (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  max_sandboxes INTEGER NOT NULL DEFAULT 0,
  max_vcpus     INTEGER NOT NULL DEFAULT 0,
  max_mem_mib   INTEGER NOT NULL DEFAULT 0,
  max_disk_gb   INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL REFERENCES tenants(id),
  key_hash   TEXT NOT NULL UNIQUE,
  prefix     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE TABLE IF NOT EXISTS usage_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id  TEXT NOT NULL,
  sandbox_id TEXT NOT NULL,
  event      TEXT NOT NULL,
  vcpus      INTEGER NOT NULL,
  mem_mib    INTEGER NOT NULL,
  ts         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_tenant_ts ON usage_events(tenant_id, ts);
CREATE TABLE IF NOT EXISTS join_tokens (
  id         TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  used_at    INTEGER,
  node_hint  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS wg_peers (
  pubkey     TEXT PRIMARY KEY,
  overlay_ip TEXT NOT NULL UNIQUE,
  hostname   TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
`

// SQLite implements Store on a single SQLite database (WAL mode).
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the database at path and applies the
// schema. The connection pool is capped at one writer — correct for SQLite
// and for hearthd's single-process write pattern (ADR-0002).
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// v4 P3 migration for pre-P3 databases: CREATE TABLE IF NOT EXISTS never
	// adds columns to an existing table. "duplicate column" is the
	// already-migrated case; anything else is fatal (a snapshot written
	// without these columns would silently drop ingress state).
	for _, alter := range []string{
		`ALTER TABLE sandboxes ADD COLUMN exposes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sandboxes ADD COLUMN allow_dynamic_ports INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate sandboxes: %w", err)
		}
	}
	// The db holds key hashes and tenant data: owner-only (also covers the
	// -wal/-shm siblings via SQLite inheriting the main file's mode).
	if err := os.Chmod(path, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warn: chmod %s: %v\n", path, err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

// Empty reports whether a snapshot has ever been saved.
func (s *SQLite) Empty() (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='snapshot'`).Scan(&v)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return false, err
}

// SaveSnapshot writes the full working set in one transaction. Same
// granularity as the legacy state.json Persist, just transactional.
func (s *SQLite) SaveSnapshot(st *state.State) error {
	st.Lock()
	defer st.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sandboxes`); err != nil {
		return err
	}
	for _, n := range st.Nodes {
		if _, err := tx.Exec(
			`INSERT INTO nodes (id, hostname, addr, cpus, mem_total_mib, mem_free_mib, vm_count, pool_size, last_heartbeat)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			n.ID, n.Hostname, n.Addr, n.CPUs, n.MemTotalMiB, n.MemFreeMiB, n.VMCount, n.PoolSize, n.LastHB,
		); err != nil {
			return err
		}
	}
	for _, sb := range st.Sandboxes {
		exposes := ""
		if len(sb.Exposes) > 0 {
			b, err := json.Marshal(sb.Exposes)
			if err != nil {
				return fmt.Errorf("marshal exposes for %s: %w", sb.ID, err)
			}
			exposes = string(b)
		}
		if _, err := tx.Exec(
			`INSERT INTO sandboxes (id, name, namespace, node_id, state, vcpus, mem_mib, ip, created_at, parent_id, tenant_id, exposes, allow_dynamic_ports)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sb.ID, sb.Name, sb.Namespace, nullable(sb.NodeID), string(sb.State),
			sb.VCPUs, sb.MemMiB, nullable(sb.IP), sb.CreatedAt, nullable(sb.ParentID), sb.TenantID,
			exposes, sb.AllowDynamicPorts,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (k, v) VALUES ('seq', ?), ('request_count', ?), ('snapshot', '1')
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		fmt.Sprintf("%d", st.Seq), fmt.Sprintf("%d", st.RequestCount),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadInto populates the in-memory state from the last snapshot.
func (s *SQLite) LoadInto(st *state.State) error {
	st.Lock()
	defer st.Unlock()

	var seq, reqCount uint64
	rows, err := s.db.Query(`SELECT k, v FROM meta WHERE k IN ('seq','request_count')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		var n uint64
		fmt.Sscanf(v, "%d", &n)
		if k == "seq" {
			seq = n
		} else {
			reqCount = n
		}
	}
	rows.Close()
	st.Seq = seq
	st.RequestCount = reqCount

	rows, err = s.db.Query(`SELECT id, hostname, addr, cpus, mem_total_mib, mem_free_mib, vm_count, pool_size, last_heartbeat FROM nodes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		n := &model.Node{}
		if err := rows.Scan(&n.ID, &n.Hostname, &n.Addr, &n.CPUs, &n.MemTotalMiB, &n.MemFreeMiB, &n.VMCount, &n.PoolSize, &n.LastHB); err != nil {
			rows.Close()
			return err
		}
		st.Nodes = append(st.Nodes, n)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT id, name, namespace, node_id, state, vcpus, mem_mib, ip, created_at, parent_id, tenant_id, exposes, allow_dynamic_ports FROM sandboxes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		sb := &model.Sandbox{}
		var nodeID, ip, parentID sql.NullString
		var stateStr, exposes string
		if err := rows.Scan(&sb.ID, &sb.Name, &sb.Namespace, &nodeID, &stateStr, &sb.VCPUs, &sb.MemMiB, &ip, &sb.CreatedAt, &parentID, &sb.TenantID, &exposes, &sb.AllowDynamicPorts); err != nil {
			rows.Close()
			return err
		}
		sb.State = model.ParseSandboxState(stateStr)
		sb.NodeID = strPtr(nodeID)
		sb.IP = strPtr(ip)
		sb.ParentID = strPtr(parentID)
		if exposes != "" {
			if err := json.Unmarshal([]byte(exposes), &sb.Exposes); err != nil {
				rows.Close()
				return fmt.Errorf("unmarshal exposes for %s: %w", sb.ID, err)
			}
		}
		st.Sandboxes = append(st.Sandboxes, sb)
	}
	rows.Close()
	return nil
}

// ---- Tenants ----

func (s *SQLite) CreateTenant(t *Tenant) error {
	_, err := s.db.Exec(
		`INSERT INTO tenants (id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, created_at) VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.MaxSandboxes, t.MaxVcpus, t.MaxMemMiB, t.MaxDiskGb, t.CreatedAt,
	)
	return err
}

func (s *SQLite) GetTenant(id string) (*Tenant, error) {
	t := &Tenant{}
	err := s.db.QueryRow(
		`SELECT id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, created_at FROM tenants WHERE id=?`, id,
	).Scan(&t.ID, &t.Name, &t.MaxSandboxes, &t.MaxVcpus, &t.MaxMemMiB, &t.MaxDiskGb, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *SQLite) ListTenants() ([]*Tenant, error) {
	rows, err := s.db.Query(`SELECT id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tenant
	for rows.Next() {
		t := &Tenant{}
		if err := rows.Scan(&t.ID, &t.Name, &t.MaxSandboxes, &t.MaxVcpus, &t.MaxMemMiB, &t.MaxDiskGb, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- API keys ----

func (s *SQLite) CreateKey(k *APIKey) error {
	_, err := s.db.Exec(
		`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at) VALUES (?,?,?,?,?,NULL)`,
		k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt,
	)
	return err
}

func (s *SQLite) LookupKeyByHash(hash string) (string, error) {
	var tenantID string
	err := s.db.QueryRow(
		`SELECT tenant_id FROM api_keys WHERE key_hash=? AND revoked_at IS NULL`, hash,
	).Scan(&tenantID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

func (s *SQLite) RevokeKey(id string, now int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- Join tokens ----

func (s *SQLite) CreateJoinToken(t *JoinToken) error {
	_, err := s.db.Exec(
		`INSERT INTO join_tokens (id, token_hash, created_at, used_at, node_hint) VALUES (?,?,?,NULL,?)`,
		t.ID, t.TokenHash, t.CreatedAt, t.NodeHint,
	)
	return err
}

// CheckJoinToken reports whether the token is currently redeemable (unused
// and within TTL) WITHOUT consuming it — the join endpoint peeks first so a
// failed enrollment doesn't burn the one-time token.
func (s *SQLite) CheckJoinToken(hash string, now int64) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM join_tokens WHERE token_hash=? AND used_at IS NULL AND created_at > ?`,
		hash, now-JoinTokenTTL,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ConsumeJoinToken atomically marks the token used; the single conditional
// UPDATE is the race-free check-and-set (SQLite single writer, ADR-0002).
// Expired tokens (older than JoinTokenTTL) are never consumable.
func (s *SQLite) ConsumeJoinToken(hash string, now int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE join_tokens SET used_at=? WHERE token_hash=? AND used_at IS NULL AND created_at > ?`,
		now, hash, now-JoinTokenTTL,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- WireGuard peers ----

// CreateWgPeer upserts by pubkey: re-join with the same key keeps its
// overlay IP and only refreshes the hostname.
func (s *SQLite) CreateWgPeer(p *WgPeer) error {
	_, err := s.db.Exec(
		`INSERT INTO wg_peers (pubkey, overlay_ip, hostname, created_at) VALUES (?,?,?,?)
		 ON CONFLICT(pubkey) DO UPDATE SET hostname=excluded.hostname`,
		p.PubKey, p.OverlayIP, p.Hostname, p.CreatedAt,
	)
	return err
}

func (s *SQLite) GetWgPeerByPubKey(pub string) (*WgPeer, error) {
	p := &WgPeer{}
	err := s.db.QueryRow(
		`SELECT pubkey, overlay_ip, hostname, created_at FROM wg_peers WHERE pubkey=?`, pub,
	).Scan(&p.PubKey, &p.OverlayIP, &p.Hostname, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *SQLite) ListWgPeers() ([]*WgPeer, error) {
	rows, err := s.db.Query(`SELECT pubkey, overlay_ip, hostname, created_at FROM wg_peers ORDER BY created_at, pubkey`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WgPeer
	for rows.Next() {
		p := &WgPeer{}
		if err := rows.Scan(&p.PubKey, &p.OverlayIP, &p.Hostname, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Usage events ----

func (s *SQLite) AppendUsage(e UsageEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO usage_events (tenant_id, sandbox_id, event, vcpus, mem_mib, ts) VALUES (?,?,?,?,?,?)`,
		e.TenantID, e.SandboxID, e.Event, e.Vcpus, e.MemMiB, e.TS,
	)
	return err
}

// ---- helpers ----

func nullable(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func strPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}
