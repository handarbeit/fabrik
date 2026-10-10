package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Instance locks (#2097, ADR 2097).
//
// An engine holds two exclusive flocks for the whole of Run():
//
//   - the per-directory lock, <fabrikDir>/.fabrik/fabrik.lock, which keeps two
//     engines out of one directory. It stays exactly "<pid>\n": the live e2e gate
//     parses it as a bare pid (tests/gate/bed.go lockedBedPID).
//   - the board lock, a per-user host-level file keyed on the board (GitHub host +
//     owner + project number), which keeps two engines on one board apart no
//     matter which directory they were started from. It records {pid, dir}.
//
// Both are advisory flocks tied to the open file description, so they vanish with
// the process (crash, SIGKILL). Neither file is ever unlinked by Fabrik.
// Both fds are close-on-exec (os.OpenFile) and are never handed to a child.

// lockLostMessage is the loud text printed when per-poll verification fails.
const lockLostMessage = "lost the instance lock; another Fabrik may own this board"

// errInstanceLockLost is returned by Run() after verification found a held lock
// replaced or removed on disk.
var errInstanceLockLost = errors.New(lockLostMessage)

// boardOwnerReadAttempts/boardOwnerReadDelay bound how long a refused loser
// waits for the winner's record to appear (the winner writes it just after it
// takes the flock).
const (
	boardOwnerReadAttempts = 6
	boardOwnerReadDelay    = 100 * time.Millisecond
)

// maxLockRecordBytes bounds how much of a lock file verify() reads.
const maxLockRecordBytes = 64 << 10

// boardRecord is the content of a board lock file.
type boardRecord struct {
	PID     int    `json:"pid"`
	Dir     string `json:"dir"`
	Started string `json:"started,omitempty"`
}

// heldLock is one flock held for the lifetime of Run().
type heldLock struct {
	name string // "directory lock" / "board lock", for messages
	path string
	f    *os.File
}

// instanceLocks is the pair of locks an engine holds.
type instanceLocks struct {
	dir   *heldLock
	board *heldLock
}

// Release unlocks and closes both locks. Idempotent and nil-safe.
func (l *instanceLocks) Release() {
	if l == nil {
		return
	}
	for _, h := range []*heldLock{l.board, l.dir} {
		if h != nil && h.f != nil {
			syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing releases it anyway
			if err := h.f.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: closing %s %s: %v\n", h.name, h.path, err)
			}
			h.f = nil
		}
	}
}

// Verify checks, for each held lock, that the file at its path is still the one
// we hold locked (device+inode of the fd against the path) and that the recorded
// pid is still our own. It logs nothing; the caller reports a failure. Cost is
// two stats and one small read per lock.
func (l *instanceLocks) Verify() error {
	if l == nil {
		return nil
	}
	for _, h := range []*heldLock{l.dir, l.board} {
		if h == nil {
			continue
		}
		if err := h.verify(); err != nil {
			return fmt.Errorf("%s (%s): %w", h.name, h.path, err)
		}
	}
	return nil
}

func (h *heldLock) verify() error {
	if h.f == nil {
		return errors.New("lock is no longer held")
	}
	var fdSt, pathSt syscall.Stat_t
	if err := syscall.Fstat(int(h.f.Fd()), &fdSt); err != nil {
		return fmt.Errorf("fstat: %w", err)
	}
	if err := syscall.Stat(h.path, &pathSt); err != nil {
		return fmt.Errorf("lock file is gone or unreadable: %w", err)
	}
	if uint64(fdSt.Dev) != uint64(pathSt.Dev) || uint64(fdSt.Ino) != uint64(pathSt.Ino) { //nolint:unconvert // Dev/Ino widths differ by platform
		return errors.New("lock file was replaced by a different file")
	}
	// Read the whole record (bounded): the board record embeds the directory path
	// and its JSON escaping, and a truncated record would read as a false lost lock.
	rec, err := io.ReadAll(io.NewSectionReader(h.f, 0, maxLockRecordBytes))
	if err != nil {
		return fmt.Errorf("could not read lock record: %w", err)
	}
	pid, perr := parseLockPID(rec)
	if perr != nil {
		return fmt.Errorf("lock record unreadable: %w", perr)
	}
	if pid != os.Getpid() {
		return fmt.Errorf("lock record names pid %d, not this process (%d)", pid, os.Getpid())
	}
	return nil
}

// parseLockPID reads the pid from either lock-file format: a bare "<pid>\n"
// (directory lock) or the board lock's JSON record.
func parseLockPID(b []byte) (int, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, errors.New("empty")
	}
	if strings.HasPrefix(s, "{") {
		var rec boardRecord
		if err := json.Unmarshal([]byte(s), &rec); err != nil {
			return 0, err
		}
		if rec.PID <= 0 {
			return 0, errors.New("no pid")
		}
		return rec.PID, nil
	}
	return strconv.Atoi(s)
}

// checkNotInWorktree is R2: refuse to run from inside a Fabrik issue worktree
// (".fabrik/worktrees/..."), which carries a full copy of the board config. A
// pure function over the path; symlinks are resolved first so a symlinked entry
// into a worktree is still caught.
func checkNotInWorktree(dir string) error {
	resolved := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		resolved = r
	}
	for _, p := range []string{dir, resolved} {
		parts := strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == ".fabrik" && parts[i+1] == "worktrees" {
				return fmt.Errorf("refusing to start in %s: it is inside a .fabrik/worktrees/ directory, i.e. another Fabrik instance's issue worktree; start Fabrik from the project directory instead", dir)
			}
		}
	}
	return nil
}

// acquireDirLock takes the per-directory flock. The file keeps its legacy
// pid-only content. Nothing is written until the flock is ours.
func acquireDirLock(fabrikDir string) (*heldLock, error) {
	lockPath := filepath.Join(fabrikDir, ".fabrik", "fabrik.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another Fabrik instance is already running for this project (lock file: %s)", lockPath)
	}
	// Write our PID for diagnostics and per-poll verification.
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0) //nolint:errcheck // diagnostic; verify() reports a missing record
	}
	return &heldLock{name: "directory lock", path: lockPath, f: f}, nil
}

// boardIdentity is host/owner/projectNumber, lower-cased; an empty host is
// github.com. OwnerType is deliberately excluded: user and organisation logins
// share one namespace.
func boardIdentity(cfg Config) string {
	host := strings.ToLower(strings.TrimSpace(cfg.GHESHost))
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("%s/%s/%d", host, strings.ToLower(strings.TrimSpace(cfg.Owner)), cfg.ProjectNum)
}

var lockSlugUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// boardLockFileName is a readable slug plus a short hash of the identity, so
// characters like ':' and '/' in a host cannot cause collisions or bad names.
func boardLockFileName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	slug := strings.Trim(lockSlugUnsafe.ReplaceAllString(identity, "-"), "-")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return slug + "-" + hex.EncodeToString(sum[:4]) + ".lock"
}

// resolveLockDir returns (and creates) the per-user directory for board locks:
// $FABRIK_LOCK_DIR, else os.UserCacheDir()/fabrik/locks. There is no fallback to
// "no lock" (FR-014).
func resolveLockDir() (string, error) {
	dir := os.Getenv("FABRIK_LOCK_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine the board lock directory (%v); set FABRIK_LOCK_DIR to a writable per-user directory", err)
		}
		dir = filepath.Join(cache, "fabrik", "locks")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create the board lock directory %s: %w; set FABRIK_LOCK_DIR to a writable per-user directory", dir, err)
	}
	return dir, nil
}

// acquireBoardLock takes the host-level flock for the engine's board and records
// {pid, dir} in it with a single write. On refusal it names the holder.
func acquireBoardLock(cfg Config, fabrikDir string) (*heldLock, error) {
	if strings.TrimSpace(cfg.Owner) == "" || cfg.ProjectNum == 0 {
		return nil, errors.New("cannot take the board lock: project owner and project number must both be configured")
	}
	lockDir, err := resolveLockDir()
	if err != nil {
		return nil, err
	}
	identity := boardIdentity(cfg)
	path := filepath.Join(lockDir, boardLockFileName(identity))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open board lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another Fabrik instance (%s) already holds the lock for board %s (lock file: %s)",
			describeBoardOwner(path), identity, path)
	}
	rec, _ := json.Marshal(boardRecord{PID: os.Getpid(), Dir: fabrikDir, Started: time.Now().UTC().Format(time.RFC3339)})
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("could not write board lock record %s: %w", path, err)
	}
	if _, err := f.WriteAt(append(rec, '\n'), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("could not write board lock record %s: %w", path, err)
	}
	return &heldLock{name: "board lock", path: path, f: f}, nil
}

// describeBoardOwner reads the holder's record through a separate open, retrying
// briefly because the winner writes it just after taking the flock.
func describeBoardOwner(path string) string {
	for i := 0; i < boardOwnerReadAttempts; i++ {
		if b, err := os.ReadFile(path); err == nil {
			var rec boardRecord
			if json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec) == nil && rec.PID > 0 {
				return fmt.Sprintf("pid %d, dir %s", rec.PID, rec.Dir)
			}
		}
		if i < boardOwnerReadAttempts-1 {
			time.Sleep(boardOwnerReadDelay)
		}
	}
	return "owner could not be identified"
}

// acquireInstanceLocks runs the startup lock sequence: R2 refusal, directory
// lock, board lock. On any failure nothing stays held.
func acquireInstanceLocks(cfg Config, fabrikDir string) (*instanceLocks, error) {
	if err := checkNotInWorktree(fabrikDir); err != nil {
		return nil, err
	}
	dir, err := acquireDirLock(fabrikDir)
	if err != nil {
		return nil, err
	}
	board, err := acquireBoardLock(cfg, fabrikDir)
	if err != nil {
		(&instanceLocks{dir: dir}).Release()
		return nil, err
	}
	return &instanceLocks{dir: dir, board: board}, nil
}
