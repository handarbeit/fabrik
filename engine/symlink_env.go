package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// symlinkEnvIfEnabled creates a symlink at <workDir>/.env pointing to the
// fabrikDir .env when cfg.SymlinkEnv is true. It is idempotent: if the
// destination already exists (file or symlink) it is left untouched. Symlink
// creation failure is non-fatal — a warning is logged and the stage proceeds.
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
	if _, err := os.Lstat(dst); err == nil {
		e.logf(issueNumber, "debug", "worktree .env already exists — skipping symlink\n")
		return
	} else if !os.IsNotExist(err) {
		e.logf(issueNumber, "warn", "symlink-env: could not stat worktree .env: %v\n", err)
		return
	}

	target, err := filepath.Rel(workDir, src)
	if err != nil {
		target = src
	}

	if err := os.Symlink(target, dst); err != nil {
		e.logf(issueNumber, "warn", "could not symlink .env into worktree: %v\n", err)
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
