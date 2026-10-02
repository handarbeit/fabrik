package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Candidate is a locally running `fabrik` process and its working directory.
type Candidate struct {
	PID int
	Dir string
}

func (g *Gate) defaultProcCwd(ctx context.Context, pid int) string {
	return ResolvePIDCwd(ctx, g.Exec, runtime.GOOS, pid)
}

// ResolvePIDCwd is run.sh's resolve_pid_cwd: the working directory of pid, or ""
// when it cannot be determined (process gone, permission denied, unsupported
// platform). Linux resolves it through /proc; macOS has no /proc, so it asks
// lsof for only the process's cwd descriptor in -F form, whose `n` lines carry
// the path.
func ResolvePIDCwd(ctx context.Context, x Commander, goos string, pid int) string {
	if goos == "linux" {
		dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			return ""
		}
		return dir
	}
	so, _, res := output(ctx, x, Cmd{Name: "lsof", Args: []string{"-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn"}})
	if res.Err != nil || res.ExitCode == 127 {
		return "" // lsof not installed
	}
	last := ""
	for _, line := range strings.Split(so, "\n") {
		if strings.HasPrefix(line, "n") {
			last = line[1:]
		}
	}
	return last
}

// DiscoverFabrikProcessDirs is run.sh's discover_fabrik_process_dirs: locally
// running `fabrik` processes (exact name match via `pgrep -x`, so
// `fabrik-test`/`fabrik-workflows`-named things are never candidates), with the
// cwd of each whose cwd could be resolved. The OS-dependent half; the pure half
// is FindCompetingTokenConsumers.
func (g *Gate) DiscoverFabrikProcessDirs(ctx context.Context) []Candidate {
	so, _, res := output(ctx, g.Exec, Cmd{Name: "pgrep", Args: []string{"-x", "fabrik"}})
	if res.Err != nil {
		return nil
	}
	var out []Candidate
	for _, f := range strings.Fields(so) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == g.Self {
			continue
		}
		if dir := g.ProcCwd(ctx, pid); dir != "" {
			out = append(out, Candidate{PID: pid, Dir: dir})
		}
	}
	return out
}

// realPath is `cd dir && pwd -P`, falling back to dir itself.
func realPath(dir string) string {
	if p, err := filepath.EvalSymlinks(dir); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return dir
}

// FindCompetingTokenConsumers is the pure filter at the heart of R1 (#1684):
// of candidates, those that (a) are NOT the bed's own directory — compared by
// resolved absolute path, since the bed's own instance is already budgeted into
// the ~4,000/5,000 estimate and must never be reported as a competitor — and (b)
// have a readable .env FABRIK_TOKEN equal to the bed's. A candidate whose
// directory doesn't exist, or has no .env or no FABRIK_TOKEN line, is silently
// excluded.
func FindCompetingTokenConsumers(bedDir, bedToken string, candidates []Candidate) []Candidate {
	return FindCompetingTokenConsumersExcluding([]string{bedDir}, bedToken, candidates)
}

// FindCompetingTokenConsumersExcluding is FindCompetingTokenConsumers with a SET
// of excluded directories (#1976, D5): on a multi-bed run every configured bed's
// own engine is excluded — the beds' engines sharing an identity is the
// scheduler's business (it serializes them), not a refusal — while any OTHER
// process sharing bedToken, the dev daemon above all, is still reported.
func FindCompetingTokenConsumersExcluding(exclude []string, bedToken string, candidates []Candidate) []Candidate {
	var found []Candidate
	excluded := map[string]bool{}
	for _, d := range exclude {
		excluded[realPath(d)] = true
	}
	for _, c := range candidates {
		if c.PID == 0 || c.Dir == "" {
			continue
		}
		if excluded[realPath(c.Dir)] {
			continue
		}
		tok := EnvFileValue(filepath.Join(c.Dir, ".env"), "FABRIK_TOKEN")
		if tok != "" && tok == bedToken {
			found = append(found, c)
		}
	}
	return found
}

// CheckCompetingTokenConsumers is the R1 (#1684) orchestration wrapper. It
// honours E2E_SKIP_TOKEN_CHECK, degrades to a warning when the bed's own token
// could not be read (nothing to compare against), downgrades to a warning when
// no pat leg is planned (the bed engine then spends the App installation's own
// budget, so only the harness's own calls compete), and otherwise refuses with
// ExitPreconditionFailed naming every competing PID/directory: a silent
// proceed-into-backoff costs an hour of already-spent budget, strictly worse
// than one operator round-trip.
//
// On a multi-bed run (#1976, D5) the check runs once per bed, against that bed's
// own token, with EVERY configured bed's directory excluded; the process listing
// is taken once. It uses all the requested auth modes for every bed — the greedy
// fallback can route any cell to any bed — which can only turn a warning into a
// refusal, never weaken it.
func (g *Gate) CheckCompetingTokenConsumers(ctx context.Context, modes []string) error {
	if g.Getenv("E2E_SKIP_TOKEN_CHECK") != "" {
		g.outln("== competing-token-consumer check skipped (E2E_SKIP_TOKEN_CHECK set) ==")
		return nil
	}
	var cands []Candidate
	discovered := false
	discover := func() []Candidate {
		if !discovered {
			cands, discovered = g.DiscoverFabrikProcessDirs(ctx), true
		}
		return cands
	}
	if !g.multiBed() {
		return g.checkBedTokenConsumers(modes, []string{g.Cfg.TestBed}, discover)
	}
	var msgs []string
	for _, b := range g.bedGates {
		if err := b.checkBedTokenConsumers(modes, g.Cfg.BedDirs, discover); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		return &ExitError{Code: ExitPreconditionFailed, Msg: strings.Join(msgs, "\n")}
	}
	return nil
}

// checkBedTokenConsumers is the check for one bed: g's own token, the given
// directories excluded.
func (g *Gate) checkBedTokenConsumers(modes, exclude []string, discover func() []Candidate) error {
	if g.Cfg.BedToken == "" {
		g.errln("warning: BED_TOKEN unreadable — skipping competing-token-consumer check (R1, #1684): nothing to compare candidate processes' tokens against")
		return nil
	}
	g.outln("== checking for competing consumers of the bed's GraphQL token (R1, #1684) ==")
	matches := FindCompetingTokenConsumersExcluding(exclude, g.Cfg.BedToken, discover())
	if len(matches) > 0 && !authModesInclude(modes, "pat") {
		g.errf("warning: other local Fabrik process(es) share the bed's FABRIK_TOKEN, but no pat leg is planned (auth legs: %s) — the bed engine uses the App installation's budget, so only the harness's own calls compete:\n", strings.Join(modes, " "))
		for _, m := range matches {
			g.errf("   pid %d   dir %s\n", m.PID, m.Dir)
		}
		return nil
	}
	if len(matches) > 0 {
		var b strings.Builder
		b.WriteString("\n############################################################\n")
		b.WriteString("## PRECONDITION FAILED: competing consumer(s) of the shared @arbeithand\n")
		b.WriteString("## GraphQL token found.\n")
		b.WriteString("##\n")
		b.WriteString("## The following local Fabrik process(es) have an .env FABRIK_TOKEN\n")
		fmt.Fprintf(&b, "## identical to this test bed's (%s):\n", g.Cfg.TestBed)
		b.WriteString("##\n")
		for _, m := range matches {
			fmt.Fprintf(&b, "##   pid %d   dir %s\n", m.PID, m.Dir)
		}
		b.WriteString("##\n")
		b.WriteString("## Any GraphQL calls that process makes come out of the SAME\n")
		b.WriteString("## 5,000/hour bucket this gate run needs (~4,000 pts/leg) — see\n")
		b.WriteString("## tests/e2e/README.md's 'Operational up/down contract'. Stop it, or\n")
		b.WriteString("## re-run with E2E_SKIP_TOKEN_CHECK=1 to proceed anyway (not\n")
		b.WriteString("## recommended — you'll likely lose the run to backoff after it has\n")
		b.WriteString("## already spent an hour).\n")
		b.WriteString("############################################################")
		return &ExitError{Code: ExitPreconditionFailed, Msg: b.String()}
	}
	g.outln("   no competing token consumers found")
	return nil
}

// CheckReviewerReachable is R2 (#1684): the review bot (Pruefer) must be alive
// before a run that needs it. Skipped when the caller supplied -run/--run (a
// narrowed subset may not include any review-gated scenario — left to the
// operator). Refuses when PRUEFER_DIR exists but its .pruefer/pruefer.lock is
// missing or names no live process (a confident "down"); only WARNS when
// PRUEFER_DIR itself doesn't exist, since that is undeterminable rather than
// confirmed-down — a genuinely remote Pruefer must never be permanently blocked
// by a local-only check.
func (g *Gate) CheckReviewerReachable(args []string) error {
	if g.Getenv("E2E_SKIP_REVIEWER_CHECK") != "" {
		g.outln("== reviewer-reachable check skipped (E2E_SKIP_REVIEWER_CHECK set) ==")
		return nil
	}
	if HasRunFlag(args) {
		g.outln("== reviewer-reachable check skipped (-run/--run supplied — a narrowed scenario subset may not include any review-gated scenario; operator's call) ==")
		return nil
	}
	dir := g.Cfg.PrueferDir
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		g.errf("warning: PRUEFER_DIR (%s) does not exist — cannot verify Pruefer's liveness locally (assuming a remote or other topology); proceeding. If Pruefer is not actually reachable, every review-gated scenario will fail ~10 min later on a review timeout. Set PRUEFER_DIR to Pruefer's deployment directory, or E2E_SKIP_REVIEWER_CHECK=1 to silence this warning.\n", dir)
		return nil
	}
	lock := filepath.Join(dir, ".pruefer", "pruefer.lock")
	data, err := os.ReadFile(lock)
	if err != nil {
		return &ExitError{Code: ExitPreconditionFailed, Msg: fmt.Sprintf(
			"PRECONDITION FAILED: Pruefer's lock file not found at %s.\n"+
				"PRUEFER_DIR (%s) exists, so this is treated as confidently down, not merely undeterminable.\n"+
				"Every review-gated scenario in this suite will time out waiting for a review that never arrives.\n"+
				"Start Pruefer, or re-run with E2E_SKIP_REVIEWER_CHECK=1 if this run genuinely doesn't need it.", lock, dir)}
	}
	// The PID Pruefer writes into its own lock file on acquisition (see
	// pruefer/daemon.go's acquireLock). Only the digits of the first line are
	// trusted, guarding against a stale/garbled lock.
	first, _, _ := strings.Cut(string(data), "\n")
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, first)
	pid, perr := strconv.Atoi(digits)
	if digits == "" || perr != nil || !pidAlive(pid) {
		shown := digits
		if shown == "" {
			shown = "<empty>"
		}
		return &ExitError{Code: ExitPreconditionFailed, Msg: fmt.Sprintf(
			"PRECONDITION FAILED: Pruefer's lock file (%s) exists but names no live process (pid='%s').\n"+
				"Every review-gated scenario in this suite will time out waiting for a review that never arrives.\n"+
				"Start Pruefer, or re-run with E2E_SKIP_REVIEWER_CHECK=1 if this run genuinely doesn't need it.", lock, shown)}
	}
	g.outf("== reviewer-reachable check passed: Pruefer alive (pid %d, lock: %s) ==\n", pid, lock)
	return nil
}
