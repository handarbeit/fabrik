package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// `gate report` (ReportCmd): the live suite's measured runtime per test, read from the
// per-leg archive's go-test.json streams (#1972) and joined with the registry's
// entry-stage / traversal fields (#1992, R1/R5). It is read-only — it starts no bed,
// runs no test and writes nothing.
//
//	gate report [--sha <sha>] [--baseline <sha>] [--format text|json]
//
// What it can and cannot measure. Runtime is real: each top-level test's terminal
// `go test -json` Elapsed, per cell. Model-quota use is NOT recorded anywhere the gate
// keeps — neither the ledger nor the archive (the engine log carries no token counts;
// the "Used N/M turns, … tokens" footer lives in the stage comments on GitHub). The
// report therefore carries a labelled proxy instead: how many pipeline stages a test
// no longer drives because it enters later than Specify (StagesSkipped). Every stage
// a test enters past is one real Claude invocation (plus its CI and review waits) the
// run does not pay for.

// RuntimeRow is one live test.
type RuntimeRow struct {
	Test          string             `json:"test"`
	Entry         registry.Stage     `json:"entry"`
	Traversal     registry.Traversal `json:"traversal"`
	FullTraversal bool               `json:"full_traversal,omitempty"`
	// StagesSkipped is the number of pipeline stages before Entry (Specify=0 …
	// Validate=5): the proxy for model quota not spent. See the package comment.
	StagesSkipped int `json:"stages_skipped"`
	// Seconds is the test's elapsed seconds per cell, taken from the latest invocation
	// that ran it in that cell.
	Seconds map[string]float64 `json:"seconds,omitempty"`
	// Total is the sum of Seconds across cells.
	Total float64 `json:"total_seconds"`
}

// RuntimeReport is the full table plus its roll-ups.
type RuntimeReport struct {
	SHA  string       `json:"sha"`
	Rows []RuntimeRow `json:"rows"`
	// Total is the suite's summed runtime in seconds; ByEntry splits it by entry stage.
	Total   float64                    `json:"total_seconds"`
	ByEntry map[registry.Stage]float64 `json:"seconds_by_entry"`
	// StagesSkipped sums the proxy over every (test, cell) that ran.
	StagesSkipped int `json:"stages_skipped_total"`
	// Unmeasured lists registry tests with no archived timing.
	Unmeasured []string `json:"unmeasured,omitempty"`
}

// stagesBefore maps an entry stage to the number of pipeline stages before it.
func stagesBefore(s registry.Stage) int {
	for i, v := range []registry.Stage{
		registry.StageSpecify, registry.StageResearch, registry.StagePlan,
		registry.StageImplement, registry.StageReview, registry.StageValidate,
	} {
		if v == s {
			return i
		}
	}
	if s == registry.StageQueued { // the holding stage sits after Validate
		return 6
	}
	return 0 // "none" (files no item) or unrecorded
}

// BuildRuntimeReport reads <archiveDir>/<cell>/<invocation>/go-test.json for every cell and
// joins the timings with reg. Invocation directory names are sortable, so for each
// (cell, test) the latest invocation that ran it wins — a --resume pass supersedes an
// interrupted one.
func BuildRuntimeReport(sha, archiveDir string, reg *registry.Registry) (RuntimeReport, error) {
	byName := map[string]registry.Entry{}
	for _, e := range reg.Tests {
		byName[e.Name] = e
	}
	seconds := map[string]map[string]float64{} // test -> cell -> seconds

	cells, err := os.ReadDir(archiveDir)
	if err != nil && !os.IsNotExist(err) {
		return RuntimeReport{}, fmt.Errorf("reading the archive %s: %w", archiveDir, err)
	}
	for _, cell := range cells {
		if !cell.IsDir() {
			continue
		}
		invs, err := os.ReadDir(filepath.Join(archiveDir, cell.Name()))
		if err != nil {
			return RuntimeReport{}, fmt.Errorf("reading %s: %w", cell.Name(), err)
		}
		names := make([]string, 0, len(invs))
		for _, inv := range invs {
			if inv.IsDir() {
				names = append(names, inv.Name())
			}
		}
		sort.Strings(names) // oldest first, so a later invocation overwrites an earlier one
		for _, inv := range names {
			f, err := os.Open(filepath.Join(archiveDir, cell.Name(), inv, "go-test.json"))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return RuntimeReport{}, err
			}
			events, rerr := ReadEvents(f)
			f.Close()
			if rerr != nil {
				return RuntimeReport{}, fmt.Errorf("reading %s/%s/go-test.json: %w", cell.Name(), inv, rerr)
			}
			for _, t := range Timings(events) {
				if t.Result == "skip" {
					continue // a skipped test ran nothing
				}
				if seconds[t.Test] == nil {
					seconds[t.Test] = map[string]float64{}
				}
				seconds[t.Test][cell.Name()] = t.Elapsed
			}
		}
	}

	rep := RuntimeReport{SHA: sha, ByEntry: map[registry.Stage]float64{}}
	tests := append([]registry.Entry(nil), reg.Tests...)
	for name := range seconds {
		if _, known := byName[name]; !known { // archived but not in this registry: keep its time
			tests = append(tests, registry.Entry{Name: name})
		}
	}
	for _, e := range tests {
		row := RuntimeRow{
			Test: e.Name, Entry: e.EntryStage, Traversal: e.Traversal, FullTraversal: e.FullTraversal,
			StagesSkipped: stagesBefore(e.EntryStage), Seconds: seconds[e.Name],
		}
		for _, s := range row.Seconds {
			row.Total += s
			rep.StagesSkipped += row.StagesSkipped
		}
		if len(row.Seconds) == 0 {
			rep.Unmeasured = append(rep.Unmeasured, e.Name)
		}
		rep.Total += row.Total
		rep.ByEntry[e.EntryStage] += row.Total
		rep.Rows = append(rep.Rows, row)
	}
	sort.SliceStable(rep.Rows, func(i, j int) bool { return rep.Rows[i].Total > rep.Rows[j].Total })
	return rep, nil
}

// Format renders the table for a terminal.
func (r RuntimeReport) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "live suite runtime for %s (measured: go-test.json Elapsed per test per cell)\n\n", shortSHA(r.SHA))
	fmt.Fprintf(&b, "%-10s  %-9s  %-4s  %8s  %s\n", "entry", "traversal", "skip", "seconds", "test")
	for _, row := range r.Rows {
		mark := ""
		if row.FullTraversal {
			mark = "  [full traversal]"
		}
		fmt.Fprintf(&b, "%-10s  %-9s  %-4d  %8.0f  %s%s\n", row.Entry, row.Traversal, row.StagesSkipped, row.Total, row.Test, mark)
	}
	fmt.Fprintf(&b, "\ntotal: %.0fs (%.1f min) across %d test(s); %d registry test(s) have no archived timing\n",
		r.Total, r.Total/60, len(r.Rows)-len(r.Unmeasured), len(r.Unmeasured))
	var entries []string
	for e := range r.ByEntry {
		entries = append(entries, string(e))
	}
	sort.Strings(entries)
	for _, e := range entries {
		fmt.Fprintf(&b, "  entry %-10s %8.0fs\n", e, r.ByEntry[registry.Stage(e)])
	}
	fmt.Fprintf(&b, "\nmodel quota: not recorded by the ledger or the archive. Proxy: %d pipeline stage(s) not driven across the (test, cell) runs above\n"+
		"(entering at stage k skips k real Claude invocations plus their CI/review waits). Token figures live in the stage-comment footers on GitHub.\n", r.StagesSkipped)
	return b.String()
}

// hasEntries reports whether any row carries an entry stage: a baseline from before
// #1992 has none, and its stages-not-driven count would read as a spurious 0.
func (r RuntimeReport) hasEntries() bool {
	for _, row := range r.Rows {
		if row.Entry != "" {
			return true
		}
	}
	return false
}

func entryLabel(s registry.Stage) string {
	if s == "" {
		return "-"
	}
	return string(s)
}

// FormatComparison renders after against the baseline report: per-test and total
// runtime deltas (negative is faster) and the change in the stages-not-driven proxy.
func FormatComparison(before, after RuntimeReport) string {
	b4 := map[string]RuntimeRow{}
	for _, row := range before.Rows {
		b4[row.Test] = row
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runtime: %s (before) -> %s (after)\n\n", shortSHA(before.SHA), shortSHA(after.SHA))
	fmt.Fprintf(&b, "%-10s -> %-10s  %9s  %9s  %9s  %s\n", "entry", "entry", "before s", "after s", "delta s", "test")
	rows := append([]RuntimeRow(nil), after.Rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Total-b4[rows[i].Test].Total < rows[j].Total-b4[rows[j].Test].Total
	})
	for _, row := range rows {
		old := b4[row.Test]
		if old.Total == 0 && row.Total == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-10s -> %-10s  %9.0f  %9.0f  %+9.0f  %s\n", entryLabel(old.Entry), entryLabel(row.Entry), old.Total, row.Total, row.Total-old.Total, row.Test)
	}
	fmt.Fprintf(&b, "\ntotal: %.0fs -> %.0fs (%+.0fs, %+.1f min)\n", before.Total, after.Total, after.Total-before.Total, (after.Total-before.Total)/60)
	if before.hasEntries() {
		fmt.Fprintf(&b, "stages not driven (model-quota proxy): %d -> %d\n", before.StagesSkipped, after.StagesSkipped)
	} else {
		fmt.Fprintf(&b, "stages not driven (model-quota proxy): %d after; not recorded for the baseline (its registry has no entry stages)\n", after.StagesSkipped)
	}
	return b.String()
}

// ReportCmd is `gate report`.
func (g *Gate) ReportCmd(ctx context.Context, argv []string) int {
	sha, baseline, format := "", "", "text"
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		take := func(flag string) (string, bool) {
			if a == flag && i+1 < len(argv) {
				i++
				return argv[i], true
			}
			if strings.HasPrefix(a, flag+"=") {
				return strings.TrimPrefix(a, flag+"="), true
			}
			return "", false
		}
		if v, ok := take("--sha"); ok {
			sha = v
		} else if v, ok := take("--baseline"); ok {
			baseline = v
		} else if v, ok := take("--format"); ok {
			format = v
		} else {
			g.errf("gate report: unknown argument %q (usage: report [--sha <sha>] [--baseline <sha>] [--format text|json])\n", a)
			return ExitUsage
		}
	}
	if format != "text" && format != "json" {
		g.errf("gate report: unknown --format %q (want text or json)\n", format)
		return ExitUsage
	}
	if g.Cfg.CoverageDir == "" {
		g.errln("gate report: the coverage ledger is disabled (E2E_COVERAGE_DIR resolved to nothing)")
		return ExitUsage
	}
	sha, code := g.resolveSHA(ctx, "report", sha)
	if code != 0 {
		return code
	}
	reg, err := registry.Load()
	if err != nil {
		g.errf("gate report: %v\n", err)
		return ExitPreflightFailed
	}
	build := func(s string, reg *registry.Registry) (RuntimeReport, bool) {
		l := &Ledger{Root: g.Cfg.CoverageDir, SHA: s, now: g.Now}
		if _, err := os.Stat(l.ArchiveDir()); err != nil {
			g.errf("gate report: no archive for %s under %s (run the gate first, or pass --sha)\n", shortSHA(s), g.Cfg.CoverageDir)
			return RuntimeReport{}, false
		}
		rep, err := BuildRuntimeReport(s, l.ArchiveDir(), reg)
		if err != nil {
			g.errf("gate report: %v\n", err)
			return RuntimeReport{}, false
		}
		return rep, true
	}
	after, ok := build(sha, reg)
	if !ok {
		return ExitPreflightFailed
	}
	if baseline == "" {
		if format == "json" {
			return g.printJSON(after)
		}
		g.outln(after.Format())
		return 0
	}
	baseline, code = g.resolveSHA(ctx, "report", baseline)
	if code != 0 {
		return code
	}
	// The baseline's entry stages are those of ITS registry, not today's: joining its
	// timings with the current registry would show every test at its new entry and
	// make the before/after comparison vacuous.
	before, ok := build(baseline, g.registryAt(ctx, baseline))
	if !ok {
		return ExitPreflightFailed
	}
	if format == "json" {
		return g.printJSON(map[string]RuntimeReport{"before": before, "after": after})
	}
	g.outln(FormatComparison(before, after))
	return 0
}

func (g *Gate) printJSON(v any) int {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		g.errf("gate report: %v\n", err)
		return 1
	}
	g.outln(string(data))
	return 0
}

// registryAt loads tests/e2e/registry/registry.json as of sha. A commit that predates the
// registry, or whose registry cannot be decoded, yields an empty one: its tests then
// report no entry stage ("-") rather than borrowing today's.
func (g *Gate) registryAt(ctx context.Context, sha string) *registry.Registry {
	so, _, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"show", sha + ":tests/e2e/registry/registry.json"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
	if res.ExitCode == 0 {
		if reg, err := registry.Decode([]byte(so)); err == nil {
			return reg
		}
	}
	return &registry.Registry{}
}

// resolveSHA turns an explicit sha, or E2E_BED_REF (default origin/main), into a full
// commit SHA. On failure it returns the exit code to use.
func (g *Gate) resolveSHA(ctx context.Context, cmd, sha string) (string, int) {
	ref := sha
	if ref == "" {
		ref = orDefault(g.Getenv("E2E_BED_REF"), "origin/main")
	}
	if len(ref) == 40 && strings.Trim(ref, "0123456789abcdef") == "" {
		return ref, 0
	}
	so, se, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"rev-parse", "--verify", ref + "^{commit}"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
	if res.ExitCode != 0 {
		g.errf("gate %s: cannot resolve %s: %s\n", cmd, ref, strings.TrimSpace(se))
		return "", ExitPreflightFailed
	}
	return strings.TrimSpace(so), 0
}
