package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// timestampAndScopeRe matches an RFC3339 UTC timestamp followed by pruefer's
// existing "[pr#<N> <tag>] " scoping (AC3).
var timestampAndScopeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z \[pr#\d+ \w+\] `)

func TestFileLogger_LineHasTimestampAndScoping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pruefer.log")

	fl, err := newFileLogger(path, logRotateMaxBytes, logRotateBackups, false)
	if err != nil {
		t.Fatalf("newFileLogger: %v", err)
	}
	defer fl.Close()

	fl.Logf(103, "warn", "diff fetch failed: %v\n", "boom")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	line := strings.TrimRight(string(data), "\n")
	if !timestampAndScopeRe.MatchString(line) {
		t.Errorf("line %q does not match timestamp+scoping pattern %s", line, timestampAndScopeRe.String())
	}
	if !strings.Contains(line, "[pr#103 warn]") {
		t.Errorf("line %q missing exact [pr#103 warn] scoping", line)
	}
	if !strings.HasSuffix(line, "diff fetch failed: boom") {
		t.Errorf("line %q missing expected message body", line)
	}
}

func TestFileLogger_ConcurrentWritesProduceUninterleavedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pruefer.log")

	fl, err := newFileLogger(path, logRotateMaxBytes, logRotateBackups, false)
	if err != nil {
		t.Fatalf("newFileLogger: %v", err)
	}
	defer fl.Close()

	const goroutines = 20
	const perGoroutine = 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				fl.Logf(g, "select", "worker %d iteration %d\n", g, i)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d (a mismatch indicates interleaved/corrupted writes)", len(lines), goroutines*perGoroutine)
	}
	lineRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z \[pr#\d+ select\] worker \d+ iteration \d+$`)
	for _, line := range lines {
		if !lineRe.MatchString(line) {
			t.Errorf("corrupted or interleaved line: %q", line)
		}
	}
}

func TestFileLogger_RotatesAtBoundAndRetainsBoundedCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pruefer.log")

	const maxBytes = 200
	const backups = 3
	fl, err := newFileLogger(path, maxBytes, backups, false)
	if err != nil {
		t.Fatalf("newFileLogger: %v", err)
	}
	defer fl.Close()

	// Each line is comfortably over 40 bytes; writing 60 lines guarantees
	// several rotations past the configured backup count.
	for i := 0; i < 60; i++ {
		fl.Logf(1, "select", "synthetic oversized line to force rotation number %d\n", i)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("main log file %s missing: %v", path, err)
	}
	for i := 1; i <= backups; i++ {
		backupPath := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(backupPath); err != nil {
			t.Errorf("expected backup %s to exist: %v", backupPath, err)
		}
	}
	beyond := fmt.Sprintf("%s.%d", path, backups+1)
	if _, err := os.Stat(beyond); !os.IsNotExist(err) {
		t.Errorf("expected no backup beyond retained count at %s, stat err = %v", beyond, err)
	}

	// Sanity: the main file itself never exceeds maxBytes by more than one
	// line's worth (rotation is checked before each write, not after).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() > maxBytes*2 {
		t.Errorf("main log file size %d unexpectedly large for maxBytes %d", info.Size(), maxBytes)
	}
}
