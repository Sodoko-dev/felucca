package state_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
)

// idRe matches the canonical ID format: prefix-<26 hex>. The random half is
// the whole id — no monotonic tail, because the sandbox id is the only thing
// guarding an ingress hostname label.
var idRe = regexp.MustCompile(`^(sb|node)-[0-9a-f]{26}$`)

func TestIDFormat(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	nodeID := st.RegisterNode("h1", "1.2.3.4:9090", 4, 8192, now)
	if !idRe.MatchString(nodeID) {
		t.Errorf("node id format: %q", nodeID)
	}

	st.Lock()
	sb := st.CreateSandbox("test", "default", nodeID, 1, 256, now)
	sbID := sb.ID
	st.Unlock()

	if !idRe.MatchString(sbID) {
		t.Errorf("sandbox id format: %q", sbID)
	}
}

// The ingress label "<expose-name>--<sandbox-id>" is one DNS label: the
// longest legal expose name plus the separator plus the id must still fit in
// 63 octets, or the sandbox is unreachable over the wildcard domain.
func TestIDFitsIngressLabelBudget(t *testing.T) {
	st := state.New()
	st.Lock()
	sb := st.CreateSandbox("test", "default", "", 1, 256, 0)
	st.Unlock()

	const maxExposeName = 32
	if got := maxExposeName + len("--") + len(sb.ID); got > 63 {
		t.Errorf("ingress label budget: %d octets with id %q, want <= 63", got, sb.ID)
	}
}

// Ids are bearer capabilities, so distinct sandboxes must never share the
// random half — and a run of ids must not be reproducible from a process-time
// seed the way a math/rand stream is.
func TestIDsAreUniqueAndUnseeded(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		st := state.New()
		st.Lock()
		sb := st.CreateSandbox("x", "default", "", 1, 256, 0)
		st.Unlock()
		if seen[sb.ID] {
			t.Fatalf("duplicate id %q at draw %d", sb.ID, i)
		}
		seen[sb.ID] = true
	}
}

func TestSeqContinuity(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	// Seq no longer appears in the id, but it still advances once per
	// generated id and is persisted.
	for i := 0; i < 3; i++ {
		st.RegisterNode("host"+string(rune('a'+i)), "addr", 1, 1024, now)
	}
	if st.Seq != 3 {
		t.Errorf("seq after 3 registrations: got %d, want 3", st.Seq)
	}
}

func TestStateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")

	// Load the reference fixture.
	fixture := filepath.Join("testdata", "state.json")
	st := state.New()
	if err := st.Load(fixture); err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	// Assert fixture fields.
	if st.Seq != 7 {
		t.Errorf("seq: got %d, want 7", st.Seq)
	}
	if st.RequestCount != 1234 {
		t.Errorf("request_count: got %d, want 1234", st.RequestCount)
	}
	if len(st.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(st.Nodes))
	}
	n := st.Nodes[0]
	if n.ID != "node-bdc82e8c-1" {
		t.Errorf("node id: %q", n.ID)
	}
	if n.Hostname != "lima-kata-lab-0" {
		t.Errorf("node hostname: %q", n.Hostname)
	}
	if n.CPUs != 8 {
		t.Errorf("node cpus: %d", n.CPUs)
	}
	if n.PoolSize != 1 {
		t.Errorf("node pool_size: %d", n.PoolSize)
	}
	if len(st.Sandboxes) != 1 {
		t.Fatalf("sandboxes: got %d, want 1", len(st.Sandboxes))
	}
	sb := st.Sandboxes[0]
	if sb.ID != "sb-1a8dd244-6" {
		t.Errorf("sandbox id: %q", sb.ID)
	}
	if sb.State != model.StateRunning {
		t.Errorf("sandbox state: %q", sb.State)
	}
	if sb.ParentID != nil {
		t.Errorf("parent_id should be nil, got %q", *sb.ParentID)
	}
	if sb.IP == nil || *sb.IP != "10.231.0.3" {
		t.Errorf("sandbox ip: %v", sb.IP)
	}

	// Persist to tmp.
	if err := st.Persist(statePath); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// The state file is the whole fleet inventory — every node's address, every
	// sandbox id, the tenant each belongs to. It was written 0644, so any local
	// account on the control plane could read it; the sibling persistence paths
	// (store/sqlite.go, wg.go's private keys) were already 0600. The mode is set
	// on the temp file and carried over by the rename, so there is never a
	// world-readable window at the real path either.
	fi, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("state file mode = %#o, want 0600 (fleet inventory must not be world-readable)", got)
	}

	// Reload from tmp and check fields survive.
	st2 := state.New()
	if err := st2.Load(statePath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st2.Seq != 7 {
		t.Errorf("reloaded seq: %d", st2.Seq)
	}
	if st2.RequestCount != 1234 {
		t.Errorf("reloaded request_count: %d", st2.RequestCount)
	}
	if len(st2.Nodes) != 1 || st2.Nodes[0].ID != "node-bdc82e8c-1" {
		t.Errorf("reloaded nodes")
	}
	if len(st2.Sandboxes) != 1 || st2.Sandboxes[0].ID != "sb-1a8dd244-6" {
		t.Errorf("reloaded sandboxes")
	}
	if st2.Sandboxes[0].ParentID != nil {
		t.Error("reloaded parent_id should be nil")
	}

	// Check that the persisted JSON has explicit nulls (not omitted).
	data, _ := os.ReadFile(statePath)
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var sbs []json.RawMessage
	json.Unmarshal(raw["sandboxes"], &sbs)
	if len(sbs) == 0 {
		t.Fatal("no sandboxes in persisted JSON")
	}
	var sbMap map[string]json.RawMessage
	json.Unmarshal(sbs[0], &sbMap)
	// parent_id must be present as explicit null.
	pidRaw, ok := sbMap["parent_id"]
	if !ok {
		t.Error("parent_id key missing from persisted sandbox JSON")
	} else if string(pidRaw) != "null" {
		t.Errorf("parent_id should be null, got %s", pidRaw)
	}
	// node_id must be present and non-null (set to "node-b601d9cc-2").
	nidRaw, ok := sbMap["node_id"]
	if !ok {
		t.Error("node_id key missing")
	} else if string(nidRaw) == "null" {
		t.Error("node_id should be non-null")
	}
}

func TestExplicitNullOnSandboxWithNoNodeID(t *testing.T) {
	// A sandbox with no node_id must serialize "node_id":null (not omitted).
	sb := &model.Sandbox{
		ID:        "sb-aabbccdd-1",
		Name:      "test",
		Namespace: "default",
		NodeID:    nil,
		State:     model.StateStopped,
		VCPUs:     1,
		MemMiB:    256,
		IP:        nil,
		CreatedAt: 0,
		ParentID:  nil,
	}
	b, err := json.Marshal(sb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)

	for _, key := range []string{"node_id", "ip", "parent_id"} {
		v, ok := m[key]
		if !ok {
			t.Errorf("key %q missing from JSON", key)
		} else if string(v) != "null" {
			t.Errorf("key %q should be null, got %s", key, v)
		}
	}
}

func TestSeqContinuesFromLoaded(t *testing.T) {
	fixture := filepath.Join("testdata", "state.json")
	st := state.New()
	if err := st.Load(fixture); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Fixture has seq=7; the next generated id must advance it to 8.
	now := time.Now().Unix()
	st.RegisterNode("newhost", "addr", 1, 1024, now)
	if st.Seq != 8 {
		t.Errorf("expected seq=8 after loading seq=7, got %d", st.Seq)
	}
}

func TestUnknownStateBecomeStopped(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "s.json")
	data := `{"seq":1,"request_count":0,"nodes":[],"sandboxes":[{"id":"sb-00000001-1","name":"x","namespace":"default","node_id":null,"state":"unknownxyz","vcpus":1,"mem_mib":256,"ip":null,"created_at":0,"parent_id":null}]}`
	os.WriteFile(p, []byte(data), 0o644)

	st := state.New()
	if err := st.Load(p); err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 1 {
		t.Fatal("expected 1 sandbox")
	}
	if st.Sandboxes[0].State != model.StateStopped {
		t.Errorf("unknown state should map to stopped, got %q", st.Sandboxes[0].State)
	}
}

func TestPickNodeLowestVMCount(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	id1 := st.RegisterNode("h1", "addr1", 4, 8192, now)
	id2 := st.RegisterNode("h2", "addr2", 4, 8192, now)

	// Set vm_count via heartbeat.
	st.Heartbeat(id1, 8000, 5, 0, now)
	st.Heartbeat(id2, 8000, 2, 0, now)

	st.Lock()
	best := st.PickNode(now)
	st.Unlock()

	if best == nil {
		t.Fatal("expected a node")
	}
	if best.ID != id2 {
		t.Errorf("expected node with lower vm_count (%s), got %s", id2, best.ID)
	}
}

// Node records are keyed on hostname and appended for every unseen one. Every
// one of them is then scanned linearly under the global state lock by FindNode,
// PickNode and the registration checks, and rewritten by every SaveSnapshot. The
// list therefore must not be able to grow without bound, whatever gets past the
// server-side checks. An ALREADY-KNOWN node must still re-register when the list
// is full, or a full table would stop the real fleet from heartbeating.
//
// Every record here is freshly heartbeated, so reclamation (see
// TestFullNodeTableReclaimsOnlyProvablyDeadRecords) finds nothing to drop and the
// cap is the hard backstop it is meant to be.
func TestRegisterNodeIsCapped(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	for i := 0; i < state.MaxNodes; i++ {
		if id := st.RegisterNode(fmt.Sprintf("h%d", i), "addr", 1, 1024, now); id == "" {
			t.Fatalf("registration %d was refused below the cap", i)
		}
	}
	if id := st.RegisterNode("one-too-many", "addr", 1, 1024, now); id != "" {
		t.Errorf("registration past MaxNodes returned %q, want \"\" — the node list is unbounded", id)
	}
	st.Lock()
	n := len(st.Nodes)
	st.Unlock()
	if n != state.MaxNodes {
		t.Errorf("node records: %d, want %d", n, state.MaxNodes)
	}
	// A node already in the list re-registers regardless.
	if id := st.RegisterNode("h0", "addr2", 2, 2048, now+1); id == "" {
		t.Error("an already-registered node was refused because the table is full")
	}
}

func TestPickNodeSkipsDown(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()
	old := now - 100 // stale heartbeat → down

	id1 := st.RegisterNode("h1", "addr1", 4, 8192, old)
	id2 := st.RegisterNode("h2", "addr2", 4, 8192, now)
	_ = id1

	st.Lock()
	best := st.PickNode(now)
	st.Unlock()

	if best == nil || best.ID != id2 {
		t.Errorf("expected ready node h2, got %v", best)
	}
}

// ---- Admission runs inside the append's critical section ----

// The "one credential, one node record" pin used to be a server-side method that
// took and released the state lock on its own, then called RegisterNode, which
// took it again to append. Two acquisitions, so N concurrent registrations from
// one credential could all pass the check before any of them appended. The check
// is now a state.NodeAdmission the store runs under the SAME lock it appends
// under, which is what this asserts: what admit is shown is the list the append
// lands in, and a refusal appends nothing.
func TestRegisterNodeGuardedRunsAdmissionAgainstTheListItAppendsTo(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	var seen []int
	admitAll := func(nodes []*model.Node) (string, bool) {
		seen = append(seen, len(nodes))
		return "", true
	}
	for i := 0; i < 3; i++ {
		reg := st.RegisterNodeGuarded(fmt.Sprintf("h%d", i), "addr", 1, 1024, now, admitAll)
		if reg.ID == "" {
			t.Fatalf("registration %d refused: %q", i, reg.Refused)
		}
	}
	// Each call saw exactly the records its predecessors appended.
	for i, n := range seen {
		if n != i {
			t.Errorf("admission call %d saw %d node records, want %d — it is not reading the live list", i, n, i)
		}
	}

	// A refusal is reported as a reason and changes nothing.
	reg := st.RegisterNodeGuarded("h-refused", "addr", 1, 1024, now, func([]*model.Node) (string, bool) {
		return "no", false
	})
	if reg.ID != "" || reg.Refused != "no" {
		t.Errorf("refused registration: id=%q refused=%q, want id=\"\" refused=\"no\"", reg.ID, reg.Refused)
	}
	st.Lock()
	n := len(st.Nodes)
	st.Unlock()
	if n != 3 {
		t.Errorf("node records after a refused registration: %d, want 3 — the refusal appended anyway", n)
	}
}

// Admission runs with the state lock HELD, which is what lets it scan s.Nodes
// directly: nothing can mutate the list underneath it, and nothing else can be
// registering at the same time. Asserted by starting a second registration from
// inside the callback and requiring it to make no progress.
//
// This is the half of the invariant a single goroutine can see. The other half —
// that the lock is never released BETWEEN admission and the append it authorizes
// — is only visible to a concurrent probe, and lives in the server package as
// TestOneNodeCredentialCannotRaceInASecondNodeRecord.
func TestRegisterNodeGuardedHoldsTheLockAcrossAdmission(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	done := make(chan struct{})
	sneakedIn := false

	reg := st.RegisterNodeGuarded("h1", "addr", 1, 1024, now, func([]*model.Node) (string, bool) {
		started := make(chan struct{})
		go func() {
			close(started)
			st.RegisterNode("h2", "addr", 1, 1024, now)
			close(done)
		}()
		<-started
		select {
		case <-done:
			sneakedIn = true
		case <-time.After(250 * time.Millisecond):
			// Still blocked on the state lock, which is the point.
		}
		return "", true
	})
	if reg.ID == "" {
		t.Fatalf("registration refused: %q", reg.Refused)
	}
	// Admission returned, the append happened, the lock went back: the second
	// registration must now complete.
	<-done

	if sneakedIn {
		t.Fatal("a second registration completed while the admission check was still running: the check and the append are not one critical section, so concurrent registrations can all pass the check before any of them appends")
	}
	st.Lock()
	n := len(st.Nodes)
	st.Unlock()
	if n != 2 {
		t.Errorf("node records: %d, want 2 (the admitted one and the one that was made to wait)", n)
	}
}

// ---- Reclamation at the ceiling ----

// MaxNodes with no way down is its own outage: once the table is full every
// genuinely new node 503s forever, and the agent retries register every 5s
// without ever reaching its heartbeat loop. Reclamation is the way down, and it
// is deliberately narrow — a record has to be silent for StaleNodeTTL AND hold no
// sandbox before it can be dropped.
func TestFullNodeTableReclaimsOnlyProvablyDeadRecords(t *testing.T) {
	now := time.Now().Unix()
	ancient := now - state.StaleNodeTTL // exactly at the threshold

	t.Run("dead records make room for a new node", func(t *testing.T) {
		st := state.New()
		for i := 0; i < state.MaxNodes; i++ {
			if id := st.RegisterNode(fmt.Sprintf("h%d", i), "addr", 1, 1024, ancient); id == "" {
				t.Fatalf("registration %d was refused below the cap", i)
			}
		}
		reg := st.RegisterNodeGuarded("fresh-node", "addr", 1, 1024, now, nil)
		if reg.ID == "" {
			t.Fatal("a new node was refused although the whole table had been silent for StaleNodeTTL and held no sandboxes: the ceiling is a permanent enrollment outage")
		}
		if len(reg.Reclaimed) != state.MaxNodes {
			t.Errorf("reclaimed %d records, want %d", len(reg.Reclaimed), state.MaxNodes)
		}
		st.Lock()
		n := len(st.Nodes)
		st.Unlock()
		if n != 1 {
			t.Errorf("node records after reclamation: %d, want 1", n)
		}
	})

	t.Run("a node that is merely rebooting is never reclaimed", func(t *testing.T) {
		st := state.New()
		// One second short of the threshold — and StaleNodeTTL is ~120,000
		// missed heartbeats, so every real reboot lands here.
		rebooting := now - state.StaleNodeTTL + 1
		for i := 0; i < state.MaxNodes; i++ {
			st.RegisterNode(fmt.Sprintf("h%d", i), "addr", 1, 1024, rebooting)
		}
		reg := st.RegisterNodeGuarded("fresh-node", "addr", 1, 1024, now, nil)
		if reg.ID != "" || len(reg.Reclaimed) != 0 {
			t.Errorf("reclaimed %d records that had been silent for less than StaleNodeTTL (id=%q): a rebooting node lost its record",
				len(reg.Reclaimed), reg.ID)
		}
		st.Lock()
		n := len(st.Nodes)
		st.Unlock()
		if n != state.MaxNodes {
			t.Errorf("node records: %d, want %d untouched", n, state.MaxNodes)
		}
	})

	t.Run("a record that still owns a sandbox is never reclaimed", func(t *testing.T) {
		st := state.New()
		var ids []string
		for i := 0; i < state.MaxNodes; i++ {
			ids = append(ids, st.RegisterNode(fmt.Sprintf("h%d", i), "addr", 1, 1024, ancient))
		}
		// Every record is old enough, but each one still carries a workload:
		// the node id is how hearthd finds the address to reach that VM.
		st.Lock()
		for _, id := range ids {
			st.CreateSandbox("sb", "default", id, 1, 256, ancient)
		}
		st.Unlock()

		reg := st.RegisterNodeGuarded("fresh-node", "addr", 1, 1024, now, nil)
		if len(reg.Reclaimed) != 0 {
			t.Errorf("reclaimed %d records that still had sandboxes on them: those microVMs are now unreachable", len(reg.Reclaimed))
		}
		if reg.ID != "" {
			t.Error("a new record was appended past MaxNodes")
		}
		st.Lock()
		n := len(st.Nodes)
		st.Unlock()
		if n != state.MaxNodes {
			t.Errorf("node records: %d, want %d untouched", n, state.MaxNodes)
		}
	})

	t.Run("only the dead half goes", func(t *testing.T) {
		st := state.New()
		live := map[string]bool{}
		for i := 0; i < state.MaxNodes; i++ {
			hb := ancient
			if i%2 == 0 {
				hb = now
			}
			id := st.RegisterNode(fmt.Sprintf("h%d", i), "addr", 1, 1024, hb)
			if i%2 == 0 {
				live[id] = true
			}
		}
		reg := st.RegisterNodeGuarded("fresh-node", "addr", 1, 1024, now, nil)
		if reg.ID == "" {
			t.Fatal("a new node was refused although half the table was provably dead")
		}
		st.Lock()
		defer st.Unlock()
		for _, n := range st.Nodes {
			if n.LastHB == ancient {
				t.Fatalf("a stale record survived: %s", n.ID)
			}
		}
		for id := range live {
			if st.FindNode(id) == nil {
				t.Fatalf("a heartbeating node was reclaimed: %s", id)
			}
		}
		if len(st.Nodes) != len(live)+1 {
			t.Errorf("node records after reclamation: %d, want %d", len(st.Nodes), len(live)+1)
		}
	})
}
