package store_test

// FLU-74 store test: unsigned values cannot make a save fail.

import (
	"math"
	"testing"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// database/sql refuses a uint64 with the high bit set. Every unsigned 64-bit
// column is clamped when bound, so no value in memory can make a save fail.
func TestUnsignedValuesAreClampedWhenBound(t *testing.T) {
	db := open(t)
	huge := uint64(1) << 63
	st := state.New()
	st.Nodes = append(st.Nodes, &model.Node{ID: "node-1", Hostname: "h", Addr: "1.2.3.4:9090", MemTotalMiB: huge, MemFreeMiB: math.MaxUint64})
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{
		ID: "sb-1", Name: "one", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: huge, CreatedAt: 1,
	})
	if err := db.SaveSnapshot(st); err != nil {
		t.Fatalf("save with 2^63 values: %v", err)
	}
	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := got.Nodes[0]; n.MemTotalMiB != math.MaxInt64 || n.MemFreeMiB != math.MaxInt64 {
		t.Errorf("node mem: total=%d free=%d, want both clamped to MaxInt64", n.MemTotalMiB, n.MemFreeMiB)
	}
	if got.Sandboxes[0].MemMiB != math.MaxInt64 {
		t.Errorf("sandbox mem: %d, want MaxInt64", got.Sandboxes[0].MemMiB)
	}
	if err := db.AppendUsage(store.UsageEvent{SandboxID: "sb-1", Event: "exec", MemMiB: huge, TS: 2}); err != nil {
		t.Errorf("append usage with 2^63: %v", err)
	}
	if err := db.CreateTemplate(&store.Template{ID: "tpl-1", Name: "t", Image: "i", MemMiB: huge}); err != nil {
		t.Errorf("create template with 2^63: %v", err)
	}
}
