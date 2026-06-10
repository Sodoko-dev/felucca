package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
)

// idRe matches the canonical ID format: prefix-XXXXXXXX-N
var idRe = regexp.MustCompile(`^(sb|node)-[0-9a-f]{8}-[0-9]+$`)

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

func TestSeqContinuity(t *testing.T) {
	st := state.New()
	now := time.Now().Unix()

	// Register 3 nodes, seq should be 1,2,3.
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = st.RegisterNode("host"+string(rune('a'+i)), "addr", 1, 1024, now)
	}
	// Each id must end with a different seq.
	seen := map[string]bool{}
	for _, id := range ids {
		seqPart := regexp.MustCompile(`-(\d+)$`).FindStringSubmatch(id)
		if len(seqPart) < 2 {
			t.Errorf("no seq in id: %q", id)
			continue
		}
		if seen[seqPart[1]] {
			t.Errorf("duplicate seq %s in ids", seqPart[1])
		}
		seen[seqPart[1]] = true
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
	// Fixture has seq=7. Next id should have seq=8.
	now := time.Now().Unix()
	id := st.RegisterNode("newhost", "addr", 1, 1024, now)
	seqMatch := regexp.MustCompile(`-(\d+)$`).FindStringSubmatch(id)
	if len(seqMatch) < 2 || seqMatch[1] != "8" {
		t.Errorf("expected seq=8 after loading seq=7, got id=%q", id)
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
