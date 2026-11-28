package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"

	"modernc.org/sqlite" // pure-Go driver: feluccad stays CGO_ENABLED=0
	sqlite3 "modernc.org/sqlite/lib"
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
  allow_dynamic_ports INTEGER NOT NULL DEFAULT 0,
  template   TEXT NOT NULL DEFAULT '',
  disk_gb    INTEGER NOT NULL DEFAULT 0,
  idle_sleep_s    INTEGER NOT NULL DEFAULT 0,
  asleep_delete_s INTEGER NOT NULL DEFAULT 0,
  last_activity   INTEGER NOT NULL DEFAULT 0,
  slept_at        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sandboxes_tenant ON sandboxes(tenant_id);
CREATE TABLE IF NOT EXISTS tenants (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  max_sandboxes INTEGER NOT NULL DEFAULT 0,
  max_vcpus     INTEGER NOT NULL DEFAULT 0,
  max_mem_mib   INTEGER NOT NULL DEFAULT 0,
  max_disk_gb   INTEGER NOT NULL DEFAULT 0,
  default_idle_sleep_s    INTEGER NOT NULL DEFAULT 0,
  default_asleep_delete_s INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL REFERENCES tenants(id),
  key_hash   TEXT NOT NULL UNIQUE,
  prefix     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
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
  ts         INTEGER NOT NULL,
  disk_gb    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_tenant_ts ON usage_events(tenant_id, ts);
CREATE TABLE IF NOT EXISTS templates (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  image         TEXT NOT NULL,
  image_sha256  TEXT NOT NULL,
  vcpus         INTEGER NOT NULL,
  mem_mib       INTEGER NOT NULL,
  disk_gb       INTEGER NOT NULL,
  image_size_gb INTEGER NOT NULL DEFAULT 0,
  pool_size     INTEGER NOT NULL,
  tenant_id     TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS join_tokens (
  id         TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  used_at    INTEGER,
  node_hint  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS node_creds (
  host       TEXT PRIMARY KEY,
  token      TEXT NOT NULL DEFAULT '',
  legacy     INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS wg_peers (
  pubkey     TEXT PRIMARY KEY,
  overlay_ip TEXT NOT NULL UNIQUE,
  hostname   TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
-- FLU-6: node ports released less than the port quarantine ago. Part of the
-- snapshot (rewritten with it), so no key: two sandboxes may have released
-- the same port of one host within the quarantine.
CREATE TABLE IF NOT EXISTS released_ports (
  host        TEXT NOT NULL,
  port        INTEGER NOT NULL,
  sandbox_id  TEXT NOT NULL,
  guest_port  INTEGER NOT NULL,
  tenant_id   TEXT NOT NULL DEFAULT '',
  released_at INTEGER NOT NULL
);
`

// SQLite implements Store on a single SQLite database (WAL mode).
//
// It holds TWO handles on the file. db is the writer: one connection, every
// INSERT/UPDATE/DELETE and every transaction. ro is read-only and serves every
// plain SELECT. In WAL mode readers do not wait for a writer, so a save that is
// slow, or that is stuck in the busy wait because another process holds the
// write lock, no longer queues key lookups, tenant reads and listings behind it
// on the single writer connection.
type SQLite struct {
	db *sql.DB
	ro *sql.DB
}

// Connection-string pragmas. They are set through the DSN rather than with
// db.Exec because database/sql may replace a connection at any time (an error,
// an idle timeout): a pragma Exec'd once applies to the connection it happened
// to run on, a DSN pragma to every connection the pool ever opens.
//
// synchronous=FULL: in WAL mode NORMAL can lose the last commits on a power cut
// (the WAL is not synced at each commit). feluccad acknowledges a create, a
// delete or a key only after the commit, so an acknowledged write must survive
// one.
const dsnPragmas = "_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"

// maxReadConns bounds the read-only pool. Readers in WAL mode run concurrently;
// a handful is plenty for the authentication and listing traffic.
const maxReadConns = 4

// OpenSQLite opens (creating if needed) the database at path and applies the
// schema. The writer pool is capped at one connection — correct for SQLite
// and for feluccad's single-process write pattern (ADR-0002) — and a second,
// read-only handle serves the reads (see SQLite).
func OpenSQLite(path string) (*SQLite, error) {
	// An in-memory database is private to the connection that opened it: the
	// writer and the read handle would be two unrelated, empty databases, and
	// every read would miss every write.
	if path == ":memory:" || strings.HasPrefix(path, "file::memory:") || strings.Contains(path, "mode=memory") {
		return nil, fmt.Errorf("open %s: an in-memory database is not supported (the writer and the read handle would be two different databases)", path)
	}
	// journal_mode is a property of the database file; it is set on the writer
	// only (a read-only connection cannot change it, and does not need to).
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&"+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	// The pragmas run when the first connection opens; surface their errors
	// here rather than at the first query.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Column migrations for pre-existing databases: CREATE TABLE IF NOT
	// EXISTS never adds columns to an existing table. "duplicate column" is
	// the already-migrated case; anything else is fatal (a snapshot written
	// without these columns would silently drop the corresponding state).
	for _, alter := range []string{
		// v4 P3 (ingress).
		`ALTER TABLE sandboxes ADD COLUMN exposes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sandboxes ADD COLUMN allow_dynamic_ports INTEGER NOT NULL DEFAULT 0`,
		// v4 P4 (templates & disk).
		`ALTER TABLE sandboxes ADD COLUMN template TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sandboxes ADD COLUMN disk_gb INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_events ADD COLUMN disk_gb INTEGER NOT NULL DEFAULT 0`,
		// v4 P5 (lifecycle policies + template visibility).
		`ALTER TABLE sandboxes ADD COLUMN idle_sleep_s INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sandboxes ADD COLUMN asleep_delete_s INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sandboxes ADD COLUMN last_activity INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sandboxes ADD COLUMN slept_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tenants ADD COLUMN default_idle_sleep_s INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tenants ADD COLUMN default_asleep_delete_s INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE templates ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		// Key expiry. Existing rows default to 0 (never expires) so upgrading
		// cannot lock an operator out of their own control plane.
		`ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// The db holds key hashes and tenant data: owner-only (also covers the
	// -wal/-shm siblings via SQLite inheriting the main file's mode).
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("chmod db file", "path", path, "err", err)
	}
	// The read handle is opened only now, after the writer has created the
	// file, the schema and the WAL. query_only makes every connection of this
	// pool refuse writes while it opens the file as a normal WAL reader (it can
	// use, and create, the -shm file like any other connection); a mode=ro URI
	// cannot always do the latter. TestReadHandleIsReadOnlyAndCarriesThePragmas
	// pins the refusal and the pragmas.
	ro, err := sql.Open("sqlite", path+"?"+dsnPragmas+"&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open read handle %s: %w", path, err)
	}
	ro.SetMaxOpenConns(maxReadConns)
	if err := ro.Ping(); err != nil {
		ro.Close()
		db.Close()
		return nil, fmt.Errorf("open read handle %s: %w", path, err)
	}
	return &SQLite{db: db, ro: ro}, nil
}

// ResetWriter closes the writer's idle connection, so the next write opens a
// fresh one. SQLite opens a file it cannot write read-only, without an error,
// and the writer pool keeps its one connection for ever: a database that was
// read-only when feluccad opened it (a read-only mount, an immutable file)
// stays read-only to this process after the operator has made it writable,
// until a restart. A connection in use is not affected; it is closed by a
// later call.
func (s *SQLite) ResetWriter() {
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxIdleConns(1)
}

// MarkStarted records the start time in meta: one small real write, the
// start-up's "can this database be written" check. Rewriting the whole
// snapshot instead would make whatever the start loaded permanent before the
// first request.
func (s *SQLite) MarkStarted(now int64) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (k, v) VALUES ('started_at', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		fmt.Sprintf("%d", now),
	)
	return err
}

// IsConstraint reports whether err is SQLite refusing a write that breaks a
// constraint (a UNIQUE name, a duplicate key): the request's conflict, not a
// database that cannot be written. Classified by the result code, not by
// the message text.
func IsConstraint(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

func (s *SQLite) Close() error {
	rerr := s.ro.Close()
	if err := s.db.Close(); err != nil {
		return err
	}
	return rerr
}

// Empty reports whether a snapshot has ever been saved.
func (s *SQLite) Empty() (bool, error) {
	var v string
	err := s.ro.QueryRow(`SELECT v FROM meta WHERE k='snapshot'`).Scan(&v)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return false, err
}

// SaveSnapshot writes the full working set, and the usage events that describe
// the transitions in it, in one transaction. Same granularity as the legacy
// state.json Persist, just transactional.
//
// It takes a Snapshot, not the State: the copy is made under the state lock by
// the caller, and the lock is NOT held here, so a save waiting out the busy
// timeout (another process holding the write lock) no longer stalls every
// request that needs the working set.
//
// The events are in the same transaction as the rows on purpose: a "slept"
// event without the sleeping row (or the reverse) would bill a sandbox for a
// state the control plane does not have.
func (s *SQLite) SaveSnapshot(snap *state.Snapshot, usage []UsageEvent) error {
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
	if _, err := tx.Exec(`DELETE FROM released_ports`); err != nil {
		return err
	}
	// The quarantined ports in the same transaction as the rows: a removed
	// route row whose port is not quarantined after a restart would hand that
	// port to the next sandbox while gateways still route the old hostname.
	for i := range snap.Quarantine {
		q := &snap.Quarantine[i]
		if _, err := tx.Exec(
			`INSERT INTO released_ports (host, port, sandbox_id, guest_port, tenant_id, released_at) VALUES (?,?,?,?,?,?)`,
			q.Host, q.Port, q.SandboxID, q.GuestPort, q.TenantID, q.ReleasedAt,
		); err != nil {
			return err
		}
	}
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		if _, err := tx.Exec(
			`INSERT INTO nodes (id, hostname, addr, cpus, mem_total_mib, mem_free_mib, vm_count, pool_size, last_heartbeat)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			n.ID, n.Hostname, n.Addr, n.CPUs, clampU64(n.MemTotalMiB), clampU64(n.MemFreeMiB), n.VMCount, n.PoolSize, n.LastHB,
		); err != nil {
			return err
		}
	}
	for i := range snap.Sandboxes {
		sb := &snap.Sandboxes[i]
		exposes := ""
		if len(sb.Exposes) > 0 {
			b, err := json.Marshal(sb.Exposes)
			if err != nil {
				return fmt.Errorf("marshal exposes for %s: %w", sb.ID, err)
			}
			exposes = string(b)
		}
		if _, err := tx.Exec(
			`INSERT INTO sandboxes (id, name, namespace, node_id, state, vcpus, mem_mib, ip, created_at, parent_id, tenant_id, exposes, allow_dynamic_ports, template, disk_gb, idle_sleep_s, asleep_delete_s, last_activity, slept_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sb.ID, sb.Name, sb.Namespace, nullable(sb.NodeID), string(sb.State),
			sb.VCPUs, clampU64(sb.MemMiB), nullable(sb.IP), sb.CreatedAt, nullable(sb.ParentID), sb.TenantID,
			exposes, sb.AllowDynamicPorts, sb.Template, sb.DiskGB,
			sb.IdleSleepS, sb.AsleepDeleteS, sb.LastActivity, sb.SleptAt,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (k, v) VALUES ('seq', ?), ('request_count', ?), ('snapshot', '1')
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		fmt.Sprintf("%d", snap.Seq), fmt.Sprintf("%d", snap.RequestCount),
	); err != nil {
		return err
	}
	if err := appendUsageTx(tx, usage); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadInto populates the in-memory state from the last snapshot. Every scan
// checks rows.Err(): SaveSnapshot is a full delete-and-reinsert, so an
// iteration cut short by a driver/IO error that still reported success would
// have the next mutation permanently delete every row that was not read.
func (s *SQLite) LoadInto(st *state.State) error {
	st.Lock()
	defer st.Unlock()

	var seq, reqCount uint64
	rows, err := s.ro.Query(`SELECT k, v FROM meta WHERE k IN ('seq','request_count')`)
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
	if err := rows.Err(); err != nil {
		return err
	}
	st.Seq = seq
	st.RequestCount = reqCount

	rows, err = s.ro.Query(`SELECT id, hostname, addr, cpus, mem_total_mib, mem_free_mib, vm_count, pool_size, last_heartbeat FROM nodes`)
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
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.ro.Query(`SELECT id, name, namespace, node_id, state, vcpus, mem_mib, ip, created_at, parent_id, tenant_id, exposes, allow_dynamic_ports, template, disk_gb, idle_sleep_s, asleep_delete_s, last_activity, slept_at FROM sandboxes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		sb := &model.Sandbox{}
		var nodeID, ip, parentID sql.NullString
		var stateStr, exposes string
		if err := rows.Scan(&sb.ID, &sb.Name, &sb.Namespace, &nodeID, &stateStr, &sb.VCPUs, &sb.MemMiB, &ip, &sb.CreatedAt, &parentID, &sb.TenantID, &exposes, &sb.AllowDynamicPorts, &sb.Template, &sb.DiskGB, &sb.IdleSleepS, &sb.AsleepDeleteS, &sb.LastActivity, &sb.SleptAt); err != nil {
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
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.ro.Query(`SELECT host, port, sandbox_id, guest_port, tenant_id, released_at FROM released_ports`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var q model.ReleasedPort
		if err := rows.Scan(&q.Host, &q.Port, &q.SandboxID, &q.GuestPort, &q.TenantID, &q.ReleasedAt); err != nil {
			rows.Close()
			return err
		}
		st.Quarantine = append(st.Quarantine, q)
	}
	rows.Close()
	return rows.Err()
}

// ---- Tenants ----

// execer is what *sql.DB and *sql.Tx have in common for a write, so a row
// insert can run alone or as one step of a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (s *SQLite) CreateTenant(t *Tenant) error { return createTenant(s.db, t) }

func createTenant(e execer, t *Tenant) error {
	_, err := e.Exec(
		`INSERT INTO tenants (id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, default_idle_sleep_s, default_asleep_delete_s, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.MaxSandboxes, t.MaxVcpus, t.MaxMemMiB, t.MaxDiskGb, t.DefaultIdleSleepS, t.DefaultAsleepDeleteS, t.CreatedAt,
	)
	return err
}

// CreateTenantWithKey inserts a tenant and its first API key in one
// transaction. As two separate inserts, a failure of the second left a tenant
// whose only key was never stored: the name is taken, and nobody holds a
// credential for it.
func (s *SQLite) CreateTenantWithKey(t *Tenant, k *APIKey) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createTenant(tx, t); err != nil {
		return err
	}
	if err := createKey(tx, k); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) GetTenant(id string) (*Tenant, error) {
	t := &Tenant{}
	err := s.ro.QueryRow(
		`SELECT id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, default_idle_sleep_s, default_asleep_delete_s, created_at FROM tenants WHERE id=?`, id,
	).Scan(&t.ID, &t.Name, &t.MaxSandboxes, &t.MaxVcpus, &t.MaxMemMiB, &t.MaxDiskGb, &t.DefaultIdleSleepS, &t.DefaultAsleepDeleteS, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *SQLite) ListTenants() ([]*Tenant, error) {
	rows, err := s.ro.Query(`SELECT id, name, max_sandboxes, max_vcpus, max_mem_mib, max_disk_gb, default_idle_sleep_s, default_asleep_delete_s, created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tenant
	for rows.Next() {
		t := &Tenant{}
		if err := rows.Scan(&t.ID, &t.Name, &t.MaxSandboxes, &t.MaxVcpus, &t.MaxMemMiB, &t.MaxDiskGb, &t.DefaultIdleSleepS, &t.DefaultAsleepDeleteS, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- API keys ----

func (s *SQLite) CreateKey(k *APIKey) error { return createKey(s.db, k) }

func createKey(e execer, k *APIKey) error {
	_, err := e.Exec(
		`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, expires_at, revoked_at) VALUES (?,?,?,?,?,?,NULL)`,
		k.ID, k.TenantID, k.KeyHash, k.Prefix, k.CreatedAt, k.ExpiresAt,
	)
	return err
}

// LookupKeyByHash resolves a presented key to its tenant. Expiry is enforced
// here in SQL next to the revocation predicate — against the database's own
// clock, so every authentication path gets it without a call-site change.
func (s *SQLite) LookupKeyByHash(hash string) (string, error) {
	var tenantID string
	err := s.ro.QueryRow(
		`SELECT tenant_id FROM api_keys
		 WHERE key_hash=? AND revoked_at IS NULL AND (expires_at = 0 OR expires_at > unixepoch())`, hash,
	).Scan(&tenantID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// ListKeys returns a tenant's keys, newest first. key_hash is deliberately not
// selected: the listing exists so an operator can find a key to revoke, and it
// must not be able to hand back the verifier even by accident.
func (s *SQLite) ListKeys(tenantID string) ([]*APIKey, error) {
	rows, err := s.ro.Query(
		`SELECT id, tenant_id, prefix, created_at, expires_at, revoked_at FROM api_keys WHERE tenant_id=? ORDER BY created_at DESC, id DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k := &APIKey{}
		var revokedAt sql.NullInt64
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Prefix, &k.CreatedAt, &k.ExpiresAt, &revokedAt); err != nil {
			return nil, err
		}
		k.KeyHash = ""
		k.RevokedAt = intPtr(revokedAt)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQLite) RevokeKey(id string, now int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeKeyByHash revokes by the hash of the presented secret and reports the
// id it burned, so a leaked key can be killed by whoever holds the leak. An
// already-expired key is still revocable — expiry is not a revocation record.
func (s *SQLite) RevokeKeyByHash(hash string, now int64) (string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRow(`SELECT id FROM api_keys WHERE key_hash=? AND revoked_at IS NULL`, hash).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=?`, now, id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return id, true, nil
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
	err := s.ro.QueryRow(
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
	return consumeJoinToken(s.db, hash, now)
}

func consumeJoinToken(e execer, hash string, now int64) (bool, error) {
	res, err := e.Exec(
		`UPDATE join_tokens SET used_at=? WHERE token_hash=? AND used_at IS NULL AND created_at > ?`,
		now, hash, now-JoinTokenTTL,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// EnrollNodeCred consumes a join token and stores the node credential it buys
// in one transaction. Done as two writes, either order loses something: a
// credential stored before the consume fails leaves a rotated credential and a
// token still redeemable; a token consumed before the credential write fails
// burns the operator's one-time token for nothing. consumed is false (and
// nothing is written) when the token is unknown, used or expired.
func (s *SQLite) EnrollNodeCred(c *NodeCred, tokenHash string, now int64) (consumed bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := consumeJoinToken(tx, tokenHash, now)
	if err != nil || !ok {
		return false, err
	}
	if err := putNodeCred(tx, c); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ---- Node credentials ----

func (s *SQLite) GetNodeCred(host string) (*NodeCred, error) {
	c := &NodeCred{}
	err := s.ro.QueryRow(
		`SELECT host, token, legacy, created_at FROM node_creds WHERE host=?`, host,
	).Scan(&c.Host, &c.Token, &c.Legacy, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// PutNodeCred upserts by host: a re-enrollment rotates the node's token.
func (s *SQLite) PutNodeCred(c *NodeCred) error { return putNodeCred(s.db, c) }

func putNodeCred(e execer, c *NodeCred) error {
	_, err := e.Exec(
		`INSERT INTO node_creds (host, token, legacy, created_at) VALUES (?,?,?,?)
		 ON CONFLICT(host) DO UPDATE SET token=excluded.token, legacy=excluded.legacy, created_at=excluded.created_at`,
		c.Host, c.Token, c.Legacy, c.CreatedAt,
	)
	return err
}

// DeleteNodeCred removes one credential row (idempotent: false when absent).
func (s *SQLite) DeleteNodeCred(host string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM node_creds WHERE host=?`, host)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListNodeCreds returns every credential row, oldest first.
func (s *SQLite) ListNodeCreds() ([]*NodeCred, error) {
	rows, err := s.ro.Query(`SELECT host, token, legacy, created_at FROM node_creds ORDER BY created_at, host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeCred
	for rows.Next() {
		c := &NodeCred{}
		if err := rows.Scan(&c.Host, &c.Token, &c.Legacy, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SeedLegacyNodeCreds writes one Legacy row per host, once per database. The
// meta marker is set even when hosts is empty, so a fresh install grandfathers
// nobody and every node it ever enrolls needs its own credential.
func (s *SQLite) SeedLegacyNodeCreds(hosts []string, now int64) (int, error) {
	var marker string
	err := s.ro.QueryRow(`SELECT v FROM meta WHERE k='node_creds_seeded'`).Scan(&marker)
	if err == nil {
		return 0, nil // already ran; a second pass would re-grandfather
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	seeded := 0
	for _, host := range hosts {
		if host == "" {
			continue
		}
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO node_creds (host, token, legacy, created_at) VALUES (?,'',1,?)`,
			host, now,
		)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			seeded++
		}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO meta (k, v) VALUES ('node_creds_seeded', ?)`, fmt.Sprintf("%d", now),
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seeded, nil
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
	err := s.ro.QueryRow(
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
	rows, err := s.ro.Query(`SELECT pubkey, overlay_ip, hostname, created_at FROM wg_peers ORDER BY created_at, pubkey`)
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

// ---- Templates ----

func (s *SQLite) CreateTemplate(t *Template) error {
	_, err := s.db.Exec(
		`INSERT INTO templates (id, name, image, image_sha256, vcpus, mem_mib, disk_gb, image_size_gb, pool_size, tenant_id, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Image, t.ImageSHA256, t.Vcpus, clampU64(t.MemMiB), t.DiskGB, t.ImageSizeGB, t.PoolSize, t.TenantID, t.CreatedAt,
	)
	return err
}

func (s *SQLite) GetTemplateByName(name string) (*Template, error) {
	t := &Template{}
	err := s.ro.QueryRow(
		`SELECT id, name, image, image_sha256, vcpus, mem_mib, disk_gb, image_size_gb, pool_size, tenant_id, created_at FROM templates WHERE name=?`, name,
	).Scan(&t.ID, &t.Name, &t.Image, &t.ImageSHA256, &t.Vcpus, &t.MemMiB, &t.DiskGB, &t.ImageSizeGB, &t.PoolSize, &t.TenantID, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *SQLite) ListTemplates() ([]*Template, error) {
	rows, err := s.ro.Query(`SELECT id, name, image, image_sha256, vcpus, mem_mib, disk_gb, image_size_gb, pool_size, tenant_id, created_at FROM templates ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Template
	for rows.Next() {
		t := &Template{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Image, &t.ImageSHA256, &t.Vcpus, &t.MemMiB, &t.DiskGB, &t.ImageSizeGB, &t.PoolSize, &t.TenantID, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteTemplate(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM templates WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- Usage events ----

func (s *SQLite) AppendUsage(e UsageEvent) error { return appendUsage(s.db, e) }

func appendUsage(x execer, e UsageEvent) error {
	_, err := x.Exec(
		`INSERT INTO usage_events (tenant_id, sandbox_id, event, vcpus, mem_mib, ts, disk_gb) VALUES (?,?,?,?,?,?,?)`,
		e.TenantID, e.SandboxID, e.Event, e.Vcpus, clampU64(e.MemMiB), e.TS, e.DiskGB,
	)
	return err
}

// appendUsageTx inserts events in order inside the caller's transaction.
func appendUsageTx(tx *sql.Tx, events []UsageEvent) error {
	for _, e := range events {
		if err := appendUsage(tx, e); err != nil {
			return err
		}
	}
	return nil
}

// AppendUsageBatch writes events in one transaction: all of them or none.
// It is how queued exec rows reach the database — one commit per flush
// instead of one synchronous write per exec request.
func (s *SQLite) AppendUsageBatch(events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendUsageTx(tx, events); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) ListUsage(tenantID string, from, upTo int64) ([]UsageEvent, error) {
	// id tiebreaks same-second transitions (created+started in one second
	// must replay in insert order or the interval folding misorders them).
	// Pre-window 'exec' rows are excluded — they only ever feed an in-window
	// counter, so loading the tenant's whole exec history would be wasteful.
	rows, err := s.ro.Query(
		`SELECT tenant_id, sandbox_id, event, vcpus, mem_mib, ts, disk_gb
		 FROM usage_events
		 WHERE tenant_id=? AND ts<=? AND (event != 'exec' OR ts >= ?)
		 ORDER BY ts, id`,
		tenantID, upTo, from,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		if err := rows.Scan(&e.TenantID, &e.SandboxID, &e.Event, &e.Vcpus, &e.MemMiB, &e.TS, &e.DiskGB); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) PruneUsage(before int64) (int64, error) {
	// Prune ONLY the high-volume per-exec counter rows. Lifecycle transition
	// events (created/started/.../deleted) are the interval skeleton the usage
	// fold replays — deleting the 'created'/'woken' anchor of a sandbox still
	// running past the retention window would make its interval un-openable and
	// silently zero its billable hours. Transitions are bounded by lifecycle
	// churn (tiny next to exec rate), so keeping them costs little.
	res, err := s.db.Exec(`DELETE FROM usage_events WHERE ts < ? AND event = 'exec'`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- helpers ----

// clampU64 binds an unsigned value into SQLite's signed 64-bit INTEGER.
// database/sql refuses a uint64 with the high bit set outright, and one such
// value in memory (a heartbeat's mem_free_mib, say) would then fail every
// snapshot save that rewrites it — forever, since the value stays in memory.
// Clamping keeps a nonsensical value from taking the store down; the handlers
// bound these fields long before they get this large.
func clampU64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

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

func intPtr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}
