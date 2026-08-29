// Package state implements the in-memory control-plane state store with
// JSON-file persistence, replicating state.zig exactly.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
}

// New creates an empty State.
func New() *State {
	return &State{}
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

// IDRandomBytes is the crypto/rand width of the random half of every id.
// A sandbox id lands in the public ingress label "<expose-name>--<id>"
// (ADR-0007), which is a single DNS label capped at 63 octets: 32 for the
// longest expose name plus the 2-byte separator leaves 29 for the id, i.e.
// "sb-" and 26 hex chars. That is 104 bits — enough that the label is a real
// bearer capability, which it must be because the gateway authenticates
// nothing on an inbound ingress request.
const IDRandomBytes = 13

// randomHex returns n cryptographically random bytes hex-encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable for id issuance.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// nextID generates a new ID with the given prefix.
// Format: "<prefix>-<26 hex chars>". Seq still advances (it is part of the
// persisted state format) but stays out of the id: a monotonic tail would
// hand an attacker the position of every other tenant's id in the stream,
// and it costs label budget that entropy needs more.
// Caller must hold the lock.
func (s *State) nextID(prefix string) string {
	s.Seq++
	return prefix + "-" + randomHex(IDRandomBytes)
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

// MaxNodes caps how many node records the working set may hold. Every node is
// scanned linearly under this mutex by FindNode/PickNode/the registration
// checks, and a full SaveSnapshot rewrites all of them on every state change —
// so the list must not be able to grow without bound. The cap is far above the
// ADR-0002 single-hearthd scale point, so it is a backstop and not a scheduling
// limit; the caller-supplied admission check (see RegisterNodeGuarded) is what
// actually closes the growth path.
const MaxNodes = 1024

// StaleNodeTTL is how long a node record must have been silent before
// RegisterNodeGuarded may reclaim it to make room at the MaxNodes ceiling.
//
// A hard ceiling with no reclamation path is its own outage: records left by
// re-enrollments under fresh wireguard keys accumulate, and once the table is
// full every genuinely new node gets a 503 forever — while the agent
// (rust/agent/src/registration.rs) retries register every 5s and never reaches
// its heartbeat loop. So the ceiling needs a way down, and reclaiming a
// LEGITIMATE node's record would be worse than the growth: it drops the address
// hearthd dials that worker's VMs at.
//
// Seven days is therefore deliberately enormous. An agent heartbeats every 5s
// and a node is "down" after 15s of silence (model.Node.NodeStatus), so this is
// ~120,000 consecutive missed heartbeats: a reboot, a long maintenance window, a
// network partition, or a hearthd restart from a slightly stale snapshot all sit
// many orders of magnitude below it. Nothing that is coming back is reclaimed.
const StaleNodeTTL = 7 * 24 * 3600

// NodeAdmission decides whether a registration may be applied. It is called by
// RegisterNodeGuarded UNDER the state lock, with the live node slice, and its
// answer is acted on before that lock is released.
//
// That is the whole point of the type. The pin it implements ("one credential,
// one node record") used to be a separate method that took and released the
// state lock on its own, so N concurrent registrations from one credential could
// all pass the check before any of them appended — a small permanent inflation
// of the node table per compromised credential. Check-and-insert now share one
// unbroken critical section, the same shape the tenant-quota TOCTOU was fixed
// with (see quotaExceededLocked, called under the lock that also inserts).
//
// It must not block, take any other lock, or call back into State.
type NodeAdmission func(nodes []*model.Node) (reason string, ok bool)

// NodeRegistration is the outcome of RegisterNodeGuarded.
type NodeRegistration struct {
	// ID is the registered node's id, or "" when nothing was registered.
	ID string
	// Refused is non-empty exactly when the admission check said no; it is the
	// reason, for the caller to log and return. An empty Refused with an empty
	// ID means the table was full and nothing could be reclaimed.
	Refused string
	// Reclaimed lists the ids of provably-stale records evicted to make room.
	// Normally empty: reclamation runs only at the ceiling.
	Reclaimed []string
}

// RegisterNode registers a node idempotently by hostname, with no admission
// check. Returns the node id, or "" if a NEW record would exceed MaxNodes (an
// existing node always re-registers, so a full list never stops the fleet from
// heartbeating).
func (s *State) RegisterNode(hostname, addr string, cpus uint32, memTotal uint64, now int64) string {
	return s.RegisterNodeGuarded(hostname, addr, cpus, memTotal, now, nil).ID
}

// RegisterNodeGuarded is RegisterNode with an admission check that runs under
// the SAME lock acquisition as the append it authorizes — see NodeAdmission for
// why that is not optional. A nil admit admits everything.
func (s *State) RegisterNodeGuarded(hostname, addr string, cpus uint32, memTotal uint64, now int64, admit NodeAdmission) NodeRegistration {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Inside the critical section, and nothing between here and the append
	// below releases the lock: whatever admit saw is still true when the record
	// lands.
	if admit != nil {
		if reason, ok := admit(s.Nodes); !ok {
			return NodeRegistration{Refused: reason}
		}
	}

	if n := s.findNodeByHostname(hostname); n != nil {
		// Update mutable fields, keep id.
		n.Addr = addr
		n.CPUs = cpus
		n.MemTotalMiB = memTotal
		n.LastHB = now
		return NodeRegistration{ID: n.ID}
	}
	var reclaimed []string
	if len(s.Nodes) >= MaxNodes {
		// Only at the ceiling. In normal operation nothing is ever evicted, so
		// this cannot be a way to lose a node record by accident.
		reclaimed = s.reclaimStaleNodesLocked(now)
		if len(s.Nodes) >= MaxNodes {
			return NodeRegistration{Reclaimed: reclaimed}
		}
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
	return NodeRegistration{ID: id, Reclaimed: reclaimed}
}

// reclaimStaleNodesLocked drops node records that are PROVABLY dead, to make
// room at the MaxNodes ceiling. Caller must hold the lock.
//
// Two conditions, both required, and both about being sure rather than being
// thorough:
//
//   - silent for at least StaleNodeTTL. See that constant: it is ~120,000 missed
//     heartbeats, so a node that is merely rebooting is nowhere near it.
//   - no sandbox references the record. A node id is how hearthd finds the
//     address to reach a sandbox's microVM; dropping a record that still owns
//     workloads would strand them exactly the way an orphaned VM is stranded.
//     This is also what makes the eviction cheap to be wrong about — a reclaimed
//     record stands for nothing that is running.
//
// A node reclaimed in error is not lost: its agent's next register call creates
// a fresh record (registration is keyed on hostname), which is precisely the
// path that was 503ing before. A table full of stale-but-still-referenced
// records reclaims nothing and still 503s — the conservative direction.
func (s *State) reclaimStaleNodesLocked(now int64) []string {
	inUse := make(map[string]bool, len(s.Sandboxes))
	for _, sb := range s.Sandboxes {
		if sb.NodeID != nil {
			inUse[*sb.NodeID] = true
		}
	}
	var reclaimed []string
	total := len(s.Nodes)
	kept := s.Nodes[:0]
	for _, n := range s.Nodes {
		if now-n.LastHB >= StaleNodeTTL && !inUse[n.ID] {
			reclaimed = append(reclaimed, n.ID)
			continue
		}
		kept = append(kept, n)
	}
	// Clear the tail so the evicted records are not retained by the backing
	// array (the list is MaxNodes-sized by the time this runs).
	for i := len(kept); i < total; i++ {
		s.Nodes[i] = nil
	}
	s.Nodes = kept
	return reclaimed
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

	// 0600, not 0644: this file is the whole fleet inventory — every node's
	// address, every sandbox id and the tenant each belongs to. hearthd is the
	// only reader and it runs as root, so nothing needs the group/other bits,
	// and the sibling persistence paths (store/sqlite.go's DB, wg.go's private
	// keys) are already 0600. The mode is on the temp file because the rename
	// carries it over: setting it afterwards would leave a world-readable
	// window at the real path on every write.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
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
	// Templates & disk (v4 P4) — same rule: dropping these on load would
	// corrupt disk-quota accounting after a restart.
	Template string `json:"template"`
	DiskGB   uint32 `json:"disk_gb"`
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
			Template:          ls.Template,
			DiskGB:            ls.DiskGB,
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
