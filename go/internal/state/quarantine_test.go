package state_test

// FLU-6: every removal of a route row quarantines its node port, in the state
// layer, once per port; the entries expire and are deep-copied by the snapshot.

import (
	"reflect"
	"testing"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
)

// quarantineState holds one node (agent at 10.0.0.1:9090) and a sandbox of
// tenant tn-a on it with the given rows; the clock reads *now.
func quarantineState(t *testing.T, now *int64, exposes ...model.Expose) (*state.State, string) {
	t.Helper()
	st := state.New()
	st.Clock = func() int64 { return *now }
	nodeID := st.RegisterNode("w1", "http://10.0.0.1:9090", 4, 8192, 1)
	st.Lock()
	defer st.Unlock()
	sb := st.CreateSandbox("a", "default", nodeID, 1, 256, 1)
	sb.TenantID = "tn-a"
	sb.Exposes = append([]model.Expose(nil), exposes...)
	return st, sb.ID
}

func TestRemoveExposeQuarantinesOnlyTheLastRowOfAGuestPort(t *testing.T) {
	now := int64(5000)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "b", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20002},
	)
	st.Lock()
	defer st.Unlock()

	if !st.RemoveExposeLocked(id, "a") {
		t.Fatal("row a not removed")
	}
	if len(st.Quarantine) != 0 {
		t.Fatalf("removing one of two names on a guest port quarantined %+v, want nothing", st.Quarantine)
	}
	if st.RemoveExposeLocked(id, "a") {
		t.Error("a second removal of a reported a row")
	}
	now = 5001
	st.RemoveExposeLocked(id, "b")
	want := []model.ReleasedPort{{Host: "10.0.0.1", Port: 20001, SandboxID: id, GuestPort: 80, TenantID: "tn-a", ReleasedAt: 5001}}
	if !reflect.DeepEqual(st.Quarantine, want) {
		t.Fatalf("after the last row on guest port 80: %+v, want %+v", st.Quarantine, want)
	}
	if rows := st.FindSandbox(id).Exposes; len(rows) != 1 || rows[0].Name != "c" {
		t.Errorf("rows left: %+v, want only c", rows)
	}
}

func TestRemoveSandboxQuarantinesEachPortOnce(t *testing.T) {
	now := int64(7000)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "b", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20002},
	)
	st.RemoveSandbox(id) // the undo path (locks itself)
	st.Lock()
	defer st.Unlock()
	want := []model.ReleasedPort{
		{Host: "10.0.0.1", Port: 20001, SandboxID: id, GuestPort: 80, TenantID: "tn-a", ReleasedAt: 7000},
		{Host: "10.0.0.1", Port: 20002, SandboxID: id, GuestPort: 81, TenantID: "tn-a", ReleasedAt: 7000},
	}
	if !reflect.DeepEqual(st.Quarantine, want) {
		t.Fatalf("quarantine: %+v, want %+v", st.Quarantine, want)
	}
	if st.FindSandbox(id) != nil {
		t.Error("sandbox not removed")
	}
}

func TestRemoveExposesOnPortRemovesEveryRowAndQuarantinesOnce(t *testing.T) {
	now := int64(100)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "b", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20002},
	)
	st.Lock()
	defer st.Unlock()
	if n := st.RemoveExposesOnPortLocked(id, 20001); n != 2 {
		t.Errorf("removed %d rows, want 2", n)
	}
	if len(st.Quarantine) != 1 || st.Quarantine[0].Port != 20001 {
		t.Errorf("quarantine: %+v, want one entry for 20001", st.Quarantine)
	}
}

// A sandbox whose node record is gone (or that never had one) holds nothing
// any route can reach: nothing is quarantined.
func TestNoNodeNoQuarantine(t *testing.T) {
	now := int64(100)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20002},
	)
	st.Lock()
	gone := "node-gone"
	st.FindSandbox(id).NodeID = &gone
	st.RemoveExposeLocked(id, "a")
	st.RemoveSandboxLocked(id)
	sb := st.CreateSandbox("nonode", "default", "", 1, 256, 1)
	sb.NodeID = nil
	sb.Exposes = []model.Expose{{Name: "x", GuestPort: 80, NodePort: 20003}}
	st.RemoveSandboxLocked(sb.ID)
	n := len(st.Quarantine)
	st.Unlock()
	if n != 0 {
		t.Errorf("quarantine for sandboxes without a node: %d entries, want 0", n)
	}
}

func TestQuarantineExpiresAndTheSnapshotDropsItAndCopiesDeep(t *testing.T) {
	now := int64(1000)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20002},
	)
	st.Lock()
	defer st.Unlock()
	st.RemoveExposeLocked(id, "a") // released at 1000
	now = 1500
	st.RemoveExposeLocked(id, "c") // released at 1500

	if got := st.QuarantineLocked(1899); len(got) != 2 {
		t.Errorf("at 899 s: %d active, want 2", len(got))
	}
	if got := st.QuarantineLocked(1900); len(got) != 1 || got[0].Port != 20002 {
		t.Errorf("at 900 s: %+v, want only 20002", got)
	}
	if left := state.QuarantineExpiresIn(st.Quarantine[0], 1100); left != 800 {
		t.Errorf("expires in: %d, want 800", left)
	}

	now = 1900
	snap := st.SnapshotLocked()
	if len(snap.Quarantine) != 1 || snap.Quarantine[0].Port != 20002 {
		t.Fatalf("snapshot quarantine: %+v, want only 20002", snap.Quarantine)
	}
	if len(st.Quarantine) != 1 {
		t.Errorf("expired entry not dropped from the working set: %+v", st.Quarantine)
	}
	st.Quarantine[0].Port = 1
	if snap.Quarantine[0].Port != 20002 {
		t.Error("the snapshot shares the quarantine slice with the working set")
	}
}

func TestUnquarantineDropsOnlyThatOwnersEntry(t *testing.T) {
	st := state.New()
	st.Quarantine = []model.ReleasedPort{
		{Host: "h", Port: 20001, SandboxID: "sb-a", GuestPort: 80, ReleasedAt: 1},
		{Host: "h", Port: 20001, SandboxID: "sb-x", GuestPort: 80, ReleasedAt: 2},
		{Host: "other", Port: 20001, SandboxID: "sb-a", GuestPort: 80, ReleasedAt: 3},
	}
	st.Lock()
	st.UnquarantineLocked("h", 20001, "sb-a", 80)
	got := append([]model.ReleasedPort(nil), st.Quarantine...)
	st.Unlock()
	if len(got) != 2 || got[0].SandboxID != "sb-x" || got[1].Host != "other" {
		t.Errorf("after unquarantine: %+v", got)
	}
}
