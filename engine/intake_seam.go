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
		// The same method production uses; the Layer 1 round trip is stood in for
		// by SetIntakeLayer1DelayForTest (#2484).
		applyAt = func(eventType string, payload []byte, receivedAt time.Time) {
			e.applyDelivery(cache, eventType, payload, receivedAt, func() {
				if d := time.Duration(e.chanTest.layer1Delay.Load()); d > 0 {
					time.Sleep(d)
				}
			})
		}
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

// SetIntakeLayer1DelayForTest makes every intake apply spend d where production
// makes the Layer 1 status refresh's GitHub round trip (#2484), unless the
// intake is closing. Call it before submitting deliveries.
func (e *Engine) SetIntakeLayer1DelayForTest(d time.Duration) {
	e.chanTest.layer1Delay.Store(int64(d))
}

// SetIntakeCloseGraceForTest overrides the grace the SIGHUP restart tail gives
// the intake (intakeSighupGrace), so a test can shorten it to make a slow drain
// outlast it (#2484). Zero keeps the production value.
func (e *Engine) SetIntakeCloseGraceForTest(d time.Duration) {
	e.chanTest.sighupGrace.Store(int64(d))
}

// SetSighupDrainSlackDisabledForTest neutralises #2484's drain fix on this
// engine: a closing intake runs the Layer 1 refresh again. The SIGHUP tail's
// grace is whatever SetIntakeCloseGraceForTest set, so a test models the old
// 5 s grace with a value of its own. It is how the twin test shows it is not
// vacuous.
func (e *Engine) SetSighupDrainSlackDisabledForTest() (restore func()) {
	e.chanTest.drainSlackOff.Store(true)
	return func() { e.chanTest.drainSlackOff.Store(false) }
}

// IntakeCountsForTest returns the intake's accepted, applied and dropped totals.
func (e *Engine) IntakeCountsForTest() (accepted, applied, dropped int64) {
	return e.intake.Counts()
}
