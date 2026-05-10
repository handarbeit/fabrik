package engine

import (
	"os"
	"path/filepath"
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
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return
	}

	dst := filepath.Join(workDir, ".env")
	if _, err := os.Lstat(dst); err == nil {
		e.logf(issueNumber, "debug", "worktree .env already exists — skipping symlink\n")
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
