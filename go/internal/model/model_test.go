package model_test

import (
	"encoding/json"
	"testing"

	"github.com/alpham/infra-saas/hearth/internal/model"
)

func strPtr(s string) *string { return &s }

func TestSandboxMarshalWithNulls(t *testing.T) {
	sb := &model.Sandbox{
		ID:        "sb-aabbccdd-1",
		Name:      "test",
		Namespace: "default",
		NodeID:    nil,
		State:     model.StateStopped,
		VCPUs:     1,
		MemMiB:    256,
		IP:        nil,
		CreatedAt: 1000,
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
			t.Errorf("key %q must be present (explicit null)", key)
		} else if string(v) != "null" {
			t.Errorf("key %q should be null, got %s", key, v)
		}
	}
}

func TestSandboxMarshalWithValues(t *testing.T) {
	sb := &model.Sandbox{
		ID:        "sb-11223344-2",
		Name:      "mybox",
		Namespace: "ns1",
		NodeID:    strPtr("node-aabbccdd-1"),
		State:     model.StateRunning,
		VCPUs:     2,
		MemMiB:    512,
		IP:        strPtr("10.0.0.5"),
		CreatedAt: 1781000000,
		ParentID:  strPtr("sb-99887766-1"),
	}
	b, err := json.Marshal(sb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)

	checkStr := func(key, want string) {
		t.Helper()
		v, ok := m[key]
		if !ok {
			t.Errorf("key %q missing", key)
			return
		}
		var s string
		json.Unmarshal(v, &s)
		if s != want {
			t.Errorf("key %q: got %q, want %q", key, s, want)
		}
	}
	checkStr("id", "sb-11223344-2")
	checkStr("name", "mybox")
	checkStr("namespace", "ns1")
	checkStr("node_id", "node-aabbccdd-1")
	checkStr("state", "running")
	checkStr("ip", "10.0.0.5")
	checkStr("parent_id", "sb-99887766-1")
}

func TestNodeMarshalReady(t *testing.T) {
	now := int64(2000)
	n := &model.Node{
		ID:          "node-aabbccdd-1",
		Hostname:    "host1",
		Addr:        "1.2.3.4:9090",
		CPUs:        4,
		MemTotalMiB: 8192,
		MemFreeMiB:  7000,
		VMCount:     2,
		PoolSize:    1,
		LastHB:      now - 5, // 5s ago → ready
	}
	b, err := n.MarshalWithNow(now)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)

	var status string
	json.Unmarshal(m["status"], &status)
	if status != "ready" {
		t.Errorf("status: got %q, want ready", status)
	}
}

func TestNodeMarshalDown(t *testing.T) {
	now := int64(2000)
	n := &model.Node{
		ID:       "node-aabbccdd-2",
		Hostname: "host2",
		LastHB:   now - 20, // 20s ago → down (>15s window)
	}
	b, err := n.MarshalWithNow(now)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)

	var status string
	json.Unmarshal(m["status"], &status)
	if status != "down" {
		t.Errorf("status: got %q, want down", status)
	}
}

func TestNodeStatusBoundary(t *testing.T) {
	n := &model.Node{LastHB: 1000}
	// exactly 15s → ready (> 15, not >= 15)
	if n.NodeStatus(1015) != "ready" {
		t.Error("exactly 15s gap should be ready")
	}
	// 16s → down
	if n.NodeStatus(1016) != "down" {
		t.Error("16s gap should be down")
	}
}

func TestParseSandboxState(t *testing.T) {
	tests := []struct {
		in   string
		want model.SandboxState
	}{
		{"creating", model.StateCreating},
		{"running", model.StateRunning},
		{"paused", model.StatePaused},
		{"stopped", model.StateStopped},
		{"sleeping", model.StateSleeping},
		{"error", model.StateError},
		{"unknown", model.StateStopped}, // unknown → stopped
		{"", model.StateStopped},
	}
	for _, tc := range tests {
		got := model.ParseSandboxState(tc.in)
		if got != tc.want {
			t.Errorf("ParseSandboxState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSandboxJSONKeyOrder(t *testing.T) {
	// Verify key order matches Zig struct field order:
	// id, name, namespace, node_id, state, vcpus, mem_mib, ip, created_at, parent_id
	sb := &model.Sandbox{
		ID: "sb-00000000-1", Name: "n", Namespace: "ns",
		NodeID: nil, State: model.StateRunning,
		VCPUs: 1, MemMiB: 256, IP: nil, CreatedAt: 0, ParentID: nil,
	}
	b, _ := json.Marshal(sb)
	s := string(b)

	keys := []string{"\"id\"", "\"name\"", "\"namespace\"", "\"node_id\"", "\"state\"",
		"\"vcpus\"", "\"mem_mib\"", "\"ip\"", "\"created_at\"", "\"parent_id\""}

	prev := 0
	for _, k := range keys {
		idx := len(s) - len(s[prev:]) + indexOf(s[prev:], k)
		if idx < prev {
			t.Errorf("key %s out of order in JSON: %s", k, s)
			return
		}
		prev = idx + len(k)
	}
}

func indexOf(s, sub string) int {
	for i := range s {
		if len(s[i:]) >= len(sub) && s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
