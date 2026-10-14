//go:build !windows

package gate

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// LockHeld reads the engine's real flock: true only while another open file
// description holds it exclusively, false for a stale record or no file.
func TestOSProcsLockHeld(t *testing.T) {
	bed := t.TempDir()
	if (osProcs{}).LockHeld(bed) {
		t.Error("no lock file: not held")
	}
	path := filepath.Join(bed, ".fabrik", "fabrik.lock")
	mustWrite(t, path, "1234\n")
	if (osProcs{}).LockHeld(bed) {
		t.Error("a stale record nobody holds: not held")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if !(osProcs{}).LockHeld(bed) {
		t.Error("an exclusive holder: held")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if (osProcs{}).LockHeld(bed) {
		t.Error("released: not held")
	}
}
