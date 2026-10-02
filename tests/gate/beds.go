package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Multi-bed orchestration (#1976, ADR-1976). Every piece of bed state the gate
// touches — the lock, bed-run.log, the isolated gitconfig, build/start/stop, the
// engine log the archive samples and the RUN INVALID scan reads, the board-lag
// probe, the reset — already derives from four sources: Cfg.TestBed,
// Cfg.EngineLog, Cfg.BedToken and resetConfig(). So a bed is a per-bed VIEW of
// the invocation's Gate (newBedGate) carrying its own values of those, and
// nothing below needs a bed parameter.
//
// With one bed (E2E_BEDS unset — the default) there are no views: the root Gate
// is the bed, beds() returns it alone, and the run is today's.

// Defaults for the bed's repo pair and board (scripts/e2e/reset.sh's, and the
// live harness's LoadEnv).
const (
	defaultRepoAlpha     = "handarbeit/fabrik-test-alpha"
	defaultRepoBeta      = "handarbeit/fabrik-test-beta"
	defaultProjectOwner  = "handarbeit"
	defaultProjectNumber = "2"
)

// BedSpec is one configured bed, resolved once per invocation.
type BedSpec struct {
	// Name is the bed's letter in E2E_BEDS order: "A" is the first entry.
	Name string
	// Dir is the bed directory (its FABRIK_TEST_DIR).
	Dir string
	// Token is the bed's .env FABRIK_TOKEN: the engine's PAT on pat legs and the
	// harness's token on every leg ("" when unreadable).
	Token string
	// AppInstallationID is the bed's .env E2E_APP_INSTALLATION_ID ("" when unset):
	// the key of the bed's App identity on app legs (D3).
	AppInstallationID string
	// Reset is the bed's repo pair and board.
	Reset ResetConfig
}

// bedName is the letter of the i-th bed (0 → "A").
func bedName(i int) string { return string(rune('A' + i)) }

// resolveBedSpec resolves one bed (D1). The repo pair and board come from the
// bed's own .env when set there, else from the gate's environment, else from the
// defaults — so a second bed carries its own board and repos in its own .env and
// no new file format is needed. The App installation is read from the bed's .env
// only: App credentials are bed-local (the harness reads them from there too).
func (g *Gate) resolveBedSpec(name, dir string) BedSpec {
	envFile := filepath.Join(dir, ".env")
	pick := func(key, def string) string {
		if v := envFileLastValue(envFile, key); v != "" {
			return v
		}
		return orDefault(g.Getenv(key), def)
	}
	return BedSpec{
		Name:              name,
		Dir:               dir,
		Token:             EnvFileValue(envFile, "FABRIK_TOKEN"),
		AppInstallationID: envFileLastValue(envFile, "E2E_APP_INSTALLATION_ID"),
		Reset: ResetConfig{
			Alpha:         pick("FABRIK_TEST_REPO_ALPHA", defaultRepoAlpha),
			Beta:          pick("FABRIK_TEST_REPO_BETA", defaultRepoBeta),
			ProjectOwner:  pick("FABRIK_TEST_PROJECT_OWNER", defaultProjectOwner),
			ProjectNumber: pick("FABRIK_TEST_PROJECT_NUMBER", defaultProjectNumber),
		},
	}
}

// beds is the gate's beds: the per-bed views of a multi-bed run, or the root
// Gate itself — the one bed — otherwise.
func (g *Gate) beds() []*Gate {
	if len(g.bedGates) > 0 {
		return g.bedGates
	}
	return []*Gate{g}
}

// multiBed reports whether two or more beds are configured.
func (g *Gate) multiBed() bool { return len(g.bedGates) > 1 }

// root is the invocation's root Gate: the view's parent, or g itself.
func (g *Gate) root() *Gate {
	if g.parent != nil {
		return g.parent
	}
	return g
}

// bedLabel names the bed in messages: "bed B (/path)", or the directory alone
// on a single-bed run.
func (g *Gate) bedLabel() string {
	if g.bed == nil || g.bed.Name == "" {
		return g.Cfg.TestBed
	}
	return fmt.Sprintf("bed %s (%s)", g.bed.Name, g.bed.Dir)
}

// buildBedGates creates the per-bed views when two or more beds are configured.
// A no-op otherwise, and idempotent.
func (g *Gate) buildBedGates() {
	if len(g.bedGates) > 0 || len(g.Cfg.BedDirs) < 2 {
		return
	}
	outLock, errLock := &sync.Mutex{}, &sync.Mutex{}
	if g.Out == g.Err {
		errLock = outLock
	}
	for i, dir := range g.Cfg.BedDirs {
		g.bedGates = append(g.bedGates, g.newBedGate(g.resolveBedSpec(bedName(i), dir), outLock, errLock))
	}
}

// newBedGate is the per-bed view of g. Every field is copied EXPLICITLY — never
// `*g`, which would copy the mutex and every piece of per-invocation state — and
// a field added to Gate must be classified here: per-bed (set from spec), shared
// read-only (copied), or shared mutable (reached through parent, like the
// INCONCLUSIVE list, or through a pointer, like cov, which propagateToBeds sets
// once the ledger is open).
func (g *Gate) newBedGate(spec BedSpec, outLock, errLock *sync.Mutex) *Gate {
	cfg := g.Cfg
	cfg.TestBed = spec.Dir
	cfg.EngineLog = filepath.Join(spec.Dir, ".fabrik", "fabrik.log")
	cfg.BedToken = spec.Token
	prefix := "[bed " + spec.Name + "] "
	b := &Gate{
		Cfg:           cfg,
		Out:           &prefixWriter{mu: outLock, w: g.Out, prefix: prefix},
		Err:           &prefixWriter{mu: errLock, w: g.Err, prefix: prefix},
		Exec:          g.Exec,
		Env:           g.Env,
		OnLeg:         g.OnLeg,
		Sleep:         g.Sleep,
		Now:           g.Now,
		ProcCwd:       g.ProcCwd,
		Self:          g.Self,
		CPUs:          g.CPUs,
		LoadAvg:       g.LoadAvg,
		Identity:      g.Identity,
		schedWaitHook: g.schedWaitHook,
		parent:        g,
		bed:           &spec,
	}
	return b
}

// propagateToBeds hands the views what the root learned after they were built:
// the host-load probe's reading, the coverage ledger and --resume.
func (g *Gate) propagateToBeds() {
	for _, b := range g.bedGates {
		b.probes.Load = g.probes.Load
		b.cov = g.cov
		b.resume = g.resume
	}
}

// bedEnv is what a multi-bed leg's `go test` invocations must see to aim the
// live harness at its own bed (D2): the bed directory, its repo pair and its
// board. On a single-bed run the legs inherit the gate's environment unchanged
// (nil) — unless the one bed was named by E2E_BEDS rather than FABRIK_TEST_DIR,
// when the harness must be told which directory that is.
func (g *Gate) bedEnv() []string {
	if g.bed == nil {
		if g.Getenv("E2E_BEDS") != "" {
			return []string{"FABRIK_TEST_DIR=" + g.Cfg.TestBed}
		}
		return nil
	}
	rc := g.bed.Reset
	return []string{
		"FABRIK_TEST_DIR=" + g.bed.Dir,
		"FABRIK_TEST_REPO_ALPHA=" + rc.Alpha,
		"FABRIK_TEST_REPO_BETA=" + rc.Beta,
		"FABRIK_TEST_PROJECT_OWNER=" + rc.ProjectOwner,
		"FABRIK_TEST_PROJECT_NUMBER=" + rc.ProjectNumber,
	}
}

// stopIdleEngine stops a bed's engine while the bed has no leg running — while it
// waits for an identity, and once it has no more cells. A running engine polls
// with its identity whether or not a leg is using it, so an idle bed's engine
// would spend the budget of an identity another bed's leg holds (the #1684 shape,
// between two beds). Every leg restarts its bed (TestSwitchTrainMode starts a
// stopped bed), so nothing is lost. A failure to stop only warns.
func (g *Gate) stopIdleEngine(why string) {
	if !bedEngineRunning(g.Cfg.TestBed) {
		return
	}
	g.outf("== stopping this bed's engine while %s, so it spends no identity's budget ==\n", why)
	if err := g.StopBedInstance(context.Background()); err != nil {
		g.errf("warning: could not stop the idle bed engine: %v\n", err)
	}
}

// flushOutput writes out a partial final line a bed's prefixed writers still
// hold. A no-op for an unprefixed (single-bed) Gate.
func (g *Gate) flushOutput() {
	for _, w := range []io.Writer{g.Out, g.Err} {
		if p, ok := w.(*prefixWriter); ok {
			p.Flush()
		}
	}
}

// prefixWriter prefixes every line written to w with the bed's name (D12), so
// two beds' concurrent output stays attributable. It buffers a partial line and
// writes each complete one in a single Write under mu, which is SHARED by every
// prefixWriter onto the same underlying stream — two beds therefore never
// interleave within a line.
type prefixWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := make([]byte, 0, len(p.prefix)+i+1)
		line = append(append(line, p.prefix...), p.buf[:i+1]...)
		p.buf = p.buf[i+1:]
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
	}
}

// Flush writes a buffered partial line, newline-terminated.
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) == 0 {
		return
	}
	line := append(append([]byte(p.prefix), p.buf...), '\n')
	p.buf = nil
	_, _ = p.w.Write(line) // best-effort: the stream is the terminal
}

// CheckBedTopology refuses (ExitPreconditionFailed), before any live spend, a
// multi-bed configuration that can never run safely:
//
//   - a bed with no readable FABRIK_TOKEN has no identity to schedule on;
//   - two beds with the same App installation (when an app leg is planned): an
//     App installs once per org, so both beds would spend one budget (R3);
//   - two beds on the same board, or sharing a repo: each bed's reset and
//     scenarios would drain and race the other's state (D9).
//
// Two beds sharing a harness login are NOT refused: their overlapping cells
// serialize on that identity instead (D4). A no-op with one bed.
func (g *Gate) CheckBedTopology(modes []string) error {
	if !g.multiBed() {
		return nil
	}
	var problems []string
	apps := map[string]*Gate{}
	boards := map[string]*Gate{}
	repos := map[string]*Gate{}
	for _, b := range g.bedGates {
		s := b.bed
		if s.Token == "" {
			problems = append(problems, fmt.Sprintf("%s: FABRIK_TOKEN is not readable from %s/.env — the bed has no identity to schedule on", b.bedLabel(), s.Dir))
		}
		if id := s.AppInstallationID; id != "" && authModesInclude(modes, "app") {
			if o, dup := apps[id]; dup {
				problems = append(problems, fmt.Sprintf("%s and %s are configured with the same GitHub App installation %s — an App installs once per org, so their app legs could never run concurrently; give each bed its own App", o.bedLabel(), b.bedLabel(), id))
			} else {
				apps[id] = b
			}
		}
		board := strings.ToLower(s.Reset.ProjectOwner) + "/#" + s.Reset.ProjectNumber
		if o, dup := boards[board]; dup {
			problems = append(problems, fmt.Sprintf("%s and %s use the same board %s/#%s — each bed's reset would drain the other's items", o.bedLabel(), b.bedLabel(), s.Reset.ProjectOwner, s.Reset.ProjectNumber))
		} else {
			boards[board] = b
		}
		for _, r := range []string{s.Reset.Alpha, s.Reset.Beta} {
			key := strings.ToLower(r)
			if o, dup := repos[key]; dup && o != b {
				problems = append(problems, fmt.Sprintf("%s and %s share the repo %s — each bed's reset would close the other's issues and PRs", o.bedLabel(), b.bedLabel(), r))
			} else if !dup {
				repos[key] = b
			}
		}
	}
	if len(problems) > 0 {
		var sb strings.Builder
		sb.WriteString("\n############################################################\n")
		fmt.Fprintf(&sb, "## PRECONDITION FAILED: the %d configured beds (E2E_BEDS) cannot run together (#1976)\n", len(g.bedGates))
		sb.WriteString("##\n")
		for _, p := range problems {
			fmt.Fprintf(&sb, "##   %s\n", p)
		}
		sb.WriteString("##\n")
		sb.WriteString("## Each bed needs its own token, board and repos (and its own App for\n")
		sb.WriteString("## app legs) in its own .env — see tests/e2e/README.md.\n")
		sb.WriteString("############################################################")
		return &ExitError{Code: ExitPreconditionFailed, Msg: sb.String()}
	}
	parts := make([]string, len(g.bedGates))
	for i, b := range g.bedGates {
		parts[i] = b.bed.Name + "=" + b.bed.Dir
	}
	g.outf("== beds (E2E_BEDS): %s ==\n", strings.Join(parts, ", "))
	return nil
}

// prepareBeds is PrepareBedAndReset for every bed, serially so each bed's
// preflight reads as one block, followed by D11's refusal of beds that would run
// different engine SHAs (the ledger is per SHA). It returns the combined
// preflight output, archived with every leg.
func (g *Gate) prepareBeds(ctx context.Context, clean bool) (string, error) {
	var all strings.Builder
	for _, b := range g.bedGates {
		text, err := b.teeOutput(func() error { return b.PrepareBedAndReset(ctx, clean) })
		all.WriteString(text)
		b.flushOutput()
		if err != nil {
			return all.String(), err
		}
	}
	shas := map[string][]string{}
	for _, b := range g.bedGates {
		sha := b.resolveEngineSHA(ctx)
		shas[sha] = append(shas[sha], b.bed.Name)
	}
	if len(shas) > 1 {
		var parts []string
		for sha, names := range shas {
			shown := shortSHA(sha)
			if shown == "" {
				shown = "(unresolvable)"
			}
			parts = append(parts, fmt.Sprintf("bed %s at %s", strings.Join(names, ", "), shown))
		}
		sort.Strings(parts)
		return all.String(), exitErr(ExitPreconditionFailed, "PRECONDITION FAILED: the beds would run different engine SHAs (%s) — the coverage ledger is per SHA, so every bed must test the same engine. Point them at one ref (E2E_BED_REF) or update their checkouts (#1976, D11).", strings.Join(parts, "; "))
	}
	g.engineSHA = g.bedGates[0].resolveEngineSHA(ctx)
	return all.String(), nil
}
