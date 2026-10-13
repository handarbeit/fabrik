//go:build windows

package sessionreap

import (
	"context"
	"syscall"
	"time"
)

// Windows has no POSIX sessions; every entry point is a no-op, matching the
// engine's existing procattr_windows.go behaviour.

// CheckSID always reports nothing to refuse: there is nothing to signal.
func CheckSID(int) error { return nil }

// Members returns no members.
func Members(int, Options) ([]int, error) { return nil, nil }

// Escalate is a no-op.
func Escalate(int, string, time.Duration, time.Duration, Options) {}

// Sweep is a no-op.
func Sweep(int, string, Options) int { return 0 }

// Discover finds nothing: Windows has no POSIX sessions.
func Discover(int, Options) ([]Session, error) { return nil, nil }

// ValidSession is always false: there is nothing to signal.
func ValidSession(Session, Options) bool { return false }

// Reap is a no-op.
func Reap(Session, Options) (int, int) { return 0, 0 }

// Empty is always true.
func Empty(Session, Options) bool { return true }

// Tracker is a no-op on Windows.
type Tracker struct{}

// Track returns a Tracker that observes nothing.
func Track(int, Options, func([]Session)) *Tracker { return &Tracker{} }

// Close is a no-op.
func (*Tracker) Close() {}

// Sessions returns nothing.
func (*Tracker) Sessions() []Session { return nil }

// Sample returns nothing.
func (*Tracker) Sample() []Session { return nil }

// Run blocks until ctx is done, sampling nothing.
func (*Tracker) Run(ctx context.Context, _ time.Duration) { <-ctx.Done() }

// CheckPGID always reports nothing to refuse: there is nothing to signal.
func CheckPGID(int) error { return nil }

// SignalGroup is a no-op on Windows.
func SignalGroup(Owner, syscall.Signal, Options) error { return nil }

// OwnsGroup reports nothing: there are no process groups to own.
func OwnsGroup(Owner, Options) (bool, bool, string) { return false, true, "" }

// RecordStart records nothing.
func RecordStart(int, Options) string { return "" }

// ForgetStart is a no-op.
func ForgetStart(int) {}

// OwnerOf returns an Owner with no token.
func OwnerOf(pid int) Owner { return Owner{PID: pid} }

// StartToken returns "" on Windows.
func StartToken(int) string { return "" }

// Environ has no reader on Windows.
func Environ(int) ([]string, error) { return nil, ErrEnvUnreadable }

// MarkedPIDs finds nothing on Windows.
func MarkedPIDs(string, map[string]struct{}, Options) (map[string][]int, error) { return nil, nil }

// ReapMarked is a no-op on Windows.
func ReapMarked(string, map[string]struct{}, string, time.Duration, Options) int { return 0 }
