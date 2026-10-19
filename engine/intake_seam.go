package engine

import (
	"path/filepath"
	"time"
)

// Test seam for the transport-neutral delivery intake (#2351). The sim bed has no
// transport layer (it applies deltas to the board cache directly), so without
// this the dedupe, ordering and echo behaviour would be reachable only from the
// engine package's own unit tests. Tests only.

// EnableDeliveryIntakeForTest builds the engine's intake and echo registry the way
// setUpIngestion does for an attached transport: the echo registry is wired into
// the board cache, and the intake applies each delivery through the cache. The
// delivery ledger is persisted under dir (empty dir: memory only), so a
// RestartEnv that hands the same dir to the next engine reloads it. Requires the
// board cache (Engine.UseBoardCacheForTest).
func (e *Engine) EnableDeliveryIntakeForTest(dir string) {
	cache := e.cache()
	e.echo = newEchoRegistry()
	var applyAt func(string, []byte, time.Time)
	if cache != nil {
		cache.SetMatchEchoFn(e.echo.MatchEcho)
		cache.SetMatchEchoReportedFn(e.echo.MatchEchoReported)
		cache.SetMatchEchoViaFn(e.echo.MatchEchoVia)
		applyAt = cache.ApplyDeltaAt
	}
	path := ""
	if dir != "" {
		path = filepath.Join(dir, deliveryLedgerFileName)
	}
	e.intake = newIntake(newDeliveryLedger(path, e.now(), nil), nil, nil, e.now)
	e.intake.applyAt = applyAt
}

// SubmitDeliveryForTest hands a delivery to the intake as a transport would and
// reports whether it was queued (false = duplicate or dropped).
func (e *Engine) SubmitDeliveryForTest(transport, id, eventType string, payload []byte) bool {
	return e.intake.Submit(delivery{ID: id, EventType: eventType, Payload: payload, Transport: transport})
}

// DrainDeliveriesForTest blocks until every queued delivery has been applied.
func (e *Engine) DrainDeliveriesForTest() { e.intake.Drain() }

// CloseDeliveryIntakeForTest stops the intake and flushes the ledger, as a
// shutdown or the pre-exec flush does.
func (e *Engine) CloseDeliveryIntakeForTest() { e.intake.Close(5 * time.Second) }
