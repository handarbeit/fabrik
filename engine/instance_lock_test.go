package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	// Wait, against a bounded deadline, until Run() owns fabrik.lock (the file
	// exists and records this process's pid) and has completed a poll. Swapping
	// earlier would either fail to remove a file Run() has not created yet, or let
	// Run() open and lock the test's replacement, in which case nothing is lost.
	lockPath := filepath.Join(dirA, ".fabrik", "fabrik.lock")
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(lockPath)
		owned := err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid())
		if owned && !eng.health.snapshot().PollSuccessAt.IsZero() {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run() returned before the lock was swapped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine never took its lock and completed a poll")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	long := dirA + "/" + strings.Repeat("d", 5000)
	l, err := acquireBoardLock(boardCfg("acme", 7), long)
	if err != nil {
		t.Fatal(err)
	}
	defer (&instanceLocks{board: l}).Release()
	if err := (&instanceLocks{board: l}).Verify(); err != nil {
		t.Errorf("long-dir board record must verify, got %v", err)
	}
}

// verifyHarness acquires real locks and returns them with the board lock's
// verify seams ready to override and a collector for tolerated-failure warnings.
func verifyHarness(t *testing.T) (*instanceLocks, *[]string) {
	t.Helper()
	dirA, _ := lockTestDirs(t)
	l, err := acquireInstanceLocks(boardCfg("acme", 7), dirA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	var warnings []string
	l.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	return l, &warnings
}

// Definitive losses are fatal on the first verification, whatever the count.
func TestVerify_DefinitiveLossesAreFatalImmediately(t *testing.T) {
	cases := map[string]func(h *heldLock){
		"path ENOENT": func(h *heldLock) {
			h.statFn = func(string) (lockFileID, error) {
				return lockFileID{}, &os.PathError{Op: "stat", Path: h.path, Err: syscall.ENOENT}
			}
		},
		"inode mismatch": func(h *heldLock) {
			h.statFn = func(string) (lockFileID, error) { return lockFileID{dev: 1, ino: 2}, nil }
			h.fstatFn = func(*os.File) (lockFileID, error) { return lockFileID{dev: 1, ino: 3}, nil }
		},
		"foreign pid": func(h *heldLock) {
			h.readFn = func(*os.File) ([]byte, error) { return []byte("1234567\n"), nil }
		},
		"empty record": func(h *heldLock) {
			h.readFn = func(*os.File) ([]byte, error) { return nil, nil }
		},
		"unparseable record": func(h *heldLock) {
			h.readFn = func(*os.File) ([]byte, error) { return []byte("{not json"), nil }
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			l, warnings := verifyHarness(t)
			inject(l.board)
			err := l.Verify()
			if err == nil {
				t.Fatal("a definitive loss must be fatal on the first verification")
			}
			var inc *inconclusiveVerifyError
			if errors.As(err, &inc) {
				t.Errorf("definitive loss was classified inconclusive: %v", err)
			}
			if len(*warnings) != 0 {
				t.Errorf("a definitive loss must not be downgraded to a warning: %v", *warnings)
			}
		})
	}
}

// Every other stat/read error is inconclusive: tolerated with a warning until
// maxInconclusiveVerifies consecutive ones, then a loss.
func TestVerify_InconclusiveErrorsAreTolerated(t *testing.T) {
	cases := map[string]func(h *heldLock, err error){
		"fstat": func(h *heldLock, err error) {
			h.fstatFn = func(*os.File) (lockFileID, error) { return lockFileID{}, err }
		},
		"stat": func(h *heldLock, err error) {
			h.statFn = func(string) (lockFileID, error) { return lockFileID{}, err }
		},
		"read": func(h *heldLock, err error) {
			h.readFn = func(*os.File) ([]byte, error) { return nil, err }
		},
	}
	errnos := []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.EACCES, syscall.EINTR}
	for name, inject := range cases {
		for _, errno := range errnos {
			t.Run(name+"/"+errno.Error(), func(t *testing.T) {
				l, warnings := verifyHarness(t)
				inject(l.board, errno)
				for i := 1; i < maxInconclusiveVerifies; i++ {
					if err := l.Verify(); err != nil {
						t.Fatalf("inconclusive verification %d must be tolerated, got %v", i, err)
					}
				}
				if got := len(*warnings); got != maxInconclusiveVerifies-1 {
					t.Errorf("want %d warnings, got %d: %v", maxInconclusiveVerifies-1, got, *warnings)
				}
				err := l.Verify()
				if err == nil {
					t.Fatalf("%d consecutive inconclusive verifications must be a loss", maxInconclusiveVerifies)
				}
				if !errors.Is(err, errno) {
					t.Errorf("the loss must carry the underlying error, got %v", err)
				}
			})
		}
	}
}

// A clean verification resets the consecutive count.
func TestVerify_CleanVerifyResetsInconclusiveCount(t *testing.T) {
	l, _ := verifyHarness(t)
	fail := true
	l.board.statFn = func(path string) (lockFileID, error) {
		if fail {
			return lockFileID{}, syscall.EIO
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return lockFileID{}, err
		}
		return lockFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil //nolint:unconvert // platform widths
	}
	for round := 0; round < 3; round++ {
		fail = true
		for i := 1; i < maxInconclusiveVerifies; i++ {
			if err := l.Verify(); err != nil {
				t.Fatalf("round %d: tolerated failure %d returned %v", round, i, err)
			}
		}
		fail = false
		if err := l.Verify(); err != nil {
			t.Fatalf("round %d: clean verify returned %v", round, err)
		}
		if l.board.inconclusive != 0 {
			t.Fatalf("round %d: count not reset (%d)", round, l.board.inconclusive)
		}
	}
}

// A failed pid write must fail startup with the real cause, not surface at the
// first poll as a lost lock.
func TestAcquireDirLock_WriteFailureIsReturned(t *testing.T) {
	dirA, _ := lockTestDirs(t)
	orig := writeDirLockRecord
	writeDirLockRecord = func(*os.File) error { return syscall.ENOSPC }
	t.Cleanup(func() { writeDirLockRecord = orig })

	h, err := acquireDirLock(dirA)
	if err == nil {
		(&instanceLocks{dir: h}).Release()
		t.Fatal("a failed pid write must be an acquire error")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("error must carry the real cause, got %v", err)
	}
	// The flock must not be left held by the failed acquire.
	writeDirLockRecord = orig
	again, err := acquireDirLock(dirA)
	if err != nil {
		t.Fatalf("failed acquire left the directory lock held: %v", err)
	}
	(&instanceLocks{dir: again}).Release()
}
