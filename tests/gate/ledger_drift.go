package gate

import (
	"context"
	"fmt"
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

	seen := map[string]bool{}
	for _, p := range changed {
		if seen[p] {
			continue
		}
		seen[p] = true
		if driftAllowed(p) {
			d.Changed++
		} else {
			d.Paths = append(d.Paths, p)
		}
	}
	sort.Strings(d.Paths)
	d.Valid = len(d.Paths) == 0
	return d
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
