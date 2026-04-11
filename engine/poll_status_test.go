package engine

import (
	"testing"
)

// TestPollStatus_NonTTY verifies that pollStatus outputs a line in non-TTY mode.
func TestPollStatus_NonTTY(t *testing.T) {
	orig := isTTY
	origTUI := tuiMode
	origLog := pollLogFile
	isTTY = false
	tuiMode = false
	pollLogFile = nil
	defer func() {
		isTTY = orig
		tuiMode = origTUI
		pollLogFile = origLog
	}()
	// Should not panic
	pollStatus("polling %s", "test")
	pollStatusClear()
}

// TestPollStatus_TTY verifies the overwrite-line behavior when stdout is a TTY.
func TestPollStatus_TTY(t *testing.T) {
	origTTY := isTTY
	origTUI := tuiMode
	origLog := pollLogFile
	isTTY = true
	tuiMode = false
	pollLogFile = nil
	defer func() {
		isTTY = origTTY
		tuiMode = origTUI
		pollLogFile = origLog
		lastStatusLen = 0
	}()
	// Should not panic; output goes to stdout with \r prefix
	pollStatus("status line")
	// Second call with shorter message covers the padding branch
	pollStatus("hi")
	pollStatusClear()
}

// TestPollStatus_TuiMode_IsNoop verifies that pollStatus is a no-op in TUI mode.
func TestPollStatus_TuiMode_IsNoop(t *testing.T) {
	orig := tuiMode
	origLog := pollLogFile
	tuiMode = true
	pollLogFile = nil
	defer func() {
		tuiMode = orig
		pollLogFile = origLog
	}()
	// Should not panic or output anything
	pollStatus("this should not appear")
	pollStatusClear()
}
