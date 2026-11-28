// Tests for FLU-8 N10: feluccad reads isolation_ok from the heartbeat, keeps it
// in memory only, says when a node turns it false or back, and counts the
// ready nodes that report it false.
package server

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/state"
)

// captureLog routes the default logger into a buffer for the test.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (p *persistFixture) nodeID(t *testing.T) string {
	t.Helper()
	p.srv.st.Lock()
	defer p.srv.st.Unlock()
	return p.srv.st.Nodes[0].ID
}

// isolationOf reads the node's in-memory value: "absent", "true" or "false".
func (p *persistFixture) isolationOf(id string) string {
	p.srv.st.Lock()
	defer p.srv.st.Unlock()
	v := p.srv.st.FindNode(id).IsolationOK
	switch {
	case v == nil:
		return "absent"
	case *v:
		return "true"
	}
	return "false"
}

func (p *persistFixture) heartbeat(t *testing.T, id, extra string) {
	t.Helper()
	body := `{"id":"` + id + `","mem_free_mib":4096,"vm_count":0,"pool_size":0` + extra + `}`
	if w := p.do("POST", "/api/v1/agents/heartbeat", body); w.Code != 200 {
		t.Fatalf("heartbeat %s: %d %s", extra, w.Code, w.Body)
	}
}

func (p *persistFixture) unfencedGauge(t *testing.T) string {
	t.Helper()
	w := p.do("GET", "/metrics", "")
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "felucca_nodes_unfenced ") {
			return strings.TrimPrefix(line, "felucca_nodes_unfenced ")
		}
	}
	t.Fatalf("no felucca_nodes_unfenced in /metrics:\n%s", w.Body)
	return ""
}

func TestHeartbeatIsolationDecodes(t *testing.T) {
	p := newPersistFixture(t)
	id := p.nodeID(t)
	for _, tc := range []struct{ extra, want string }{
		{``, "absent"},
		{`,"isolation_ok":true`, "true"},
		{`,"isolation_ok":false`, "false"},
		{``, "absent"}, // each heartbeat says what it carries: an older agent again is unknown
		{`,"isolation_ok":null`, "absent"},
	} {
		p.heartbeat(t, id, tc.extra)
		if got := p.isolationOf(id); got != tc.want {
			t.Errorf("heartbeat %q: %s, want %s", tc.extra, got, tc.want)
		}
	}
	if w := p.do("POST", "/api/v1/agents/heartbeat", `{"id":"`+id+`","isolation_ok":"yes"}`); w.Code != 400 {
		t.Errorf("isolation_ok of the wrong type: %d %s, want 400", w.Code, w.Body)
	}
}

// A warning when a node turns false, an info line when it turns fine again,
// nothing while it stays the same.
func TestIsolationTransitionsAreLoggedOnce(t *testing.T) {
	p := newPersistFixture(t)
	id := p.nodeID(t)
	logs := captureLog(t)
	count := func(msg string) int {
		n := 0
		for _, line := range strings.Split(logs(), "\n") {
			if strings.Contains(line, msg) && strings.Contains(line, "node="+id) {
				n++
			}
		}
		return n
	}
	const down, back = "node reports its isolation down", "node reports its isolation in place again"

	for _, extra := range []string{``, `,"isolation_ok":true`, `,"isolation_ok":true`} {
		p.heartbeat(t, id, extra)
	}
	if d, b := count(down), count(back); d != 0 || b != 0 {
		t.Fatalf("logged %d down and %d back lines while fine, want none:\n%s", d, b, logs())
	}
	p.heartbeat(t, id, `,"isolation_ok":false`)
	p.heartbeat(t, id, `,"isolation_ok":false`)
	if d, b := count(down), count(back); d != 1 || b != 0 {
		t.Fatalf("after turning false: %d down and %d back lines, want 1 and 0:\n%s", d, b, logs())
	}
	for _, line := range strings.Split(logs(), "\n") {
		if strings.Contains(line, down) {
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "hostname=worker-1") || !strings.Contains(line, "addr=http://") {
				t.Errorf("down line lacks level, hostname or address: %s", line)
			}
		}
	}
	p.heartbeat(t, id, `,"isolation_ok":true`)
	p.heartbeat(t, id, `,"isolation_ok":true`)
	if d, b := count(down), count(back); d != 1 || b != 1 {
		t.Fatalf("after turning true: %d down and %d back lines, want 1 and 1:\n%s", d, b, logs())
	}
	for _, line := range strings.Split(logs(), "\n") {
		if strings.Contains(line, back) && !strings.Contains(line, "level=INFO") {
			t.Errorf("back line is not info: %s", line)
		}
	}
}

// The gauge counts the ready nodes whose last heartbeat said false, and drops
// a node that went silent.
func TestNodesUnfencedGauge(t *testing.T) {
	p := newPersistFixture(t)
	a := p.nodeID(t)
	now := time.Now().Unix()
	b := p.srv.st.RegisterNode("worker-2", "http://127.0.0.1:1", 4, 8192, now)
	c := p.srv.st.RegisterNode("worker-3", "http://127.0.0.1:2", 4, 8192, now)

	if got := p.unfencedGauge(t); got != "0" {
		t.Errorf("no reports: %s, want 0", got)
	}
	p.heartbeat(t, a, `,"isolation_ok":false`)
	p.heartbeat(t, b, `,"isolation_ok":true`)
	p.heartbeat(t, c, ``)
	if got := p.unfencedGauge(t); got != "1" {
		t.Errorf("one false, one true, one absent: %s, want 1", got)
	}
	p.heartbeat(t, b, `,"isolation_ok":false`)
	if got := p.unfencedGauge(t); got != "2" {
		t.Errorf("two false: %s, want 2", got)
	}
	// b goes silent: down by the node status rule (more than 15 s), not counted.
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		p.srv.st.FindNode(b).LastHB = time.Now().Unix() - 16
	}()
	if got := p.unfencedGauge(t); got != "1" {
		t.Errorf("one of them silent: %s, want 1", got)
	}
	p.heartbeat(t, a, `,"isolation_ok":true`)
	if got := p.unfencedGauge(t); got != "0" {
		t.Errorf("the ready one fine again: %s, want 0", got)
	}
}

// The node's API shape does not change with the value, and the value is not
// in the database: a save and a load bring every node back unknown.
func TestIsolationIsNotServedNorSaved(t *testing.T) {
	p := newPersistFixture(t)
	id := p.nodeID(t)
	var bodies []string
	for _, extra := range []string{``, `,"isolation_ok":true`, `,"isolation_ok":false`} {
		p.heartbeat(t, id, extra)
		func() {
			p.srv.st.Lock()
			defer p.srv.st.Unlock()
			p.srv.st.FindNode(id).LastHB = 1000 // a fixed body: the heartbeat stamps the clock
		}()
		w := p.do("GET", "/api/v1/nodes", "")
		if w.Code != 200 {
			t.Fatalf("list nodes: %d %s", w.Code, w.Body)
		}
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[0] != bodies[2] {
		t.Errorf("node JSON differs with isolation_ok:\nabsent %s\ntrue   %s\nfalse  %s", bodies[0], bodies[1], bodies[2])
	}
	if strings.Contains(bodies[0], "isolation") {
		t.Errorf("node JSON carries the value: %s", bodies[0])
	}

	// Memory says false now; the snapshot copy and the store do not carry it.
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		if snap := p.srv.st.SnapshotLocked(); snap.Nodes[0].IsolationOK != nil {
			t.Error("the snapshot copy carries isolation_ok")
		}
	}()
	if err := p.srv.saveState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded := state.New()
	if err := p.sqlite.LoadInto(loaded); err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := loaded.FindNode(id); n == nil || n.IsolationOK != nil {
		t.Errorf("loaded node: %+v, want it back with isolation unknown", n)
	}
	if p.isolationOf(id) != "false" {
		t.Error("the save changed memory")
	}
}
