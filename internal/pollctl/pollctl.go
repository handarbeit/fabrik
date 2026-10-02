// Package pollctl is the wire protocol of the bed-only poll hold/trigger seam
// (#1978, ADR-1978), shared by the engine (which obeys it) and the live e2e
// harness (which drives it). It is TEST-ONLY: nothing here is reachable from a
// production configuration — the engine enables the seam only when
// FABRIK_TEST_POLL_CONTROL names a control file, and the variable is
// deliberately absent from the CLI help and docs/USER_GUIDE.md.
//
// The protocol is two small JSON files, each with exactly one writer, both
// replaced by atomic rename so a reader never sees a torn file:
//
//   - the request file (ControlPath), written by the harness: whether to hold
//     polls, until when, and a monotonically increasing trigger sequence;
//   - the ack file (AckPath), written by the engine: whether it is held, which
//     hold generation and trigger sequence it has applied, and how the last
//     triggered poll ended.
//
// A missing or unparseable file reads as the zero value, so a fresh bed (or one
// mid-write) is simply "not held, nothing requested".
package pollctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvVar names the control file. Set on the bed's environment by both launch
// sites (tests/gate/bed.go, tests/e2e/lifecycle.go); unset in production.
const EnvVar = "FABRIK_TEST_POLL_CONTROL"

// MaxHold bounds how long the engine honours a hold. The harness stamps
// HoldUntil = now + MaxHold, so a harness that was hard-killed (and so never ran
// its t.Cleanup release) cannot leave the bed held: the engine self-releases.
const MaxHold = 10 * time.Minute

// Outcome values written to Ack.Outcome for a triggered poll.
const (
	// OutcomeRan: poll() ran to completion.
	OutcomeRan = "ran"
	// OutcomeError: poll() returned an error.
	OutcomeError = "error"
	// OutcomeBlocked: the poll was refused — the REST hard gate, or the
	// minimum-poll-interval floor after the bounded retries — so no poll ran
	// and the caller must not assert on one.
	OutcomeBlocked = "blocked"
)

// Request is the harness-written half of the protocol.
type Request struct {
	// Hold is whether automatic polls (and reconcile ticks) are held.
	Hold bool `json:"hold"`
	// HoldUntil is an absolute unix-seconds deadline after which the engine
	// releases the hold on its own. Zero means no deadline.
	HoldUntil int64 `json:"hold_until"`
	// HoldGen increases every time the harness changes Hold; the engine echoes
	// the last one it applied in Ack.HoldGen so a waiter can tell its own
	// request was applied rather than a stale ack.
	HoldGen int64 `json:"hold_gen"`
	// TriggerSeq increases for every requested triggered poll.
	TriggerSeq int64 `json:"trigger_seq"`
}

// Ack is the engine-written half of the protocol.
type Ack struct {
	// Held is whether the engine is currently holding polls.
	Held bool `json:"held"`
	// HoldGen is the last Request.HoldGen the engine applied. A hold is only
	// acknowledged once no poll is running and none will start.
	HoldGen int64 `json:"hold_gen"`
	// DoneSeq is the last Request.TriggerSeq whose poll has finished.
	DoneSeq int64 `json:"done_seq"`
	// Outcome is how poll DoneSeq ended: one of the Outcome* constants.
	Outcome string `json:"outcome,omitempty"`
	// Detail is a short human-readable explanation of Outcome.
	Detail string `json:"detail,omitempty"`
}

// ControlPath returns the request-file path for a bed directory.
func ControlPath(bedDir string) string {
	return filepath.Join(bedDir, ".fabrik", "poll-control.json")
}

// AckPath returns the ack-file path that pairs with a request-file path.
func AckPath(controlPath string) string {
	return strings.TrimSuffix(controlPath, ".json") + ".ack.json"
}

// Env returns the environment entry that enables the seam for bedDir's bed.
// Both bed launch sites append it, so a restarted bed cannot silently lose the
// seam.
func Env(bedDir string) string {
	return EnvVar + "=" + ControlPath(bedDir)
}

// ReadRequest reads the request file; missing or unparseable → zero value.
func ReadRequest(path string) Request {
	var r Request
	readJSON(path, &r)
	return r
}

// ReadAck reads the ack file; missing or unparseable → zero value.
func ReadAck(controlPath string) Ack {
	var a Ack
	readJSON(AckPath(controlPath), &a)
	return a
}

// WriteRequest atomically replaces the request file.
func WriteRequest(path string, r Request) error { return writeJSON(path, r) }

// WriteAck atomically replaces the ack file that pairs with controlPath.
func WriteAck(controlPath string, a Ack) error { return writeJSON(AckPath(controlPath), a) }

// Clear removes both files. Absent files are not an error.
func Clear(controlPath string) error {
	var errs []error
	for _, p := range []string{controlPath, AckPath(controlPath)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func readJSON(path string, v any) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, v); err != nil {
		// A torn or foreign file reads as "nothing requested".
		return
	}
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("closing temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
