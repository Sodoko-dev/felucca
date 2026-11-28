package ingress

import "testing"

// TestQuarantineOutlastsEveryPathToAReleasedPort fails when the quarantine no
// longer covers the oldest table the gateway serves, its refresh, a worker call
// in flight and an idle upstream connection, plus the margin.
func TestQuarantineOutlastsEveryPathToAReleasedPort(t *testing.T) {
	need := GatewayMaxStaleCeiling + GatewayRefreshCeiling + WorkerCallTimeout + UpstreamIdleTimeout + Margin
	if PortQuarantine < need {
		t.Fatalf("PortQuarantine %v < %v (max-stale ceiling %v + refresh ceiling %v + worker call %v + upstream idle %v + margin %v)",
			PortQuarantine, need, GatewayMaxStaleCeiling, GatewayRefreshCeiling, WorkerCallTimeout, UpstreamIdleTimeout, Margin)
	}
}
