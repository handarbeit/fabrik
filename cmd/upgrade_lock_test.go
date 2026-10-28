//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/handarbeit/fabrik/internal/lockprobe"
)

// holdDirLock takes an exclusive flock on dir's instance lock from a separate
// descriptor, as a live daemon does, and records pid in the file.
func holdDirLock(t *testing.T, dir, pid string) string {
	t.Helper()
	p := lockprobe.Path(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(pid + "\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return p
}

func lockHeldStill(t *testing.T, p string) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		t.Fatalf("holder lost its lock: %v", err)
	}
}

// snapshotExceptLock is the .fabrik tree minus the lock file the test itself holds.
func snapshotExceptLock(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := snapshot(t, filepath.Join(dir, ".fabrik"))
	delete(m, "fabrik.lock")
	return m
}

func requireRefusal(t *testing.T, err error, pid string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	for _, want := range []string{"pid " + pid, "Stop the daemon", "run 'fabrik upgrade' again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}
}

func TestRunUpgrade_HeldLockRefusesMigration(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	lock := holdDirLock(t, dir, "4242")
	before := snapshotExceptLock(t, dir)

	requireRefusal(t, runUpgrade(nil), "4242")

	if !reflect.DeepEqual(before, snapshotExceptLock(t, dir)) {
		t.Error(".fabrik changed despite the refusal")
	}
	lockHeldStill(t, lock)
	if b, _ := os.ReadFile(lock); string(b) != "4242\n" {
		t.Errorf("lock file changed: %q", b)
	}
}

func TestRunUpgrade_HeldLockRefusesForce(t *testing.T) {
	for _, extra := range [][]string{{"--force"}, {"--no-migrate", "--force"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			dir := deployFixture(t, migrationTags[0])
			holdDirLock(t, dir, "99")
			before := snapshotExceptLock(t, dir)
			requireRefusal(t, runUpgrade(extra), "99")
			if !reflect.DeepEqual(before, snapshotExceptLock(t, dir)) {
				t.Error(".fabrik changed despite the refusal")
			}
		})
	}
}

func TestRunUpgrade_HeldLockUnknownPID(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	p := holdDirLock(t, dir, "")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := runUpgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "pid unknown") {
		t.Fatalf("want a refusal naming an unknown pid, got %v", err)
	}
}

func TestRunUpgrade_HeldLockExemptsNoMigrateAndReconcile(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	holdDirLock(t, dir, "4242")
	if err := runUpgrade([]string{"--no-migrate"}); err != nil {
		t.Fatalf("--no-migrate under a held lock: %v", err)
	}
	if err := runUpgrade([]string{"--reconcile"}); err != nil {
		t.Fatalf("--reconcile under a held lock: %v", err)
	}
}

func TestRunUpgrade_StaleLockMigrates(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	p := lockprobe.Path(dir)
	if err := os.WriteFile(p, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("stale lock must not block: %v", err)
	}
	for k, v := range stageRefs(t, dir) {
		if !strings.Contains(v, ":") {
			t.Errorf("%s = %q was not migrated", k, v)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "999999\n" {
		t.Errorf("lock file changed: %q", b)
	}
}

func TestRunUpgrade_NoLockFileCreated(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockprobe.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("upgrade created a lock file: %v", err)
	}
}

// Non-vacuity (SC-005): with the probe neutralised the held-lock scenario
// migrates, so the refusal tests above exercise the check.
func TestRunUpgrade_HeldLockNonVacuity(t *testing.T) {
	dir := deployFixture(t, migrationTags[0])
	holdDirLock(t, dir, "4242")
	orig := upgradeLockProbe
	upgradeLockProbe = func(string) lockprobe.Result { return lockprobe.Result{} }
	t.Cleanup(func() { upgradeLockProbe = orig })
	before := snapshotExceptLock(t, dir)
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("neutralised check: %v", err)
	}
	if reflect.DeepEqual(before, snapshotExceptLock(t, dir)) {
		t.Fatal("neutralised check changed nothing; the refusal tests would be vacuous")
	}
}
