// Package workspace is the one place that decides where Fabrik's runtime
// churn lives (#2470, ADR 2470): per-issue worktrees, bare clones, sessions,
// logs and debug output. Every other package asks a Layout for those paths,
// so the configured workspace_root cannot be honoured in some places and
// missed in others (TestNoHandBuiltWorkspacePaths pins it).
//
// What does not move: config.yaml, .env, fabrik.lock, the local API socket
// and everything under state/ stay in the bed, because the CLI, TUI and MCP
// tools find a running daemon through them. fabrik.log, history.json and
// warnings.json stay there too.
//
// The package is stdlib plus yaml.v3, so tui, watch, cmd, engine and the gate
// runner can all import it without importing each other.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The subdirectories that move with the workspace root.
const (
	Worktrees = "worktrees"
	Repos     = "repos"
	Sessions  = "sessions"
	Logs      = "logs"
	Debug     = "debug"
)

// EnvVar is the environment variable layer of the precedence
// (flag > env > config.yaml > default).
const EnvVar = "FABRIK_WORKSPACE_ROOT"

// Names lists every moved subdirectory, in creation order.
var Names = []string{Worktrees, Repos, Sessions, Logs, Debug}

// Layout is an immutable description of one bed's workspace. Root is the
// directory holding the moved subdirectories; it is <Bed>/.fabrik unless
// workspace_root is set. A Layout is a value, not a process global, so
// parallel engines in one process (the sim bed) never share one.
type Layout struct {
	Bed  string
	Root string
}

// Default returns the layout of a bed with workspace_root unset: every path
// is <bed>/.fabrik/<name>, exactly what Fabrik used before #2470.
func Default(bed string) Layout {
	return Layout{Bed: bed, Root: filepath.Join(bed, ".fabrik")}
}

func (l Layout) root() string {
	if l.Root != "" {
		return l.Root
	}
	return filepath.Join(l.Bed, ".fabrik")
}

// IsDefault reports whether the workspace is the in-bed <bed>/.fabrik.
func (l Layout) IsDefault() bool {
	return filepath.Clean(l.root()) == filepath.Clean(filepath.Join(l.Bed, ".fabrik"))
}

// Dir returns the path of a moved subdirectory (Worktrees, Repos, Sessions,
// Logs or Debug).
func (l Layout) Dir(name string) string {
	return filepath.Join(l.root(), name)
}

// WorktreePath returns the worktree of an issue: <worktrees>/<ownerRepo>/issue-N,
// or the legacy flat <worktrees>/issue-N when ownerRepo is empty.
func (l Layout) WorktreePath(ownerRepo string, issue int) string {
	name := fmt.Sprintf("issue-%d", issue)
	if ownerRepo == "" {
		return filepath.Join(l.Dir(Worktrees), name)
	}
	return filepath.Join(l.Dir(Worktrees), ownerRepo, name)
}

// StateDir is the bed's state directory. It never moves: the lock, the local
// API socket and the daemon's decision state live there.
func (l Layout) StateDir() string {
	return filepath.Join(l.Bed, ".fabrik", "state")
}

// expandHome expands a leading "~" or "~/".
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding ~ in workspace_root: %w", err)
	}
	return filepath.Join(home, p[1:]), nil
}

// Pick returns the winning raw value of the precedence flag > env > yaml, or ""
// when all three are blank. Resolve and the daemon's flag plumbing share it so
// the order is stated once.
func Pick(flag, env, yaml string) string {
	for _, v := range []string{flag, env, yaml} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// Resolve applies the precedence flag > env > yaml > default and returns the
// bed's layout. A relative value is resolved against the bed (never the
// process's working directory) and a leading ~ is expanded. The path need not
// exist; Establish checks that.
func Resolve(bed, flag, env, yaml string) (Layout, error) {
	absBed, err := filepath.Abs(bed)
	if err != nil {
		return Layout{}, fmt.Errorf("resolving bed directory %q: %w", bed, err)
	}
	val := Pick(flag, env, yaml)
	if val == "" {
		return Default(absBed), nil
	}
	val, err = expandHome(val)
	if err != nil {
		return Layout{}, err
	}
	if !filepath.IsAbs(val) {
		val = filepath.Join(absBed, val)
	}
	return Layout{Bed: absBed, Root: filepath.Clean(val)}, nil
}

// yamlRoot is the only config.yaml key this package reads.
type yamlRoot struct {
	WorkspaceRoot string `yaml:"workspace_root"`
}

// Load resolves the layout of a bed for a consumer that has no Config of its
// own (fabrik watch, fabrik resume, the gate runner): the given flag value,
// then FABRIK_WORKSPACE_ROOT, then <bed>/.fabrik/config.yaml. An unreadable or
// malformed config.yaml is treated as unset, so a read-only consumer never
// fails on a file the daemon itself reports.
func Load(bed, flag string) (Layout, error) {
	yamlVal := ""
	if data, err := os.ReadFile(filepath.Join(bed, ".fabrik", "config.yaml")); err == nil {
		var y yamlRoot
		if yaml.Unmarshal(data, &y) == nil {
			yamlVal = y.WorkspaceRoot
		}
	}
	return Resolve(bed, flag, os.Getenv(EnvVar), yamlVal)
}

// evalPath resolves symlinks of the deepest existing ancestor of p and
// re-appends the remainder, so a path that does not exist (yet) still compares
// equal to its resolved form.
func evalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Resolved returns p with symlinks resolved (tolerating a missing tail). All
// worktree path comparisons go through it or Same (R5): `/var` vs
// `/private/var` on macOS and an external volume reached through a symlink
// must give the same answer.
func Resolved(p string) string { return evalPath(p) }

// Same reports whether two paths name the same location after symlink
// resolution.
func Same(a, b string) bool {
	return evalPath(a) == evalPath(b)
}

// Within reports whether child is parent or lies beneath it, after symlink
// resolution.
func Within(parent, child string) bool {
	p, c := evalPath(parent), evalPath(child)
	if p == c {
		return true
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
