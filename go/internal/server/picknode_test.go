package server

// E1: creates avoid a worker that reports its isolation down, and warn (at
// most once a minute) when no other ready worker exists.

import (
	"strings"
	"testing"
	"time"
)

func TestCreatesAvoidAnUnfencedWorkerAndWarnOnTheFallback(t *testing.T) {
	p := newPersistFixture(t)
	logs := captureLog(t)
	nodeID := p.srv.st.Nodes[0].ID
	down := `{"id":"` + nodeID + `","mem_free_mib":100,"vm_count":0,"isolation_ok":false}`
	if w := p.do("POST", "/api/v1/agents/heartbeat", down); w.Code != 200 {
		t.Fatalf("heartbeat: %d", w.Code)
	}
	// The only ready worker reports isolation down: placed there anyway.
	for i := 0; i < 2; i++ {
		id := p.create(t, `{"name":"a"}`)
		if sb := p.mem(id); sb == nil || *sb.NodeID != nodeID {
			t.Fatalf("create %d placed on %v, want the only ready worker", i, sb)
		}
	}
	if n := strings.Count(logs(), "creates are being placed on workers that report their isolation down"); n != 1 {
		t.Errorf("fallback warnings: %d, want 1 (at most one a minute)\n%s", n, logs())
	}

	// A second ready worker with its isolation in place and more sandboxes.
	other := p.srv.st.RegisterNode("worker-2", p.srv.st.Snapshot().Nodes[0].Addr, 4, 8192, time.Now().Unix())
	if w := p.do("POST", "/api/v1/agents/heartbeat", `{"id":"`+other+`","mem_free_mib":100,"vm_count":9,"isolation_ok":true}`); w.Code != 200 {
		t.Fatalf("heartbeat: %d", w.Code)
	}
	id := p.create(t, `{"name":"b"}`)
	if sb := p.mem(id); sb == nil || *sb.NodeID != other {
		t.Errorf("create with a fenced worker ready: placed on %v, want %s", sb.NodeID, other)
	}
}
