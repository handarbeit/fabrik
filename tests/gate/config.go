// Package gate is the Go port of scripts/e2e/run.sh and scripts/e2e/reset.sh —
// the orchestrator of the live e2e release gate (#1994, ADR-1994).
//
// It compiles WITHOUT the e2e build tag, so `go test -race ./...` builds and
// tests it on every PR, while the live tests it drives (tests/e2e) stay
// tag-gated. It lives at tests/gate rather than under tests/e2e on purpose: the
// live legs run `go test -tags=e2e ./tests/e2e/...`, and anything beneath that
// pattern would be swept into every leg and its -json outcome report.
//
// The port is behaviour-preserving. tests/gate/README.md maps every run.sh
// function to its Go home and every bash test case to its Go counterpart.
//
// Structure (the seams the follow-on chain #1972–#1976 plugs into):
//
//   - Cell / Scheduler (schedule.go): one leg of the gate and the thing that
//     yields and runs them. Today a serial auth × train loop.
//   - Event / Classification / LegResult (events.go, leg.go): a typed outcome
//     stream decoded from `go test -json`. Classify is where an INCONCLUSIVE
//     outcome is added; Gate.OnLeg is where a ledger observes each leg.
//   - Preflight (gate.go): an ordered probe list that runs before any live spend.
//   - Commander (exec.go): every subprocess goes through it, so tests inject
//     stubs instead of needing a bed, a network or a `gh` login.
package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Exit codes. scripts/cut-release.sh's interpret_e2e_exit_code keys on these;
// they are a contract, not an implementation detail.
const (
	ExitUsage              = 2 // usage error (unchanged from bash's generic shell code)
	ExitBudgetExhausted    = 3 // GraphQL rate-limit backoff engaged mid-run: RUN INVALID
	ExitPreflightFailed    = 4 // the suite never started; a bed-state problem
	ExitPregateFailed      = 5 // sim suite or github wire-contract tests failed
	ExitPostSuiteWatchdog  = 6 // go test exited but the post-suite tail stalled (#1676)
	ExitPreconditionFailed = 7 // competing token consumer / Pruefer down / auth-mode precondition
	// ExitCoverageIncomplete (#1972): every leg this invocation ran passed, but the
	// ledger still lacks a valid PASS for some required (test, leg) pair — or the
	// coverage check run by `gate coverage` found the gate not yet accepted.
	ExitCoverageIncomplete = 8
)

// ExitError carries the process exit code a failed step wants.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("exit %d", e.Code)
}

func exitErr(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// TrainIsolatedRE names the scenarios that deliberately exhaust a repo's
// merge-train state and so cannot share that repo with anything else under
// "on" (see the long comment this replaced in run.sh: the runaway-guard scenario
// poisons RepoBeta for an hour, which TestCrossRepoSpawn would inherit).
const TrainIsolatedRE = "TestMergeTrainRunawayGuardPausesBatch"

// The E2E_MATRIX modes (#1975, ADR-1975). Sparse is the default: one baseline
// cell runs every live test and each other cell runs only the tests sensitive to
// what that cell changes. Full restores the four complete auth × train legs.
const (
	MatrixSparse = "sparse"
	MatrixFull   = "full"
)

// Config is everything the gate reads from its environment, resolved once.
// Numeric-looking knobs that are only ever passed through to `go test`
// (-timeout, -parallel) stay strings, exactly as the bash passed them.
type Config struct {
	RepoRoot   string
	TestBed    string // FABRIK_TEST_DIR, default $HOME/dev/fabrik-test
	EngineLog  string // $TestBed/.fabrik/fabrik.log
	BedToken   string // FABRIK_TOKEN from $TestBed/.env; "" when unreadable
	PrueferDir string

	Timeout    string // E2E_TIMEOUT, default 4h (a go duration string, passed through)
	Parallel   string // E2E_PARALLEL, default 4
	ParallelOn string // E2E_PARALLEL_ON, default 2

	BedPollSeconds string // E2E_BED_POLL_SECONDS, default 60

	// Matrix is E2E_MATRIX: MatrixSparse (default via LoadConfig) or MatrixFull.
	// The zero value means full, so a Config built by hand (the tests') keeps the
	// pre-#1975 plan shapes.
	Matrix string

	GHAPITimeout          time.Duration // E2E_GH_API_TIMEOUT (secs), default 30s
	PostSuiteDrainTimeout time.Duration // E2E_POST_SUITE_DRAIN_TIMEOUT (secs), default 30s
	PostSuiteWatchdog     time.Duration // E2E_POST_SUITE_WATCHDOG (secs), default 300s
	StallWarn             time.Duration // E2E_STALL_WARN_MINUTES, default 15m

	// Fixed behaviour exposed as fields only so tests can shrink the waits.
	StallCheckInterval time.Duration // how often the stall detector looks (60s)
	BannerWait         time.Duration // bed startup-banner wait (90s)
	StopWait           time.Duration // wait for a SIGTERMed bed to exit (60s)
	KillGrace          time.Duration // SIGTERM → SIGKILL grace for a reaped child (10s)

	TmpDir string // $TMPDIR, default /tmp

	// The per-SHA coverage ledger (#1972, ADR-1972).
	CoverageDir        string        // E2E_COVERAGE_DIR, default <RepoRoot>/.e2e-coverage ("" disables the ledger)
	CoverageKeepSHAs   int           // E2E_COVERAGE_KEEP_SHAS: SHAs whose bulky archive logs are kept (default 5)
	IssueRepo          string        // E2E_ISSUE_REPO: where a skip's cited #N lives (default handarbeit/fabrik)
	ArchiveLogInterval time.Duration // how often the engine log is sampled during a leg (10s)

	// The INCONCLUSIVE outcome (#1973, ADR-1973).
	InconclusiveRetries int // E2E_INCONCLUSIVE_RETRIES: in-leg re-runs of the inconclusive tests (default 2; 0 disables)
	InconclusiveWarn    int // E2E_INCONCLUSIVE_WARN: a leg with MORE inconclusives than this warns, pointing at #1974 (default 3)

	// The environment probes (#1974, ADR-1974). A zero threshold disables that
	// probe, so a Config built by hand (the tests') probes nothing.
	LagProbeThreshold  time.Duration // E2E_LAG_PROBE_THRESHOLD (secs): board-add→listing lag above which preflight waits (default 30s)
	LoadProbeFactor    float64       // E2E_LOAD_PROBE_FACTOR: 1-minute load threshold as a multiple of the CPU count (default 2)
	LoadProbeThreshold float64       // E2E_LOAD_PROBE_THRESHOLD: an absolute 1-minute load threshold, overriding the factor (0 = unset)
	ProbeWaitMax       time.Duration // E2E_PROBE_WAIT_MAX (secs): bound on each probe's wait-and-re-probe loop (default 10m)
	ProbeInterval      time.Duration // E2E_PROBE_INTERVAL (secs): pause between re-probes (default 30s)
	LagPollInterval    time.Duration // how often the lag probe re-reads the board listing within one measurement (3s)
}

// LoadConfig resolves a Config from getenv. repoRoot is the git toplevel the
// gate was started in. It does NOT read the bed's .env; see LoadBedToken.
func LoadConfig(getenv func(string) string, repoRoot string) (Config, error) {
	home := getenv("HOME")
	testBed := getenv("FABRIK_TEST_DIR")
	if testBed == "" {
		testBed = filepath.Join(home, "dev", "fabrik-test")
	}
	c := Config{
		RepoRoot:       repoRoot,
		TestBed:        testBed,
		EngineLog:      filepath.Join(testBed, ".fabrik", "fabrik.log"),
		PrueferDir:     orDefault(getenv("PRUEFER_DIR"), filepath.Join(home, "dev", "fabrik")),
		Timeout:        orDefault(getenv("E2E_TIMEOUT"), "4h"),
		Parallel:       orDefault(getenv("E2E_PARALLEL"), "4"),
		ParallelOn:     orDefault(getenv("E2E_PARALLEL_ON"), "2"),
		BedPollSeconds: orDefault(getenv("E2E_BED_POLL_SECONDS"), "60"),

		StallCheckInterval: 60 * time.Second,
		BannerWait:         90 * time.Second,
		StopWait:           60 * time.Second,
		KillGrace:          10 * time.Second,
		TmpDir:             orDefault(getenv("TMPDIR"), "/tmp"),

		CoverageDir:        orDefault(getenv("E2E_COVERAGE_DIR"), filepath.Join(repoRoot, ".e2e-coverage")),
		CoverageKeepSHAs:   5,
		IssueRepo:          orDefault(getenv("E2E_ISSUE_REPO"), "handarbeit/fabrik"),
		ArchiveLogInterval: 10 * time.Second,

		InconclusiveRetries: 2,
		InconclusiveWarn:    3,

		LoadProbeFactor: 2,
		LagPollInterval: 3 * time.Second,
	}
	var err error
	if c.Matrix, err = parseMatrix(getenv("E2E_MATRIX")); err != nil {
		return c, err
	}
	if c.LagProbeThreshold, err = secsEnv(getenv, "E2E_LAG_PROBE_THRESHOLD", 30); err != nil {
		return c, err
	}
	if c.ProbeWaitMax, err = secsEnv(getenv, "E2E_PROBE_WAIT_MAX", 600); err != nil {
		return c, err
	}
	if c.ProbeInterval, err = secsEnv(getenv, "E2E_PROBE_INTERVAL", 30); err != nil {
		return c, err
	}
	if c.LoadProbeFactor, err = floatEnv(getenv, "E2E_LOAD_PROBE_FACTOR", c.LoadProbeFactor); err != nil {
		return c, err
	}
	if c.LoadProbeThreshold, err = floatEnv(getenv, "E2E_LOAD_PROBE_THRESHOLD", 0); err != nil {
		return c, err
	}
	if c.InconclusiveRetries, err = countEnv(getenv, "E2E_INCONCLUSIVE_RETRIES", c.InconclusiveRetries); err != nil {
		return c, err
	}
	if c.InconclusiveWarn, err = countEnv(getenv, "E2E_INCONCLUSIVE_WARN", c.InconclusiveWarn); err != nil {
		return c, err
	}
	if c.GHAPITimeout, err = secsEnv(getenv, "E2E_GH_API_TIMEOUT", 30); err != nil {
		return c, err
	}
	if c.PostSuiteDrainTimeout, err = secsEnv(getenv, "E2E_POST_SUITE_DRAIN_TIMEOUT", 30); err != nil {
		return c, err
	}
	if c.PostSuiteWatchdog, err = secsEnv(getenv, "E2E_POST_SUITE_WATCHDOG", 300); err != nil {
		return c, err
	}
	if v := strings.TrimSpace(getenv("E2E_COVERAGE_KEEP_SHAS")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			return c, fmt.Errorf("E2E_COVERAGE_KEEP_SHAS=%q is not a positive integer", v)
		}
		c.CoverageKeepSHAs = n
	}
	mins, err := secsEnv(getenv, "E2E_STALL_WARN_MINUTES", 15)
	if err != nil {
		return c, err
	}
	// secsEnv returned the integer as seconds; this knob is in minutes.
	c.StallWarn = mins * 60
	return c, nil
}

// parseMatrix resolves E2E_MATRIX. Unset or "sparse" is sparse, "full" is full;
// anything else is rejected rather than silently falling back to either.
func parseMatrix(v string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case "", MatrixSparse:
		return MatrixSparse, nil
	case MatrixFull:
		return MatrixFull, nil
	default:
		return "", fmt.Errorf("E2E_MATRIX=%q is invalid (must be %s, %s, or unset for %s)", v, MatrixSparse, MatrixFull, MatrixSparse)
	}
}

// sparseMatrix reports whether the sparse auth × train plan applies.
func (c Config) sparseMatrix() bool { return c.Matrix == MatrixSparse }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// countEnv parses an integer env var that must be a non-negative count; unset
// keeps def. Like the other tunables, garbage is a hard error, never a silent
// default.
func countEnv(getenv func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q is not a non-negative integer", key, v)
	}
	return n, nil
}

// floatEnv parses a non-negative decimal env var; unset keeps def. Garbage is a
// hard error, like the other tunables.
func floatEnv(getenv func(string) string, key string, def float64) (float64, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f != f {
		return 0, fmt.Errorf("%s=%q is not a non-negative number", key, v)
	}
	return f, nil
}

// secsEnv parses an integer env var as a count of seconds (bash did integer
// arithmetic on these, so a non-integer was a hard error there too).
func secsEnv(getenv func(string) string, key string, def int) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return time.Duration(def) * time.Second, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q is not a non-negative integer", key, v)
	}
	return time.Duration(n) * time.Second, nil
}

// EnvFileValue returns the value of the first KEY= line of an env file with no
// unquoting (bash: grep '^KEY=' | head -1 | cut -d= -f2-), or "" when the file
// or key is absent. It is what BED_TOKEN and the competing-consumer comparison
// use; contrast envFileLastValue.
func EnvFileValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line[len(prefix):]
		}
	}
	return ""
}

// envFileLastValue is the auth-mode variant (bash env_file_value): the LAST
// KEY= line, with one leading and one trailing quote character stripped.
func envFileLastValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	val := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			val = line[len(prefix):]
		}
	}
	if val != "" && (val[0] == '"' || val[0] == '\'') {
		val = val[1:]
	}
	if val != "" && (val[len(val)-1] == '"' || val[len(val)-1] == '\'') {
		val = val[:len(val)-1]
	}
	return val
}
