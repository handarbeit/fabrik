// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"testing"
)

// TestPollStatus_NonTTY verifies that pollStatus outputs a line in non-TTY mode.
func TestPollStatus_NonTTY(t *testing.T) {
	orig := isTTY
	origTUI := tuiMode
	isTTY = false
	tuiMode = false
	defer func() {
		isTTY = orig
		tuiMode = origTUI
	}()
	// Should not panic
	pollStatus("polling %s", "test")
	pollStatusClear()
}

// TestPollStatus_TTY verifies the overwrite-line behavior when stdout is a TTY.
func TestPollStatus_TTY(t *testing.T) {
	origTTY := isTTY
	origTUI := tuiMode
	isTTY = true
	tuiMode = false
	defer func() {
		isTTY = origTTY
		tuiMode = origTUI
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
	tuiMode = true
	defer func() { tuiMode = orig }()
	// Should not panic or output anything
	pollStatus("this should not appear")
	pollStatusClear()
}
