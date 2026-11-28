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

	// Expired at 900 s, but kept until PortQuarantine + Margin (960 s).
	now = 1959
	if snap := st.SnapshotLocked(); len(snap.Quarantine) != 2 {
		t.Fatalf("snapshot at 959 s: %+v, want both entries kept within the margin", snap.Quarantine)
	}
	now = 1960
	snap := st.SnapshotLocked()
	if len(snap.Quarantine) != 1 || snap.Quarantine[0].Port != 20002 {
		t.Fatalf("snapshot quarantine at 960 s: %+v, want only 20002", snap.Quarantine)
	}
	if len(st.Quarantine) != 1 {
		t.Errorf("expired entry not dropped from the working set: %+v", st.Quarantine)
	}
	st.Quarantine[0].Port = 1
	if snap.Quarantine[0].Port != 20002 {
		t.Error("the snapshot shares the quarantine slice with the working set")
	}
}

// RQ-11 / GF-4: a backward clock step cannot lengthen a quarantine: an entry
// released "in the future" is set to the time it is first looked at, and its
// quarantine ends 900 s after the step. A forward step that is corrected
// within the margin, with a save in between, loses nothing.
func TestQuarantineSurvivesClockSteps(t *testing.T) {
	q := model.ReleasedPort{Host: "h", Port: 20001, ReleasedAt: 10000}
	if left := state.QuarantineExpiresIn(q, 10000-3600); left != 900 {
		t.Errorf("an hour after a backward step: %d s left, want at most 900", left)
	}

	back := state.New()
	back.Quarantine = []model.ReleasedPort{q}
	back.Lock()
	step := int64(10000 - 3600) // the clock stepped back an hour
	if got := back.QuarantineLocked(step); len(got) != 1 || got[0].ReleasedAt != step {
		t.Errorf("after the step: %+v, want the entry released at the step (%d)", got, step)
	}
	if got := back.QuarantineLocked(step + 899); len(got) != 1 {
		t.Errorf("899 s after the step: %+v, want still in quarantine", got)
	}
	if got := back.QuarantineLocked(step + 900); len(got) != 0 {
		t.Errorf("900 s after the step: %+v, want the quarantine over (not an hour later)", got)
	}
	back.Unlock()

	now := int64(10000)
	st := state.New()
	st.Clock = func() int64 { return now }
	st.Quarantine = []model.ReleasedPort{q}
	st.Lock()
	defer st.Unlock()
	now = 10000 + 930 // stepped forward past the quarantine, within the margin
	st.SnapshotLocked()
	now = 10000 + 10 // corrected
	if got := st.QuarantineLocked(now); len(got) != 1 {
		t.Errorf("after a forward step, a save and the correction: %+v, want the entry still in quarantine", got)
	}
}

// RQ-12: a node re-registering under a new address takes its quarantined
// ports along; when another node record still has the old host, both keep
// them.
func TestReRegistrationMovesTheQuarantineToTheNewHost(t *testing.T) {
	st := state.New()
	st.RegisterNode("w1", "http://10.0.0.1:9090", 4, 8192, 1)
	st.Lock()
	st.Quarantine = []model.ReleasedPort{
		{Host: "10.0.0.1", Port: 20001, SandboxID: "sb-a", ReleasedAt: 1},
		{Host: "10.0.0.9", Port: 20002, SandboxID: "sb-b", ReleasedAt: 1},
	}
	st.Unlock()
	st.RegisterNode("w1", "http://10.0.0.2:9090", 4, 8192, 2)
	st.Lock()
	got := append([]model.ReleasedPort(nil), st.Quarantine...)
	st.Unlock()
	if len(got) != 2 || got[0].Host != "10.0.0.2" || got[1].Host != "10.0.0.9" {
		t.Fatalf("after the address change: %+v, want 10.0.0.1 rewritten to 10.0.0.2 and nothing else touched", got)
	}

	// A second record on 10.0.0.2: moving w1 away copies the entries.
	st.RegisterNode("w1-old", "http://10.0.0.2:9091", 4, 8192, 3)
	st.RegisterNode("w1", "http://10.0.0.3:9090", 4, 8192, 4)
	st.Lock()
	hosts := map[string]int{}
	for _, q := range st.Quarantine {
		if q.Port == 20001 {
			hosts[q.Host]++
		}
	}
	st.Unlock()
	if hosts["10.0.0.2"] != 1 || hosts["10.0.0.3"] != 1 {
		t.Errorf("entries of a shared host: %v, want one under each", hosts)
	}
}

// RQ-3: a port is quarantined when its last row goes even if another row of
// the sandbox has the same guest port on a different node port.
func TestSameGuestPortOnAnotherNodePortSharesNothing(t *testing.T) {
	now := int64(100)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "web", GuestPort: 8080, NodePort: 20001},
		model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20002},
	)
	st.Lock()
	defer st.Unlock()
	st.RemoveExposeLocked(id, "web")
	if len(st.Quarantine) != 1 || st.Quarantine[0].Port != 20001 {
		t.Errorf("quarantine: %+v, want 20001", st.Quarantine)
	}
}

func TestRepointGuestPortQuarantinesTheOldPorts(t *testing.T) {
	now := int64(100)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "a", GuestPort: 8080, NodePort: 20001},
		model.Expose{Name: "b", GuestPort: 8080, NodePort: 20001},
		model.Expose{Name: "c", GuestPort: 81, NodePort: 20003},
	)
	st.Lock()
	defer st.Unlock()
	if old := st.RepointGuestPortLocked(id, 8080, 20005); !reflect.DeepEqual(old, []uint16{20001}) {
		t.Errorf("old ports: %v", old)
	}
	for _, e := range st.FindSandbox(id).Exposes {
		if e.GuestPort == 8080 && e.NodePort != 20005 || e.Name == "c" && e.NodePort != 20003 {
			t.Errorf("row after repoint: %+v", e)
		}
	}
	if len(st.Quarantine) != 1 || st.Quarantine[0].Port != 20001 {
		t.Errorf("quarantine: %+v, want 20001 once", st.Quarantine)
	}
}

func TestSplitGuestPortsAreListed(t *testing.T) {
	st := state.New()
	st.Lock()
	defer st.Unlock()
	a := st.CreateSandbox("a", "default", "", 1, 256, 1)
	a.Exposes = []model.Expose{
		{Name: "web", GuestPort: 8080, NodePort: 20001},
		{Name: "8080", GuestPort: 8080, NodePort: 20002},
		{Name: "x", GuestPort: 8080, NodePort: 20002}, // same port again: one entry
		{Name: "api", GuestPort: 81, NodePort: 20003},
	}
	b := st.CreateSandbox("b", "default", "", 1, 256, 1)
	b.Exposes = []model.Expose{{Name: "a", GuestPort: 80, NodePort: 20005}, {Name: "b", GuestPort: 80, NodePort: 20005}}
	got := st.SplitGuestPortsLocked()
	want := []state.SplitGuestPort{{SandboxID: a.ID, GuestPort: 8080, NodePorts: []uint16{20001, 20002}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("split guest ports: %+v, want %+v", got, want)
	}
}

// GF-2: the quarantine keys on the node port: a row on the same guest port
// but another node port leaves the removed one's port released.
func TestRemoveExposeQuarantinesAPortNoRemainingRowNames(t *testing.T) {
	now := int64(100)
	st, id := quarantineState(t, &now,
		model.Expose{Name: "web", GuestPort: 8080, NodePort: 20001},
		model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20002},
	)
	st.Lock()
	defer st.Unlock()
	sb := st.FindSandbox(id)
	if !state.GuestPortShared(sb, sb.Exposes[0]) {
		t.Error("GuestPortShared: another row has the guest port, on another node port; the worker's mapping is still needed")
	}
	st.RemoveExposeLocked(id, "web")
	if len(st.Quarantine) != 1 || st.Quarantine[0].Port != 20001 {
		t.Errorf("quarantine: %+v, want 20001", st.Quarantine)
	}
}

func TestDoubleBookedRowsAreListed(t *testing.T) {
	st := state.New()
	n1 := st.RegisterNode("w1", "http://10.0.0.1:9090", 4, 8192, 1)
	n2 := st.RegisterNode("w1b", "http://10.0.0.1:9091", 4, 8192, 1) // same host
	n3 := st.RegisterNode("w2", "http://10.0.0.2:9090", 4, 8192, 1)
	st.Lock()
	defer st.Unlock()
	mk := func(node string, ports ...uint16) string {
		sb := st.CreateSandbox("x", "default", node, 1, 256, 1)
		for i, p := range ports {
			sb.Exposes = append(sb.Exposes, model.Expose{Name: string(rune('a' + i)), GuestPort: uint16(80 + i), NodePort: p})
		}
		return sb.ID
	}
	a := mk(n1, 20001, 20002)
	b := mk(n2, 20001)
	mk(n3, 20002)        // another host: not a double booking
	mk(n1, 20005, 20005) // one sandbox, two names: not one either
	got := st.DoubleBookedLocked()
	want := []state.DoubleBooking{{Host: "10.0.0.1", Port: 20001, SandboxIDs: []string{a, b}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("double bookings: %+v, want %+v", got, want)
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
