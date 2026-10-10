package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// lockTestDirs gives a test its own board-lock directory and two fabrik dirs.
func lockTestDirs(t *testing.T) (dirA, dirB string) {
	t.Helper()
	t.Setenv("FABRIK_LOCK_DIR", t.TempDir())
	mk := func() string {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, ".fabrik"), 0755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	return mk(), mk()
}

func boardCfg(owner string, num int) Config {
	return Config{Owner: owner, ProjectNum: num}
}

// FR-016 (1): same board, different directories — the second refuses and names
// the first's pid and directory.
func TestBoardLock_SecondEngineSameBoardRefused(t *testing.T) {
	dirA, dirB := lockTestDirs(t)
	first, err := acquireInstanceLocks(boardCfg("acme", 7), dirA)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	second, err := acquireInstanceLocks(boardCfg("acme", 7), dirB)
	if err == nil {
		second.Release()
		t.Fatal("second engine for the same board was not refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pid "+strconv.Itoa(os.Getpid())) || !strings.Contains(msg, dirA) {
		t.Errorf("refusal must name the holder's pid and dir %q, got: %v", dirA, err)
	}
	// The loser must have released its directory lock.
	if again, err := acquireDirLock(dirB); err != nil {
		t.Errorf("directory lock of the refused loser was left held: %v", err)
	} else {
		(&instanceLocks{dir: again}).Release()
	}
}

// FR-016 (2): different boards on one host both run; so do case variants of one
// board's identity collapse to a single lock.
func TestBoardLock_DifferentBoardsCoexist(t *testing.T) {
	dirA, dirB := lockTestDirs(t)
	a, err := acquireInstanceLocks(boardCfg("acme", 7), dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := acquireInstanceLocks(boardCfg("acme", 8), dirB)
	if err != nil {
		t.Fatalf("a different board must not contend: %v", err)
	}
	b.Release()
}

func TestBoardIdentity_Normalisation(t *testing.T) {
	if boardIdentity(Config{Owner: "Acme", ProjectNum: 7}) != boardIdentity(Config{Owner: "acme", ProjectNum: 7, OwnerType: "user", GHESHost: "GitHub.com"}) {
		t.Error("owner case, OwnerType and an explicit github.com host must not change the identity")
	}
	if boardIdentity(Config{Owner: "acme", ProjectNum: 7}) == boardIdentity(Config{Owner: "acme", ProjectNum: 7, GHESHost: "ghe.example.com:8443"}) {
		t.Error("a GHES host must be a different board")
	}
	name := boardLockFileName(boardIdentity(Config{Owner: "acme", ProjectNum: 7, GHESHost: "ghe.example.com:8443"}))
	if strings.ContainsAny(name, "/:") {
		t.Errorf("lock file name %q has unsafe characters", name)
	}
	dirA, dirB := lockTestDirs(t)
	a, err := acquireInstanceLocks(boardCfg("Acme", 7), dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	if b, err := acquireInstanceLocks(boardCfg("acme", 7), dirB); err == nil {
		b.Release()
		t.Error("case variants of one board must contend")
	}
}

// Stale record from a dead pid, and release-then-reacquire (the re-exec shape),
// must not block.
func TestBoardLock_StaleRecordAndReacquire(t *testing.T) {
	dirA, dirB := lockTestDirs(t)
	cfg := boardCfg("acme", 7)
	lockDir, _ := resolveLockDir()
	stale := filepath.Join(lockDir, boardLockFileName(boardIdentity(cfg)))
	if err := os.WriteFile(stale, []byte(`{"pid":999999,"dir":"/gone"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := acquireInstanceLocks(cfg, dirA)
	if err != nil {
		t.Fatalf("a leftover record must not block: %v", err)
	}
	l.Release()
	l.Release() // idempotent
	l2, err := acquireInstanceLocks(cfg, dirB)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	l2.Release()
}

// A refused loser that cannot read the holder's record says so.
func TestBoardLock_OwnerUnidentified(t *testing.T) {
	dirA, dirB := lockTestDirs(t)
	cfg := boardCfg("acme", 7)
	first, err := acquireInstanceLocks(cfg, dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if err := first.board.f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireInstanceLocks(cfg, dirB); err == nil || !strings.Contains(err.Error(), "owner could not be identified") {
		t.Errorf("want 'owner could not be identified', got %v", err)
	}
}

// FR-014: an unusable lock directory is a startup error, never a silent skip.
func TestBoardLock_UnusableLockDirIsError(t *testing.T) {
	dirA, _ := lockTestDirs(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABRIK_LOCK_DIR", filepath.Join(blocker, "sub"))
	if l, err := acquireInstanceLocks(boardCfg("acme", 7), dirA); err == nil {
		l.Release()
		t.Fatal("expected an error when the lock dir cannot be created")
	}
	t.Setenv("FABRIK_LOCK_DIR", t.TempDir())
	if l, err := acquireInstanceLocks(boardCfg("", 0), dirA); err == nil {
		l.Release()
		t.Fatal("expected an error for an incomplete board identity")
	}
}

// FR-016 (3): refuse to start inside .fabrik/worktrees/.
func TestCheckNotInWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, ".fabrik", "worktrees", "o-r", "issue-5", "sub")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkNotInWorktree(wt); err == nil || !strings.Contains(err.Error(), "worktree") {
		t.Errorf("worktree dir must be refused, got %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	if err := checkNotInWorktree(link); err == nil {
		t.Error("a symlink into a worktree must be refused")
	}
	if err := checkNotInWorktree(root); err != nil {
		t.Errorf("an ordinary dir must pass: %v", err)
	}
	if err := checkNotInWorktree(filepath.Join(root, ".fabrik")); err != nil {
		t.Errorf(".fabrik itself must pass: %v", err)
	}
	// And through the full startup sequence.
	t.Setenv("FABRIK_LOCK_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(wt, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	if l, err := acquireInstanceLocks(boardCfg("acme", 7), wt); err == nil {
		l.Release()
		t.Error("acquireInstanceLocks must refuse inside a worktree")
	}
}

// FR-016 (4): a lock file replaced or removed under a running engine is caught.
func TestVerify_DetectsReplacedOrRemovedLockFiles(t *testing.T) {
	for _, which := range []string{"dir", "board"} {
		for _, mode := range []string{"replace", "remove"} {
			t.Run(which+"/"+mode, func(t *testing.T) {
				dirA, _ := lockTestDirs(t)
				l, err := acquireInstanceLocks(boardCfg("acme", 7), dirA)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Release()
				if err := l.Verify(); err != nil {
					t.Fatalf("fresh locks must verify: %v", err)
				}
				h := l.dir
				if which == "board" {
					h = l.board
				}
				if err := os.Remove(h.path); err != nil {
					t.Fatal(err)
				}
				if mode == "replace" {
					if err := os.WriteFile(h.path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := l.Verify(); err == nil {
					t.Error("Verify must report the lost lock")
				}
			})
		}
	}
}

func TestVerify_DetectsForeignPID(t *testing.T) {
	dirA, _ := lockTestDirs(t)
	l, err := acquireInstanceLocks(boardCfg("acme", 7), dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := l.dir.f.WriteAt([]byte("1234567\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Verify(); err == nil || !strings.Contains(err.Error(), "not this process") {
		t.Errorf("a foreign recorded pid must be a loss, got %v", err)
	}
}

// Run()-level: the next poll after the lock file is replaced stops dispatch and
// exits with the lost-lock error, without cleaning fabrik:locked labels.
func TestRun_LostLockStopsEngine(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1
	dirA, _ := lockTestDirs(t)
	eng.fabrikDir = dirA

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()

	// Wait for the first poll to have happened, then swap the directory lock.
	lockPath := filepath.Join(dirA, ".fabrik", "fabrik.lock")
	time.Sleep(300 * time.Millisecond)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost the instance lock") {
			t.Fatalf("Run() = %v, want the lost-lock error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("engine kept running after its lock file was replaced")
	}
	if !eng.lockLost.Load() {
		t.Error("lockLost not set")
	}
}

// Verify must not read a long-directory board record as truncated (a false lost
// lock): the record embeds the fabrik directory path, which can approach PATH_MAX.
func TestVerify_LongDirRecordIsNotTruncated(t *testing.T) {
	dirA, _ := lockTestDirs(t)
	long := dirA + "/" + strings.Repeat("d", 900)
	l, err := acquireBoardLock(boardCfg("acme", 7), long)
	if err != nil {
		t.Fatal(err)
	}
	defer (&instanceLocks{board: l}).Release()
	if err := (&instanceLocks{board: l}).Verify(); err != nil {
		t.Errorf("long-dir board record must verify, got %v", err)
	}
}
