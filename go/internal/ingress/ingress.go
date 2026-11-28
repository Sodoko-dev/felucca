// Package ingress holds the numbers that keep a hostname from reaching the
// wrong sandbox, in one place, because they only work together.
//
// When a sandbox gives up a node port (unexpose, delete, the sweep of dynamic
// exposes), something else on the same worker may be handed that port later.
// Until then, three things can still send traffic for the OLD sandbox to it:
//
//   - the gateway's route table, which may be old: the gateway serves from a
//     table at most GatewayMaxStaleCeiling old, and the table it holds may be one
//     refresh interval (at most GatewayRefreshCeiling) older than the newest
//     feluccad could give it;
//   - a feluccad call to a worker that was already under way when the port was
//     released, and that may still finish and act on it (WorkerCallTimeout);
//   - an idle upstream connection in the gateway's pool, which a request can
//     pick up until UpstreamIdleTimeout after its last response.
//
// PortQuarantine is how long feluccad keeps a released node port away from
// every other sandbox. It must outlast all three stacked end to end, plus
// Margin for clocks and scheduling; the test in this package fails when the
// numbers stop adding up. Change one and the test tells you which other must
// move.
//
// This package must not import server, store, state or gateway: both feluccad
// and the gateway import it.
package ingress

import "time"

const (
	// PortQuarantine is how long feluccad keeps a released node port away
	// from other sandboxes.
	PortQuarantine = 900 * time.Second

	// GatewayMaxStaleCeiling is the largest --max-stale the gateway accepts:
	// the oldest route table it will serve from.
	GatewayMaxStaleCeiling = 600 * time.Second

	// GatewayRefreshCeiling is the largest --refresh the gateway accepts.
	GatewayRefreshCeiling = 60 * time.Second

	// WorkerCallTimeout bounds one feluccad call to a worker's agent.
	WorkerCallTimeout = 30 * time.Second

	// UpstreamIdleTimeout is how long the gateway keeps an idle upstream
	// connection for reuse (its transport's IdleConnTimeout).
	UpstreamIdleTimeout = 90 * time.Second

	// Margin covers clock steps, scheduling and the time between a check and
	// the act it guards.
	Margin = 60 * time.Second
)
