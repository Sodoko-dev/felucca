// Persistence (FLU-5): the one save path, store health and its recovery loop,
// the gate that refuses changes while the store is failing, and the two usage
// queues (lifecycle events, written with the snapshot; exec rows, flushed in
// the background).
//
// The contract this file exists for: a request that changes durable state is
// answered 2xx only once the change is in the database. Before it, every save
// site logged a failed save and answered success anyway, so a database another
// process held locked (or a full disk) acknowledged creates, sleeps, exposes and
// deletes that a restart then lost.
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

const (
	// storeRetryInterval is how often the recovery loop retries the save while
	// the store is failing.
	storeRetryInterval = 2 * time.Second
	// storeRetryAfter is the Retry-After (seconds) on every 503 the store's
	// state causes.
	storeRetryAfter = "5"
	// maxExecUsageQueue bounds the exec rows waiting for the flusher. At the
	// bound the OLDEST rows are dropped (and counted): an exec must never wait
	// on, or be refused because of, its metering row.
	maxExecUsageQueue = 10000
	// execUsageFlushInterval is the flusher's cadence.
	execUsageFlushInterval = time.Second
)

// Process-lifetime counters for /metrics, kept off the state lock like the
// latency histograms (metrics.go).
var (
	// persistFailures counts failed saves: snapshot saves and the row-level
	// writes whose failure a handler reports as an error.
	persistFailures atomic.Uint64
	// usageWriteFailures counts usage rows whose write failed (per attempt)
	// and exec rows dropped from the full queue.
	usageWriteFailures atomic.Uint64
)

// storeHealth is whether the database has caught up with memory. failing is set
// by any failed save and cleared ONLY by a successful snapshot save — which
// writes whatever memory holds at that moment, so "healthy" means nothing
// acknowledged or applied is missing from the database any more.
type storeHealth struct {
	mu      sync.Mutex
	failing bool
	since   time.Time // first failure of the current outage
	lastErr error
	// recovering is true while a recovery loop runs; at most one does.
	recovering bool
}

// storeFailing reports whether the last save failed and none has succeeded
// since. It takes only the health mutex, never the state lock.
func (srv *Server) storeFailing() bool {
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	return srv.health.failing
}

// StoreHealthy is storeFailing for cmd/feluccad (start-up reporting).
func (srv *Server) StoreHealthy() bool { return !srv.storeFailing() }

// storeWriteFailed records a failed write: counted, the store marked failing,
// and the recovery loop started if none is running.
func (srv *Server) storeWriteFailed(err error) {
	persistFailures.Add(1)
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	if !srv.health.failing {
		srv.health.failing = true
		srv.health.since = time.Now()
		slog.Error("store failing: changes are refused until a save succeeds", "err", err)
	}
	srv.health.lastErr = err
	if !srv.health.recovering {
		srv.health.recovering = true
		go srv.storeRecoveryLoop()
	}
}

// storeSaved records a successful snapshot save.
func (srv *Server) storeSaved() {
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	if srv.health.failing {
		slog.Info("store recovered: the database has caught up with memory",
			"outage", time.Since(srv.health.since).Round(time.Millisecond), "last_err", srv.health.lastErr)
	}
	srv.health.failing = false
	srv.health.lastErr = nil
}

// stopRecoveringIfHealthy ends the recovery loop's run when the store is
// healthy. The check and the flag change share the mutex storeWriteFailed
// takes, so a failure that lands right after this returns true starts a new
// loop rather than being missed by an exiting one.
func (srv *Server) stopRecoveringIfHealthy() bool {
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	if srv.health.failing {
		return false
	}
	srv.health.recovering = false
	return true
}

// storeRecoveryLoop retries the save every storeRetryInterval while the store
// is failing. Nothing else would: the gate refuses every change, so without it
// a store that comes back would never be written to again and the control
// plane would refuse changes forever. A panic is recovered per attempt — this
// goroutine has no net/http underneath it, and a panic here would end the
// process.
func (srv *Server) storeRecoveryLoop() {
	t := time.NewTicker(storeRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-srv.quit:
			srv.health.mu.Lock()
			srv.health.recovering = false
			srv.health.mu.Unlock()
			return
		}
		if srv.stopRecoveringIfHealthy() {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("store recovery: save panicked", "panic", r)
				}
			}()
			if err := srv.save(nil); err != nil {
				slog.Warn("store still failing", "err", err)
			}
		}()
		if srv.stopRecoveringIfHealthy() {
			return
		}
	}
}

// stopBackground ends the recovery loop (tests; feluccad never stops).
func (srv *Server) stopBackground() {
	srv.quitOnce.Do(func() { close(srv.quit) })
}

// save is THE save path: every change to the working set reaches the database
// through here.
//
//   - It copies the working set and takes the queued lifecycle events under the
//     state lock, releases it, and only then talks to the database. A save that
//     waits out the busy timeout no longer holds every request behind it.
//   - saveMu serializes saves, so the copies reach the database in the order
//     they were taken: a stale copy can never be written after a newer one.
//     Lock order: exposeMu -> saveMu -> st. It must never be called with the
//     state lock held.
//   - On failure the events go back to the head of the queue (the next save
//     writes them with the state they describe), the store is marked failing,
//     and undo — if the caller passed one — runs BEFORE saveMu is released.
//     That last part is the point of taking it here: no other save, the
//     recovery loop included, can store the state that is being undone.
//
// undo takes the state lock itself.
func (srv *Server) save(undo func()) error {
	srv.saveMu.Lock()
	defer srv.saveMu.Unlock()

	var snap *state.Snapshot
	var events []store.UsageEvent
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		snap = srv.st.SnapshotLocked()
		events = srv.pendingUsage
		srv.pendingUsage = nil
	}()

	err := srv.writeSnapshot(snap, events)
	if err == nil {
		srv.storeSaved()
		return nil
	}
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		srv.pendingUsage = append(events, srv.pendingUsage...)
	}()
	if undo != nil {
		undo()
	}
	srv.storeWriteFailed(err)
	return err
}

// Save is save(nil) for cmd/feluccad's start-up save.
func (srv *Server) Save() error { return srv.save(nil) }

// writeSnapshot calls the store, turning a panic into an error. A panic there
// is a failed save like any other: the events are re-queued, the store is
// marked failing and the recovery loop retries, instead of the events being
// lost with the goroutine (or, in the lifecycle sweep, the process).
func (srv *Server) writeSnapshot(snap *state.Snapshot, events []store.UsageEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("save panicked: %v", r)
		}
	}()
	return srv.db.SaveSnapshot(snap, events)
}

// ---- Lifecycle usage events ----

// queueUsageLocked queues a lifecycle event for sb. The caller must hold the
// state lock, and must be in the same critical section as the state change the
// event describes: the next save then writes both in one transaction, or
// neither.
func (srv *Server) queueUsageLocked(sb *model.Sandbox, event string) {
	if sb == nil {
		return
	}
	srv.pendingUsage = append(srv.pendingUsage, usageEventFor(sb, event))
}

// ---- Exec usage rows ----

// execUsageQueue holds exec rows until the flusher writes them. Its own mutex:
// exec is the hot path and must not wait on the state lock or the database for
// its metering row.
type execUsageQueue struct {
	mu   sync.Mutex
	rows []store.UsageEvent
	// dropped counts rows dropped since the flusher last logged it.
	dropped uint64
}

// queueExecUsage queues one exec row, dropping the oldest when full.
func (srv *Server) queueExecUsage(e store.UsageEvent) {
	q := &srv.execUsage
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rows = appendBounded(q, q.rows, e)
}

// appendBounded appends rows to dst keeping at most maxExecUsageQueue, oldest
// dropped first and counted. Caller holds q.mu.
func appendBounded(q *execUsageQueue, dst []store.UsageEvent, rows ...store.UsageEvent) []store.UsageEvent {
	dst = append(dst, rows...)
	if over := len(dst) - maxExecUsageQueue; over > 0 {
		dst = dst[over:]
		q.dropped += uint64(over)
		usageWriteFailures.Add(uint64(over))
	}
	return dst
}

// flushExecUsage writes the queued exec rows in one transaction. On failure
// they go back to the head of the queue (still bounded). Tests call it
// directly to flush synchronously.
func (srv *Server) flushExecUsage() error {
	srv.execFlushMu.Lock()
	defer srv.execFlushMu.Unlock()

	q := &srv.execUsage
	q.mu.Lock()
	batch := q.rows
	q.rows = nil
	dropped := q.dropped
	q.dropped = 0
	q.mu.Unlock()
	if dropped > 0 {
		slog.Error("usage: exec rows dropped, the queue was full", "dropped", dropped, "max", maxExecUsageQueue)
	}
	if len(batch) == 0 {
		return nil
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("usage flush panicked: %v", r)
			}
		}()
		return srv.db.AppendUsageBatch(batch)
	}()
	if err == nil {
		return nil
	}
	usageWriteFailures.Add(uint64(len(batch)))
	q.mu.Lock()
	q.rows = appendBounded(q, batch, q.rows...)
	q.mu.Unlock()
	return err
}

// UsageFlushLoop flushes the exec rows every execUsageFlushInterval until stop
// closes, then once more.
func (srv *Server) UsageFlushLoop(stop <-chan struct{}) {
	t := time.NewTicker(execUsageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := srv.flushExecUsage(); err != nil {
				slog.Warn("usage: exec rows not written, kept for the next flush", "err", err)
			}
		case <-stop:
			if err := srv.flushExecUsage(); err != nil {
				slog.Error("usage: final exec flush failed", "err", err)
			}
			return
		}
	}
}

// ---- Answers ----

// refuseStoreUnavailable answers a request the store's state does not let
// feluccad serve: nothing was done.
func refuseStoreUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", storeRetryAfter)
	writeJSON(w, 503, []byte(`{"error":"store unavailable"}`))
}

// appliedNotSaved answers a request the worker carried out but whose save
// failed: memory keeps what the worker did (it is true), the database does
// not have it yet, and the recovery loop will write it. Nothing is undone on
// the worker; every verb that answers this is idempotent there, so a retry is
// safe.
func appliedNotSaved(w http.ResponseWriter) {
	w.Header().Set("Retry-After", storeRetryAfter)
	writeJSON(w, 503, []byte(`{"error":"applied but not saved","applied":true}`))
}

// refusedWhileStoreFailing is the gate: true (and the 503 written) when the
// store is failing. Called after authentication and before any lookup or
// worker call, so a refused request has done nothing at all.
func (srv *Server) refusedWhileStoreFailing(w http.ResponseWriter) bool {
	if !srv.storeFailing() {
		return false
	}
	refuseStoreUnavailable(w)
	return true
}

// changesDurableState classifies an authenticated /api/ request: true for the
// routes that change what the database holds and are therefore refused while
// the store is failing.
//
// Not refused: every GET (reads), exec (it changes nothing durable but an
// activity clock and a metering row, and it is the data plane), the agents'
// heartbeat, node registration (refused inside agentRegister only when it
// spends a join token: the node rows are rebuilt by registration anyway), and
// the gateway's activity report. /metrics, /healthz and the node join are not
// routed through here.
func changesDurableState(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return false
	}
	switch {
	case strings.HasPrefix(path, "/api/v1/sandboxes/") && strings.HasSuffix(path, "/exec"):
		return false
	case path == "/api/v1/sandboxes", strings.HasPrefix(path, "/api/v1/sandboxes/"),
		path == "/api/v1/routes/ensure",
		path == "/api/v1/tenants", strings.HasPrefix(path, "/api/v1/tenants/"),
		strings.HasPrefix(path, "/api/v1/keys/"),
		path == "/api/v1/templates", strings.HasPrefix(path, "/api/v1/templates/"),
		path == "/api/v1/join-tokens":
		return true
	}
	return false
}

// ---- Clean-up after a create or fork whose last save failed ----

// removeUnsavedVM deletes, on the worker, a VM whose create (or fork) the
// caller was told failed: the record was set to error by the save's undo and
// the answer was 503. It runs after the response has been written — the
// request never waits for a second worker call.
//
// When the worker deletes it, the record goes and a "deleted" event is queued
// (the undo already queued "stopped", so the usage interval is closed either
// way); then a save is attempted. When it does not, the record stays in error:
// visible, and deletable by its owner.
func (srv *Server) removeUnsavedVM(agentAddr, id, reqID string) {
	resp, err := srv.dialNode(agentAddr).do(http.MethodDelete, "/v1/vms/"+id, nil, reqID)
	if err != nil || (resp.Status >= 300 && resp.Status != 404) {
		status := 0
		if resp != nil {
			status = resp.Status
		}
		slog.Error("unsaved VM could not be removed from its worker: the sandbox stays in error and can be deleted",
			"sandbox", id, "node", agentAddr, "request_id", reqID, "status", status, "err", err)
		return
	}
	removed := false
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || sb.State != model.StateError {
			return // already deleted by its owner, or no longer ours to remove
		}
		srv.queueUsageLocked(sb, "deleted")
		srv.st.RemoveSandboxLocked(id)
		removed = true
	}()
	if !removed {
		return
	}
	if err := srv.save(nil); err != nil {
		slog.Error("unsaved VM removed from its worker; the removal is not saved yet (the recovery loop will write it)",
			"sandbox", id, "request_id", reqID, "err", err)
	}
}
