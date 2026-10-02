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

// loadCoverageInputs scans tests/e2e for the live set (#1933's rule), hashes
// every test (R3) and reads the registry for skip_ok_legs.
func (g *Gate) loadCoverageInputs() (*covInputs, error) {
	e2eDir := filepath.Join(g.Cfg.RepoRoot, "tests", "e2e")
	live, err := registry.ScanLiveTests(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("scanning the live tests: %w", err)
	}
	hashes, err := HashTests(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("hashing the live tests: %w", err)
	}
	data, err := os.ReadFile(g.registryPath())
	if err != nil {
		return nil, fmt.Errorf("reading the registry: %w", err)
	}
	reg, err := registry.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decoding the registry: %w", err)
	}
	in := &covInputs{live: live, liveSet: map[string]bool{}, hashes: hashes, entries: map[string]registry.Entry{}}
	for _, n := range live {
		in.liveSet[n] = true
	}
	for _, e := range reg.Tests {
		in.entries[e.Name] = e
	}
	return in, nil
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
	if err := writeFileAtomic(g.pregatePath(head), mustJSON(pregateRecord{V: LedgerVersion, Head: head, TS: g.Now().UTC().Format(time.RFC3339)})); err != nil {
		g.errf("warning: recording the pre-gate pass: %v\n", err)
	}
}

// pregateLine is the informational pre-gate status for the coverage summary.
func (g *Gate) pregateLine(ctx context.Context) string {
	head, ok := g.pregateRecorded(ctx)
	switch {
	case head == "":
		return ""
	case ok:
		return fmt.Sprintf("pre-gate (sim + wire-contract): recorded as passed for HEAD %s", shortSHA(head))
	default:
		return fmt.Sprintf("pre-gate (sim + wire-contract): no clean-tree pass on record for HEAD %s", shortSHA(head))
	}
}
