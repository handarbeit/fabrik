//go:build windows

package sessionreap

import "time"

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
