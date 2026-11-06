package gate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the reviewer-token budget guard (#2536, ADR-2536).

const (
	reviewerTok = "ghp_REVIEWERSECRET"
	t0          = "2026-11-05T12:00:00Z"
)

var epoch0, _ = time.Parse(time.RFC3339, t0)

// reviewerGate is a testGate whose bed .env names a reviewer token, with a fake
// identity op set, the minimums on and a controllable clock. The clock advances
// by whatever the guard sleeps.
func reviewerGate(t *testing.T, fi *fakeIdentity) (*Gate, *clock) {
	t.Helper()
	g, _, _, _ := testGate(t)
	mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nFABRIK_REVIEWER_TOKEN="+reviewerTok+"\n")
	g.Cfg.BedToken = "bed-token"
	g.Cfg.ReviewerToken = reviewerTok
	g.Cfg.ReviewerMinCore = 500
	g.Cfg.ReviewerMinGraphQL = 100
	g.Cfg.ReviewerHoldMax = 10 * time.Minute
	g.Cfg.ProbeInterval = 30 * time.Second
	g.Identity = fi
	ck := &clock{now: epoch0}
	g.Now = ck.Now
	g.Sleep = ck.Sleep
	return g, ck
}

type clock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	onTick func(*clock) // after each sleep
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	h := c.onTick
	c.mu.Unlock()
	if h != nil {
		h(c)
	}
}

func reviewerFake() *fakeIdentity {
	return &fakeIdentity{
		logins:     map[string]string{reviewerTok: "reviewer-login"},
		budgets:    map[string]int{reviewerTok: 4000},
		cores:      map[string]int{reviewerTok: 4900},
		coreResets: map[string]string{reviewerTok: "2026-11-05T12:40:00Z"},
	}
}

func (f *fakeIdentity) setCore(token string, n int) {
	f.mu.Lock()
	f.cores[token] = n
	f.mu.Unlock()
}

func noSecret(t *testing.T, label, text string) {
	t.Helper()
	if strings.Contains(text, reviewerTok) {
		t.Errorf("%s contains the reviewer token:\n%s", label, text)
	}
}

func TestReviewerConfigDefaultsAndOverrides(t *testing.T) {
	get := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	c, err := LoadConfig(get(nil), t.TempDir())
	if err != nil || c.ReviewerMinCore != 500 || c.ReviewerMinGraphQL != 100 || c.ReviewerHoldMax != 10*time.Minute {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_REVIEWER_MIN_CORE": "1200", "E2E_REVIEWER_MIN_GRAPHQL": "0", "E2E_REVIEWER_HOLD_MAX": "90"}), t.TempDir())
	if err != nil || c.ReviewerMinCore != 1200 || c.ReviewerMinGraphQL != 0 || c.ReviewerHoldMax != 90*time.Second {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	for _, k := range []string{"E2E_REVIEWER_MIN_CORE", "E2E_REVIEWER_MIN_GRAPHQL", "E2E_REVIEWER_HOLD_MAX"} {
		if _, err := LoadConfig(get(map[string]string{k: "lots"}), t.TempDir()); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=lots must be refused, got %v", k, err)
		}
	}
}

func TestEnvFileReviewerTokenUsesTheHarnessSemantics(t *testing.T) {
	p := t.TempDir() + "/.env"
	mustWrite(t, p, "# FABRIK_REVIEWER_TOKEN=commented\n\n  FABRIK_REVIEWER_TOKEN = \"quoted\"  \r\nFABRIK_REVIEWER_TOKEN=second\n")
	if got := EnvFileReviewerToken(p); got != "quoted" {
		t.Errorf("first line wins, trimmed and unquoted: %q", got)
	}
	mustWrite(t, p, "FABRIK_REVIEWER_TOKEN='it'\n")
	if got := EnvFileReviewerToken(p); got != "it" {
		t.Errorf("single quotes: %q", got)
	}
	if got := EnvFileReviewerToken(t.TempDir() + "/missing"); got != "" {
		t.Errorf("a missing file is empty: %q", got)
	}
	mustWrite(t, p, "FABRIK_TOKEN=x\n")
	if got := EnvFileReviewerToken(p); got != "" {
		t.Errorf("an absent key is empty: %q", got)
	}
}

func TestParseCoreBudget(t *testing.T) {
	rem, lim, reset, err := parseCoreBudget("5000 0 1793880000")
	if err != nil || rem != 0 || lim != 5000 || reset != time.Unix(1793880000, 0).UTC().Format(time.RFC3339) {
		t.Errorf("%d/%d %q %v", rem, lim, reset, err)
	}
	if _, _, reset, err := parseCoreBudget("5000 12"); err != nil || reset != "" {
		t.Errorf("a missing reset is unknown, not an error: %q %v", reset, err)
	}
	for _, bad := range []string{"", "null null null", "5000 -1 1", "5000"} {
		if _, _, _, err := parseCoreBudget(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestClassifyRateLimitErr(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"gh exited 1: API rate limit exceeded for user ID 82237", true},
		{"gh exited 1: GraphQL: API rate limit already exceeded for user ID 82237. (RATE_LIMITED)", true},
		{"gh exited 1: error (rate_limited)", true},
		{"gh exited 1: You have exceeded a secondary rate limit", true},
		{"gh exited 1: HTTP 502 Bad Gateway", false},
		{"gh exited 1: dial tcp: i/o timeout", false},
		{"gh api timed out after 30s", false},
	} {
		if got := classifyRateLimitErr(errors.New(tc.msg)); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.msg, got, tc.want)
		}
	}
}

func TestOSIdentityOpsCoreBudgetIsBoundedTokenScopedAndFree(t *testing.T) {
	g, fe, _, _ := testGate(t)
	fe.handler = func(_ context.Context, c Cmd) Result {
		if strings.HasPrefix(argsLine(c), "gh api rate_limit") {
			if envValue(c.Env, "GH_TOKEN") != reviewerTok {
				writeStderr(c, "HTTP 401: Bad credentials\n")
				return Result{ExitCode: 1}
			}
			writeStdout(c, "5000 0 1793880000\n")
		}
		return Result{}
	}
	ops := osIdentityOps{g: g}
	rem, lim, reset, err := ops.CoreBudget(context.Background(), reviewerTok)
	if err != nil || rem != 0 || lim != 5000 || reset == "" {
		t.Fatalf("%d/%d %q %v", rem, lim, reset, err)
	}
	if _, _, _, err := ops.CoreBudget(context.Background(), "other"); err == nil || strings.Contains(err.Error(), "other") {
		t.Errorf("a failure carries gh's text and never the token: %v", err)
	}
	for _, c := range fe.callsNamed("gh") {
		if !c.Session || c.Timeout != g.Cfg.GHAPITimeout || envValue(c.Env, "GH_TOKEN") == "" {
			t.Errorf("bounded, session-reaped, token-scoped: %+v", c)
		}
		if strings.Contains(argsLine(c), reviewerTok) {
			t.Errorf("the token must travel in GH_TOKEN, never argv: %s", argsLine(c))
		}
		if !strings.Contains(argsLine(c), "api rate_limit") {
			t.Errorf("core is read from the free /rate_limit endpoint: %s", argsLine(c))
		}
	}
	fe.handler = func(context.Context, Cmd) Result { return Result{ExitCode: 143, TimedOut: true} }
	if _, _, _, err := ops.CoreBudget(context.Background(), reviewerTok); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("a timeout is an error: %v", err)
	}
}

func TestReviewerTokensDeduplicateByToken(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	for _, b := range g.beds() {
		b.Cfg.ReviewerToken = "shared"
	}
	refs := g.reviewerTokens()
	if len(refs) != 1 || len(refs[0].beds) != 2 {
		t.Fatalf("beds sharing a token are one read: %+v", refs)
	}
	g.beds()[1].Cfg.ReviewerToken = "second"
	if refs = g.reviewerTokens(); len(refs) != 2 {
		t.Errorf("distinct tokens: %+v", refs)
	}
	g.beds()[0].Cfg.ReviewerToken = ""
	if refs = g.reviewerTokens(); len(refs) != 1 || refs[0].token != "second" {
		t.Errorf("a bed with none contributes nothing: %+v", refs)
	}
}

func TestCheckReviewerBudget(t *testing.T) {
	t.Run("core 0/5000 refuses with exit 7, naming the login and the reset, never the token", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0)
		g, _ := reviewerGate(t, fi)
		err := g.CheckReviewerBudget(context.Background())
		if exitCode(err) != ExitPreconditionFailed {
			t.Fatalf("got %v", err)
		}
		for _, want := range []string{"reviewer-login", "REST core budget 0/5000 resets 2026-11-05T12:40:00Z", "E2E_REVIEWER_MIN_CORE"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in:\n%s", want, err)
			}
		}
		noSecret(t, "refusal", err.Error())
	})
	t.Run("a healthy budget passes", func(t *testing.T) {
		g, _ := reviewerGate(t, reviewerFake())
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Fatal(err)
		}
		if out := g.Out.(*bytes.Buffer).String(); !strings.Contains(out, "reviewer budget ok: reviewer-login") {
			t.Errorf("out:\n%s", out)
		}
	})
	t.Run("a GraphQL budget below its minimum refuses the same way", func(t *testing.T) {
		fi := reviewerFake()
		fi.budgets[reviewerTok] = 40
		g, _ := reviewerGate(t, fi)
		err := g.CheckReviewerBudget(context.Background())
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "GraphQL budget 40/5000") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an exhausted GraphQL bucket, whose read is itself refused, counts as exhausted", func(t *testing.T) {
		fi := reviewerFake()
		fi.graphqlErr = map[string]error{reviewerTok: errors.New("gh exited 1: GraphQL: API rate limit exceeded for user ID 82237")}
		g, _ := reviewerGate(t, fi)
		err := g.CheckReviewerBudget(context.Background())
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "GraphQL budget 0/?") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an override replaces the default for its bucket; 0 disables it", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 800)
		g, _ := reviewerGate(t, fi)
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Fatalf("800 clears the default 500: %v", err)
		}
		g.Cfg.ReviewerMinCore = 1000
		if exitCode(g.CheckReviewerBudget(context.Background())) != ExitPreconditionFailed {
			t.Error("800 is below an override of 1000")
		}
		g.Cfg.ReviewerMinCore = 0
		fi.setCore(reviewerTok, 0)
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Errorf("a 0 minimum disables the bucket: %v", err)
		}
	})
	t.Run("an exhausted token cannot name itself: the bed and variable are named instead", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0)
		fi.loginErr = map[string]error{reviewerTok: errors.New("gh exited 1: API rate limit exceeded for user ID 82237")}
		g, _ := reviewerGate(t, fi)
		err := g.CheckReviewerBudget(context.Background())
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "FABRIK_REVIEWER_TOKEN (login unavailable") {
			t.Fatalf("got %v", err)
		}
		noSecret(t, "refusal", err.Error())
	})
	t.Run("beds sharing one reviewer token are read once", func(t *testing.T) {
		fi := reviewerFake()
		g, _, _, _ := twoBedGate(t)
		g.Identity = fi
		g.Cfg.ReviewerMinCore, g.Cfg.ReviewerMinGraphQL = 500, 100
		for _, b := range g.beds() {
			b.Cfg.ReviewerToken = reviewerTok
			b.Cfg.ReviewerMinCore, b.Cfg.ReviewerMinGraphQL = 500, 100
		}
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(fi.coreReads) != 1 || len(fi.probes) != 1 {
			t.Errorf("one read of each bucket, got core=%d graphql=%d", len(fi.coreReads), len(fi.probes))
		}
	})
	t.Run("an unreadable probe is reported and does not block", func(t *testing.T) {
		fi := reviewerFake()
		fi.coreErr = map[string]error{reviewerTok: errors.New("gh exited 1: HTTP 502 for " + reviewerTok)}
		fi.graphqlErr = map[string]error{reviewerTok: errors.New("gh timed out after 30s")}
		g, _ := reviewerGate(t, fi)
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Fatalf("an unreadable probe must not block: %v", err)
		}
		errOut := g.Err.(*bytes.Buffer).String()
		if !strings.Contains(errOut, "unavailable") || !strings.Contains(errOut, "REST core") || !strings.Contains(errOut, "GraphQL") {
			t.Errorf("both unreadable reads are reported:\n%s", errOut)
		}
		noSecret(t, "warnings", errOut+g.Out.(*bytes.Buffer).String())
	})
	t.Run("no reviewer token: nothing to check", func(t *testing.T) {
		fi := reviewerFake()
		g, _ := reviewerGate(t, fi)
		g.Cfg.ReviewerToken = ""
		if err := g.CheckReviewerBudget(context.Background()); err != nil || len(fi.coreReads) != 0 {
			t.Errorf("err=%v reads=%d", err, len(fi.coreReads))
		}
	})
	t.Run("non-vacuity: with the check disabled the gate proceeds on core 0/5000", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0)
		g, _ := reviewerGate(t, fi)
		g.reviewerBudgetCheckDisabledForTest = true
		if err := g.CheckReviewerBudget(context.Background()); err != nil {
			t.Errorf("the old behaviour proceeds: %v", err)
		}
		if len(fi.coreReads) != 0 {
			t.Errorf("a disabled check reads nothing")
		}
	})
}

// A gate whose reviewer token reads core 0/5000 exits 7 before the live
// preflights spend anything — and after only the free pre-gate.
func TestRunRefusesAnExhaustedReviewerTokenBeforeAnyLivePreflight(t *testing.T) {
	run := func(t *testing.T, disabled bool) (int, *fakeIdentity, *runFake, string) {
		rf := newRunFake()
		g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREP=1")
		mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nFABRIK_REVIEWER_TOKEN="+reviewerTok+"\n")
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0)
		fi.logins["bed-token"] = "bed-login"
		g.Identity = fi
		g.LivePreflights = DefaultLivePreflights()
		g.Cfg.ReviewerMinCore, g.Cfg.ReviewerMinGraphQL = 500, 100
		g.Cfg.ReviewerHoldMax = 0 // a leg-start hold would refuse anyway: only the preflight is under test
		g.reviewerBudgetCheckDisabledForTest = disabled
		code := g.Run(context.Background(), nil)
		return code, fi, rf, g.Err.(*bytes.Buffer).String()
	}
	code, fi, rf, errOut := run(t, false)
	if code != ExitPreconditionFailed {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"reviewer-login", "0/5000", "2026-11-05T12:40:00Z"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("missing %q in:\n%s", want, errOut)
		}
	}
	noSecret(t, "stderr", errOut)
	if strings.Contains(strings.Join(fi.resolves, ","), "bed-token") {
		t.Errorf("bed-identity ran before the reviewer check: resolves=%v", fi.resolves)
	}
	if strings.Contains(rf.milestones(), "bed:") || strings.Contains(rf.milestones(), "leg:") {
		t.Errorf("no bed work or leg may run: %s", rf.milestones())
	}

	// Non-vacuity: the same run with the guard disabled proceeds past it.
	code, fi, _, errOut = run(t, true)
	if code == ExitPreconditionFailed && strings.Contains(errOut, "REVIEWER_TOKEN") {
		t.Fatalf("a disabled guard must not refuse:\n%s", errOut)
	}
	if len(fi.coreReads) != 0 {
		t.Errorf("a disabled guard reads nothing")
	}
}

func TestReviewerCellGate(t *testing.T) {
	cell := Cell{Auth: "app", Train: "on"}
	ran := func(g *Gate) *int {
		n := new(int)
		g.runLegFn = func(context.Context, Cell) error { *n++; return nil }
		return n
	}
	t.Run("a healthy budget starts the cell, reading once", func(t *testing.T) {
		fi := reviewerFake()
		g, _ := reviewerGate(t, fi)
		n := ran(g)
		if err := g.runLeg(context.Background(), cell); err != nil || *n != 1 {
			t.Fatalf("err=%v ran=%d", err, *n)
		}
	})
	t.Run("below threshold with a reset inside the bound: held, logged, then started", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 10)
		fi.coreResets[reviewerTok] = "2026-11-05T12:05:00Z" // 5 min away, bound 10
		g, ck := reviewerGate(t, fi)
		ck.onTick = func(c *clock) {
			if !c.now.Before(epoch0.Add(5 * time.Minute)) {
				fi.setCore(reviewerTok, 5000)
			}
		}
		n := ran(g)
		if err := g.runLeg(context.Background(), cell); err != nil || *n != 1 {
			t.Fatalf("err=%v ran=%d", err, *n)
		}
		out := g.Out.(*bytes.Buffer).String()
		for _, want := range []string{"== holding app/on: reviewer reviewer-login REST core 10/5000", "reviewer budget recovered"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if len(ck.slept) == 0 {
			t.Error("the hold must sleep")
		}
		noSecret(t, "output", out)
	})
	t.Run("below threshold, reset beyond the bound: not started, budget-exhausted outcome, nothing recorded", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0) // resets 40 min out, bound 10
		g, ck := reviewerGate(t, fi)
		n := ran(g)
		err := g.runLeg(context.Background(), cell)
		var rbe *ReviewerBudgetError
		if !errors.As(err, &rbe) || exitCode(rbe.ExitError) != ExitBudgetExhausted {
			t.Fatalf("got %v", err)
		}
		if *n != 0 || len(ck.slept) != 0 {
			t.Errorf("the leg must not run nor wait: ran=%d slept=%v", *n, ck.slept)
		}
		noSecret(t, "error", err.Error())
		if !strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), "reviewer-login") {
			t.Errorf("the refusal names the login and the way back: %v", err)
		}
	})
	t.Run("a missing or past reset is not recovering", func(t *testing.T) {
		for _, reset := range []string{"", "2026-11-05T11:00:00Z"} {
			fi := reviewerFake()
			fi.setCore(reviewerTok, 0)
			fi.coreResets[reviewerTok] = reset
			g, _ := reviewerGate(t, fi)
			n := ran(g)
			var rbe *ReviewerBudgetError
			if err := g.runLeg(context.Background(), cell); !errors.As(err, &rbe) || *n != 0 {
				t.Errorf("reset %q: err=%v ran=%d", reset, err, *n)
			}
		}
	})
	t.Run("still short at the end of the bound: not started", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 10)
		fi.coreResets[reviewerTok] = "2026-11-05T12:09:00Z" // inside the bound, but never recovers here
		g, ck := reviewerGate(t, fi)
		n := ran(g)
		var rbe *ReviewerBudgetError
		if err := g.runLeg(context.Background(), cell); !errors.As(err, &rbe) || *n != 0 {
			t.Fatalf("err=%v ran=%d", err, *n)
		}
		if len(ck.slept) == 0 {
			t.Error("it should have held before giving up")
		}
	})
	t.Run("a budget that falls between cells holds the second one only", func(t *testing.T) {
		fi := reviewerFake()
		g, _ := reviewerGate(t, fi)
		n := ran(g)
		if err := g.runLeg(context.Background(), cell); err != nil {
			t.Fatal(err)
		}
		fi.setCore(reviewerTok, 0)
		var rbe *ReviewerBudgetError
		if err := g.runLeg(context.Background(), Cell{Auth: "app", Train: "off"}); !errors.As(err, &rbe) {
			t.Fatalf("got %v", err)
		}
		if *n != 1 {
			t.Errorf("the first cell ran, the second did not: %d", *n)
		}
	})
	t.Run("an unreadable probe is reported and the cell starts", func(t *testing.T) {
		fi := reviewerFake()
		fi.coreErr = map[string]error{reviewerTok: errors.New("gh exited 1: HTTP 502")}
		fi.graphqlErr = map[string]error{reviewerTok: errors.New("gh timed out after 30s")}
		g, _ := reviewerGate(t, fi)
		n := ran(g)
		if err := g.runLeg(context.Background(), cell); err != nil || *n != 1 {
			t.Fatalf("err=%v ran=%d", err, *n)
		}
		if !strings.Contains(g.Err.(*bytes.Buffer).String(), "unavailable") {
			t.Errorf("the unreadable read is reported:\n%s", g.Err.(*bytes.Buffer).String())
		}
	})
	t.Run("a cancelled context during a hold returns the cancellation", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 10)
		fi.coreResets[reviewerTok] = "2026-11-05T12:05:00Z"
		g, ck := reviewerGate(t, fi)
		ctx, cancel := context.WithCancel(context.Background())
		ck.onTick = func(*clock) { cancel() }
		n := ran(g)
		if err := g.runLeg(ctx, cell); !errors.Is(err, context.Canceled) || *n != 0 {
			t.Errorf("err=%v ran=%d", err, *n)
		}
	})
	t.Run("non-vacuity: with the check disabled the exhausted cell starts", func(t *testing.T) {
		fi := reviewerFake()
		fi.setCore(reviewerTok, 0)
		g, _ := reviewerGate(t, fi)
		g.reviewerBudgetCheckDisabledForTest = true
		n := ran(g)
		if err := g.runLeg(context.Background(), cell); err != nil || *n != 1 {
			t.Errorf("the old behaviour starts the cell: err=%v ran=%d", err, *n)
		}
	})
}

// The cell check sits before RunLeg, so a refused cell has written no ledger
// record, noted no invocation and restarted nothing: the real RunLeg is never
// reached. A leg that is already running is not interrupted by a later drop.
func TestReviewerCellGateNeverReachesRunLegAndNeverInterruptsOne(t *testing.T) {
	fi := reviewerFake()
	g, _ := reviewerGate(t, fi)
	var started, finished bool
	g.runLegFn = func(context.Context, Cell) error {
		started = true
		fi.setCore(reviewerTok, 0) // someone else drains the account mid-leg
		finished = true
		return nil
	}
	if err := g.runLeg(context.Background(), Cell{Auth: "app", Train: "on"}); err != nil || !started || !finished {
		t.Fatalf("a running leg completes despite the drop: err=%v", err)
	}
	// And the gate itself never reads the budget while the leg runs.
	if n := len(fi.coreReads); n != 1 {
		t.Errorf("one read before the cell, none during it: %d", n)
	}
}

func TestMultiBedReviewerBudgetShortfallDoesNotPoisonTheEngineIdentity(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	s := newMultiSched(h.g, sparseAll)
	s.finish(context.Background(), 0, &queued{cell: cAppOn}, nil, reviewerBudgetErr("reviewer token budget exhausted"))
	if len(s.exhausted) != 0 {
		t.Errorf("the engine identity is not exhausted by the reviewer's budget: %v", s.exhausted)
	}
	if !s.stopNew || exitCode(s.firstErr) != ExitBudgetExhausted {
		t.Errorf("stopNew=%v firstErr=%v", s.stopNew, s.firstErr)
	}
	if got := strings.Join(s.results[0], ";"); !strings.Contains(got, "not started (reviewer token budget exhausted") || strings.Contains(got, "voided") {
		t.Errorf("results: %s", got)
	}
}

// Through the real scheduler path: a bed whose reviewer account is drained
// declines its cell, records nothing, and the run ends with exit 3.
func TestMultiBedReviewerShortfallLeavesTheCellUnrun(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	fi := reviewerFake()
	fi.setCore(reviewerTok, 0)
	h.g.Identity = fi
	h.g.Cfg.ReviewerMinCore, h.g.Cfg.ReviewerMinGraphQL = 500, 100
	for _, b := range h.g.beds() {
		b.Cfg.ReviewerMinCore, b.Cfg.ReviewerMinGraphQL = 500, 100
		b.Cfg.ReviewerHoldMax = 0
	}
	h.g.beds()[0].Cfg.ReviewerToken = reviewerTok
	h.g.Now = time.Now
	for _, b := range h.g.beds() {
		b.Now = time.Now
	}
	_, done := h.run(context.Background(), []Cell{cAppOn})
	err := h.result(done)
	var rbe *ReviewerBudgetError
	if !errors.As(err, &rbe) || exitCode(err) != ExitBudgetExhausted {
		t.Fatalf("got %v", err)
	}
	if len(h.ran) != 0 {
		t.Errorf("the refused cell must not run: %v", h.ran)
	}
	if out := h.out(); !strings.Contains(out, "not started (reviewer token budget exhausted") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestLogReviewerBudget(t *testing.T) {
	t.Run("the start and end lines carry the reviewer as core plus graphql, and no token", func(t *testing.T) {
		fi := reviewerFake()
		fi.cores[reviewerTok] = 4321
		g, _ := reviewerGate(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "pat", Train: "off"}, "start")
		g.logIdentityBudget(context.Background(), Cell{Auth: "pat", Train: "off"}, "end")
		out := g.Out.(*bytes.Buffer).String()
		for _, want := range []string{
			"== reviewer budget (leg: pat/off, start): reviewer-login — core 4321/5000 resets 2026-11-05T12:40:00Z; graphql 4000/5000 resets 2026-10-02T10:15:00Z ==",
			"== reviewer budget (leg: pat/off, end): reviewer-login — core 4321/5000",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		noSecret(t, "budget lines", out+g.Err.(*bytes.Buffer).String())
	})
	t.Run("an unreadable bucket warns once and still prints the line", func(t *testing.T) {
		fi := reviewerFake()
		fi.coreErr = map[string]error{reviewerTok: errors.New("gh exited 1: HTTP 502")}
		g, _ := reviewerGate(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "pat", Train: "off"}, "start")
		if n := strings.Count(g.Err.(*bytes.Buffer).String(), "reading core failed"); n != 1 {
			t.Errorf("one warning, got %d:\n%s", n, g.Err.(*bytes.Buffer).String())
		}
		if !strings.Contains(g.Out.(*bytes.Buffer).String(), "core unavailable; graphql 4000/5000") {
			t.Errorf("out:\n%s", g.Out.(*bytes.Buffer).String())
		}
	})
	t.Run("non-vacuity: disabled, the old output has no reviewer line", func(t *testing.T) {
		g, _ := reviewerGate(t, reviewerFake())
		g.reviewerBudgetCheckDisabledForTest = true
		g.logIdentityBudget(context.Background(), Cell{Auth: "pat", Train: "off"}, "start")
		if strings.Contains(g.Out.(*bytes.Buffer).String(), "reviewer budget") {
			t.Errorf("out:\n%s", g.Out.(*bytes.Buffer).String())
		}
	})
}

func TestReviewerConsumers(t *testing.T) {
	setup := func(t *testing.T, env string, skip bool) (*Gate, string) {
		g, _, _, _ := testGate(t)
		mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nFABRIK_REVIEWER_TOKEN="+reviewerTok+"\n")
		g.Cfg.BedToken, g.Cfg.ReviewerToken = "bed-token", reviewerTok
		other := t.TempDir()
		mustWrite(t, other+"/.env", env)
		g.Exec.(*fakeExec).handler = func(_ context.Context, c Cmd) Result {
			if c.Name == "pgrep" {
				writeStdout(c, "4242\n4243\n")
			}
			return Result{}
		}
		g.ProcCwd = func(_ context.Context, pid int) string {
			if pid == 4243 {
				return g.Cfg.TestBed // the bed's own engine
			}
			return other
		}
		if skip {
			g.Env = append(g.Env, "E2E_SKIP_TOKEN_CHECK=1")
		}
		return g, other
	}
	t.Run("a non-bed process carrying the reviewer token is refused, naming pid and dir", func(t *testing.T) {
		g, other := setup(t, "FABRIK_REVIEWER_TOKEN="+reviewerTok+"\n", false)
		err := g.CheckCompetingTokenConsumers(context.Background(), []string{"app"}) // no pat leg: no downgrade for the reviewer
		if exitCode(err) != ExitPreconditionFailed {
			t.Fatalf("got %v", err)
		}
		for _, want := range []string{"pid 4242", other, "FABRIK_REVIEWER_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in:\n%s", want, err)
			}
		}
		if strings.Contains(err.Error(), "pid 4243") {
			t.Error("the bed's own engine is never a competitor")
		}
		noSecret(t, "refusal", err.Error())
	})
	t.Run("a process running on the reviewer token as its FABRIK_TOKEN counts too", func(t *testing.T) {
		g, _ := setup(t, "FABRIK_TOKEN="+reviewerTok+"\n", false)
		if exitCode(g.CheckCompetingTokenConsumers(context.Background(), []string{"app"})) != ExitPreconditionFailed {
			t.Error("expected a refusal")
		}
	})
	t.Run("only the beds' own engines: passes", func(t *testing.T) {
		g, _ := setup(t, "FABRIK_TOKEN=unrelated\n", false)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"app"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("E2E_SKIP_TOKEN_CHECK skips it", func(t *testing.T) {
		g, _ := setup(t, "FABRIK_REVIEWER_TOKEN="+reviewerTok+"\n", true)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"app"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the engine-token and reviewer-token findings are reported together", func(t *testing.T) {
		g, _ := setup(t, "FABRIK_TOKEN=bed-token\nFABRIK_REVIEWER_TOKEN="+reviewerTok+"\n", false)
		err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"})
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "@arbeithand") || !strings.Contains(err.Error(), "REVIEWER_TOKEN") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("non-vacuity: disabled, the reviewer consumer goes unnoticed", func(t *testing.T) {
		g, _ := setup(t, "FABRIK_REVIEWER_TOKEN="+reviewerTok+"\n", false)
		g.reviewerBudgetCheckDisabledForTest = true
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"app"}); err != nil {
			t.Errorf("the old behaviour proceeds: %v", err)
		}
	})
}
