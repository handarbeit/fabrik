package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const markerName = "workspace-root"

var issueDirRE = regexp.MustCompile(`^issue-\d+$`)

func (l Layout) markerPath() string {
	return filepath.Join(l.StateDir(), markerName)
}

// Establish makes the layout usable: it refuses a root change that would
// orphan live worktrees (CheckMigration, R6), prepares the directories
// (Prepare, FR-011) and records the root. It is called once, from engine.New,
// before anything is migrated or created under the new root.
func (l Layout) Establish() error {
	if err := l.CheckMigration(); err != nil {
		return err
	}
	if err := l.Prepare(); err != nil {
		return err
	}
	return l.recordRoot()
}

// Prepare creates the moved subdirectories under a configured root. The root
// must already exist as a directory, or its parent must: only the leaf is
// created, so an unmounted volume (`/Volumes/X/fabrik` with X missing) fails
// instead of silently recreating the workspace on the system disk (FR-011).
// The in-bed default is left to the lazy creation Fabrik always did.
func (l Layout) Prepare() error {
	if l.IsDefault() {
		return nil
	}
	root := l.root()
	fi, err := os.Stat(root)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("workspace_root %s is not a directory", root)
		}
	case os.IsNotExist(err):
		parent := filepath.Dir(root)
		pfi, perr := os.Stat(parent)
		if perr != nil || !pfi.IsDir() {
			return fmt.Errorf("workspace_root %s does not exist and neither does its parent %s: is the volume mounted? Create the parent directory first; Fabrik never creates a missing mount path", root, parent)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			return fmt.Errorf("creating workspace_root %s: %w", root, err)
		}
	default:
		return fmt.Errorf("checking workspace_root %s: %w", root, err)
	}
	for _, n := range Names {
		if err := os.MkdirAll(l.Dir(n), 0o755); err != nil {
			return fmt.Errorf("creating %s under workspace_root: %w", l.Dir(n), err)
		}
	}
	probe, err := os.CreateTemp(root, ".fabrik-write-probe-*")
	if err != nil {
		return fmt.Errorf("workspace_root %s is not writable: %w", root, err)
	}
	name := probe.Name()
	probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("workspace_root %s is not writable: %w", root, err)
	}
	return nil
}

// previousRoot returns the root the last run used: the marker content, or the
// in-bed default when there is no readable marker (a fresh or pre-feature bed).
func (l Layout) previousRoot() string {
	data, err := os.ReadFile(l.markerPath())
	if err != nil {
		return filepath.Join(l.Bed, ".fabrik")
	}
	s := strings.TrimSpace(string(data))
	if s == "" || !filepath.IsAbs(s) {
		return filepath.Join(l.Bed, ".fabrik")
	}
	return s
}

// liveWorktrees lists issue worktree directories under root, both the
// per-repo layout (<owner>-<repo>/issue-N) and the legacy flat one.
func liveWorktrees(root string) []string {
	wt := filepath.Join(root, Worktrees)
	entries, err := os.ReadDir(wt)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(wt, e.Name())
		if issueDirRE.MatchString(e.Name()) {
			out = append(out, p)
			continue
		}
		subs, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, s := range subs {
			if s.IsDir() && issueDirRE.MatchString(s.Name()) {
				out = append(out, filepath.Join(p, s.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// CheckMigration refuses to continue when the workspace root changed since
// the last run and the previous root still holds worktrees (R6). Fabrik
// refuses rather than moves: nothing is moved, deleted or rewritten, so a
// refusal is lossless and reversible. "Active" means any issue-N directory:
// whether the janitor would reap one needs GitHub and the item Store, neither
// of which exists yet at startup. A missing or unreadable marker means the
// in-bed default, which still catches live work there.
func (l Layout) CheckMigration() error {
	prev := l.previousRoot()
	if Same(prev, l.root()) {
		return nil
	}
	live := liveWorktrees(prev)
	if len(live) == 0 {
		return nil
	}
	const show = 10
	list := live
	more := ""
	if len(list) > show {
		more = fmt.Sprintf("\n  ... and %d more", len(list)-show)
		list = list[:show]
	}
	return fmt.Errorf("workspace_root changed from %s to %s, but the old root still holds %d worktree(s):\n  %s%s\n"+
		"Fabrik does not move worktrees. Either set workspace_root back to %s, or let that work finish and remove the old worktrees "+
		"(push or discard their changes first; `git worktree remove <path>` from the matching bare clone under %s), then restart",
		prev, l.root(), len(live), strings.Join(list, "\n  "), more, prev, filepath.Join(prev, Repos))
}

// recordRoot writes the marker the next start compares against. The default
// root records nothing, and removes a marker left by an earlier configured
// root, so a bed that never used the setting gains no new file.
func (l Layout) recordRoot() error {
	if l.IsDefault() {
		if err := os.Remove(l.markerPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clearing workspace root marker: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(l.StateDir(), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	tmp := l.markerPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(l.root()+"\n"), 0o600); err != nil {
		return fmt.Errorf("recording workspace root: %w", err)
	}
	if err := os.Rename(tmp, l.markerPath()); err != nil {
		return fmt.Errorf("recording workspace root: %w", err)
	}
	return nil
}
