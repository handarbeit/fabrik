//go:build e2e

package e2e

import (
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/pollhold"
)

// HoldPolls, TriggerPoll and ReleasePolls drive the bed engine's test-only poll
// hold/trigger seam (#1978, ADR-1978); the protocol, the exclusivity rule and the
// bounded waits live in the untagged tests/e2e/pollhold so plain `go test ./...`
// covers them.
//
// Use them ONLY for a test whose subject is a state window — "the engine sees
// exactly 7 Queued members at once" — where a free-running poll can land between
// two of the harness's writes and see only part of the state. Live e2e is the one
// layer that runs with real timing, and free-running polls are part of what it
// tests: pipeline, convergence and gate tests stay free-running. Waiting for
// GitHub to catch up (the awaitVisible family, #1974) is the tool for lag; this is
// the tool for a write window.
//
// The flow: seed free-running (paused/inert state needs polling and hydrating
// anyway), HoldPolls, make the window, wait until every write is visible, then
// TriggerPoll, assert on that poll, and release (ReleasePolls, or the t.Cleanup
// HoldPolls registered) so the rest of the scenario runs free-running.
//
// Holding polls stops dispatch, catch-up, settle scans and reconcile for the WHOLE
// bed, so HoldPolls fails any test that is not exclusive in
// tests/e2e/registry/registry.json (and the registry test fails the build before a
// live run for any test that reaches these helpers without being exclusive).
// Workers an earlier poll already dispatched keep running. E2E_POLL_SEAM=off turns
// all three into logged no-ops, so the pre-seam free-running behaviour — the
// straddle, as Inconclusive — can be reproduced.

// HoldPolls holds the bed's automatic polls and returns once the engine has
// acknowledged that no poll is running and none will start.
func HoldPolls(t *testing.T) {
	t.Helper()
	pollController().Hold(t)
}

// TriggerPoll makes the held engine run exactly one complete poll and returns
// once it has finished.
func TriggerPoll(t *testing.T) {
	t.Helper()
	pollController().Trigger(t)
}

// ReleasePolls lets the bed's polls run freely again. HoldPolls registers it with
// t.Cleanup, so calling it explicitly is only needed to resume mid-test.
func ReleasePolls(t *testing.T) {
	t.Helper()
	pollController().Release(t)
}

// pollController is the controller for the bed LoadEnv resolves: the same
// FABRIK_TEST_DIR the test's own env names. (LoadEnv itself is reserved for
// top-level test bodies — the registry scan keys live tests on it.)
func pollController() *pollhold.Controller {
	return pollhold.New(getenvOr("FABRIK_TEST_DIR", expandHome(defaultFabrikTestDir)))
}
