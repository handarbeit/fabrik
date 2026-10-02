package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// covInputs is what the ledger needs to know about the live suite, computed
// BEFORE any live spend: a source file that cannot be parsed fails the run at
// the preflight stage, never mid-gate.
type covInputs struct {
	live    []string
	liveSet map[string]bool
	hashes  map[string]string
	entries map[string]registry.Entry
}

func (g *Gate) registryPath() string {
	if p := g.Getenv("E2E_PARITY_REGISTRY"); p != "" {
		return p
	}
	return filepath.Join(g.Cfg.RepoRoot, "tests", "e2e", "registry", "registry.json")
}

// loadSelection scans tests/e2e for the live set (#1933's rule) and reads the
// registry. It is what the sparse plan (#1975) needs, and it is loaded whether or
// not the ledger is enabled: the plan must not depend on E2E_COVERAGE_DIR.
func (g *Gate) loadSelection() (*SparseInput, error) {
	live, err := registry.ScanLiveTests(filepath.Join(g.Cfg.RepoRoot, "tests", "e2e"))
	if err != nil {
		return nil, fmt.Errorf("scanning the live tests: %w", err)
	}
	data, err := os.ReadFile(g.registryPath())
	if err != nil {
		return nil, fmt.Errorf("reading the registry: %w", err)
	}
	reg, err := registry.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decoding the registry: %w", err)
	}
	sel := &SparseInput{Live: live, Entries: map[string]registry.Entry{}}
	for _, e := range reg.Tests {
		sel.Entries[e.Name] = e
	}
	return sel, nil
}

// loadCoverageInputs is loadSelection plus the per-test source hashes (R3).
func (g *Gate) loadCoverageInputs() (*covInputs, error) {
	sel, err := g.loadSelection()
	if err != nil {
		return nil, err
	}
	hashes, err := HashTests(filepath.Join(g.Cfg.RepoRoot, "tests", "e2e"))
	if err != nil {
		return nil, fmt.Errorf("hashing the live tests: %w", err)
	}
	in := &covInputs{live: sel.Live, liveSet: map[string]bool{}, hashes: hashes, entries: sel.Entries}
	for _, n := range sel.Live {
		in.liveSet[n] = true
	}
	return in, nil
}

// selection is the sparse-plan input of already loaded coverage inputs.
func (in *covInputs) selection() *SparseInput {
	return &SparseInput{Live: in.live, Entries: in.entries}
}

// buildPlanInput is the ONE place a PlanInput is made, for both `gate run` and
// `gate coverage`, so the plan the gate runs and the required set the ledger
// checks can never be built from different inputs. sel is nil only under
// E2E_MATRIX=full, which plans the full 2×2.
func (g *Gate) buildPlanInput(modes []string, callerArgs []string, sel *SparseInput) PlanInput {
	p := PlanInput{
		AuthModes:    modes,
		TrainMode:    g.Getenv("E2E_TRAIN_MODE"),
		CallerHasRun: HasRunFlag(callerArgs),
		Parallel:     g.Cfg.Parallel,
		ParallelOn:   g.Cfg.ParallelOn,
		Args:         callerArgs,
	}
	if g.Cfg.sparseMatrix() {
		p.Sparse = sel
	}
	return p
}

// covState is one invocation's live ledger context, shared by every leg.
type covState struct {
	ledger     *Ledger
	inputs     *covInputs
	invocation string
	head       string
	preflight  string
	resume     bool
	noRecord   bool // the caller's -run/-skip narrows to subtests: recording would over-credit
	once       sync.Once
}

// newInvocationID is a sortable, collision-free name for one gate invocation.
func newInvocationID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func (g *Gate) repoHead(ctx context.Context) string {
	so, _, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"rev-parse", "HEAD"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
	if res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(so)
}

// resolveEngineSHA is the full SHA of the engine under test: what PreflightBed
// resolved the ref to, or — under E2E_SKIP_PREP, which skips preflight — the
// bed checkout's HEAD.
func (g *Gate) resolveEngineSHA(ctx context.Context) string {
	if g.engineSHA != "" {
		return g.engineSHA
	}
	so, _, res := g.bedGit(ctx, "rev-parse", "HEAD")
	if res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(so)
}

// openCoverage opens the ledger for the engine SHA under test, prints the R2
// drift verdict and prunes old archives. It returns nil (and says why) when the
// ledger cannot be used and the run is not --resume; --resume requires it.
func (g *Gate) openCoverage(ctx context.Context, in *covInputs, args RunArgs, callerArgs []string, preflight string) (*covState, error) {
	sha := g.resolveEngineSHA(ctx)
	if sha == "" {
		if args.Resume {
			return nil, exitErr(ExitPreflightFailed, "--resume: cannot determine the engine SHA under test (no preflight SHA and the bed HEAD does not resolve)")
		}
		g.errln("warning: cannot determine the engine SHA under test — this run is NOT recorded in the coverage ledger")
		return nil, nil
	}
	l, err := OpenLedger(g.Cfg.CoverageDir, sha, g.Now)
	if err != nil {
		return nil, exitErr(1, "gate: cannot open the coverage ledger under %s: %v", g.Cfg.CoverageDir, err)
	}
	cs := &covState{ledger: l, inputs: in, invocation: newInvocationID(g.Now()), head: g.repoHead(ctx),
		preflight: preflight, resume: args.Resume, noRecord: hasSubtestFilter(callerArgs)}
	if cs.noRecord {
		g.errln("warning: -run/-skip narrows to subtests — a subtest-filtered run executes only part of a test, so this run is NOT recorded in the coverage ledger")
	}
	d := g.DriftCheck(ctx, sha)
	g.outln(d.Report())
	if !d.Valid {
		g.errln("warning: the ledger certifies engine SHA " + shortSHA(sha) + ", not the tree being gated — coverage accepted from it is only as good as that SHA")
	}
	if rm := PruneArchives(g.Cfg.CoverageDir, g.Cfg.CoverageKeepSHAs, sha); len(rm) > 0 {
		g.outf("== pruned archived logs of %d old engine SHA(s) (E2E_COVERAGE_KEEP_SHAS=%d); outcome records kept ==\n", len(rm), g.Cfg.CoverageKeepSHAs)
	}
	return cs, nil
}

// teeOutput runs f with g.Out and g.Err also copied into the returned buffer —
// the invocation's preflight output, archived with every leg (R7).
func (g *Gate) teeOutput(f func() error) (string, error) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := &lockedWriter{mu: &mu, w: &buf}
	origOut, origErr := g.Out, g.Err
	g.Out, g.Err = io.MultiWriter(origOut, w), io.MultiWriter(origErr, w)
	defer func() { g.Out, g.Err = origOut, origErr }()
	err := f()
	mu.Lock()
	defer mu.Unlock()
	return buf.String(), err
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// coverageReport builds the report for the ledger's SHA over a required set.
func (g *Gate) coverageReport(ctx context.Context, l *Ledger, in *covInputs, legs []string, required map[string][]string, withDrift bool) *Report {
	ev := &Evaluator{Snap: l.Load(), Hashes: in.hashes, Entries: in.entries, States: g.newIssueStates()}
	r := BuildReport(ctx, ev, l.SHA, legs, required)
	r.Invocations = l.InvocationCount()
	r.Matrix, r.FullMatrixPairs = g.Cfg.Matrix, 4*len(in.live)
	if withDrift {
		d := g.DriftCheck(ctx, l.SHA)
		r.Drift = &d
	}
	return r
}

// ---- pre-gate record (R4: the pre-gate runs once per SHA and is recorded) ----

type pregateRecord struct {
	V    int    `json:"v"`
	Head string `json:"head"`
	TS   string `json:"ts"`
	// Retried / LoadAvg (#1973 R5): the pre-gate passed only after one retry of a
	// step that hit the known TSan fork/exec crash, on a host with this load.
	Retried bool   `json:"retried,omitempty"`
	LoadAvg string `json:"load_avg,omitempty"`
}

// pregateRetryNote is one crash retry of a pre-gate step (#1973 R5), appended to
// pregate/<head>.retries.jsonl whether or not the retry passed — a retry that
// fails again writes no pass record, so it needs a home of its own.
type pregateRetryNote struct {
	V         int    `json:"v"`
	Head      string `json:"head"`
	TS        string `json:"ts"`
	Step      string `json:"step"`
	Signature string `json:"signature"`
	LoadAvg   string `json:"load_avg"`
	Outcome   string `json:"outcome"` // "pass" | "fail" | "crashed again"
}

func (g *Gate) pregateRetriesPath(head string) string {
	return filepath.Join(g.Cfg.CoverageDir, "pregate", head+".retries.jsonl")
}

// recordPregateRetry appends the note. Unlike the pass record it needs no clean
// tree: it documents what happened, it does not vouch for anything. It is
// best-effort — the pre-gate runs before the ledger is open and a failure to
// write a note must never change its verdict.
func (g *Gate) recordPregateRetry(ctx context.Context, n pregateRetryNote) {
	if g.Cfg.CoverageDir == "" {
		return
	}
	head := g.repoHead(ctx)
	if head == "" {
		return
	}
	n.V, n.Head, n.TS = LedgerVersion, head, g.Now().UTC().Format(time.RFC3339)
	err := os.MkdirAll(filepath.Dir(g.pregateRetriesPath(head)), 0o755)
	if err == nil {
		err = appendLine(g.pregateRetriesPath(head), n)
	}
	if err != nil {
		g.errf("warning: recording the pre-gate crash retry: %v\n", err)
	}
}

func (g *Gate) pregatePath(head string) string {
	return filepath.Join(g.Cfg.CoverageDir, "pregate", head+".json")
}

// pregateRecorded: a pre-gate pass is on record for this exact HEAD and the
// working tree is clean (beyond FABRIK_PREGATE_ALLOWED_DIRTY_REGEX) — the same
// fail-closed bar as FABRIK_PREGATE_VERIFIED_SHA, never weaker.
func (g *Gate) pregateRecorded(ctx context.Context) (head string, ok bool) {
	if g.Cfg.CoverageDir == "" {
		return "", false
	}
	head = g.repoHead(ctx)
	if head == "" {
		return "", false
	}
	data, err := os.ReadFile(g.pregatePath(head))
	if err != nil {
		return head, false
	}
	var rec pregateRecord
	if json.Unmarshal(data, &rec) != nil || rec.V != LedgerVersion || rec.Head != head {
		return head, false
	}
	return head, len(g.dirtyLines(ctx)) == 0
}

// recordPregatePass writes the pass record — only for a clean tree.
func (g *Gate) recordPregatePass(ctx context.Context) {
	if g.Cfg.CoverageDir == "" {
		return
	}
	head := g.repoHead(ctx)
	if head == "" || len(g.dirtyLines(ctx)) != 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.pregatePath(head)), 0o755); err != nil {
		g.errf("warning: recording the pre-gate pass: %v\n", err)
		return
	}
	rec := pregateRecord{V: LedgerVersion, Head: head, TS: g.Now().UTC().Format(time.RFC3339)}
	if r := g.pregateRetry; r != nil && r.Outcome == "pass" {
		rec.Retried, rec.LoadAvg = true, r.LoadAvg
	}
	if err := writeFileAtomic(g.pregatePath(head), mustJSON(rec)); err != nil {
		g.errf("warning: recording the pre-gate pass: %v\n", err)
	}
}

func (g *Gate) readPregateRecord(head string) (pregateRecord, bool) {
	var rec pregateRecord
	data, err := os.ReadFile(g.pregatePath(head))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return rec, false
	}
	return rec, true
}

// pregateLine is the informational pre-gate status for the coverage summary.
func (g *Gate) pregateLine(ctx context.Context) string {
	head, ok := g.pregateRecorded(ctx)
	switch {
	case head == "":
		return ""
	case ok:
		line := fmt.Sprintf("pre-gate (sim + wire-contract): recorded as passed for HEAD %s", shortSHA(head))
		if rec, ok := g.readPregateRecord(head); ok && rec.Retried {
			line += fmt.Sprintf(" (after 1 TSan-crash retry, load average %s)", rec.LoadAvg)
		}
		return line
	default:
		return fmt.Sprintf("pre-gate (sim + wire-contract): no clean-tree pass on record for HEAD %s", shortSHA(head))
	}
}
