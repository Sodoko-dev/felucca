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
	"errors"
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
	// the store is failing (srv.retryInterval; tests shorten it).
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

// errStoreFailing is what a request's save returns, without asking the
// database, while the store is already failing (P16).
var errStoreFailing = errors.New("store failing: not attempted, the recovery loop retries the save")

// Process-lifetime counters for /metrics, kept off the state lock like the
// latency histograms (metrics.go).
var (
	// persistFailures counts failed writes: snapshot saves, exec usage
	// flushes, and the row-level writes whose failure a handler reports as an
	// error. A save refused because the store is already failing is not a
	// write and is not counted.
	persistFailures atomic.Uint64
	// usageWriteFailures counts failed write attempts that carried usage rows
	// (one per attempt, snapshot save or exec flush, however many rows it
	// carried): rows still queued and retried.
	usageWriteFailures atomic.Uint64
	// usageRowsDropped counts exec rows dropped from the full queue, each
	// once: rows lost. Apart from usageWriteFailures so an alert can tell
	// "retrying" from "lost".
	usageRowsDropped atomic.Uint64
	// storeReadErrors counts failed store reads (key lookups, quota and policy
	// reads, credentials, listings). A read outage is otherwise invisible: the
	// write health stays 1 and /healthz 200 while every keyed request is 503.
	storeReadErrors atomic.Uint64
)

// storeReadFailed counts a failed store read. Every read-error branch calls
// it, whatever it answers.
func storeReadFailed() { storeReadErrors.Add(1) }

// storeHealth is whether the database has caught up with memory. failing is set
// by any failed write and cleared ONLY by the recovery loop, when one attempt
// wrote both the snapshot (whatever memory holds at that moment) and the
// queued exec rows — so "healthy" means nothing acknowledged or applied is
// missing from the database any more.
type storeHealth struct {
	mu      sync.Mutex
	failing bool
	since   time.Time // first failure of the current outage
	lastErr error
	// recovering is true while a recovery loop runs; at most one does.
	recovering bool
	// onRecovered runs once, after the next recovery (cmd/feluccad: rename a
	// legacy state file whose import was saved only then).
	onRecovered func()
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
// and the recovery loop started if none is running. It logs after releasing
// the health mutex: /healthz and the gate take it, and a blocked stderr must
// not stall them.
func (srv *Server) storeWriteFailed(err error) {
	persistFailures.Add(1)
	srv.health.mu.Lock()
	first := !srv.health.failing
	if first {
		srv.health.failing = true
		srv.health.since = time.Now()
	}
	srv.health.lastErr = err
	start := !srv.health.recovering
	srv.health.recovering = true
	srv.health.mu.Unlock()
	if first {
		slog.Error("store failing: changes are refused until a save succeeds", "err", err)
	}
	if start {
		srv.goBackground(srv.storeRecoveryLoop)
	}
}

// goBackground runs fn on its own goroutine, counted in srv.bg so that
// WaitBackground can wait for it: work that writes the database after the
// response (the recovery loop, a clean-up, a registration's save) must not
// outlive a test's database.
func (srv *Server) goBackground(fn func()) {
	srv.bg.Add(1)
	go func() {
		defer srv.bg.Done()
		fn()
	}()
}

// WaitBackground waits for the background work started so far (tests; call
// StopBackground first, or a failing store's recovery loop never ends).
func (srv *Server) WaitBackground() { srv.bg.Wait() }

// storeSaved records a recovery: the snapshot and the exec rows are written.
func (srv *Server) storeSaved() {
	srv.health.mu.Lock()
	was, since, lastErr := srv.health.failing, srv.health.since, srv.health.lastErr
	srv.health.failing = false
	srv.health.lastErr = nil
	srv.health.mu.Unlock()
	if was {
		slog.Info("store recovered: the database has caught up with memory",
			"outage", time.Since(since).Round(time.Millisecond), "last_err", lastErr)
	}
}

// OnStoreRecovered sets fn to run once, after the recovery loop next finds
// the database caught up. cmd/feluccad uses it for a legacy state file whose
// import could not be saved at start-up.
func (srv *Server) OnStoreRecovered(fn func()) {
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	srv.health.onRecovered = fn
}

// stopRecoveringIfHealthy ends the recovery loop's run when the store is
// healthy. The check and the flag change share the mutex storeWriteFailed
// takes, so a failure that lands right after this returns true starts a new
// loop rather than being missed by an exiting one.
//
// It returns, when it ends the run, the onRecovered callback to call (nil when
// none is set), which then is cleared.
func (srv *Server) stopRecoveringIfHealthy() (bool, func()) {
	srv.health.mu.Lock()
	defer srv.health.mu.Unlock()
	if srv.health.failing {
		return false, nil
	}
	srv.health.recovering = false
	fn := srv.health.onRecovered
	srv.health.onRecovered = nil
	return true, fn
}

// storeRecoveryLoop retries every srv.retryInterval while the store is
// failing. Nothing else would: the gate refuses every change and a request's
// save fails at once, so without it a store that comes back would never be
// written to again and the control plane would refuse changes forever. A panic
// is recovered per attempt — this goroutine has no net/http underneath it, and
// a panic here would end the process.
func (srv *Server) storeRecoveryLoop() {
	t := time.NewTicker(srv.retryInterval)
	defer t.Stop()
	stopIfHealthy := func() bool {
		ok, fn := srv.stopRecoveringIfHealthy()
		if fn != nil {
			fn()
		}
		// The legacy credential seed of a start whose database could not be
		// written: run again now that it can.
		if ok && srv.seedPending.Swap(false) {
			srv.seedLegacyNodeCreds()
		}
		return ok
	}
	for {
		select {
		case <-t.C:
		case <-srv.quit:
			srv.health.mu.Lock()
			srv.health.recovering = false
			srv.health.mu.Unlock()
			return
		}
		if stopIfHealthy() {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("store recovery: save panicked", "panic", r)
				}
			}()
			if err := srv.recoverStore(); err != nil {
				slog.Warn("store still failing", "err", err)
			}
		}()
		if stopIfHealthy() {
			return
		}
	}
}

// recoverStore is one attempt of the recovery loop: a fresh writer connection,
// then the exec flush, then the snapshot save. The store is healthy again only
// when both writes succeeded in this attempt; marked by either alone, health
// would flap between the flusher and the loop while one of them still fails.
//
// The exec flush runs first and OUTSIDE saveMu: a request's save waits for
// saveMu, and with a locked database every write here can take the whole busy
// timeout, so holding saveMu across both made "a request's save fails at once"
// a wait of two timeouts. The snapshot save and the healthy mark are under
// saveMu together. A request whose change is in memory but not in this copy
// reaches saveMu only after the mark, so its own save is a real write, not a
// refusal the loop has already stopped retrying for.
func (srv *Server) recoverStore() error {
	// Before each retry: a connection opened while the file could not be
	// written stays read-only for its whole life (store.SQLite.ResetWriter).
	srv.db.ResetWriter()
	if err := srv.flushExecUsage(); err != nil {
		return err
	}
	srv.saveMu.Lock()
	defer srv.saveMu.Unlock()
	if err := srv.saveLocked(nil, nil); err != nil {
		return err
	}
	srv.storeSaved()
	return nil
}

// StopBackground ends the recovery loop (tests, and cmd/feluccad's shutdown).
func (srv *Server) StopBackground() {
	srv.quitOnce.Do(func() { close(srv.quit) })
}

// save is THE save path of a request (and of the sweep and the clean-ups):
// every change to the working set reaches the database through here.
//
//   - It copies the working set and takes the queued lifecycle events under the
//     state lock, releases it, and only then talks to the database. A save that
//     waits out the busy timeout no longer holds every request behind it.
//   - saveMu serializes saves, so the copies reach the database in the order
//     they were taken: a stale copy can never be written after a newer one.
//     Lock order: exposeMu -> saveMu -> st. It must never be called with the
//     state lock held.
//   - mutate, when not nil, runs under saveMu and the state lock immediately
//     before the copy. A change made there cannot be stored by any other save
//     before this one: a save of another request that runs between a
//     handler's change and its own save would otherwise write the change, and
//     then this save's undo could not take it back out of the database.
//   - On failure the events go back to the head of the queue (the next save
//     writes them with the state they describe), the store is marked failing,
//     and undo — if the caller passed one — runs BEFORE saveMu is released, so
//     no later save, the recovery loop included, can store the state that is
//     being undone.
//   - While the store is ALREADY failing it does not ask the database (P16):
//     mutate and undo run as for a failed write and errStoreFailing is
//     returned at once — "at once" meaning as soon as it holds saveMu, which
//     is after at most one snapshot attempt (the save ahead of it, or the
//     recovery loop's, which holds saveMu for its snapshot write only). Each
//     save would otherwise wait out its own busy timeout in turn, and a dozen
//     changes in flight at the onset of a lock outlast the server's
//     WriteTimeout. The recovery loop is the one that retries.
//
// mutate runs with the state lock held (it uses the *Locked state methods);
// undo takes the state lock itself.
func (srv *Server) save(mutate, undo func()) error {
	srv.saveMu.Lock()
	defer srv.saveMu.Unlock()
	if srv.storeFailing() {
		if mutate != nil {
			func() {
				srv.st.Lock()
				defer srv.st.Unlock()
				mutate()
			}()
		}
		if undo != nil {
			undo()
		}
		return errStoreFailing
	}
	return srv.saveLocked(mutate, undo)
}

// saveState is save for a caller with nothing to change under the save mutex
// and nothing to undo: it writes what memory holds.
func (srv *Server) saveState() error { return srv.save(nil, nil) }

// saveLocked is the write of save, for a caller that holds saveMu: save, and
// the two callers that wait for the database however long it takes (the
// recovery loop and the start-up save).
func (srv *Server) saveLocked(mutate, undo func()) error {
	var snap *state.Snapshot
	var events []store.UsageEvent
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		if mutate != nil {
			mutate()
		}
		snap = srv.st.SnapshotLocked()
		events = srv.pendingUsage
		srv.pendingUsage = nil
	}()

	err := srv.writeSnapshot(snap, events)
	if err == nil {
		return nil
	}
	if len(events) > 0 {
		usageWriteFailures.Add(1)
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

// StartupWrite is cmd/feluccad's start-up check that the database can be
// written. full (an empty database, or a legacy import) saves the whole
// working set, waiting for the database like the recovery loop; otherwise one
// small row is written (store.MarkStarted), so what the start loaded is not
// rewritten before the first request. A failure marks the store failing and
// starts the recovery loop.
func (srv *Server) StartupWrite(full bool) error {
	if full {
		srv.saveMu.Lock()
		defer srv.saveMu.Unlock()
		return srv.saveLocked(nil, nil)
	}
	if err := srv.db.MarkStarted(time.Now().Unix()); err != nil {
		srv.storeWriteFailed(err)
		return err
	}
	return nil
}

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
		usageRowsDropped.Add(uint64(over))
	}
	return dst
}

// flushExecUsage writes the queued exec rows in one transaction. On failure
// they go back to the head of the queue (still bounded), and the store is
// marked failing like for any failed write: with a full disk and only exec
// traffic, nothing else would notice, /healthz would say 200 and the next
// sleep would pass the gate only to be answered "applied but not saved".
// Tests call it directly to flush synchronously.
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
	// One per failed attempt: the same backlog retried every second is the
	// same rows, not new failures.
	usageWriteFailures.Add(1)
	q.mu.Lock()
	q.rows = appendBounded(q, batch, q.rows...)
	q.mu.Unlock()
	srv.storeWriteFailed(err)
	return err
}

// UsageFlushLoop flushes the exec rows every execUsageFlushInterval until stop
// closes, then once more. While the store is failing it leaves the rows to
// the recovery loop, whose attempt flushes them with the snapshot.
func (srv *Server) UsageFlushLoop(stop <-chan struct{}) {
	t := time.NewTicker(execUsageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if srv.storeFailing() {
				continue
			}
			if err := srv.flushExecUsage(); err != nil {
				slog.Warn("usage: exec rows not written, kept for the next flush", "err", err)
			}
		case <-stop:
			srv.finalExecFlush()
			return
		}
	}
}

// finalFlushRetry is the pause between attempts of the final exec flush.
const finalFlushRetry = time.Second

// defaultFinalFlushFor bounds the final flush when no deadline was set.
const defaultFinalFlushFor = 10 * time.Second

// SetFinalFlushDeadline bounds the final exec flush (cmd/feluccad: just inside
// the stop's hard limit, so the flush can say what it lost before the process
// is ended).
func (srv *Server) SetFinalFlushDeadline(t time.Time) { srv.finalFlushUntil.Store(t.UnixNano()) }

// finalExecFlush is the flush at the stop. A database held locked fails one
// attempt after its busy time-out; one attempt only lost every queued row with
// the process. It is tried again every finalFlushRetry until it succeeds or
// the deadline passes; then one ERROR says how many rows are lost.
func (srv *Server) finalExecFlush() {
	until := time.Now().Add(defaultFinalFlushFor)
	if d := srv.finalFlushUntil.Load(); d != 0 {
		until = time.Unix(0, d)
	}
	for attempt := 1; ; attempt++ {
		err := srv.flushExecUsage()
		if err == nil {
			if attempt > 1 {
				slog.Info("usage: final exec flush written", "attempts", attempt)
			}
			return
		}
		if time.Now().Add(finalFlushRetry).After(until) {
			srv.execUsage.mu.Lock()
			lost := len(srv.execUsage.rows)
			srv.execUsage.mu.Unlock()
			slog.Error("usage: final exec flush failed; the queued exec usage rows are lost",
				"rows", lost, "attempts", attempt, "err", err)
			return
		}
		slog.Warn("usage: final exec flush failed, trying again", "attempt", attempt, "err", err)
		time.Sleep(finalFlushRetry)
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
//
// The exec exemption is exactly the route serveAPI sends to execSandbox (plain
// and streamed exec are one route): POST /api/v1/sandboxes/{id}/exec. "exec"
// is also a valid expose name, so a looser match would let
// DELETE /api/v1/sandboxes/{id}/expose/exec through while failing.
func changesDurableState(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return false
	}
	if _, ok := matchSuffix(path, "/api/v1/sandboxes/", "/exec"); ok && method == http.MethodPost {
		return false
	}
	switch {
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
//
// A record that is no longer in error once the worker has deleted the VM was
// moved on by its owner meanwhile (a start, answered 200): it is NOT removed
// — that answer was true when given — but it is set back to error with a
// "stopped" event, since its VM is gone (P5'). Left as it was, it would be a
// running record with no VM and an open usage interval.
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
	changed, removed := false, false
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		switch {
		case sb == nil:
			return // already deleted by its owner
		case sb.State != model.StateError:
			sb.State = model.StateError
			srv.queueUsageLocked(sb, "stopped")
		default:
			srv.queueUsageLocked(sb, "deleted")
			srv.st.RemoveSandboxLocked(id)
			removed = true
		}
		changed = true
	}()
	if !changed {
		return
	}
	if !removed {
		slog.Warn("unsaved VM removed from its worker after its owner had moved the record on: the record is set back to error",
			"sandbox", id, "request_id", reqID)
	}
	if err := srv.saveState(); err != nil {
		slog.Error("unsaved VM removed from its worker; the record's change is not saved yet (the recovery loop will write it)",
			"sandbox", id, "request_id", reqID, "err", err)
	}
}

// rowWriteFailed answers a failed row-level write (a tenant, a key, a template,
// a join token, a wg peer). A constraint error is the request's conflict — two
// concurrent creates of one name, an id collision — answered 409 with
// conflict, and the database is fine: marking the store failing for it would
// refuse every change and fail /healthz for a working database. Anything else
// marks the store failing and is answered 500.
func (srv *Server) rowWriteFailed(w http.ResponseWriter, err error, conflict string) {
	if store.IsConstraint(err) {
		writeJSON(w, 409, []byte(conflict))
		return
	}
	srv.storeWriteFailed(err)
	writeJSON(w, 500, []byte(`{"error":"store error"}`))
}
