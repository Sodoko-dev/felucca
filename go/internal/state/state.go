// Package state implements the in-memory control-plane state store with
// JSON-file persistence, replicating state.zig exactly.
package state

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/model"
)

// State is the central in-memory store. All exported methods are safe for
// concurrent use via an internal mutex.
type State struct {
	mu sync.Mutex

	Nodes     []*model.Node
	Sandboxes []*model.Sandbox

	RequestCount uint64
	Seq          uint64

	// Wake/fork/exec metrics.
	WakeMsLast uint64
	WakeTotal  uint64
	WakeMsSum  uint64
	ForksTotal uint64
	ExecsTotal uint64

	rng *rand.Rand
}

// New creates a State seeded from the current time.
func New() *State {
	return &State{
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Lock acquires the state mutex. Use for multi-step read/write sequences.
func (s *State) Lock() { s.mu.Lock() }

// Unlock releases the state mutex.
func (s *State) Unlock() { s.mu.Unlock() }

// BumpRequests increments the request counter (called on every inbound request).
func (s *State) BumpRequests() {
	s.mu.Lock()
	s.RequestCount++
	s.mu.Unlock()
}

// ---- ID generation ----

// nextID generates a new ID with the given prefix.
// Format: "<prefix>-<08x random uint32>-<seq>"
// Caller must hold the lock.
func (s *State) nextID(prefix string) string {
	s.Seq++
	r := s.rng.Uint32()
	return fmt.Sprintf("%s-%08x-%d", prefix, r, s.Seq)
}

// ---- Nodes ----

// FindNode returns the node with the given id, or nil.
// Caller must hold the lock.
func (s *State) FindNode(id string) *model.Node {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (s *State) findNodeByHostname(hostname string) *model.Node {
	for _, n := range s.Nodes {
		if n.Hostname == hostname {
			return n
		}
	}
	return nil
}

// RegisterNode registers a node idempotently by hostname. Returns the node id.
func (s *State) RegisterNode(hostname, addr string, cpus uint32, memTotal uint64, now int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n := s.findNodeByHostname(hostname); n != nil {
		// Update mutable fields, keep id.
		n.Addr = addr
		n.CPUs = cpus
		n.MemTotalMiB = memTotal
		n.LastHB = now
		return n.ID
	}
	id := s.nextID("node")
	n := &model.Node{
		ID:          id,
		Hostname:    hostname,
		Addr:        addr,
		CPUs:        cpus,
		MemTotalMiB: memTotal,
		MemFreeMiB:  memTotal,
		VMCount:     0,
		PoolSize:    0,
		LastHB:      now,
	}
	s.Nodes = append(s.Nodes, n)
	return id
}

// Heartbeat updates a node's live stats. Returns false if the node is unknown.
func (s *State) Heartbeat(id string, memFree uint64, vmCount, poolSize uint32, now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.FindNode(id)
	if n == nil {
		return false
	}
	n.MemFreeMiB = memFree
	n.VMCount = vmCount
	n.PoolSize = poolSize
	n.LastHB = now
	return true
}

// PickNode picks the ready node with the lowest vm_count.
// Caller must hold the lock.
func (s *State) PickNode(now int64) *model.Node {
	var best *model.Node
	for _, n := range s.Nodes {
		if n.NodeStatus(now) != "ready" {
			continue
		}
		if best == nil || n.VMCount < best.VMCount {
			best = n
		}
	}
	return best
}

// ---- Sandboxes ----

// FindSandbox returns the sandbox with the given id, or nil.
// Caller must hold the lock.
func (s *State) FindSandbox(id string) *model.Sandbox {
	for _, sb := range s.Sandboxes {
		if sb.ID == id {
			return sb
		}
	}
	return nil
}

// CreateSandbox creates a sandbox in "creating" state and appends it to the store.
// Caller must hold the lock.
func (s *State) CreateSandbox(name, namespace, nodeID string, vcpus uint32, memMiB uint64, now int64) *model.Sandbox {
	id := s.nextID("sb")
	nid := nodeID
	sb := &model.Sandbox{
		ID:        id,
		Name:      name,
		Namespace: namespace,
		NodeID:    &nid,
		State:     model.StateCreating,
		VCPUs:     vcpus,
		MemMiB:    memMiB,
		IP:        nil,
		CreatedAt: now,
		ParentID:  nil,
	}
	s.Sandboxes = append(s.Sandboxes, sb)
	return sb
}

// CreateForkChild creates a child sandbox in "creating" state.
// Caller must hold the lock.
func (s *State) CreateForkChild(name, namespace, nodeID string, vcpus uint32, memMiB uint64, parentID string, now int64) *model.Sandbox {
	id := s.nextID("sb")
	nid := nodeID
	pid := parentID
	sb := &model.Sandbox{
		ID:        id,
		Name:      name,
		Namespace: namespace,
		NodeID:    &nid,
		State:     model.StateCreating,
		VCPUs:     vcpus,
		MemMiB:    memMiB,
		IP:        nil,
		CreatedAt: now,
		ParentID:  &pid,
	}
	s.Sandboxes = append(s.Sandboxes, sb)
	return sb
}

// SetSandboxState updates a sandbox's state field.
func (s *State) SetSandboxState(id string, st model.SandboxState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb := s.FindSandbox(id); sb != nil {
		sb.State = st
	}
}

// SetSandboxIP updates a sandbox's IP field. Empty string clears to nil.
func (s *State) SetSandboxIP(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.FindSandbox(id)
	if sb == nil {
		return
	}
	if ip == "" {
		sb.IP = nil
	} else {
		v := ip
		sb.IP = &v
	}
}

// RemoveSandbox removes a sandbox by id.
func (s *State) RemoveSandbox(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sb := range s.Sandboxes {
		if sb.ID == id {
			s.Sandboxes = append(s.Sandboxes[:i], s.Sandboxes[i+1:]...)
			return
		}
	}
}

// RecordWake updates wake metrics.
func (s *State) RecordWake(ms uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WakeMsLast = ms
	s.WakeTotal++
	s.WakeMsSum += ms
}

// RecordFork increments the fork counter.
func (s *State) RecordFork() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ForksTotal++
}

// RecordExec increments the exec attempt counter.
func (s *State) RecordExec() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExecsTotal++
}

// ---- Persistence ----

// persistState is the on-disk JSON format, mirroring Zig's output exactly.
type persistState struct {
	Seq          uint64            `json:"seq"`
	RequestCount uint64            `json:"request_count"`
	Nodes        []json.RawMessage `json:"nodes"`
	Sandboxes    []*model.Sandbox  `json:"sandboxes"`
}

// Persist atomically writes state to path (temp file + rename).
// The node status field is computed with each node's own last_heartbeat as "now"
// so status is always "ready", matching Zig's persist behavior exactly.
func (s *State) Persist(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Marshal nodes using last_heartbeat as "now" so status is always "ready"
	// (exactly what Zig does: n.writeJson(&b, n.last_heartbeat)).
	nodes := make([]json.RawMessage, len(s.Nodes))
	for i, n := range s.Nodes {
		b, err := n.MarshalWithNow(n.LastHB)
		if err != nil {
			return fmt.Errorf("marshal node: %w", err)
		}
		nodes[i] = json.RawMessage(b)
	}

	ps := persistState{
		Seq:          s.Seq,
		RequestCount: s.RequestCount,
		Nodes:        nodes,
		Sandboxes:    s.Sandboxes,
	}

	data, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// loadNode is used when reading nodes from the state file.
type loadNode struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	Addr        string `json:"addr"`
	CPUs        uint32 `json:"cpus"`
	MemTotalMiB uint64 `json:"mem_total_mib"`
	MemFreeMiB  uint64 `json:"mem_free_mib"`
	VMCount     uint32 `json:"vm_count"`
	PoolSize    uint32 `json:"pool_size"`
	// "status" is ignored on load (computed at runtime).
	LastHB int64 `json:"last_heartbeat"`
}

// loadSandbox allows tolerating null/missing fields when reading from file.
type loadSandbox struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Namespace string  `json:"namespace"`
	NodeID    *string `json:"node_id"`
	State     string  `json:"state"`
	VCPUs     uint32  `json:"vcpus"`
	MemMiB    uint64  `json:"mem_mib"`
	IP        *string `json:"ip"`
	CreatedAt int64   `json:"created_at"`
	ParentID  *string `json:"parent_id"`
	// Ingress (v4 P3) — Persist writes the model wholesale; reading must not
	// silently drop these (dead routes + orphaned DNAT rules on restart).
	Exposes           []model.Expose `json:"exposes"`
	AllowDynamicPorts bool           `json:"allow_dynamic_ports"`
}

// loadFile is the shape used when reading the persisted JSON.
type loadFile struct {
	Seq          uint64            `json:"seq"`
	RequestCount uint64            `json:"request_count"`
	Nodes        []json.RawMessage `json:"nodes"`
	Sandboxes    []json.RawMessage `json:"sandboxes"`
}

// Load reads state from path. Missing file is not an error at the call site;
// the caller decides whether to warn. Unknown sandbox states map to "stopped".
func (s *State) Load(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var lf loadFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	s.Seq = lf.Seq
	s.RequestCount = lf.RequestCount

	for _, raw := range lf.Nodes {
		var ln loadNode
		if err := json.Unmarshal(raw, &ln); err != nil {
			continue
		}
		if ln.ID == "" {
			continue
		}
		s.Nodes = append(s.Nodes, &model.Node{
			ID:          ln.ID,
			Hostname:    ln.Hostname,
			Addr:        ln.Addr,
			CPUs:        ln.CPUs,
			MemTotalMiB: ln.MemTotalMiB,
			MemFreeMiB:  ln.MemFreeMiB,
			VMCount:     ln.VMCount,
			PoolSize:    ln.PoolSize,
			LastHB:      ln.LastHB,
		})
	}

	for _, raw := range lf.Sandboxes {
		var ls loadSandbox
		if err := json.Unmarshal(raw, &ls); err != nil {
			continue
		}
		if ls.ID == "" {
			continue
		}
		// Default missing namespace.
		ns := ls.Namespace
		if ns == "" {
			ns = "default"
		}
		// Default missing vcpus/mem.
		vcpus := ls.VCPUs
		if vcpus == 0 {
			vcpus = 1
		}
		memMiB := ls.MemMiB
		if memMiB == 0 {
			memMiB = 256
		}
		sb := &model.Sandbox{
			ID:        ls.ID,
			Name:      ls.Name,
			Namespace: ns,
			NodeID:    ls.NodeID,
			State:     model.ParseSandboxState(ls.State),
			VCPUs:     vcpus,
			MemMiB:    memMiB,
			IP:        ls.IP,
			CreatedAt: ls.CreatedAt,
			ParentID:  ls.ParentID,
			Exposes:           ls.Exposes,
			AllowDynamicPorts: ls.AllowDynamicPorts,
		}
		s.Sandboxes = append(s.Sandboxes, sb)
	}

	return nil
}

// EnsureStateDir creates the directory for path if it doesn't exist.
func EnsureStateDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o755)
}
