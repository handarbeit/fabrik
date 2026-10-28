//go:build !windows

package lockprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func shortRetry(t *testing.T) {
	t.Helper()
	oa, od := pidReadAttempts, pidReadDelay
	pidReadAttempts, pidReadDelay = 2, time.Millisecond
	t.Cleanup(func() { pidReadAttempts, pidReadDelay = oa, od })
}

func writeLock(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Path(dir)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func holdExclusive(t *testing.T, p string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

func TestProbe_HeldWithPID(t *testing.T) {
	shortRetry(t)
	dir := t.TempDir()
	p := writeLock(t, dir, fmt.Sprintf("%d\n", 4242))
	holdExclusive(t, p)
	got := Probe(dir)
	if !got.Held || got.PID != 4242 {
		t.Fatalf("Probe = %+v, want held pid 4242", got)
	}
	// The original holder still holds it, and the file is unchanged.
	if b, _ := os.ReadFile(p); string(b) != "4242\n" {
		t.Fatalf("lock file changed: %q", b)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		t.Fatalf("holder lost its lock: %v", err)
	}
}

func TestProbe_HeldEmptyFileIsHeldWithUnknownPID(t *testing.T) {
	shortRetry(t)
	dir := t.TempDir()
	p := writeLock(t, dir, "")
	holdExclusive(t, p)
	got := Probe(dir)
	if !got.Held || got.PID != 0 {
		t.Fatalf("Probe = %+v, want held pid 0", got)
	}
}

func TestProbe_StaleFileNotHeld(t *testing.T) {
	dir := t.TempDir()
	p := writeLock(t, dir, "999999\n")
	if got := Probe(dir); got.Held {
		t.Fatalf("stale lock reported held: %+v", got)
	}
	if b, _ := os.ReadFile(p); string(b) != "999999\n" {
		t.Fatalf("lock file changed: %q", b)
	}
}

func TestProbe_AbsentNotHeldAndNotCreated(t *testing.T) {
	dir := t.TempDir()
	if got := Probe(dir); got.Held {
		t.Fatalf("absent lock reported held: %+v", got)
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("probe created the lock file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".fabrik")); !os.IsNotExist(err) {
		t.Fatalf("probe created .fabrik: %v", err)
	}
}

func TestProbe_UnreadableNotHeld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	dir := t.TempDir()
	p := writeLock(t, dir, "1\n")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o644) })
	if got := Probe(dir); got.Held {
		t.Fatalf("unreadable lock reported held: %+v", got)
	}
}

func TestProbe_PIDAppearsAfterRetry(t *testing.T) {
	oa, od := pidReadAttempts, pidReadDelay
	pidReadAttempts, pidReadDelay = 20, 10*time.Millisecond
	t.Cleanup(func() { pidReadAttempts, pidReadDelay = oa, od })
	dir := t.TempDir()
	p := writeLock(t, dir, "")
	holdExclusive(t, p)
	go func() {
		time.Sleep(30 * time.Millisecond)
		os.WriteFile(p, []byte("777\n"), 0o644)
	}()
	if got := Probe(dir); !got.Held || got.PID != 777 {
		t.Fatalf("Probe = %+v, want held pid 777", got)
	}
}
