package store_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

func open(t *testing.T) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// poisonScan swaps tbl for a view over the same rows whose WHERE clause raises
// a SQLite error on the row named "poison" and on no other. Reading the view
// therefore hands back the earlier rows and then dies mid-iteration — the
// shape of a driver/IO failure, SQLITE_BUSY, or on-disk corruption during a
// startup scan.
func poisonScan(t *testing.T, path, tbl, nameCol string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	for _, stmt := range []string{
		fmt.Sprintf(`ALTER TABLE %s RENAME TO %s_rows`, tbl, tbl),
		fmt.Sprintf(`CREATE VIEW %s AS SELECT * FROM %s_rows
		             WHERE %s <> 'poison' OR json_extract(%s, '$.x') IS NULL`, tbl, tbl, nameCol, nameCol),
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("poison %s: %v", tbl, err)
		}
	}
}

func strp(s string) *string { return &s }

func TestSnapshotRoundTrip(t *testing.T) {
	db := open(t)

	empty, err := db.Empty()
	if err != nil || !empty {
		t.Fatalf("fresh db Empty() = %v, %v; want true, nil", empty, err)
	}

	st := state.New()
	st.Seq = 7
	st.RequestCount = 42
	st.Nodes = append(st.Nodes, &model.Node{
		ID: "node-1", Hostname: "h1", Addr: "1.2.3.4:9090",
		CPUs: 8, MemTotalMiB: 16000, MemFreeMiB: 12000, VMCount: 3, PoolSize: 1, LastHB: 100,
	})
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{
		ID: "sb-1", Name: "one", Namespace: "default", NodeID: strp("node-1"),
		State: model.StateRunning, VCPUs: 2, MemMiB: 512, IP: strp("10.231.0.2"),
		CreatedAt: 123, TenantID: "tn-aa",
		Exposes:           []model.Expose{{Name: "odoo", GuestPort: 8069, NodePort: 20001}},
		AllowDynamicPorts: true,
		Template:          "odoo-v18",
		DiskGB:            8,
	}, &model.Sandbox{
		ID: "sb-2", Name: "two", Namespace: "ns", NodeID: nil,
		State: model.StateSleeping, VCPUs: 1, MemMiB: 256, IP: nil,
		CreatedAt: 456, ParentID: strp("sb-1"),
	})

	if err := db.SaveSnapshot(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	empty, _ = db.Empty()
	if empty {
		t.Fatal("Empty() = true after SaveSnapshot")
	}

	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Seq != 7 || got.RequestCount != 42 {
		t.Errorf("counters: seq=%d rc=%d", got.Seq, got.RequestCount)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "node-1" || got.Nodes[0].MemFreeMiB != 12000 {
		t.Errorf("nodes: %+v", got.Nodes)
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("sandboxes: %d", len(got.Sandboxes))
	}
	var sb1, sb2 *model.Sandbox
	for _, sb := range got.Sandboxes {
		switch sb.ID {
		case "sb-1":
			sb1 = sb
		case "sb-2":
			sb2 = sb
		}
	}
	if sb1 == nil || sb1.TenantID != "tn-aa" || sb1.IP == nil || *sb1.IP != "10.231.0.2" || sb1.State != model.StateRunning {
		t.Errorf("sb-1: %+v", sb1)
	}
	// Ingress state must survive the snapshot (a restart that drops it
	// leaves dead routes + orphaned agent DNAT rules).
	if sb1 != nil && (len(sb1.Exposes) != 1 || sb1.Exposes[0] != (model.Expose{Name: "odoo", GuestPort: 8069, NodePort: 20001}) || !sb1.AllowDynamicPorts) {
		t.Errorf("sb-1 ingress: exposes=%+v dyn=%v", sb1.Exposes, sb1.AllowDynamicPorts)
	}
	if sb2 != nil && (len(sb2.Exposes) != 0 || sb2.AllowDynamicPorts) {
		t.Errorf("sb-2 ingress should be empty: %+v", sb2.Exposes)
	}
	// Template/disk state must survive too (dropping it corrupts disk-quota
	// accounting after a restart).
	if sb1 != nil && (sb1.Template != "odoo-v18" || sb1.DiskGB != 8) {
		t.Errorf("sb-1 template/disk: %q/%d", sb1.Template, sb1.DiskGB)
	}
	if sb2 != nil && (sb2.Template != "" || sb2.DiskGB != 0) {
		t.Errorf("sb-2 template/disk should be zero: %q/%d", sb2.Template, sb2.DiskGB)
	}
	if sb2 == nil || sb2.TenantID != "" || sb2.IP != nil || sb2.ParentID == nil || *sb2.ParentID != "sb-1" {
		t.Errorf("sb-2: %+v", sb2)
	}

	// Second snapshot replaces, never accumulates.
	st.Sandboxes = st.Sandboxes[:1]
	if err := db.SaveSnapshot(st); err != nil {
		t.Fatalf("save2: %v", err)
	}
	got2 := state.New()
	if err := db.LoadInto(got2); err != nil {
		t.Fatalf("load2: %v", err)
	}
	if len(got2.Sandboxes) != 1 {
		t.Errorf("after replace: %d sandboxes", len(got2.Sandboxes))
	}
}

// A scan that dies part-way through must surface as an error. SaveSnapshot is
// a full delete-and-reinsert, so a short load reported as success takes every
// unread sandbox with it on the very next mutation while its microVM keeps
// running as an orphan.
func TestLoadIntoShortScanIsAnError(t *testing.T) {
	for _, tc := range []struct {
		table, nameCol string
		fill           func(st *state.State, name string)
	}{
		{"sandboxes", "name", func(st *state.State, name string) {
			st.Sandboxes = append(st.Sandboxes, &model.Sandbox{
				ID: "sb-" + name, Name: name, Namespace: "default",
				State: model.StateRunning, VCPUs: 1, MemMiB: 256, CreatedAt: 1,
			})
		}},
		{"nodes", "hostname", func(st *state.State, name string) {
			st.Nodes = append(st.Nodes, &model.Node{
				ID: "node-" + name, Hostname: name, Addr: "1.2.3.4:9090",
				CPUs: 4, MemTotalMiB: 8000, MemFreeMiB: 4000,
			})
		}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			db, err := store.OpenSQLite(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()

			st := state.New()
			for _, name := range []string{"ok-1", "ok-2", "poison", "ok-3"} {
				tc.fill(st, name)
			}
			if err := db.SaveSnapshot(st); err != nil {
				t.Fatalf("save: %v", err)
			}
			poisonScan(t, path, tc.table, tc.nameCol)

			got := state.New()
			if err := db.LoadInto(got); err == nil {
				t.Fatalf("LoadInto reported success on a broken scan; loaded %d nodes / %d sandboxes of 4",
					len(got.Nodes), len(got.Sandboxes))
			}
		})
	}
}

func TestTenantsAndKeys(t *testing.T) {
	db := open(t)

	tn := &store.Tenant{ID: "tn-1", Name: "acme", MaxSandboxes: 2, CreatedAt: 1}
	if err := db.CreateTenant(tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := db.CreateTenant(&store.Tenant{ID: "tn-2", Name: "acme", CreatedAt: 2}); err == nil {
		t.Error("duplicate tenant name accepted")
	}

	got, err := db.GetTenant("tn-1")
	if err != nil || got == nil || got.Name != "acme" || got.MaxSandboxes != 2 {
		t.Fatalf("get tenant: %+v, %v", got, err)
	}
	if missing, _ := db.GetTenant("tn-x"); missing != nil {
		t.Error("GetTenant unknown id should be nil")
	}
	ts, _ := db.ListTenants()
	if len(ts) != 1 {
		t.Errorf("list tenants: %d", len(ts))
	}

	key := &store.APIKey{ID: "key-1", TenantID: "tn-1", KeyHash: "abc123", Prefix: "hearth_sk_xy", CreatedAt: 1}
	if err := db.CreateKey(key); err != nil {
		t.Fatalf("create key: %v", err)
	}
	tid, err := db.LookupKeyByHash("abc123")
	if err != nil || tid != "tn-1" {
		t.Fatalf("lookup: %q, %v", tid, err)
	}
	if tid, _ := db.LookupKeyByHash("nope"); tid != "" {
		t.Error("unknown hash should resolve to empty")
	}

	ok, err := db.RevokeKey("key-1", 99)
	if err != nil || !ok {
		t.Fatalf("revoke: %v, %v", ok, err)
	}
	if tid, _ := db.LookupKeyByHash("abc123"); tid != "" {
		t.Error("revoked key still resolves")
	}
	if ok, _ := db.RevokeKey("key-1", 100); ok {
		t.Error("double revoke should report not found")
	}
}

// An expired key must stop authenticating on its own, with no call-site
// change: expiry is what bounds the window a leaked key is useful in.
func TestAPIKeyExpiry(t *testing.T) {
	db := open(t)
	if err := db.CreateTenant(&store.Tenant{ID: "tn-1", Name: "acme", CreatedAt: 1}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	now := time.Now().Unix()
	for _, k := range []*store.APIKey{
		{ID: "key-never", TenantID: "tn-1", KeyHash: "hash-never", Prefix: "hearth_sk_nv", CreatedAt: now},
		{ID: "key-live", TenantID: "tn-1", KeyHash: "hash-live", Prefix: "hearth_sk_lv", CreatedAt: now, ExpiresAt: now + 3600},
		{ID: "key-expired", TenantID: "tn-1", KeyHash: "hash-expired", Prefix: "hearth_sk_ex", CreatedAt: now - 7200, ExpiresAt: now - 1},
	} {
		if err := db.CreateKey(k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}

	if tid, err := db.LookupKeyByHash("hash-never"); err != nil || tid != "tn-1" {
		t.Errorf("expires_at 0 should never expire: %q, %v", tid, err)
	}
	if tid, err := db.LookupKeyByHash("hash-live"); err != nil || tid != "tn-1" {
		t.Errorf("unexpired key: %q, %v", tid, err)
	}
	if tid, _ := db.LookupKeyByHash("hash-expired"); tid != "" {
		t.Errorf("expired key still authenticates as %q", tid)
	}

	// Expiry is not revocation: the operator can still burn the row.
	id, ok, err := db.RevokeKeyByHash("hash-expired", now)
	if err != nil || !ok || id != "key-expired" {
		t.Errorf("revoke expired key: %q, %v, %v", id, ok, err)
	}
}

// ListKeys + RevokeKeyByHash are the recovery path for a leaked secret: the
// key id is shown once at creation, so without them a key whose id was lost
// can only be revoked with raw SQL against hearth.db.
func TestListKeysAndRevokeByHash(t *testing.T) {
	db := open(t)
	for _, tn := range []*store.Tenant{
		{ID: "tn-1", Name: "acme", CreatedAt: 1},
		{ID: "tn-2", Name: "other", CreatedAt: 1},
	} {
		if err := db.CreateTenant(tn); err != nil {
			t.Fatalf("create tenant %s: %v", tn.ID, err)
		}
	}
	// Year 2100: far enough out that the live-key assertions below are about
	// listing and revocation, not about expiry.
	const farFuture int64 = 4102444800
	for _, k := range []*store.APIKey{
		{ID: "key-old", TenantID: "tn-1", KeyHash: "hash-old", Prefix: "hearth_sk_od", CreatedAt: 10},
		{ID: "key-new", TenantID: "tn-1", KeyHash: "hash-new", Prefix: "hearth_sk_nw", CreatedAt: 20, ExpiresAt: farFuture},
		{ID: "key-other", TenantID: "tn-2", KeyHash: "hash-other", Prefix: "hearth_sk_ot", CreatedAt: 30},
	} {
		if err := db.CreateKey(k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}

	keys, err := db.ListKeys("tn-1")
	if err != nil || len(keys) != 2 {
		t.Fatalf("list keys: %d, %v", len(keys), err)
	}
	if keys[0].ID != "key-new" || keys[1].ID != "key-old" {
		t.Errorf("list order: [%s %s]; want newest first", keys[0].ID, keys[1].ID)
	}
	if keys[0].ExpiresAt != farFuture || keys[1].ExpiresAt != 0 {
		t.Errorf("expires_at: %d, %d; want %d, 0", keys[0].ExpiresAt, keys[1].ExpiresAt, farFuture)
	}
	for _, k := range keys {
		if k.KeyHash != "" {
			t.Errorf("%s: listing leaked the key hash %q", k.ID, k.KeyHash)
		}
		if k.RevokedAt != nil {
			t.Errorf("%s: revoked_at should be nil, got %d", k.ID, *k.RevokedAt)
		}
	}

	// Revoking by secret must burn exactly the presented key, and no other
	// tenant's rows may be visible or reachable through the listing.
	id, ok, err := db.RevokeKeyByHash("hash-old", 55)
	if err != nil || !ok || id != "key-old" {
		t.Fatalf("revoke by hash: %q, %v, %v", id, ok, err)
	}
	if tid, _ := db.LookupKeyByHash("hash-old"); tid != "" {
		t.Error("revoked key still resolves")
	}
	if tid, _ := db.LookupKeyByHash("hash-new"); tid != "tn-1" {
		t.Error("sibling key should be untouched")
	}
	if _, ok, _ := db.RevokeKeyByHash("hash-old", 56); ok {
		t.Error("double revoke by hash should report not found")
	}
	if _, ok, err := db.RevokeKeyByHash("hash-nobody", 57); ok || err != nil {
		t.Errorf("unknown hash: %v, %v", ok, err)
	}

	keys, _ = db.ListKeys("tn-1")
	if len(keys) != 2 || keys[1].RevokedAt == nil || *keys[1].RevokedAt != 55 {
		t.Errorf("revoked key should still be listed with revoked_at: %+v", keys[1])
	}
	if other, _ := db.ListKeys("tn-2"); len(other) != 1 || other[0].ID != "key-other" {
		t.Errorf("list is not tenant-scoped: %+v", other)
	}
}

// The expires_at migration has to be safe on a database written before the
// column existed: keys issued then must keep working (0 = never expires), and
// re-opening must not fail or rewrite them.
func TestAPIKeyExpiryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE api_keys (
		   id         TEXT PRIMARY KEY,
		   tenant_id  TEXT NOT NULL REFERENCES tenants(id),
		   key_hash   TEXT NOT NULL UNIQUE,
		   prefix     TEXT NOT NULL,
		   created_at INTEGER NOT NULL,
		   revoked_at INTEGER
		 )`,
		`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, created_at, revoked_at)
		 VALUES ('key-legacy', 'tn-1', 'hash-legacy', 'hearth_sk_lg', 1, NULL)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	raw.Close()

	// Twice: the ALTER is expected to be a no-op on the second open.
	for i := 0; i < 2; i++ {
		db, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if i == 0 {
			if err := db.CreateTenant(&store.Tenant{ID: "tn-1", Name: "acme", CreatedAt: 1}); err != nil {
				t.Fatalf("create tenant: %v", err)
			}
		}
		if tid, err := db.LookupKeyByHash("hash-legacy"); err != nil || tid != "tn-1" {
			t.Errorf("open %d: pre-migration key stopped working: %q, %v", i, tid, err)
		}
		keys, err := db.ListKeys("tn-1")
		if err != nil || len(keys) != 1 || keys[0].ID != "key-legacy" || keys[0].ExpiresAt != 0 {
			t.Errorf("open %d: migrated key: %+v, %v", i, keys, err)
		}
		db.Close()
	}
}

func TestUsageEvents(t *testing.T) {
	db := open(t)
	for i, ev := range []string{"created", "slept", "woken", "deleted"} {
		err := db.AppendUsage(store.UsageEvent{
			TenantID: "tn-1", SandboxID: "sb-1", Event: ev, Vcpus: 1, MemMiB: 256, TS: int64(i),
		})
		if err != nil {
			t.Fatalf("append %s: %v", ev, err)
		}
	}
}

func TestJoinTokens(t *testing.T) {
	db := open(t)

	tok := &store.JoinToken{ID: "jt-1", TokenHash: "deadbeef", CreatedAt: 1, NodeHint: "hetzner-1"}
	if err := db.CreateJoinToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := db.CreateJoinToken(&store.JoinToken{ID: "jt-2", TokenHash: "deadbeef", CreatedAt: 2}); err == nil {
		t.Error("duplicate token hash accepted")
	}

	// Peek does not consume.
	if ok, err := db.CheckJoinToken("deadbeef", 50); err != nil || !ok {
		t.Fatalf("check: %v, %v", ok, err)
	}
	ok, err := db.ConsumeJoinToken("deadbeef", 50)
	if err != nil || !ok {
		t.Fatalf("consume: %v, %v", ok, err)
	}
	if ok, _ := db.CheckJoinToken("deadbeef", 51); ok {
		t.Error("check after consume should return false")
	}
	if ok, _ := db.ConsumeJoinToken("deadbeef", 51); ok {
		t.Error("second consume should return false")
	}
	if ok, _ := db.ConsumeJoinToken("nope", 52); ok {
		t.Error("unknown hash should return false")
	}

	// Expiry: a token older than JoinTokenTTL is neither checkable nor
	// consumable.
	old := &store.JoinToken{ID: "jt-old", TokenHash: "oldhash", CreatedAt: 100}
	if err := db.CreateJoinToken(old); err != nil {
		t.Fatalf("create old token: %v", err)
	}
	expiredNow := 100 + store.JoinTokenTTL + 1
	if ok, _ := db.CheckJoinToken("oldhash", expiredNow); ok {
		t.Error("expired token should not check")
	}
	if ok, _ := db.ConsumeJoinToken("oldhash", expiredNow); ok {
		t.Error("expired token should not consume")
	}
	// Just inside the TTL it still works.
	if ok, _ := db.ConsumeJoinToken("oldhash", 100+store.JoinTokenTTL-1); !ok {
		t.Error("unexpired token should consume")
	}
}

func TestWgPeers(t *testing.T) {
	db := open(t)

	if missing, err := db.GetWgPeerByPubKey("absent"); err != nil || missing != nil {
		t.Fatalf("GetWgPeerByPubKey absent = %+v, %v; want nil, nil", missing, err)
	}

	p1 := &store.WgPeer{PubKey: "pkB=", OverlayIP: "10.100.0.2", Hostname: "w1", CreatedAt: 1}
	if err := db.CreateWgPeer(p1); err != nil {
		t.Fatalf("create peer: %v", err)
	}
	p2 := &store.WgPeer{PubKey: "pkA=", OverlayIP: "10.100.0.3", Hostname: "w2", CreatedAt: 2}
	if err := db.CreateWgPeer(p2); err != nil {
		t.Fatalf("create peer 2: %v", err)
	}

	// Re-join with same pubkey: keeps overlay IP, updates hostname.
	rejoin := &store.WgPeer{PubKey: "pkB=", OverlayIP: "10.100.0.99", Hostname: "w1-renamed", CreatedAt: 9}
	if err := db.CreateWgPeer(rejoin); err != nil {
		t.Fatalf("rejoin upsert: %v", err)
	}
	got, err := db.GetWgPeerByPubKey("pkB=")
	if err != nil || got == nil {
		t.Fatalf("get peer: %+v, %v", got, err)
	}
	if got.OverlayIP != "10.100.0.2" || got.Hostname != "w1-renamed" || got.CreatedAt != 1 {
		t.Errorf("upsert: %+v; want IP 10.100.0.2, hostname w1-renamed, created_at 1", got)
	}

	// Distinct pubkey claiming an allocated IP must fail (unique overlay_ip).
	if err := db.CreateWgPeer(&store.WgPeer{PubKey: "pkC=", OverlayIP: "10.100.0.2", CreatedAt: 3}); err == nil {
		t.Error("duplicate overlay_ip accepted")
	}

	peers, err := db.ListWgPeers()
	if err != nil || len(peers) != 2 {
		t.Fatalf("list peers: %d, %v", len(peers), err)
	}
	if peers[0].PubKey != "pkB=" || peers[1].PubKey != "pkA=" {
		t.Errorf("list order: [%s %s]; want created_at then pubkey", peers[0].PubKey, peers[1].PubKey)
	}
}

func TestMigrationFromJSON(t *testing.T) {
	// Build a State, persist as legacy JSON, then import via Load + SaveSnapshot.
	tmp := t.TempDir()
	jsonPath := filepath.Join(tmp, "state.json")

	src := state.New()
	src.Seq = 3
	src.Sandboxes = append(src.Sandboxes, &model.Sandbox{
		ID: "sb-legacy", Name: "old", Namespace: "default",
		State: model.StateStopped, VCPUs: 1, MemMiB: 256, CreatedAt: 9,
	})
	if err := src.Persist(jsonPath); err != nil {
		t.Fatalf("legacy persist: %v", err)
	}

	db := open(t)
	imported := state.New()
	if err := imported.Load(jsonPath); err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	if err := db.SaveSnapshot(imported); err != nil {
		t.Fatalf("import save: %v", err)
	}

	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Seq != 3 || len(got.Sandboxes) != 1 || got.Sandboxes[0].ID != "sb-legacy" {
		t.Errorf("migrated state: seq=%d sandboxes=%+v", got.Seq, got.Sandboxes)
	}
}

// ---- Node credentials ----

func TestNodeCredRoundTripAndRotation(t *testing.T) {
	db := open(t)

	if c, err := db.GetNodeCred("10.100.0.2"); err != nil || c != nil {
		t.Fatalf("absent cred: got %+v err %v, want nil nil", c, err)
	}
	if err := db.PutNodeCred(&store.NodeCred{Host: "10.100.0.2", Token: "hearth_nt_one", CreatedAt: 10}); err != nil {
		t.Fatalf("put: %v", err)
	}
	c, err := db.GetNodeCred("10.100.0.2")
	if err != nil || c == nil || c.Token != "hearth_nt_one" || c.Legacy {
		t.Fatalf("get: %+v err %v", c, err)
	}
	// A re-enrollment rotates in place rather than accumulating rows.
	if err := db.PutNodeCred(&store.NodeCred{Host: "10.100.0.2", Token: "hearth_nt_two", CreatedAt: 20}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	c, err = db.GetNodeCred("10.100.0.2")
	if err != nil || c == nil || c.Token != "hearth_nt_two" || c.CreatedAt != 20 {
		t.Fatalf("after rotation: %+v err %v", c, err)
	}
}

// ListNodeCreds feeds the in-memory index that authenticates INBOUND agent
// calls, and DeleteNodeCred retires a row keyed under the old bare-host layout
// once it has been rewritten. Both are load-bearing: a listing that missed a row
// would silently stop that node authenticating, and a delete that did not delete
// would leave a second agent on the same host inheriting the first's credential.
func TestListAndDeleteNodeCreds(t *testing.T) {
	db := open(t)

	if creds, err := db.ListNodeCreds(); err != nil || len(creds) != 0 {
		t.Fatalf("empty store: %d creds err=%v", len(creds), err)
	}
	for _, c := range []*store.NodeCred{
		{Host: "10.100.0.2:9090", Token: "hearth_nt_a", CreatedAt: 10},
		{Host: "10.100.0.3:9090", Token: "hearth_nt_b", CreatedAt: 20},
		{Host: "10.100.0.4:9090", Legacy: true, CreatedAt: 30},
	} {
		if err := db.PutNodeCred(c); err != nil {
			t.Fatalf("put %s: %v", c.Host, err)
		}
	}
	creds, err := db.ListNodeCreds()
	if err != nil || len(creds) != 3 {
		t.Fatalf("list: %d creds err=%v, want 3", len(creds), err)
	}
	seen := map[string]*store.NodeCred{}
	for _, c := range creds {
		seen[c.Host] = c
	}
	if c := seen["10.100.0.2:9090"]; c == nil || c.Token != "hearth_nt_a" {
		t.Errorf("listed row lost its token: %+v", c)
	}
	if c := seen["10.100.0.4:9090"]; c == nil || !c.Legacy || c.Token != "" {
		t.Errorf("legacy row: %+v, want tokenless and flagged", c)
	}

	ok, err := db.DeleteNodeCred("10.100.0.3:9090")
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if c, err := db.GetNodeCred("10.100.0.3:9090"); err != nil || c != nil {
		t.Errorf("row survived the delete: %+v err=%v", c, err)
	}
	// Idempotent: retiring an already-retired row is not an error.
	if ok, err := db.DeleteNodeCred("10.100.0.3:9090"); err != nil || ok {
		t.Errorf("second delete: ok=%v err=%v, want false nil", ok, err)
	}
	if creds, err := db.ListNodeCreds(); err != nil || len(creds) != 2 {
		t.Errorf("after delete: %d creds err=%v, want 2", len(creds), err)
	}
}

// Grandfathering a pre-existing fleet onto the shared token has to be a
// one-shot: if a later start could re-seed from whatever is in the snapshot by
// then, an address someone registered after the upgrade would be promoted onto
// the control-plane admin token — the hole per-node credentials close.
func TestSeedLegacyNodeCredsRunsOnce(t *testing.T) {
	db := open(t)

	n, err := db.SeedLegacyNodeCreds([]string{"198.51.100.7", "198.51.100.8"}, 100)
	if err != nil || n != 2 {
		t.Fatalf("first seed: n=%d err=%v, want 2", n, err)
	}
	for _, host := range []string{"198.51.100.7", "198.51.100.8"} {
		c, err := db.GetNodeCred(host)
		if err != nil || c == nil || !c.Legacy || c.Token != "" {
			t.Errorf("seeded %s: %+v err %v, want a tokenless legacy row", host, c, err)
		}
	}

	n2, err := db.SeedLegacyNodeCreds([]string{"203.0.113.9"}, 200)
	if err != nil || n2 != 0 {
		t.Fatalf("second seed: n=%d err=%v, want 0", n2, err)
	}
	if c, err := db.GetNodeCred("203.0.113.9"); err != nil || c != nil {
		t.Errorf("host offered to a later seed: %+v err %v, want nil", c, err)
	}
}

// A fresh database grandfathers nobody, and the marker still lands so the
// first node it ever sees is not promoted onto the shared token either.
func TestSeedLegacyNodeCredsOnFreshDatabase(t *testing.T) {
	db := open(t)

	if n, err := db.SeedLegacyNodeCreds(nil, 100); err != nil || n != 0 {
		t.Fatalf("fresh seed: n=%d err=%v, want 0", n, err)
	}
	if n, err := db.SeedLegacyNodeCreds([]string{"203.0.113.9"}, 200); err != nil || n != 0 {
		t.Fatalf("seed after a fresh install: n=%d err=%v, want 0", n, err)
	}
	if c, err := db.GetNodeCred("203.0.113.9"); err != nil || c != nil {
		t.Errorf("credential on a fresh install: %+v err %v, want nil", c, err)
	}
}
