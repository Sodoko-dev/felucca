package state_test

// E1: placement prefers workers that do not report their isolation down.

import (
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/state"
)

func pick(st *state.State, now int64) string {
	st.Lock()
	defer st.Unlock()
	if n := st.PickNode(now); n != nil {
		return n.ID
	}
	return ""
}

func TestPickNodePrefersNodesWithIsolationInPlace(t *testing.T) {
	now := time.Now().Unix()
	down, up := false, true
	st := state.New()
	broken := st.RegisterNode("broken", "addr1", 4, 8192, now)
	fine := st.RegisterNode("fine", "addr2", 4, 8192, now)
	st.Heartbeat(broken, 8000, 0, 0, &down, now) // fewer sandboxes, isolation down
	st.Heartbeat(fine, 8000, 5, 0, &up, now)

	for i := 0; i < 3; i++ {
		if got := pick(st, now); got != fine {
			t.Fatalf("pick %d: %s, want the node with its isolation in place (%s)", i, got, fine)
		}
	}
	// Isolation in place again: the lower vm_count wins again.
	st.Heartbeat(broken, 8000, 0, 0, &up, now)
	if got := pick(st, now); got != broken {
		t.Errorf("after the repair: %s, want the lowest vm_count (%s)", got, broken)
	}
	// A node that never sent the flag counts as fine.
	st.Heartbeat(broken, 8000, 0, 0, nil, now)
	if got := pick(st, now); got != broken {
		t.Errorf("flag absent: %s, want it treated as fine (%s)", got, broken)
	}
}

func TestPickNodeFallsBackWhenEveryReadyNodeIsUnfenced(t *testing.T) {
	now := time.Now().Unix()
	down, up := false, true
	st := state.New()
	a := st.RegisterNode("a", "addr1", 4, 8192, now)
	b := st.RegisterNode("b", "addr2", 4, 8192, now)
	stale := st.RegisterNode("stale", "addr3", 4, 8192, now-100) // not ready
	st.Heartbeat(a, 8000, 3, 0, &down, now)
	st.Heartbeat(b, 8000, 1, 0, &down, now)
	st.Heartbeat(stale, 8000, 0, 0, &up, now-100)
	if got := pick(st, now); got != b {
		t.Errorf("every ready node unfenced: %s, want the lowest of them (%s), never the node that is not ready", got, b)
	}
}
