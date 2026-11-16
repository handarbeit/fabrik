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

// rootSentinel is written into a configured root by Prepare. A previous root
// that exists but has neither it nor a worktrees directory is not a workspace
// Fabrik used: with a mount-point root it is the empty directory left on the
// system disk while the volume is unmounted (see liveWorktrees).
const rootSentinel = ".fabrik-workspace"

var issueDirRE = regexp.MustCompile(`^issue-\d+$`)

func (l Layout) markerPath() string {
	return filepath.Join(l.StateDir(), markerName)
}

// Establish makes the layout usable: it refuses a root change that would
// orphan live worktrees (CheckMigration, R6), prepares the directories
// (Prepare, FR-011) and records the root (Record). The engine runs the first
// two from New, before anything is migrated or created under the new root, and
// Record from Run once it holds the instance lock, so a second instance that
// loses the lock never rewrites the marker.
func (l Layout) Establish() error {
	if err := l.CheckMigration(); err != nil {
		return err
	}
	if err := l.Prepare(); err != nil {
		return err
	}
	return l.Record()
}

// Prepare creates the moved subdirectories under a configured root. The root
// itself must already exist as a directory and is never created: a root that is
// the mount point of a volume (`/mnt/fabrik`) would otherwise be silently
// recreated on the system disk when the volume is not mounted, and a nested
// path (`/Volumes/X/fabrik`) has the same failure one level up (FR-011). The
// in-bed default is left to the lazy creation Fabrik always did.
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
		return fmt.Errorf("workspace_root %s does not exist: is the volume mounted? Create the directory first (on the volume, not under a mount point that may be unmounted); Fabrik never creates the root itself", root)
	default:
		return fmt.Errorf("checking workspace_root %s: %w", root, err)
	}
	for _, n := range Names {
		if err := os.MkdirAll(l.Dir(n), 0o755); err != nil {
			return fmt.Errorf("creating %s under workspace_root: %w", l.Dir(n), err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, rootSentinel), []byte("fabrik workspace root\n"), 0o644); err != nil {
		return fmt.Errorf("workspace_root %s is not writable: %w", root, err)
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
// per-repo layout (<owner>-<repo>/issue-N) and the legacy flat one. It returns
// an error when the root cannot be inspected, so the caller can refuse rather
// than treat "unknown" as "empty": a root on an unmounted volume must not read
// as having no worktrees.
//
// expectUsed is true for a root a previous run configured (not the in-bed
// default). Such a root that exists without a worktrees directory must carry
// the sentinel Prepare wrote, else it cannot be told from a mount point whose
// volume is not mounted, and is refused rather than read as empty.
func liveWorktrees(root string, expectUsed bool) ([]string, error) {
	wt := filepath.Join(root, Worktrees)
	entries, err := os.ReadDir(wt)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %s: %w", wt, err)
		}
		// Not there. A root that was never used, or one the operator removed
		// after cleaning up, has an existing parent. A missing parent too means
		// the volume holding it may simply not be mounted.
		if _, serr := os.Stat(root); serr == nil {
			if expectUsed {
				if _, ferr := os.Stat(filepath.Join(root, rootSentinel)); ferr != nil {
					return nil, fmt.Errorf("%s exists but is not a workspace Fabrik prepared (no %s): is the volume mounted?", root, rootSentinel)
				}
			}
			return nil, nil
		}
		if pfi, perr := os.Stat(filepath.Dir(root)); perr != nil || !pfi.IsDir() {
			return nil, fmt.Errorf("%s and its parent %s do not exist (is the volume mounted?)", root, filepath.Dir(root))
		}
		return nil, nil
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
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		for _, s := range subs {
			if s.IsDir() && issueDirRE.MatchString(s.Name()) {
				out = append(out, filepath.Join(p, s.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// CheckMigration refuses to continue when the workspace root changed since
// the last run and the previous root still holds worktrees (R6). Fabrik
// refuses rather than moves: nothing is moved, deleted or rewritten, so a
// refusal is lossless and reversible. "Active" means any issue-N directory:
// whether the janitor would reap one needs GitHub and the item Store, neither
// of which exists yet at startup. A missing or unreadable marker means the
// in-bed default, which still catches live work there. A previous root that
// cannot be inspected (an unmounted volume) refuses too: unknown is not empty.
func (l Layout) CheckMigration() error {
	prev := l.previousRoot()
	if Same(prev, l.root()) {
		return nil
	}
	live, err := liveWorktrees(prev, !Same(prev, Default(l.Bed).root()))
	if err != nil {
		return fmt.Errorf("workspace_root changed from %s to %s, but the old root cannot be inspected: %w\n"+
			"Fabrik will not assume it is empty. Make the old root reachable (mount the volume) and restart, or set workspace_root back to %s. "+
			"If the volume is mounted and you know the old root holds no worktrees, create an empty %s file in it",
			prev, l.root(), err, prev, filepath.Join(prev, rootSentinel))
	}
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

// Record writes the marker the next start compares against. The default
// root records nothing, and removes a marker left by an earlier configured
// root, so a bed that never used the setting gains no new file.
func (l Layout) Record() error {
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
