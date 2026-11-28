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
	"sort"
	"sync"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/agentclient"
	"github.com/alpham/infra-saas/felucca/internal/ingress"
	"github.com/alpham/infra-saas/felucca/internal/model"
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

	// Quarantine lists the node ports released less than
	// ingress.PortQuarantine ago (FLU-6), plus expired entries not yet dropped
	// (SnapshotLocked drops them). Entries are created ONLY by the removal
	// methods below, in the same critical section as the row they describe, so
	// no caller can remove a route row and forget its port.
	Quarantine []model.ReleasedPort

	// Clock returns unix seconds; nil means the wall clock. Tests set it to
	// age quarantine entries.
	Clock func() int64
}

// Now is the state's clock (unix seconds). Caller need not hold the lock.
func (s *State) Now() int64 {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().Unix()
}

// quarantineS is ingress.PortQuarantine in the unit the entries carry;
// keepS is how long an entry is kept at all (see SnapshotLocked).
var (
	quarantineS = int64(ingress.PortQuarantine / time.Second)
	keepS       = int64((ingress.PortQuarantine + ingress.Margin) / time.Second)
)

// The quarantine runs on the wall clock, which can step.
//
//   - Backward: an entry whose release time is in the future is set to "now"
//     in the working set, under the lock, the first time it is looked at
//     (QuarantineLocked, SnapshotLocked). Its quarantine then ends
//     PortQuarantine after the step, not PortQuarantine after the clock has
//     caught up with the old release time. QuarantineExpiresIn clamps as well,
//     for an entry seen before that.
//   - Forward: shortens every quarantine; nothing can tell that from time
//     passing. Entries are dropped for good only once PortQuarantine + Margin
//     old, so a step within the margin that is corrected before then loses
//     nothing.

// QuarantineExpiresIn is the number of seconds until q's quarantine ends
// (<= 0: ended), at most PortQuarantine.
func QuarantineExpiresIn(q model.ReleasedPort, now int64) int64 {
	left := q.ReleasedAt + quarantineS - now
	if left > quarantineS {
		left = quarantineS
	}
	return left
}

// QuarantineActive reports whether an entry is still within its quarantine
// at now.
func QuarantineActive(q model.ReleasedPort, now int64) bool {
	return QuarantineExpiresIn(q, now) > 0
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
// ADR-0002 single-feluccad scale point, so it is a backstop and not a scheduling
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
// feluccad dials that worker's VMs at.
//
// Seven days is therefore deliberately enormous. An agent heartbeats every 5s
// and a node is "down" after 15s of silence (model.Node.NodeStatus), so this is
// ~120,000 consecutive missed heartbeats: a reboot, a long maintenance window, a
// network partition, or a feluccad restart from a slightly stale snapshot all sit
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
		// Update mutable fields, keep id. A new address moves the node's
		// quarantined ports with it, in this critical section.
		s.rehostQuarantineLocked(n, addr)
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

// rehostQuarantineLocked is the quarantine's half of a node's address change:
// the entries are keyed on the worker host (what route rows carry), so a
// re-registration under a new host would orphan them while gateways still
// route the old host to this worker's ports. They are rewritten to the new
// host; when another node record still has the old host (two records, one
// worker), they are copied instead, so both keys stay protected.
//
// The key is the host STRING: two spellings of one IPv6 address (or a name
// and its address) are two keys, and nothing here joins them.
// Caller holds the lock.
func (s *State) rehostQuarantineLocked(n *model.Node, addr string) {
	oldHost, _ := agentclient.SplitHostPort(n.Addr)
	newHost, _ := agentclient.SplitHostPort(addr)
	if oldHost == newHost {
		return
	}
	shared := false
	for _, o := range s.Nodes {
		if o != n {
			if h, _ := agentclient.SplitHostPort(o.Addr); h == oldHost {
				shared = true
				break
			}
		}
	}
	var copies []model.ReleasedPort
	for i := range s.Quarantine {
		if s.Quarantine[i].Host != oldHost {
			continue
		}
		if shared {
			c := s.Quarantine[i]
			c.Host = newHost
			copies = append(copies, c)
		} else {
			s.Quarantine[i].Host = newHost
		}
	}
	s.Quarantine = append(s.Quarantine, copies...)
}

// reclaimStaleNodesLocked drops node records that are PROVABLY dead, to make
// room at the MaxNodes ceiling. Caller must hold the lock.
//
// Two conditions, both required, and both about being sure rather than being
// thorough:
//
//   - silent for at least StaleNodeTTL. See that constant: it is ~120,000 missed
//     heartbeats, so a node that is merely rebooting is nowhere near it.
//   - no sandbox references the record. A node id is how feluccad finds the
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

// Heartbeat updates a node's live stats and the isolation it reports
// (isolationOK nil: the heartbeat did not carry it). It returns a copy of the
// node as updated and the isolation value it held before, so the caller can
// tell a change from a repeat; ok is false if the node is unknown.
func (s *State) Heartbeat(id string, memFree uint64, vmCount, poolSize uint32, isolationOK *bool, now int64) (node model.Node, wasIsolationOK *bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.FindNode(id)
	if n == nil {
		return model.Node{}, nil, false
	}
	wasIsolationOK = n.IsolationOK
	n.MemFreeMiB = memFree
	n.VMCount = vmCount
	n.PoolSize = poolSize
	n.LastHB = now
	n.IsolationOK = isolationOK
	return *n, wasIsolationOK, true
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
	s.RemoveSandboxLocked(id)
}

// RemoveSandboxLocked is RemoveSandbox for a caller that holds the lock — one
// that has to remove the record in the same critical section as something
// else (the usage event describing the removal).
//
// Every node port the sandbox's route rows held is quarantined here (FLU-6),
// once per port: two names on one guest port share one node port.
func (s *State) RemoveSandboxLocked(id string) {
	for i, sb := range s.Sandboxes {
		if sb.ID == id {
			released := map[uint16]bool{}
			for _, e := range sb.Exposes {
				if !released[e.NodePort] {
					released[e.NodePort] = true
					s.releaseLocked(sb, e)
				}
			}
			s.Sandboxes = append(s.Sandboxes[:i], s.Sandboxes[i+1:]...)
			return
		}
	}
}

// Two questions about removing a row, with two answers, because the worker
// and the quarantine key on different things:
//
//   - GuestPortShared: may the worker's mapping be removed? The worker keeps
//     one mapping per (VM, guest port) and removes BY GUEST PORT, so the call
//     is skipped while another row of the sandbox has the same guest port —
//     whatever node port that row names. Rows written before this release can
//     hold one guest port on two node ports (a stale one and the live one);
//     removing the stale name must not take the live mapping with it.
//   - NodePortNamed: is the node port released? Only when no remaining row of
//     the sandbox names it; then it is quarantined.

// GuestPortShared reports whether another row of sb (another name) has e's
// guest port: the worker's mapping for that guest port is still needed.
func GuestPortShared(sb *model.Sandbox, e model.Expose) bool {
	for _, o := range sb.Exposes {
		if o.Name != e.Name && o.GuestPort == e.GuestPort {
			return true
		}
	}
	return false
}

// NodePortNamed reports whether a row of sb other than the one named except
// names nodePort.
func NodePortNamed(sb *model.Sandbox, nodePort uint16, except string) bool {
	for _, o := range sb.Exposes {
		if o.Name != except && o.NodePort == nodePort {
			return true
		}
	}
	return false
}

// RemoveExposeLocked removes the route row name of sandbox id and reports
// whether there was one. Its node port is quarantined when no remaining row
// of the sandbox names it (NodePortNamed). Caller holds the lock.
func (s *State) RemoveExposeLocked(id, name string) bool {
	sb := s.FindSandbox(id)
	if sb == nil {
		return false
	}
	var gone *model.Expose
	kept := sb.Exposes[:0]
	for _, e := range sb.Exposes {
		if e.Name == name && gone == nil {
			c := e
			gone = &c
			continue
		}
		kept = append(kept, e)
	}
	if gone == nil {
		return false
	}
	sb.Exposes = kept
	if !NodePortNamed(sb, gone.NodePort, gone.Name) {
		s.releaseLocked(sb, *gone)
	}
	if len(sb.Exposes) == 0 {
		sb.Exposes = nil
	}
	return true
}

// RemoveExposesOnPortLocked removes every route row of sandbox id that maps
// nodePort, quarantining the port once, and returns how many rows went. It is
// for a row the worker has proved stale (it handed the port to another
// sandbox). Caller holds the lock.
func (s *State) RemoveExposesOnPortLocked(id string, nodePort uint16) int {
	sb := s.FindSandbox(id)
	if sb == nil {
		return 0
	}
	var first *model.Expose
	n := 0
	kept := sb.Exposes[:0]
	for _, e := range sb.Exposes {
		if e.NodePort == nodePort {
			if first == nil {
				c := e
				first = &c
			}
			n++
			continue
		}
		kept = append(kept, e)
	}
	sb.Exposes = kept
	if len(sb.Exposes) == 0 {
		sb.Exposes = nil
	}
	if first != nil {
		s.releaseLocked(sb, *first)
	}
	return n
}

// RepointGuestPortLocked moves every row of sandbox id on guestPort to
// nodePort and quarantines each node port they held before (once per port).
// It is for a worker that answered an expose of that guest port with a
// different port than the rows name: the worker is the authority, its old
// mapping is gone, and a port left in a row it no longer has would otherwise
// leave the rows later without being quarantined. Returns the old ports.
// Caller holds the lock.
func (s *State) RepointGuestPortLocked(id string, guestPort, nodePort uint16) []uint16 {
	sb := s.FindSandbox(id)
	if sb == nil {
		return nil
	}
	var old []uint16
	seen := map[uint16]bool{}
	for i := range sb.Exposes {
		e := &sb.Exposes[i]
		if e.GuestPort != guestPort || e.NodePort == nodePort {
			continue
		}
		if !seen[e.NodePort] {
			seen[e.NodePort] = true
			old = append(old, e.NodePort)
			s.releaseLocked(sb, *e)
		}
		e.NodePort = nodePort
	}
	return old
}

// DoubleBooking is one worker host's node port that route rows of more than
// one sandbox name.
type DoubleBooking struct {
	Host       string
	Port       uint16
	SandboxIDs []string
}

// DoubleBookedLocked lists the (host, node port) pairs that rows of two or
// more different sandboxes name: rows written before FLU-6, when a delete
// that timed out in feluccad left a row whose port the worker then gave to
// another sandbox. Sorted by host, then port. Caller holds the lock.
func (s *State) DoubleBookedLocked() []DoubleBooking {
	type key struct {
		host string
		port uint16
	}
	owners := map[key][]string{}
	var keys []key
	for _, sb := range s.Sandboxes {
		host, ok := s.NodeHostLocked(sb)
		if !ok {
			continue
		}
		for _, e := range sb.Exposes {
			k := key{host, e.NodePort}
			ids := owners[k]
			if len(ids) == 0 {
				keys = append(keys, k)
			}
			if len(ids) == 0 || ids[len(ids)-1] != sb.ID {
				owners[k] = append(ids, sb.ID)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].host != keys[j].host {
			return keys[i].host < keys[j].host
		}
		return keys[i].port < keys[j].port
	})
	var out []DoubleBooking
	for _, k := range keys {
		if ids := owners[k]; len(ids) > 1 {
			out = append(out, DoubleBooking{Host: k.host, Port: k.port, SandboxIDs: ids})
		}
	}
	return out
}

// SplitGuestPort is one guest port of a sandbox that its rows map to more
// than one node port.
type SplitGuestPort struct {
	SandboxID string
	GuestPort uint16
	NodePorts []uint16
}

// SplitGuestPortsLocked lists the guest ports that rows of one sandbox map to
// two or more node ports: rows written before this release (an unexpose that
// timed out in feluccad, then a fresh mapping). The worker holds one mapping
// per guest port, so all but one of those ports are stale. Ordered as the
// sandboxes and their rows are. Caller holds the lock.
func (s *State) SplitGuestPortsLocked() []SplitGuestPort {
	var out []SplitGuestPort
	for _, sb := range s.Sandboxes {
		var order []uint16
		ports := map[uint16][]uint16{}
		for _, e := range sb.Exposes {
			ps := ports[e.GuestPort]
			if len(ps) == 0 {
				order = append(order, e.GuestPort)
			}
			dup := false
			for _, p := range ps {
				dup = dup || p == e.NodePort
			}
			if !dup {
				ports[e.GuestPort] = append(ps, e.NodePort)
			}
		}
		for _, g := range order {
			if len(ports[g]) > 1 {
				out = append(out, SplitGuestPort{SandboxID: sb.ID, GuestPort: g, NodePorts: ports[g]})
			}
		}
	}
	return out
}

// NodeHostLocked is the worker host of a sandbox: the host part of its node's
// agent address, as route rows carry it. ok is false when the sandbox has no
// node record (a port there is not reachable through any route). Caller holds
// the lock.
func (s *State) NodeHostLocked(sb *model.Sandbox) (host string, ok bool) {
	if sb == nil || sb.NodeID == nil {
		return "", false
	}
	n := s.FindNode(*sb.NodeID)
	if n == nil {
		return "", false
	}
	host, _ = agentclient.SplitHostPort(n.Addr)
	return host, true
}

// releaseLocked quarantines e's node port. A sandbox without a node record
// releases nothing that can be routed: no entry. Caller holds the lock.
func (s *State) releaseLocked(sb *model.Sandbox, e model.Expose) {
	host, ok := s.NodeHostLocked(sb)
	if !ok || e.NodePort == 0 {
		return
	}
	s.Quarantine = append(s.Quarantine, model.ReleasedPort{
		Host: host, Port: e.NodePort, SandboxID: sb.ID, GuestPort: e.GuestPort,
		TenantID: sb.TenantID, ReleasedAt: s.Now(),
	})
}

// clampFutureLocked sets the release time of every entry released "in the
// future" (the clock stepped back) to now; see the clock note above
// QuarantineExpiresIn. Caller holds the lock.
func (s *State) clampFutureLocked(now int64) {
	for i := range s.Quarantine {
		if s.Quarantine[i].ReleasedAt > now {
			s.Quarantine[i].ReleasedAt = now
		}
	}
}

// QuarantineLocked returns copies of the entries still in quarantine at now.
// Caller holds the lock.
func (s *State) QuarantineLocked(now int64) []model.ReleasedPort {
	s.clampFutureLocked(now)
	var out []model.ReleasedPort
	for _, q := range s.Quarantine {
		if QuarantineActive(q, now) {
			out = append(out, q)
		}
	}
	return out
}

// UnquarantineLocked drops the entries of (host, port) that sandbox sbID
// released for guestPort: the worker handed the port back to its last owner
// for the same service, so its old hostname reaches the same service again.
// Entries other sandboxes left on the same port stay. Caller holds the lock.
func (s *State) UnquarantineLocked(host string, port uint16, sbID string, guestPort uint16) {
	kept := s.Quarantine[:0]
	for _, q := range s.Quarantine {
		if q.Host == host && q.Port == port && q.SandboxID == sbID && q.GuestPort == guestPort {
			continue
		}
		kept = append(kept, q)
	}
	s.Quarantine = kept
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

// Snapshot is a point-in-time copy of everything the store persists from the
// working set. It shares no memory with the State it was taken from, so it can
// be written to the database after the lock is released while handlers keep
// changing the live records.
//
// The copy has to be DEEP. Records are changed in place under the lock (state
// and activity clocks, a heartbeat's stats), and slices are compacted in place
// (expose removal does `kept := sb.Exposes[:0]`, node reclamation does the same
// to Nodes), so a copy of the pointers, or of a slice header, would be read by
// the database writer while a handler rewrites the same memory.
type Snapshot struct {
	Nodes        []model.Node
	Sandboxes    []model.Sandbox
	Seq          uint64
	RequestCount uint64
	// Quarantine is the released node ports still in quarantine (FLU-6). It
	// is in the snapshot, not written on its own, so a route row's removal and
	// its port's quarantine reach the database in one transaction.
	Quarantine []model.ReleasedPort
}

// SnapshotLocked deep-copies the persisted part of the working set. Quarantine
// entries older than PortQuarantine + Margin are dropped from the working set
// here: this is the one place that runs after every change. The margin keeps
// an entry a forward clock step expired early until the step can have been
// corrected (see QuarantineExpiresIn). Caller must hold the lock.
func (s *State) SnapshotLocked() *Snapshot {
	now := s.Now()
	s.clampFutureLocked(now)
	kept := s.Quarantine[:0]
	for _, q := range s.Quarantine {
		if now-q.ReleasedAt < keepS {
			kept = append(kept, q)
		}
	}
	s.Quarantine = kept
	snap := &Snapshot{
		Nodes:        make([]model.Node, len(s.Nodes)),
		Sandboxes:    make([]model.Sandbox, len(s.Sandboxes)),
		Seq:          s.Seq,
		RequestCount: s.RequestCount,
		Quarantine:   append([]model.ReleasedPort(nil), s.Quarantine...),
	}
	for i, n := range s.Nodes {
		snap.Nodes[i] = *n
		// Not part of the persisted set: a reported value is only true until
		// the next heartbeat (FLU-8 N10).
		snap.Nodes[i].IsolationOK = nil
	}
	for i, sb := range s.Sandboxes {
		c := *sb
		c.NodeID = copyStr(sb.NodeID)
		c.IP = copyStr(sb.IP)
		c.ParentID = copyStr(sb.ParentID)
		if sb.Exposes != nil {
			c.Exposes = append([]model.Expose(nil), sb.Exposes...)
		}
		snap.Sandboxes[i] = c
	}
	return snap
}

// Snapshot is SnapshotLocked for a caller that does not hold the lock.
func (s *State) Snapshot() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SnapshotLocked()
}

func copyStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

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
	// address, every sandbox id and the tenant each belongs to. feluccad is the
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
			ID:                ls.ID,
			Name:              ls.Name,
			Namespace:         ns,
			NodeID:            ls.NodeID,
			State:             model.ParseSandboxState(ls.State),
			VCPUs:             vcpus,
			MemMiB:            memMiB,
			IP:                ls.IP,
			CreatedAt:         ls.CreatedAt,
			ParentID:          ls.ParentID,
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
