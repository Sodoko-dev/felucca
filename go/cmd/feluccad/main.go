// feluccad — Felucca control plane (Go port).
// Listens on the configured bind address, exposes the REST API, schedules
// sandboxes onto agents, serves the static UI, and persists in-memory state to
// a JSON file.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/server"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
	"github.com/alpham/infra-saas/felucca/internal/wg"
)

// maxHeaderBytes caps the request head (request line + all headers) on every
// listener. Go's default is 1 MiB, which is three orders of magnitude more than
// any request feluccad serves needs and is per-request attacker-controlled work:
// X-Forwarded-For is parsed on the auth path, so a ~1 MiB header was a ~1 MiB
// allocation an unauthenticated client could ask for at will. 16 KiB leaves
// generous room for a bearer, a trace id and a proxy chain.
const maxHeaderBytes = 16 << 10

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// A bind feluccad cannot honour literally is fatal: the listener must be
	// the one the operator wrote, never a wildcard feluccad substituted for a
	// host it failed to parse.
	if err := cfg.ValidateBind(); err != nil {
		slog.Error("bind", "err", err)
		os.Exit(1)
	}

	// Malformed trusted-proxy CIDRs are fatal for the same reason: the
	// operator believes forwarded client addresses are honoured, and silently
	// dropping the entry changes who the throttle counts.
	if err := cfg.ValidateTrustedProxies(); err != nil {
		slog.Error("trusted-proxies", "err", err)
		os.Exit(1)
	}

	// The admin API is remote root on every worker in the fleet, so refuse to
	// listen without a usable token — same reasoning as the wg-overlay guard
	// below, applied to every deployment. Open mode exists only when the
	// operator names it.
	if err := cfg.ValidateAuth(); err != nil {
		slog.Error("auth", "err", err)
		os.Exit(1)
	}
	if cfg.InsecureNoAuth {
		slog.Warn("INSECURE MODE: --insecure-no-auth is set, the admin API accepts every caller unauthenticated — loopback binds and lab use only",
			"bind", cfg.Bind, "addr", cfg.ListenAddr())
		// Worth saying twice: off-box reachability turns the lab escape hatch
		// into an open control plane, which is remote root on every worker.
		if host, _, err := net.SplitHostPort(cfg.ListenAddr()); err == nil {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				slog.Warn("INSECURE MODE on a non-loopback address: every host that can route here has full admin access", "addr", cfg.ListenAddr())
			}
		}
	}

	// A bind that cannot be reached at feluccad's overlay address takes the
	// whole enrolled fleet offline without a single error on this side: agents
	// just get connection refused. Cheap to say here, expensive to find later.
	if problem := cfg.OverlayBindProblem(); problem != "" {
		slog.Warn("OVERLAY UNREACHABLE: "+problem, "bind", cfg.Bind, "addr", cfg.ListenAddr(), "wg_ip", cfg.WgIP)
	}

	// Ensure the db directory exists (best effort).
	if err := state.EnsureStateDir(cfg.DBPath); err != nil {
		slog.Warn("could not create db dir", "err", err)
	}

	// A pre-rename install keeps its database under the old product name.
	// Opening a fresh felucca.db beside it would look like a clean boot while
	// every tenant, sandbox and node silently vanished — and a regenerated
	// wg.key would strand every enrolled worker. Refuse and say what to move.
	if _, statErr := os.Stat(cfg.DBPath); os.IsNotExist(statErr) {
		if legacy := legacyDBPath(cfg.DBPath); legacy != "" {
			slog.Error("pre-rename database found and the configured one is absent: refusing to start with empty state",
				"legacy", legacy, "expected", cfg.DBPath,
				"fix", "move the state directory as described in docs/CHANGELOG.md (Rename: Hearth → Felucca)")
			os.Exit(1)
		}
	}

	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	srv, err := boot(cfg, db)
	if err != nil {
		slog.Error("refusing to start", "db", cfg.DBPath, "err", err)
		os.Exit(1)
	}

	// WireGuard overlay (v4 P2): bring up wg-felucca and re-add enrolled peers.
	// Empty wg_ip means the overlay is disabled (single-network deployments).
	if cfg.WgIP != "" {
		// Fail fast on config the join flow depends on: a worker that joins
		// against a misconfigured hub burns operator time (and, pre-fix,
		// tokens). The overlay also must never run in open mode — minting
		// join tokens would be unauthenticated kernel network config.
		if cfg.Token == "" {
			slog.Error("wg overlay requires an auth token (--token): refusing open-mode overlay")
			os.Exit(1)
		}
		if cfg.WgEndpoint == "" {
			slog.Error("wg overlay requires --wg-endpoint (public host:port workers dial)")
			os.Exit(1)
		}
		if _, _, err := net.ParseCIDR(cfg.WgIP); err != nil {
			slog.Error("wg overlay: bad --wg-ip", "ip", cfg.WgIP, "err", err)
			os.Exit(1)
		}
		pub, err := wg.EnsureKey(cfg.WgKeyPath)
		if err != nil {
			slog.Error("wg key", "err", err)
			os.Exit(1)
		}
		if err := wg.EnsureInterface(cfg.WgIP, cfg.WgPort, cfg.WgKeyPath); err != nil {
			slog.Error("wg interface", "err", err)
			os.Exit(1)
		}
		peers, err := db.ListWgPeers()
		if err != nil {
			slog.Error("wg peers", "err", err)
			os.Exit(1)
		}
		batch := make([]wg.Peer, 0, len(peers))
		for _, p := range peers {
			batch = append(batch, wg.Peer{PubKey: p.PubKey, OverlayIP: p.OverlayIP})
		}
		if err := wg.AddPeers(batch); err != nil {
			// Peers stay persisted; joins re-install live ones. Warn, don't die.
			slog.Warn("wg re-add peers", "count", len(batch), "err", err)
		}
		srv.SetWgPubKey(pub)
		slog.Info("wg overlay up", "ip", cfg.WgIP, "iface", wg.InterfaceName, "port", cfg.WgPort, "peers", len(peers))
	}

	// SIGTERM and SIGINT stop feluccad (stopOnSignal); registered before
	// anything is served so a signal is never the default kill.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)

	// Lifecycle sweep (v4 P5.2): idle auto-sleep, asleep-TTL auto-delete,
	// hourly usage-event retention. Runs until the stop.
	stop := make(chan struct{})
	go srv.LifecycleLoop(stop)
	// Exec metering rows are queued by the exec path and written here, once a
	// second, so an exec never waits on the database; once more at the stop.
	flushed := make(chan struct{})
	go func() {
		srv.UsageFlushLoop(stop)
		close(flushed)
	}()
	// Every listener, for the stop to close.
	var servers []*http.Server

	addr := cfg.ListenAddr()
	slog.Info("feluccad listening", "addr", addr, "ui_dir", cfg.UIDir, "db", cfg.DBPath,
		"auth", map[bool]string{true: "off (--insecure-no-auth)", false: "on"}[cfg.InsecureNoAuth])

	httpSrv := &http.Server{
		Addr:           addr,
		Handler:        srv.Handler(),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Optional Let's Encrypt TLS (v4 P2.3). Empty tls_domain means plain
	// HTTP only — exactly the pre-TLS behavior.
	if cfg.TLSDomain != "" {
		if err := os.MkdirAll(cfg.TLSCacheDir, 0o700); err != nil {
			slog.Error("tls: could not create autocert cache dir", "path", cfg.TLSCacheDir, "err", err)
			os.Exit(1)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),
			Cache:      autocert.DirCache(cfg.TLSCacheDir),
		}
		slog.Info("tls enabled", "domain", cfg.TLSDomain, "autocert_cache", cfg.TLSCacheDir)

		// :443 — public TLS endpoint, same handler/timeouts as the plain server.
		tlsSrv := &http.Server{
			Addr:           ":443",
			Handler:        srv.Handler(),
			ReadTimeout:    30 * time.Second,
			WriteTimeout:   60 * time.Second,
			MaxHeaderBytes: maxHeaderBytes,
			TLSConfig:      m.TLSConfig(),
		}
		servers = append(servers, tlsSrv)
		go func() {
			// Cert/key paths empty: certificates come from autocert.
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("tls listen :443", "err", err)
				os.Exit(1)
			}
		}()

		// :80 — ACME HTTP-01 challenges + redirect everything else to https.
		// Same bounds as the other two listeners: this one is public whenever
		// tls_domain is set, and it used to be a bare http.ListenAndServe with
		// Go's 1 MiB header default and no timeouts at all — the one listener
		// the maxHeaderBytes invariant did not actually cover.
		acmeSrv := &http.Server{
			Addr:           ":80",
			Handler:        m.HTTPHandler(nil),
			ReadTimeout:    30 * time.Second,
			WriteTimeout:   60 * time.Second,
			MaxHeaderBytes: maxHeaderBytes,
		}
		servers = append(servers, acmeSrv)
		go func() {
			if err := acmeSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("tls listen :80", "err", err)
				os.Exit(1)
			}
		}()
	}

	// The plain cfg.Port listener stays up UNCONDITIONALLY, even with TLS on:
	// overlay-joined agents speak plain HTTP to feluccad's overlay IP inside the
	// wg tunnel (the tunnel provides the encryption). Removing this listener
	// would cut every enrolled agent off from the control plane.
	servers = append(servers, httpSrv)
	stopped := make(chan struct{})
	go func() {
		stopOnSignal(sigs, servers, srv, stop, flushed, os.Exit)
		close(stopped)
	}()
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	<-stopped
	slog.Info("feluccad stopped")
	// Returning runs the deferred db.Close and exits with status 0. db.Close
	// does NOT wait for a transaction in flight; what makes the stop safe for
	// one is the drain in stopOnSignal, which lets the requests finish (their
	// saves included) before anything is closed.
}

// The stop's limits. The drain gives requests in flight stopDrain to finish;
// the whole stop, flush included, ends the process with status 1 at
// stopHardLimit after the first signal (systemd's TimeoutStopSec, 30 s in
// deploy/systemd/feluccad.service, then sends SIGKILL). Variables for tests.
var (
	stopDrain     = 10 * time.Second
	stopHardLimit = 20 * time.Second
)

// stopOnSignal waits for SIGTERM or SIGINT, then stops feluccad:
//
//  1. Shutdown of every listener, all at once, for at most stopDrain: no new request is
//     accepted, and the requests in flight finish, their saves and their exec
//     rows included. A create cut between the worker's answer and its last
//     save would leave a VM the database does not know.
//  2. Close of whatever is left after stopDrain: those connections are cut
//     under their requests, whose clients get no answer and must treat the
//     outcome as unknown.
//  3. The background loops end and the queued exec rows get their final flush
//     (flushed closes after it) — after the drain, so the rows of the
//     requests that finished in it are in it.
//
// A second signal, or stopHardLimit after the first, ends the process at once
// with status 1 through exit: a flush that hangs on a locked database must not
// keep the process until SIGKILL, and an operator's second Ctrl-C means now.
func stopOnSignal(sigs <-chan os.Signal, servers []*http.Server, srv *server.Server, stop chan<- struct{}, flushed <-chan struct{}, exit func(int)) {
	sig := <-sigs
	slog.Info("stopping: draining the requests in flight, then flushing the exec usage rows",
		"signal", sig.String(), "drain", stopDrain, "hard_limit", stopHardLimit)
	hard := time.AfterFunc(stopHardLimit, func() {
		slog.Error("stop did not finish in time: exiting now", "hard_limit", stopHardLimit)
		exit(1)
	})
	defer hard.Stop()
	go func() {
		if sig, ok := <-sigs; ok {
			slog.Error("second signal during the stop: exiting now", "signal", sig.String())
			exit(1)
		}
	}()

	// All listeners drain at once, on one deadline: one after the other, the
	// later ones kept accepting while the first drained and then got an
	// expired context, so their requests were cut with no drain at all.
	ctx, cancel := context.WithTimeout(context.Background(), stopDrain)
	defer cancel()
	var drains sync.WaitGroup
	for _, s := range servers {
		drains.Add(1)
		go func(s *http.Server) {
			defer drains.Done()
			if err := s.Shutdown(ctx); err != nil {
				slog.Warn("drain did not finish: closing the connections left", "addr", s.Addr, "err", err)
			}
		}(s)
	}
	drains.Wait()
	for _, s := range servers {
		if err := s.Close(); err != nil {
			slog.Warn("closing a listener", "addr", s.Addr, "err", err)
		}
	}
	close(stop)
	<-flushed
	srv.StopBackground()
}

// boot loads the working set from an opened store (or imports the legacy JSON
// state file into an empty one), builds the server and makes the start-up save.
//
// What ends the process, and what does not:
//   - a store that cannot be READ, or a load cut short: an error. LoadInto
//     appends as it scans and does not clear what it already appended, so a
//     failed load leaves a partial working set — and the first save deletes
//     every row before reinserting the ones in memory. Serving from a short
//     load destroys the rest of the database on the next write; refusing to
//     start is recoverable by an operator, a truncated sandboxes table is not.
//   - a legacy state file that is present but does not load: an error, for the
//     same reason. It used to be a warning, and the start-up save then marked
//     the empty database as initialized, so the next start never looked at the
//     file again: the empty state was permanent.
//   - a store that cannot be WRITTEN (the start-up save fails): NOT an error.
//     feluccad starts with the store failing — it serves reads and the route
//     table, refuses changes, and the server's recovery loop keeps retrying —
//     because every running sandbox's ingress depends on this process answering
//     the gateway, and a write outage is no reason to take that away.
//
// The start-up write is the whole snapshot only for an empty database or a
// legacy import (there is nothing else to write then). A database that loaded
// gets one small real write instead (store.MarkStarted): rewriting the
// snapshot on every start made whatever the start loaded — a lossy load after
// a downgrade, say — permanent before the first request.
//
// The legacy file is renamed only after its import was saved: if the save
// fails, the database is still empty, and the next start imports it again.
// When the recovery loop saves it later, the file is renamed then: left in
// place, it would be imported again if the database were ever recreated.
func boot(cfg *config.Config, db store.Store) (*server.Server, error) {
	st := state.New()
	empty, err := db.Empty()
	if err != nil {
		return nil, fmt.Errorf("store cannot be read: %w", err)
	}
	imported := false
	if empty {
		// One-time migration: import the legacy JSON state if present. Any
		// stat error other than "not there" counts as present: an unreadable
		// file is not an absent one.
		if _, statErr := os.Stat(cfg.StatePath); !os.IsNotExist(statErr) {
			if err := st.Load(cfg.StatePath); err != nil {
				return nil, fmt.Errorf("legacy state file %s is present but does not load — fix or move it; starting without it would make the empty state permanent at the first change: %w",
					cfg.StatePath, err)
			}
			imported = true
		}
	} else if err := db.LoadInto(st); err != nil {
		return nil, fmt.Errorf("could not load state, refusing to serve a partial working set the first write would make permanent: %w", err)
	}

	srv := server.New(cfg, st, db)
	// Records a previous process left "creating" (it stopped, or was killed,
	// during a create's worker call) become error before anything is served:
	// no create of that process runs in this one, and a creating record
	// refuses every lifecycle verb. Their change is part of the start-up
	// write, which is then the whole snapshot.
	settled := srv.SettleInterruptedCreates()
	renameLegacy := func() {
		if err := os.Rename(cfg.StatePath, cfg.StatePath+".imported"); err != nil {
			slog.Warn("could not rename legacy state file", "err", err)
		}
		slog.Info("migrated legacy state", "from", cfg.StatePath, "to", cfg.DBPath)
	}
	// Set before the write, so a recovery can never come before it.
	if imported {
		srv.OnStoreRecovered(renameLegacy)
	}
	if err := srv.StartupWrite(empty || settled > 0); err != nil {
		slog.Error("STORE NOT WRITABLE at start-up: serving reads and the route table, refusing changes, retrying the save until it succeeds",
			"db", cfg.DBPath, "err", err)
		return srv, nil
	}
	if imported {
		srv.OnStoreRecovered(nil)
		renameLegacy()
	}
	return srv, nil
}

// legacyDBPath returns where a pre-rename (Hearth) install would have kept the
// database that cfg now expects at db, if a file still exists there; "" when
// the path has no product name in it or nothing is there.
func legacyDBPath(db string) string {
	old := strings.ReplaceAll(db, "felucca", "hearth")
	if old == db {
		return ""
	}
	if _, err := os.Stat(old); err == nil {
		return old
	}
	return ""
}
