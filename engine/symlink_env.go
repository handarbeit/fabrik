package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// symlinkEnvIfEnabled creates a symlink at <workDir>/.env pointing to the
// fabrikDir .env when cfg.SymlinkEnv is true. It is idempotent: a real file at
// the destination, or a symlink that resolves, is left untouched. A symlink
// that no longer resolves is repaired (#2470, R4). Symlink creation failure is
// non-fatal — a warning is logged and the stage proceeds.
//
// With the default workspace the target is relative (byte-identical to before
// the workspace root existed); a worktree under a configured root may sit on
// another volume, where a relative chain of ../ is fragile, so the target is
// the absolute path of the bed's .env.
func (e *Engine) symlinkEnvIfEnabled(issueNumber int, workDir string) {
	if !e.cfg.SymlinkEnv {
		return
	}

	src := filepath.Join(e.fabrikDir, ".env")
	if _, err := os.Stat(src); err != nil {
		if !os.IsNotExist(err) {
			e.logf(issueNumber, "warn", "symlink-env: could not stat source .env: %v\n", err)
		}
		return
	}

	dst := filepath.Join(workDir, ".env")
	repaired, err := linkEnv(src, dst, workDir, !e.layout().IsDefault())
	switch {
	case err != nil:
		e.logf(issueNumber, "warn", "could not symlink .env into worktree: %v\n", err)
	case repaired:
		e.logf(issueNumber, "info", "repaired broken worktree .env symlink\n")
	}
}

// linkEnv makes dst a working symlink to src. A real (non-link) file, or a link
// that resolves, is left alone. A link whose target is missing is replaced
// atomically — a temporary link renamed over it — so a worker reading the file
// never sees a moment with no .env. repaired reports a replaced broken link.
func linkEnv(src, dst, workDir string, absolute bool) (repaired bool, err error) {
	target := src
	if !absolute {
		if rel, relErr := filepath.Rel(workDir, src); relErr == nil {
			target = rel
		}
	}
	fi, lerr := os.Lstat(dst)
	switch {
	case lerr == nil:
		if fi.Mode()&os.ModeSymlink == 0 {
			return false, nil // a real file: never overwritten
		}
		if _, serr := os.Stat(dst); serr == nil {
			return false, nil // resolves: leave it
		}
		tmp := dst + ".fabrik-repair-" + strconv.Itoa(os.Getpid())
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return false, err
		}
		return true, nil
	case os.IsNotExist(lerr):
		return false, os.Symlink(target, dst)
	default:
		return false, lerr
	}
}

// ensureEnvExcluded adds ".env" to the per-worktree git exclude file so that
// git stash -u does not capture the symlink created by symlinkEnvIfEnabled.
// This must be called before any git stash operation. It is idempotent and
// non-fatal — failures are logged as warnings.
func (e *Engine) ensureEnvExcluded(issueNumber int, workDir string) {
	if !e.cfg.SymlinkEnv {
		return
	}
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		e.logf(issueNumber, "warn", "symlink-env: could not determine git-dir for exclude: %v\n", err)
		return
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(workDir, gitDir)
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0755); err != nil {
		e.logf(issueNumber, "warn", "symlink-env: could not create git info dir: %v\n", err)
		return
	}
	excludePath := filepath.Join(infoDir, "exclude")
	existing, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == ".env" {
			return
		}
	}
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		e.logf(issueNumber, "warn", "symlink-env: could not open git exclude: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(".env\n"); err != nil {
		e.logf(issueNumber, "warn", "symlink-env: could not write git exclude entry: %v\n", err)
	}
}
