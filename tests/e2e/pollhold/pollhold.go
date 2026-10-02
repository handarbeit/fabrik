// Package pollhold is the controller behind the live harness's HoldPolls /
// TriggerPoll / ReleasePolls helpers (#1978, ADR-1978): hold the bed engine's
// automatic polls, make it run exactly one complete poll and wait for it to say
// so, then release.
//
// Some live scenarios need the engine to take one specific decision in one
// specific poll — their subject is a state window ("the engine sees exactly 7
// Queued members at once"). Waiting for GitHub to catch up (#1974's awaitVisible)
// cannot close a race where the harness's own multi-step write straddles a
// free-running poll. Holding polls closes it: build the window, wait until it is
// visible, trigger one poll, assert on that poll, release.
//
// The package is deliberately untagged, like tests/e2e/awaitvisible and
// tests/e2e/inconclusive: the harness (tests/e2e, build tag e2e) is only COMPILED
// by CI, so the protocol and the exclusivity rule live here and are covered by
// plain `go test ./...`. The wire format is internal/pollctl, shared with the
// engine; the thin tagged wrappers are tests/e2e/poll_control.go.
//
// Holding polls stops dispatch, catch-up, settle scans and reconcile for the
// WHOLE bed, so only a test the registry marks exclusive (#1977) may do it.
package pollhold

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/pollctl"
	"github.com/handarbeit/fabrik/tests/e2e/inconclusive"
	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// EnvDisable, when set to "off", turns the three helpers into logged no-ops so a
// run can reproduce the pre-seam, free-running behaviour (the straddle then shows
// up as Inconclusive). The gate never sets it.
const EnvDisable = "E2E_POLL_SEAM"

// TB is the subset of *testing.T the controller uses, so its unit tests can drive
// it with a fake.
type TB interface {
	Helper()
	Name() string
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skip(args ...any)
	Cleanup(func())
}

// Controller drives one bed's control file.
type Controller struct {
	// Path is the request file (pollctl.ControlPath of the bed).
	Path string
	// IsExclusive reports whether the named top-level test is exclusive in the
	// registry.
	IsExclusive func(name string) (exclusive bool, err error)
	// Disabled makes Hold/Trigger/Release log and return (EnvDisable=off).
	Disabled bool

	// AckTimeout bounds waiting for a hold or release to be acknowledged — which
	// the engine only does once any in-flight poll has finished, so it must be
	// longer than a poll.
	AckTimeout time.Duration
	// TriggerTimeout bounds waiting for a triggered poll to report completion; a
	// poll that never completes is a test failure, never a hang.
	TriggerTimeout time.Duration
	// Interval is how often the ack is re-read.
	Interval time.Duration
	// MaxHold is stamped into the request as hold_until, after which the engine
	// releases on its own.
	MaxHold time.Duration

	// Now and Sleep are test seams; nil means time.Now and time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// New returns a Controller for the bed in bedDir, enforcing exclusivity against
// the embedded registry.json.
func New(bedDir string) *Controller {
	return &Controller{
		Path:           pollctl.ControlPath(bedDir),
		IsExclusive:    registryExclusive,
		Disabled:       os.Getenv(EnvDisable) == "off",
		AckTimeout:     5 * time.Minute,
		TriggerTimeout: 10 * time.Minute,
		Interval:       200 * time.Millisecond,
		MaxHold:        pollctl.MaxHold,
	}
}

func registryExclusive(name string) (bool, error) {
	reg, err := registry.Load()
	if err != nil {
		return false, err
	}
	for _, e := range reg.Tests {
		if e.Name == name {
			return e.Exclusive, nil
		}
	}
	return false, fmt.Errorf("%s is not in tests/e2e/registry/registry.json", name)
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// topLevel is the registry key of a (sub)test: the part before the first "/".
func topLevel(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Hold holds the bed's automatic polls and returns once the engine has
// acknowledged that no poll is running and none will start. It fails the test
// unless the test is exclusive in the registry, and registers Release with
// t.Cleanup so a failing or panicking test never leaves the bed held.
func (c *Controller) Hold(t TB) {
	t.Helper()
	if c.Disabled {
		t.Logf("HoldPolls: %s=off — polls stay free-running", EnvDisable)
		return
	}
	top := topLevel(t.Name())
	ex, err := c.IsExclusive(top)
	if err != nil {
		t.Fatalf("HoldPolls: cannot establish that %s is exclusive: %v", top, err)
		return
	}
	if !ex {
		t.Fatalf("HoldPolls: %s is not exclusive in tests/e2e/registry/registry.json — holding polls stops the whole bed, so only exclusive tests may do it (#1978)", top)
		return
	}
	if _, err := os.Stat(pollctl.AckPath(c.Path)); err != nil {
		t.Fatalf("HoldPolls: the bed has no poll-control seam (no %s): it was started without %s",
			pollctl.AckPath(c.Path), pollctl.EnvVar)
		return
	}

	req, ack := pollctl.ReadRequest(c.Path), pollctl.ReadAck(c.Path)
	gen := maxInt64(req.HoldGen, ack.HoldGen) + 1
	next := pollctl.Request{
		Hold:       true,
		HoldUntil:  c.now().Add(c.MaxHold).Unix(),
		HoldGen:    gen,
		TriggerSeq: maxInt64(req.TriggerSeq, ack.DoneSeq),
	}
	if err := pollctl.WriteRequest(c.Path, next); err != nil {
		t.Fatalf("HoldPolls: writing the hold request: %v", err)
		return
	}
	// Registered before waiting, so a hold the engine never acknowledges is still
	// released.
	t.Cleanup(func() { c.release(t) })

	deadline := c.now().Add(c.AckTimeout)
	for {
		a := pollctl.ReadAck(c.Path)
		if a.HoldGen >= gen && a.Held {
			t.Logf("HoldPolls: bed polls held (generation %d)", gen)
			return
		}
		if c.now().After(deadline) {
			t.Fatalf("HoldPolls: the engine did not acknowledge the hold within %s (last ack %+v) — a poll may be stuck", c.AckTimeout, a)
			return
		}
		c.sleep(c.Interval)
	}
}

// Trigger makes the held engine run exactly one complete poll and returns once it
// has finished. A poll that errored fails the test; one that ran nothing (the
// engine's rate-limit gate or minimum-poll-interval floor refused it) is
// Inconclusive — the precondition never arose — never a pass; one that never
// reports completion fails the test after TriggerTimeout.
func (c *Controller) Trigger(t TB) {
	t.Helper()
	if c.Disabled {
		t.Logf("TriggerPoll: %s=off — nothing to trigger", EnvDisable)
		return
	}
	req, ack := pollctl.ReadRequest(c.Path), pollctl.ReadAck(c.Path)
	if !req.Hold {
		t.Fatalf("TriggerPoll: polls are not held — call HoldPolls first")
		return
	}
	seq := maxInt64(req.TriggerSeq, ack.DoneSeq) + 1
	req.TriggerSeq = seq
	if err := pollctl.WriteRequest(c.Path, req); err != nil {
		t.Fatalf("TriggerPoll: writing the trigger request: %v", err)
		return
	}

	deadline := c.now().Add(c.TriggerTimeout)
	for {
		a := pollctl.ReadAck(c.Path)
		if a.DoneSeq >= seq {
			switch a.Outcome {
			case pollctl.OutcomeRan:
				t.Logf("TriggerPoll: triggered poll %d completed", seq)
				return
			case pollctl.OutcomeBlocked:
				t.Skip(inconclusive.Message("TriggerPoll: the triggered poll ran nothing (%s) — the state window was never observed by a poll", a.Detail))
				return
			default:
				t.Fatalf("TriggerPoll: the triggered poll failed (%s): %s", a.Outcome, a.Detail)
				return
			}
		}
		if c.now().After(deadline) {
			t.Fatalf("TriggerPoll: the triggered poll did not complete within %s (last ack %+v)", c.TriggerTimeout, a)
			return
		}
		c.sleep(c.Interval)
	}
}

// Release lets the bed's polls run freely again and returns once the engine has
// acknowledged. Idempotent, and a no-op when polls are not held. Problems are
// reported with Errorf, never Fatalf, so it is safe from t.Cleanup.
func (c *Controller) Release(t TB) {
	t.Helper()
	c.release(t)
}

func (c *Controller) release(t TB) {
	t.Helper()
	if c.Disabled {
		return
	}
	req, ack := pollctl.ReadRequest(c.Path), pollctl.ReadAck(c.Path)
	if !req.Hold {
		return
	}
	gen := maxInt64(req.HoldGen, ack.HoldGen) + 1
	next := pollctl.Request{Hold: false, HoldGen: gen, TriggerSeq: maxInt64(req.TriggerSeq, ack.DoneSeq)}
	if err := pollctl.WriteRequest(c.Path, next); err != nil {
		t.Errorf("ReleasePolls: writing the release request: %v — the engine will self-release after %s", err, c.MaxHold)
		return
	}
	deadline := c.now().Add(c.AckTimeout)
	for {
		a := pollctl.ReadAck(c.Path)
		if a.HoldGen >= gen && !a.Held {
			t.Logf("ReleasePolls: bed polls released (generation %d)", gen)
			return
		}
		if c.now().After(deadline) {
			t.Errorf("ReleasePolls: the engine did not acknowledge the release within %s (last ack %+v) — it will self-release at the hold deadline", c.AckTimeout, a)
			return
		}
		c.sleep(c.Interval)
	}
}
