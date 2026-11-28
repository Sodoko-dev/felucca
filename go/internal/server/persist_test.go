// Tests for FLU-74: no value a node reports can make every later save fail.
// Internal package so they can reach the save path directly.
package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One heartbeat carrying a value database/sql cannot bind (a uint64 with the
// high bit set) used to make EVERY later save fail: the value sits in memory
// and is rewritten by each snapshot. Both halves of the fix are pinned: the
// handler refuses it, and the store clamps whatever reaches it anyway.
func TestHugeMemoryValueCannotPoisonEverySave(t *testing.T) {
	srv, _, _ := newExposeTestServer(t)
	nodeID := srv.st.Nodes[0].ID

	body := fmt.Sprintf(`{"id":%q,"mem_free_mib":%d,"vm_count":1,"pool_size":0}`, nodeID, uint64(1)<<63)
	w := doRequest(srv.Handler(), "POST", "/api/v1/agents/heartbeat", body, adminAuth)
	if w.Code != 400 {
		t.Errorf("heartbeat with mem_free_mib 2^63: %d %s, want 400", w.Code, w.Body)
	}

	// The store half, reached directly: whatever is in memory must be savable.
	srv.st.Heartbeat(nodeID, uint64(1)<<63, 1, 0, time.Now().Unix())
	if err := srv.persist(); err != nil {
		t.Fatalf("save after a 2^63 heartbeat: %v — one value in memory now fails every save", err)
	}
}

// The bound is 2^40 MiB on both routes that report node memory.
func TestNodeMemoryBounds(t *testing.T) {
	srv, _, _ := newExposeTestServer(t)
	h := srv.Handler()
	nodeID := srv.st.Nodes[0].ID
	hb := func(v uint64) int {
		return doRequest(h, "POST", "/api/v1/agents/heartbeat",
			fmt.Sprintf(`{"id":%q,"mem_free_mib":%d}`, nodeID, v), adminAuth).Code
	}
	if c := hb(1 << 40); c != 200 {
		t.Errorf("heartbeat at the bound: %d, want 200", c)
	}
	if c := hb(1<<40 + 1); c != 400 {
		t.Errorf("heartbeat above the bound: %d, want 400", c)
	}
	reg := func(v uint64) *httptest.ResponseRecorder {
		return doRequest(h, "POST", "/api/v1/agents/register",
			fmt.Sprintf(`{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":%d}`, v), adminAuth)
	}
	if w := reg(1<<40 + 1); w.Code != 400 || !strings.Contains(w.Body.String(), "mem_total_mib") {
		t.Errorf("register above the bound: %d %s, want 400", w.Code, w.Body)
	}
	if w := reg(1 << 40); w.Code != 200 {
		t.Errorf("register at the bound: %d %s, want 200", w.Code, w.Body)
	}
}
