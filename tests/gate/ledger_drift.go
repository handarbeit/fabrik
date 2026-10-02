package gate

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// driftAllowedPrefixes are the paths whose change cannot alter engine behaviour:
// the live suite, its scripts, and the gate runner itself (#1994's tests/gate,
// which #1973–#1977 all change). A diff confined to them leaves the ledger valid
// (R2); anything else means a new engine SHA and a new, empty ledger.
var driftAllowedPrefixes = []string{"tests/e2e/", "scripts/e2e/", "tests/gate/"}

// Drift is the R2 verdict: is the ledger recorded for an engine SHA still a
// valid certificate for the tree being gated?
type Drift struct {
	SHA   string
	Valid bool
	// Paths are the changed paths outside the allowlist (the invalidating ones).
	Paths []string
	// Reason is set when Valid is false for a reason other than Paths.
	Reason string
	// Changed counts the allowed paths that differ (informational).
	Changed int
}

// DriftCheck compares the working tree in RepoRoot against engineSHA: committed
// differences, uncommitted tracked edits and untracked files all count. An
// engine SHA that is not a commit in RepoRoot (the bed's clone fetched it, this
// checkout has not) cannot be compared and is reported invalid — fail closed —
// with the fix in the message.
func (g *Gate) DriftCheck(ctx context.Context, engineSHA string) Drift {
	d := Drift{SHA: engineSHA}
	git := func(args ...string) (string, Result) {
		so, _, res := output(ctx, g.Exec, Cmd{Name: "git", Args: args, Dir: g.Cfg.RepoRoot, Env: g.Env})
		return so, res
	}
	if _, res := git("cat-file", "-e", engineSHA+"^{commit}"); res.ExitCode != 0 || res.Err != nil {
		d.Reason = fmt.Sprintf("engine SHA %s is not a commit in %s — run 'git fetch origin' there so the drift check can compare against it", shortSHA(engineSHA), g.Cfg.RepoRoot)
		return d
	}
	so, res := git("diff", "--name-only", engineSHA)
	if res.ExitCode != 0 || res.Err != nil {
		d.Reason = fmt.Sprintf("git diff --name-only %s failed (exit %d)", shortSHA(engineSHA), res.ExitCode)
		return d
	}
	changed := splitLines(so)
	so, res = git("ls-files", "--others", "--exclude-standard")
	if res.ExitCode != 0 || res.Err != nil {
		d.Reason = fmt.Sprintf("git ls-files --others failed (exit %d)", res.ExitCode)
		return d
	}
	changed = append(changed, splitLines(so)...)

	// A caller that declares its own benign self-writes (cut-release.sh's step-4
	// release-notes and known-versions edits, via FABRIK_PREGATE_ALLOWED_DIRTY_REGEX)
	// has them excused here too — but only while they are UNCOMMITTED edits: a
	// path also changed by a commit since the engine SHA is a real difference.
	declared, err := g.declaredDirtyPaths(ctx)
	if err != nil {
		d.Reason = err.Error()
		return d
	}
	if len(declared) > 0 {
		so, res = git("diff", "--name-only", engineSHA, "HEAD")
		if res.ExitCode != 0 || res.Err != nil {
			d.Reason = fmt.Sprintf("git diff --name-only %s HEAD failed (exit %d)", shortSHA(engineSHA), res.ExitCode)
			return d
		}
		for _, p := range splitLines(so) {
			delete(declared, p)
		}
	}

	seen := map[string]bool{}
	for _, p := range changed {
		if seen[p] {
			continue
		}
		seen[p] = true
		if driftAllowed(p) || declared[p] {
			d.Changed++
		} else {
			d.Paths = append(d.Paths, p)
		}
	}
	sort.Strings(d.Paths)
	d.Valid = len(d.Paths) == 0
	return d
}

// declaredDirtyPaths are the working-tree paths whose `git status --porcelain`
// line matches FABRIK_PREGATE_ALLOWED_DIRTY_REGEX (empty when it is unset or does
// not compile — fail closed, nothing is excused).
func (g *Gate) declaredDirtyPaths(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	pat := g.Getenv("FABRIK_PREGATE_ALLOWED_DIRTY_REGEX")
	if pat == "" {
		return out, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return out, nil
	}
	so, _, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"status", "--porcelain"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
	if res.ExitCode != 0 || res.Err != nil {
		return nil, fmt.Errorf("git status --porcelain failed (exit %d)", res.ExitCode)
	}
	for _, line := range strings.Split(so, "\n") {
		if len(line) > 3 && re.MatchString(line) {
			out[strings.TrimSpace(line[3:])] = true
		}
	}
	return out, nil
}

func driftAllowed(path string) bool {
	for _, p := range driftAllowedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Report is the printed R2 result: which SHA, and which paths (if any)
// invalidate it.
func (d Drift) Report() string {
	var b strings.Builder
	switch {
	case d.Valid:
		fmt.Fprintf(&b, "ledger drift check: engine SHA %s — VALID (%d test/gate-only path(s) differ; none touch the engine)", shortSHA(d.SHA), d.Changed)
	case d.Reason != "":
		fmt.Fprintf(&b, "ledger drift check: engine SHA %s — INVALID: %s", shortSHA(d.SHA), d.Reason)
	default:
		fmt.Fprintf(&b, "ledger drift check: engine SHA %s — INVALID: %d path(s) outside tests/e2e, scripts/e2e and tests/gate differ:", shortSHA(d.SHA), len(d.Paths))
		for i, p := range d.Paths {
			if i == 10 {
				fmt.Fprintf(&b, "\n  … and %d more", len(d.Paths)-10)
				break
			}
			fmt.Fprintf(&b, "\n  %s", p)
		}
	}
	return b.String()
}
