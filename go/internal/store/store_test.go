package store_test

import (
	"path/filepath"
	"testing"

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
