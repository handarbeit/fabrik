//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Defaults wire to the canonical test bed. Override via env vars in CI or
// when the bed lives elsewhere.
const (
	defaultFabrikTestDir = "~/dev/fabrik-test"
	defaultRepoAlpha     = "handarbeit/fabrik-test-alpha"
	defaultRepoBeta      = "handarbeit/fabrik-test-beta"
	defaultProjectNumber = 2
	defaultProjectOwner  = "handarbeit"
)

// markerPaths is the single source of truth for every non-train scenario's
// marker file target: one fixed, distinct e2e/markers/<scenario>.md path per
// writer test function, so parallel scenarios can never conflict on a shared
// file (#1394). Unlike the merge-train members' uniqueMemberPath (which needs
// per-run uniqueness because a landed batch merges files into main and a
// fixed path would collide with the existing blob's SHA on the Contents API),
// these files are edited through a normal agent git commit in a worktree — a
// real diff, not a raw Contents-API PUT — so a fixed path across runs is
// fine, and it's what lets TestMarkerPathsAreUnique enumerate the set
// statically (AC1) rather than needing to execute the tests to know the
// paths.
//
// TestPausedMergedPRRecovery's 3 sub-variants deliberately share one path:
// they run strictly sequentially (t.Run, no t.Parallel between them) and each
// variant's PR merges before the next is filed, so there is no concurrent
// write to race.
//
// TestConvergenceRace is deliberately absent from this map. Its entire
// premise (documented in convergence_race_test.go's own top-of-file comment)
// is that its two issues collide on the same anchor line of the shared
// README.md, forcing a genuine merge conflict that Fabrik must resolve via a
// rebase reinvoke — giving it a unique path would eliminate the exact
// condition it exists to provoke (R4).
var markerPaths = map[string]string{
	"TestSmokeSingleRepoFullPipeline": "e2e/markers/smoke-full-pipeline.md",
	"TestYoloAutoMergeLabel":          "e2e/markers/auto-merge-yolo.md",
	"TestPausedMergedPRRecovery":      "e2e/markers/paused-merged-pr-recovery.md",
	"TestBaseBranchPipeline":          "e2e/markers/base-branch-pipeline.md",
	"TestCruiseFullPipeline":          "e2e/markers/cruise-full-pipeline.md",
	"TestCIFixReinvoke":               "e2e/markers/ci-fix-reinvoke.md",
	"TestCIFixReinvokeCycleLimit":     "e2e/markers/ci-fix-reinvoke-cycle-limit.md",
	"TestConjunctiveCIReviewGate":     "e2e/markers/conjunctive-ci-review-gate.md",
}

// markerPath returns the unique marker file path for the named scenario.
// Panics on an unknown name so a typo'd lookup fails loudly at test setup
// rather than silently producing an empty path in an issue body.
func markerPath(name string) string {
	path, ok := markerPaths[name]
	if !ok {
		panic(fmt.Sprintf("markerPath: no entry for %q — add it to markerPaths in harness.go", name))
	}
	return path
}

// Env carries the resolved test-bed paths and identifiers. Constructed by
// LoadEnv from environment variables (with sensible defaults).
type Env struct {
	FabrikTestDir string // absolute path to ~/dev/fabrik-test
	LogPath       string // absolute path to the test instance's fabrik.log
	RepoAlpha     string // "owner/repo" form
	RepoBeta      string // "owner/repo" form
	ProjectOwner  string // org or user owning the test project
	ProjectNumber int    // GitHub Project v2 number
	GHToken       string // FABRIK_TOKEN from the test bed's .env (passed to gh as GH_TOKEN)
}

// LoadEnv resolves the test-bed environment. Fails the test cleanly if the
// bed is not set up.
func LoadEnv(t *testing.T) *Env {
	t.Helper()

	dir := getenvOr("FABRIK_TEST_DIR", expandHome(defaultFabrikTestDir))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("test bed not present at %s — see tests/e2e/README.md for setup", dir)
	}

	// Read FABRIK_TOKEN from the test bed's own .env so we don't depend on
	// the operator having the right token in their current shell.
	envFile := filepath.Join(dir, ".env")
	token, err := readEnvFileValue(envFile, "FABRIK_TOKEN")
	if err != nil {
		t.Fatalf("could not read FABRIK_TOKEN from %s: %v", envFile, err)
	}

	return &Env{
		FabrikTestDir: dir,
		LogPath:       filepath.Join(dir, ".fabrik", "fabrik.log"),
		RepoAlpha:     getenvOr("FABRIK_TEST_REPO_ALPHA", defaultRepoAlpha),
		RepoBeta:      getenvOr("FABRIK_TEST_REPO_BETA", defaultRepoBeta),
		ProjectOwner:  getenvOr("FABRIK_TEST_PROJECT_OWNER", defaultProjectOwner),
		ProjectNumber: defaultProjectNumber,
		GHToken:       token,
	}
}

// AssertFabrikRunning verifies the test-bed Fabrik instance is alive.
// Skips the test if not — we don't auto-start it (yet).
//
// Detection strategy: check the lock file. Fabrik atomically writes its PID
// to .fabrik/fabrik.lock on startup and unlinks it on shutdown. If the file
// exists and the named PID is still alive, Fabrik is running.
func AssertFabrikRunning(t *testing.T, env *Env) {
	t.Helper()
	lockPath := filepath.Join(env.FabrikTestDir, ".fabrik", "fabrik.lock")
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Skipf("Fabrik instance not running at %s — no lock file (start it: cd %s && ./fabrik -notui --auto-upgrade &)",
			env.FabrikTestDir, env.FabrikTestDir)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(contents)), "%d", &pid); err != nil || pid <= 0 {
		t.Skipf("Fabrik lock file at %s is malformed (%q)", lockPath, contents)
	}
	if err := syscallSignalZero(pid); err != nil {
		t.Skipf("Fabrik lock file claims pid %d but process is dead (%v) — stale lock at %s; remove and restart",
			pid, err, lockPath)
	}
}

// syscallSignalZero sends signal 0 to a pid — a no-op signal that returns an
// error if the process doesn't exist. POSIX-portable liveness check.
func syscallSignalZero(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}

// FileIssue creates an issue in the named repo with the given title, body, and
// labels. Returns the issue number. Registers a t.Cleanup to close the issue
// at test end.
func FileIssue(t *testing.T, env *Env, repo, title, body string, labels ...string) int {
	t.Helper()

	args := []string{"issue", "create", "-R", repo, "--title", title, "--body", body}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	out, err := ghOutput(env, args...)
	if err != nil {
		t.Fatalf("file issue %q in %s: %v\n%s", title, repo, err, out)
	}
	// gh prints the URL on the last non-empty line.
	url := lastNonEmpty(out)
	num := parseIssueNumberFromURL(url)
	if num == 0 {
		t.Fatalf("could not parse issue number from gh output: %q", out)
	}

	t.Cleanup(func() {
		// Best-effort close; don't fail teardown.
		_, _ = ghOutput(env, "issue", "close", fmt.Sprint(num), "-R", repo, "--reason", "completed", "--comment", "Closing: e2e test teardown")
	})

	return num
}

// AddIssueToProject adds the issue to the test bed's project, returning the
// project-item ID for subsequent field edits.
func AddIssueToProject(t *testing.T, env *Env, repo string, issueNumber int) string {
	t.Helper()
	url := fmt.Sprintf("https://github.com/%s/issues/%d", repo, issueNumber)
	out, err := ghOutput(env, "project", "item-add", fmt.Sprint(env.ProjectNumber),
		"--owner", env.ProjectOwner, "--url", url, "--format", "json")
	if err != nil {
		t.Fatalf("add #%d to project: %v\n%s", issueNumber, err, out)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse project item-add output: %v\n%s", err, out)
	}
	return parsed.ID
}

// SetIssueStatus moves the project item to the named Status column (e.g. "Specify").
func SetIssueStatus(t *testing.T, env *Env, itemID, columnName string) {
	t.Helper()
	projectID, statusFieldID, optionID := resolveStatusOption(t, env, columnName)
	_, err := ghOutput(env, "project", "item-edit",
		"--id", itemID, "--project-id", projectID,
		"--field-id", statusFieldID, "--single-select-option-id", optionID)
	if err != nil {
		t.Fatalf("set issue status to %s: %v", columnName, err)
	}
}

// pollRandMu guards pollRand, since ~15 of the 16 e2e test functions run
// under t.Parallel() and several call multiple wait-helpers concurrently.
var pollRandMu sync.Mutex
var pollRand = newPollRand()

// newPollRand builds the package-scoped RNG source used by pollSleep. It
// self-seeds randomly unless E2E_JITTER_SEED is set to a valid uint64, in
// which case both PCG seed halves derive from it, making the jitter sequence
// reproducible for harness unit tests and for local repro of flaky-looking
// failures. A malformed E2E_JITTER_SEED falls back to auto-seeding rather
// than failing — a bad env var shouldn't break every e2e test.
func newPollRand() *rand.Rand {
	if s := os.Getenv("E2E_JITTER_SEED"); s != "" {
		if seed, err := strconv.ParseUint(s, 10, 64); err == nil {
			// Reusing seed for both PCG state and sequence is intentional:
			// we only need a reproducible sequence from a single uint64 for
			// test determinism, not two independent streams.
			return rand.New(rand.NewPCG(seed, seed))
		}
	}
	return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
}

// jitterWithRand returns a duration uniformly distributed over
// [base*0.8, base*1.2) using the supplied RNG. It is pure (no global state)
// so unit tests can exercise it with their own seeded generator.
func jitterWithRand(r *rand.Rand, base time.Duration) time.Duration {
	f := r.Float64() // [0, 1)
	return base + time.Duration(float64(base)*0.20*(2*f-1))
}

// pollSleep sleeps for base, jittered ±20% (uniform, a fixed proportional-
// jitter band centered on base — not AWS's "equal jitter", which is
// asymmetric and floored at base/2). This is deliberately not full jitter
// (rand in [0, base]), whose
// mean of base/2 would roughly double the harness's steady-state request
// rate against the exact shared GitHub API/GraphQL budget this jitter is
// meant to protect. It's also deliberately not decorrelated jitter, which
// grows a retry sequence from the previous sleep — these wait-helpers are
// steady-state condition polls, not retry-after-failure backoff, so there's
// no "previous sleep" to grow from. Fixed base ± 20% preserves the mean
// interval (so total request volume and suite wall-clock stay essentially
// unchanged) while still desynchronizing concurrent scenarios, since each
// poller's phase drifts every iteration and lockstep breaks within a few
// cycles. This does widen a wait-helper's existing tail-sleep deadline
// overshoot from at most one base to at most base*1.2 (e.g. 15s to 18s) —
// negligible against per-scenario timeouts measured in minutes to hours.
func pollSleep(base time.Duration) {
	pollRandMu.Lock()
	d := jitterWithRand(pollRand, base)
	pollRandMu.Unlock()
	time.Sleep(d)
}

// defaultPollBase is the steady-state interval for the issue/PR wait-helpers.
//
// Once the board reads moved to direct GraphQL (~1 point each), the suite's
// residual rate-limit cost is dominated by these helpers: `gh issue view --json`
// and `gh pr list --json` are also GraphQL, ~1 point per call, and a dozen
// parallel scenarios each hold one or two waits open continuously. Measured over
// the 2026-07-28 run that is ~4,860 points/hour against a 5,000/hour budget —
// inside the ceiling, but with no useful margin.
//
// Halving the call rate halves that residual. The cost is up to one extra
// interval of detection latency per wait, which is negligible against scenarios
// that run 3-25 minutes. Board-status polling (WaitForProjectStatus) stays at
// 10s: it is now ~1 point per call and is the helper most often on the critical
// path between an engine action and the next assertion.
//
// Override with E2E_POLL_INTERVAL (any time.ParseDuration value) to tune without
// a code change — e.g. E2E_POLL_INTERVAL=15s to restore the old cadence when
// running a single scenario in isolation, where budget is not a constraint.
const defaultPollBase = 30 * time.Second

func pollBase() time.Duration {
	if s := os.Getenv("E2E_POLL_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return defaultPollBase
}

// WaitForProjectStatus polls the project board until the item for issueNumber
// reports Status == columnName, or fails after timeout. Use this after
// SetIssueStatus when a subsequent action (e.g. an external PR merge that
// closes the issue) would otherwise race the engine's next board poll: it
// confirms GitHub has propagated the new column before the action lands.
func WaitForProjectStatus(t *testing.T, env *Env, repo string, issueNumber int, columnName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if items, err := fetchBoardItems(env); err == nil {
			if it, ok := findBoardItem(items, repo, issueNumber); ok {
				last = it.Status
				if it.Status == columnName {
					return
				}
			}
		}
		pollSleep(10 * time.Second)
	}
	t.Fatalf("timed out waiting for project status %q on %s#%d (last observed %q)",
		columnName, repo, issueNumber, last)
}

// WaitForIssueLabel polls the issue until it has all of the named labels, or
// timeout expires.
func WaitForIssueLabel(t *testing.T, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		labels, err := tryIssueLabels(env, repo, issueNumber)
		if err != nil {
			t.Logf("WaitForIssueLabel: transient gh error on %s#%d: %v (will retry)", repo, issueNumber, err)
			pollSleep(pollBase())
			continue
		}
		for _, l := range labels {
			if l == label {
				return
			}
		}
		pollSleep(pollBase())
	}
	last, _ := tryIssueLabels(env, repo, issueNumber)
	t.Fatalf("timed out waiting for label %q on %s#%d (had: %v)", label, repo, issueNumber, last)
}

// IssueLabels returns the current labels on the issue.
func IssueLabels(t *testing.T, env *Env, repo string, issueNumber int) []string {
	t.Helper()
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo,
		"--json", "labels", "--jq", "[.labels[].name]")
	if err != nil {
		t.Fatalf("read labels for %s#%d: %v", repo, issueNumber, err)
	}
	var labels []string
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &labels)
	return labels
}

// AddLabel adds a label to the issue. Fails the test if gh refuses (e.g. label not seeded).
func AddLabel(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "edit", fmt.Sprint(issueNumber), "-R", repo, "--add-label", label); err != nil {
		t.Fatalf("add label %q on %s#%d: %v", label, repo, issueNumber, err)
	}
}

// RemoveLabel removes a label from the issue.
func RemoveLabel(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "edit", fmt.Sprint(issueNumber), "-R", repo, "--remove-label", label); err != nil {
		t.Fatalf("remove label %q on %s#%d: %v", label, repo, issueNumber, err)
	}
}

// CommentOnIssue posts a comment as the test bed's PAT identity. Used to simulate
// user input (e.g. resuming a FABRIK_BLOCKED_ON_INPUT pause).
func CommentOnIssue(t *testing.T, env *Env, repo string, issueNumber int, body string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "comment", fmt.Sprint(issueNumber), "-R", repo, "--body", body); err != nil {
		t.Fatalf("post comment on %s#%d: %v", repo, issueNumber, err)
	}
}

// WaitForIssueClosed polls until the issue is CLOSED or timeout expires.
// Transient gh errors (rate-limit blips, network hiccups) are logged and the
// poll continues — only the timeout itself fails the test, since over a 45-min
// wait one stray exit-1 from `gh` would otherwise kill an otherwise-healthy run.
func WaitForIssueClosed(t *testing.T, env *Env, repo string, issueNumber int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := tryIssueState(env, repo, issueNumber)
		if err != nil {
			t.Logf("WaitForIssueClosed: transient gh error on %s#%d: %v (will retry)", repo, issueNumber, err)
		} else if state == "CLOSED" {
			return
		}
		pollSleep(pollBase())
	}
	state, _ := tryIssueState(env, repo, issueNumber)
	t.Fatalf("timed out waiting for %s#%d to close (last observed: %q)", repo, issueNumber, state)
}

// defaultReviewFailFastWindow is how long a linked PR may sit ready-for-review
// with zero reviews before WaitForIssueClosedWithReviewCheck fails fast,
// rather than running out the full close-timeout (handarbeit/fabrik#1396).
//
// Pruefer (the bed's real reviewer once .pruefer/config.yaml's watched_repos
// includes the test repos — see tests/e2e/README.md "Reviewer topology")
// polls every 120s with a concurrency_cap of 3 across its watched repos. 10
// minutes is ~5x that cadence — comfortable headroom for a busy-but-healthy
// Pruefer — while still catching a genuinely dropped review-dispatch well
// before the engine's own first ReviewWaitTimeout window (15 min default)
// completes, let alone the ~45-minute wall-clock the issue's Problem section
// observed.
//
// Override with E2E_REVIEW_FAILFAST_WINDOW (any time.ParseDuration value),
// mirroring E2E_POLL_INTERVAL's tuning convention, if real-world flakiness
// data says the bound needs adjusting.
const defaultReviewFailFastWindow = 10 * time.Minute

func reviewFailFastWindow() time.Duration {
	if s := os.Getenv("E2E_REVIEW_FAILFAST_WINDOW"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return defaultReviewFailFastWindow
}

// reviewFailFastDue is the pure decision behind the fail-fast check: given
// when the PR became ready for review (readyAt, zero if not yet ready or
// unknown), how many reviews it currently has, the current time, and the
// configured window, should the wait fail now?
//
// Deliberately author- and state-agnostic (any review counts, including
// DISMISSED) — this is a test-side diagnostic, not a reimplementation of
// engine/reviews.go's hasReviews gate-clearing semantics (R2 requires the
// engine's clearing rule stay untouched).
func reviewFailFastDue(readyAt time.Time, reviewCount int, now time.Time, window time.Duration) bool {
	if readyAt.IsZero() {
		return false
	}
	if reviewCount > 0 {
		return false
	}
	return now.Sub(readyAt) >= window
}

// tryPRReviewState is the non-fatal read behind WaitForIssueClosedWithReviewCheck's
// fail-fast check: draft state, creation time, and review count for a PR, in
// one gh call. Returns an error on any gh/JSON failure so callers can treat
// it like the other try* helpers — log and retry, never fail the whole wait
// on one transient blip.
func tryPRReviewState(env *Env, repo string, prNumber int) (isDraft bool, createdAt time.Time, reviewCount int, err error) {
	out, err := ghOutput(env, "pr", "view", fmt.Sprint(prNumber), "-R", repo,
		"--json", "isDraft,createdAt,reviews")
	if err != nil {
		return false, time.Time{}, 0, err
	}
	var parsed struct {
		IsDraft   bool              `json:"isDraft"`
		CreatedAt time.Time         `json:"createdAt"`
		Reviews   []json.RawMessage `json:"reviews"`
	}
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		return false, time.Time{}, 0, fmt.Errorf("parse PR review state for %s PR #%d: %w", repo, prNumber, uerr)
	}
	return parsed.IsDraft, parsed.CreatedAt, len(parsed.Reviews), nil
}

// WaitForIssueClosedWithReviewCheck is WaitForIssueClosed plus a bounded
// fail-fast check on the linked PR's review state (handarbeit/fabrik#1396,
// R3/R4). If the PR has been ready-for-review (not draft) for
// reviewFailFastWindow() with zero reviews, it fails immediately with a
// message that names that specific cause — distinguishable from the generic
// close-timeout below — rather than running out the full timeout waiting on
// a review that (per the issue's Problem section) may never come because a
// bot-review dispatch was dropped.
//
// The draft→ready transition is self-observed across polls rather than read
// from GitHub's readyForReviewAt field: that field isn't in this gh CLI's
// supported --json fields for `pr view` (verified during Plan). The first
// poll that observes isDraft==false anchors readyAt — to "now" if an earlier
// poll observed isDraft==true (a real draft→ready transition happened
// in-window), or to the PR's createdAt if it was never seen as a draft
// (opened ready from the start; the ~poll-interval error this introduces is
// negligible against a 10-minute window). If a later poll observes
// isDraft==true again (a PR re-drafted after going ready — unusual but
// possible), readyAt resets to zero so the zero-review clock doesn't keep
// running through a period the PR genuinely wasn't reviewable.
//
// Once a review is observed (reviewCount > 0), the PR-review-state poll is
// skipped for the remainder of the wait — reviewFailFastDue can never fire
// again once a review exists, so there's no reason to keep paying the extra
// `gh pr view` call every cycle (see tests/e2e/README.md's GraphQL-budget
// note, which assumes exactly this bound).
//
// Contract for callers: this helper only trusts readyAt's createdAt fallback
// to be within poll-interval error of "actually ready" if the wait begins at
// or near PR creation (true of all current call sites — either no PR exists
// yet, or, per the "opt-in" note below, a review has already organically
// landed via the Review stage's own gate before this helper is ever called).
// A future call site that invokes this well after a long-idle, never-drafted,
// still-unreviewed PR was created would see readyAt anchored to that stale
// createdAt and could fail on its very first poll — that's a real risk for a
// hypothetical caller, not a mitigated one; verify it doesn't apply before
// adopting this helper at a new site.
//
// Not wired into every WaitForIssueClosed call site — see the Plan stage's
// "Opt-in helper, not a default-on change" decision (handarbeit/fabrik#1396):
// only scenarios that drive a real, unreviewed PR through the organic Review
// gate benefit: sites that submit their own review before waiting, or that
// never create an organically-reviewed PR at all (FABRIK_NO_WORK_NEEDED,
// merge-train Queued members, externally-force-merged PRs), would never
// trigger the check or would trigger it spuriously.
func WaitForIssueClosedWithReviewCheck(t *testing.T, env *Env, repo string, issueNumber int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var sawDraft bool
	var readyAt time.Time
	var reviewLanded bool
	for time.Now().Before(deadline) {
		state, err := tryIssueState(env, repo, issueNumber)
		if err != nil {
			t.Logf("WaitForIssueClosedWithReviewCheck: transient gh error on %s#%d: %v (will retry)", repo, issueNumber, err)
		} else if state == "CLOSED" {
			return
		}

		if !reviewLanded {
			if prNumber, prErr := tryLinkedPRNumber(env, repo, issueNumber); prErr == nil {
				if isDraft, createdAt, reviewCount, rsErr := tryPRReviewState(env, repo, prNumber); rsErr == nil {
					if isDraft {
						sawDraft = true
						readyAt = time.Time{}
					} else if readyAt.IsZero() {
						if sawDraft {
							readyAt = time.Now()
						} else {
							readyAt = createdAt
						}
					}
					if reviewCount > 0 {
						reviewLanded = true
					} else if reviewFailFastDue(readyAt, reviewCount, time.Now(), reviewFailFastWindow()) {
						t.Fatalf("%s#%d: PR #%d has been ready for review for over %s with zero reviews — "+
							"likely a dropped bot-review dispatch, not a Fabrik engine defect (see handarbeit/fabrik#1396)",
							repo, issueNumber, prNumber, reviewFailFastWindow())
					}
				}
			}
		}

		pollSleep(pollBase())
	}
	state, _ := tryIssueState(env, repo, issueNumber)
	t.Fatalf("timed out waiting for %s#%d to close (last observed: %q)", repo, issueNumber, state)
}

// WaitForLabelAbsent polls until the named label is no longer on the issue.
// Tolerant of transient gh errors — see WaitForIssueClosed for rationale.
func WaitForLabelAbsent(t *testing.T, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		labels, err := tryIssueLabels(env, repo, issueNumber)
		if err != nil {
			t.Logf("WaitForLabelAbsent: transient gh error on %s#%d: %v (will retry)", repo, issueNumber, err)
			pollSleep(pollBase())
			continue
		}
		present := false
		for _, l := range labels {
			if l == label {
				present = true
				break
			}
		}
		if !present {
			return
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for label %q to disappear from %s#%d", label, repo, issueNumber)
}

// tryIssueState is the non-fatal counterpart to IssueState — returns (state, err)
// instead of t.Fatalf'ing. Used by long-running pollers so a single transient
// gh failure doesn't kill an otherwise-healthy multi-minute wait.
func tryIssueState(env *Env, repo string, issueNumber int) (string, error) {
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo, "--json", "state", "--jq", ".state")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// tryIssueLabels is the non-fatal counterpart to IssueLabels.
func tryIssueLabels(env *Env, repo string, issueNumber int) ([]string, error) {
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo,
		"--json", "labels", "--jq", "[.labels[].name]")
	if err != nil {
		return nil, err
	}
	var labels []string
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &labels); uerr != nil {
		return nil, fmt.Errorf("parse labels: %w", uerr)
	}
	return labels, nil
}

// AssertLabelWasApplied queries the issue's timeline events and asserts the
// named label was applied at some point during the issue's lifetime. Survives
// the case where Fabrik adds-and-removes a label faster than a polling
// interval can observe (e.g. fabrik:auto-merge-enabled on a PR that merges
// instantly because CI is trivial and there's no branch protection).
func AssertLabelWasApplied(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/%d/timeline", owner, name, issueNumber),
		"--jq", `[.[] | select(.event == "labeled") | .label.name]`)
	if err != nil {
		t.Fatalf("query timeline for %s#%d: %v\n%s", repo, issueNumber, err, out)
	}
	// --paginate concatenates one JSON array per page. Split, parse each, union.
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(line), &names); err != nil {
			t.Fatalf("failed to unmarshal timeline labels JSON: %v (line: %q)", err, line)
		}
		for _, n := range names {
			seen[n] = true
		}
	}
	if seen[label] {
		return
	}
	t.Fatalf("label %q was never applied to %s#%d (labels ever applied: %v)",
		label, repo, issueNumber, mapKeys(seen))
}

// AssertLabelWasNeverApplied is the inverse of AssertLabelWasApplied. It
// queries the issue timeline and fails if the named label was applied at any
// point during the issue's lifetime. Useful for asserting that the engine
// never took a path it should have suppressed (e.g. fabrik:auto-merge-enabled
// must never appear for cruise items).
func AssertLabelWasNeverApplied(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/%d/timeline", owner, name, issueNumber),
		"--jq", `[.[] | select(.event == "labeled") | .label.name]`)
	if err != nil {
		t.Fatalf("query timeline for %s#%d: %v\n%s", repo, issueNumber, err, out)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(line), &names); err != nil {
			t.Fatalf("failed to unmarshal timeline labels JSON: %v (line: %q)", err, line)
		}
		for _, n := range names {
			seen[n] = true
		}
	}
	if !seen[label] {
		return
	}
	t.Fatalf("label %q was applied to %s#%d (must never have been applied; labels ever applied: %v)",
		label, repo, issueNumber, mapKeys(seen))
}

// WaitForLinkedPR polls until a PR linked to the issue's branch
// (fabrik/issue-<N>) appears with state=open, then returns its PR number.
// Fails after timeout. Call this before MergePR to obtain the PR number.
func WaitForLinkedPR(t *testing.T, env *Env, repo string, issueNum int, timeout time.Duration) int {
	t.Helper()
	branch := "fabrik/issue-" + strconv.Itoa(issueNum)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := ghOutput(env, "pr", "list", "-R", repo,
			"--head", branch, "--state", "open",
			"--json", "number", "--jq", ".[0].number")
		if err == nil {
			s := strings.TrimSpace(out)
			if s != "" && s != "null" {
				var num int
				if _, scanErr := fmt.Sscanf(s, "%d", &num); scanErr == nil && num > 0 {
					return num
				}
			}
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for an open PR with head=%s in %s", branch, repo)
	return 0
}

// MergePR merges the numbered PR in repo using a standard merge commit.
// Use this in tests where a human merge is required (e.g. cruise mode, which
// suppresses auto-merge and waits for a human to approve and merge the PR).
func MergePR(t *testing.T, env *Env, repo string, prNumber int) {
	t.Helper()
	// --admin bypasses not-yet-green required checks. The test repo has
	// enforce_admins:false, so an admin merge is permitted, and it faithfully
	// models the scenario these tests simulate: an external human merging the PR.
	// Without it, a merge issued before the ~10-min slow-gate (and other required
	// checks) go green is rejected with "the base branch policy prohibits the
	// merge" — a non-deterministic failure that depends on CI timing.
	out, err := ghOutput(env, "pr", "merge", fmt.Sprint(prNumber), "-R", repo, "--merge", "--admin")
	if err != nil {
		t.Fatalf("merge PR #%d in %s: %v\n%s", prNumber, repo, err, out)
	}
}

// CreateThrowawayBaseBranch creates a throwaway branch on repo, forked off
// main's current head, for use as a non-default base:<branch> label target.
// Registers a t.Cleanup to delete it (best-effort) at test end.
func CreateThrowawayBaseBranch(t *testing.T, env *Env, repo, branchName string) {
	t.Helper()
	sha := defaultBranchSHA(t, env, repo, "main")
	if out, err := ghOutput(env, "api", "--method", "POST",
		fmt.Sprintf("repos/%s/git/refs", repo),
		"-f", "ref=refs/heads/"+branchName,
		"-f", "sha="+sha); err != nil {
		t.Fatalf("create throwaway base branch %s on %s: %v\n%s", branchName, repo, err, out)
	}
	t.Cleanup(func() {
		_, _ = ghOutput(env, "api", "--method", "DELETE",
			fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, branchName))
	})
	t.Logf("created throwaway base branch %s on %s (sha %s)", branchName, repo, sha)
}

// PRBaseRef returns the PR's base branch name (baseRefName) — used to confirm
// a PR targets a non-default base branch rather than silently falling back
// to the repo default.
func PRBaseRef(t *testing.T, env *Env, repo string, prNumber int) string {
	t.Helper()
	out, err := ghOutput(env, "pr", "view", fmt.Sprint(prNumber), "-R", repo,
		"--json", "baseRefName", "--jq", ".baseRefName")
	if err != nil {
		t.Fatalf("read base ref of PR #%d in %s: %v\n%s", prNumber, repo, err, out)
	}
	return strings.TrimSpace(out)
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// IssueState returns "OPEN" or "CLOSED".
func IssueState(t *testing.T, env *Env, repo string, issueNumber int) string {
	t.Helper()
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo, "--json", "state", "--jq", ".state")
	if err != nil {
		t.Fatalf("read state for %s#%d: %v", repo, issueNumber, err)
	}
	return strings.TrimSpace(out)
}

// WaitForChildIssueInRepo polls the named child repo for the first issue
// authored by the bot during this test run. Returns the child issue number.
// Used to detect cross-repo spawn outcomes.
//
// Filter: state=OPEN, labels include "fabrik:sub-issue", createdAt >= since.
func WaitForChildIssueInRepo(t *testing.T, env *Env, childRepo string, since time.Time, timeout time.Duration) int {
	t.Helper()
	sinceStr := since.UTC().Format(time.RFC3339)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := ghOutput(env, "issue", "list", "-R", childRepo,
			"--label", "fabrik:sub-issue", "--state", "open",
			"--search", "created:>="+sinceStr,
			"--json", "number,createdAt", "--jq", "[.[] | .number]")
		if err == nil {
			var nums []int
			if json.Unmarshal([]byte(strings.TrimSpace(out)), &nums) == nil && len(nums) > 0 {
				return nums[0]
			}
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for a child sub-issue in %s (since %s)", childRepo, sinceStr)
	return 0
}

// AssertBlockedBy verifies that `issueRepo#issueNumber` is recorded as
// blocked-by `blockerRepo#blockerNumber` via the Issue Dependencies API.
func AssertBlockedBy(t *testing.T, env *Env, issueRepo string, issueNumber int, blockerRepo string, blockerNumber int) {
	t.Helper()
	owner, name, ok := splitRepo(issueRepo)
	if !ok {
		t.Fatalf("bad repo: %q", issueRepo)
	}
	q := fmt.Sprintf(`
query {
  repository(owner: "%s", name: "%s") {
    issue(number: %d) {
      blockedBy(first: 10) { nodes { number repository { nameWithOwner } } }
    }
  }
}`, owner, name, issueNumber)
	out, err := ghOutput(env, "api", "graphql", "-f", "query="+q,
		"--jq", ".data.repository.issue.blockedBy.nodes")
	if err != nil {
		t.Fatalf("query blockedBy for %s#%d: %v\n%s", issueRepo, issueNumber, err, out)
	}
	type node struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	var nodes []node
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &nodes); err != nil {
		t.Fatalf("parse blockedBy response: %v\n%s", err, out)
	}
	for _, n := range nodes {
		if n.Number == blockerNumber && n.Repository.NameWithOwner == blockerRepo {
			return
		}
	}
	t.Fatalf("%s#%d not blocked by %s#%d (blockedBy was: %+v)", issueRepo, issueNumber, blockerRepo, blockerNumber, nodes)
}

// FetchRepoFileContent reads a file's content from repo's default branch via
// the GitHub Contents API and returns it decoded. Mirrors CreateMemberPR's
// write shape (mergetrain_helpers.go) but as a read. Fails the test
// immediately on a fetch/decode error (API or plumbing failure) — a
// content-mismatch check is the caller's responsibility, kept separate so a
// transient API hiccup here is never mistaken for "the expected content is
// absent". GitHub wraps the base64 content with embedded newlines, which
// must be stripped before decoding.
func FetchRepoFileContent(t *testing.T, env *Env, repo, path string) string {
	t.Helper()
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/contents/%s", repo, path),
		"--jq", ".content")
	if err != nil {
		t.Fatalf("fetch %s from %s: %v\n%s", path, repo, err, out)
	}
	encoded := strings.ReplaceAll(strings.TrimSpace(out), "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode content of %s from %s: %v", path, repo, err)
	}
	return string(decoded)
}

// CloseIssue best-effort closes the named issue (used in t.Cleanup).
func CloseIssue(env *Env, repo string, issueNumber int) {
	_, _ = ghOutput(env, "issue", "close", fmt.Sprint(issueNumber), "-R", repo,
		"--reason", "completed", "--comment", "Closing: e2e test teardown")
}

// ResetOpenIssues closes every OPEN issue on the test repos. Useful at session
// start to wipe state from prior runs.
func ResetOpenIssues(t *testing.T, env *Env) {
	t.Helper()
	for _, repo := range []string{env.RepoAlpha, env.RepoBeta} {
		out, err := ghOutput(env, "issue", "list", "-R", repo, "--state", "open",
			"--json", "number", "--jq", "[.[] | .number]")
		if err != nil {
			t.Logf("ResetOpenIssues: could not list issues in %s: %v", repo, err)
			continue
		}
		var nums []int
		_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &nums)
		for _, n := range nums {
			CloseIssue(env, repo, n)
			t.Logf("ResetOpenIssues: closed %s#%d", repo, n)
		}
	}
}

func splitRepo(repo string) (owner, name string, ok bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// LogOffset captures the size of the test bed's fabrik.log at this moment.
// Pass it to WaitForLogLine to scan starting from this offset — call this
// BEFORE the triggering action (filing an issue, posting a comment, etc.) so
// the scan doesn't miss lines emitted between the trigger and the wait.
//
// Returns 0 if the log file does not yet exist (Fabrik will create it).
func LogOffset(t *testing.T, env *Env) int64 {
	t.Helper()
	info, err := os.Stat(env.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", env.LogPath, err)
	}
	return info.Size()
}

// WaitForLogLine scans the test bed's fabrik.log starting at startOffset
// (typically the value returned by LogOffset before the trigger action) and
// returns the first line matching the substring, or fails after timeout.
//
// Avoids the seek-to-EOF race that would otherwise miss lines emitted
// between the trigger and the start of the wait.
//
// Prefer observable GitHub state over log scraping where possible — log
// formats are less stable than label/state transitions.
func WaitForLogLine(t *testing.T, env *Env, substring string, startOffset int64, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	f, err := os.Open(env.LogPath)
	if err != nil {
		t.Fatalf("open %s: %v", env.LogPath, err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, 0); err != nil {
		t.Fatalf("seek %s to %d: %v", env.LogPath, startOffset, err)
	}
	r := bufio.NewReader(f)

	for ctx.Err() == nil {
		line, err := r.ReadString('\n')
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if strings.Contains(line, substring) {
			return line
		}
	}
	t.Fatalf("timed out waiting for log line containing %q (scanned from offset %d)", substring, startOffset)
	return ""
}

// tryLogLineContaining does a single, non-fatal scan of the test bed's
// fabrik.log starting at startOffset for a line containing substring —
// unlike WaitForLogLine, it does not wait/retry and does not fail the test;
// it returns ("", nil) if no matching line is present yet. Used to assert the
// *absence* of a log line over a bounded settle window (e.g. AC7's "did not
// re-dispatch" check), where WaitForLogLine's fail-on-timeout semantics are
// the wrong shape — the caller wants "confirm this didn't happen," not "wait
// until it does."
func tryLogLineContaining(env *Env, substring string, startOffset int64) (string, error) {
	f, err := os.Open(env.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, 0); err != nil {
		return "", err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if strings.Contains(line, substring) {
			return line, nil
		}
		if err != nil {
			return "", nil
		}
	}
}

// allLogLinesContaining does a single, non-fatal scan of the test bed's
// fabrik.log starting at startOffset for every line containing substring —
// unlike tryLogLineContaining, which returns only the first match, this
// returns all of them. Used where a caller must distinguish which of several
// matching lines actually matters (e.g. AC7's per-review-ID check), rather
// than merely detecting presence or absence.
func allLogLinesContaining(env *Env, substring string, startOffset int64) ([]string, error) {
	f, err := os.Open(env.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, 0); err != nil {
		return nil, err
	}
	var matches []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if strings.Contains(line, substring) {
			matches = append(matches, line)
		}
		if err != nil {
			return matches, nil
		}
	}
}

// ── small internals ────────────────────────────────────────────────────────

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// readEnvFileValue returns the value of `key` from a .env-style file.
// Tolerates leading/trailing whitespace, surrounding quotes on the value,
// and blank/comment lines. Does not (yet) handle escapes or line
// continuations — keep secrets on single lines.
func readEnvFileValue(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != key {
			continue
		}
		val := strings.TrimSpace(parts[1])
		// Strip a single matched pair of surrounding double or single quotes.
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		return val, nil
	}
	return "", fmt.Errorf("%s not found in %s", key, path)
}

func ghOutput(env *Env, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+env.GHToken)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

// parseIssueNumberFromURL extracts the trailing integer from an issue URL,
// tolerating optional trailing slash and trailing whitespace.
// URL form: https://github.com/owner/repo/issues/NUM[/]
func parseIssueNumberFromURL(url string) int {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &n); err != nil {
		return 0
	}
	return n
}

// --- project board reads -------------------------------------------------
//
// These go through hand-written GraphQL rather than the `gh project`
// subcommands, because `gh project item-list` and `gh project field-list`
// resolve field values with a follow-up query per item. Their rate-limit cost
// therefore scales with the requested item limit rather than with the data
// actually needed. Measured against the live test board on 2026-07-28, over a
// 5,000 points/hour GraphQL budget:
//
//	gh project item-list --limit 200   ~101 points
//	gh project field-list              ~106 points
//	the equivalent queries below         ~2 points
//
// The wait-helpers poll every 10-15s, so at ~101 points a call a single
// scenario polling continuously cost ~24,000 points/hour — 4.8x the entire
// hourly budget on its own. A full E2E_PARALLEL=4 run exhausted the bed
// token's GraphQL budget in about 14 minutes, after which every remaining
// scenario failed on `gh` exit 1 (the failures look like engine regressions
// but are not). At ~2 points a call the same run costs ~1,900 points/hour and
// fits comfortably.
//
// So: do not "simplify" these back into `gh project` subcommands, and do not
// try to buy headroom by lengthening the poll intervals instead — at the old
// cost even a fully serial suite does not fit.
//
// Both queries select the project through repositoryOwner with inline
// fragments on Organization and User, so they work whether the test project is
// owned by an org (the current bed, owner_type: organization) or by a user.

// Both queries select `id` on projectV2 purely so the caller can tell a
// resolved-but-empty project from one that did not resolve at all. GitHub
// returns `projectV2: null` with NO top-level error for a valid owner and a
// wrong or stale project number, so `gh` exits 0 and an unguarded caller reads
// the result as "the board is empty".
//
// That distinction has to be drawn on the project node, not on the item count:
// an empty board is a legitimate, routine state — scripts/e2e/reset.sh drains
// every item as the first step of a clean run. Erroring on "no items" would
// make the harness fail spuriously against a freshly reset bed.
const boardItemsQuery = `
query($login:String!,$num:Int!,$after:String){
  repositoryOwner(login:$login){
    ... on Organization{ projectV2(number:$num){ id items(first:100, after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{ content{... on Issue{number repository{nameWithOwner}}}
             fieldValueByName(name:"Status"){... on ProjectV2ItemFieldSingleSelectValue{name}} } } } }
    ... on User{ projectV2(number:$num){ id items(first:100, after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{ content{... on Issue{number repository{nameWithOwner}}}
             fieldValueByName(name:"Status"){... on ProjectV2ItemFieldSingleSelectValue{name}} } } } }
  }
}`

// fields(first:50) is not paginated: a project with more than 50 custom fields
// would be pathological, and a page-through loop here would cost a round trip on
// every scenario's setup for a case that does not occur. pageInfo is selected so
// that if Status ever does fall outside the page, the error says so explicitly
// rather than claiming the field does not exist.
const statusFieldQuery = `
query($login:String!,$num:Int!){
  repositoryOwner(login:$login){
    ... on Organization{ projectV2(number:$num){ id
      fields(first:50){pageInfo{hasNextPage} nodes{... on ProjectV2SingleSelectField{id name options{id name}}}} } }
    ... on User{ projectV2(number:$num){ id
      fields(first:50){pageInfo{hasNextPage} nodes{... on ProjectV2SingleSelectField{id name options{id name}}}} } }
  }
}`

// boardItem is one issue on the project board, reduced to the fields the
// wait-helpers assert on. Repo is in "owner/repo" form.
type boardItem struct {
	Repo   string
	Number int
	Status string
}

// fetchBoardItems returns every issue item on the test board with its Status
// column, following pagination. Items whose content is not an issue (draft
// cards, PRs) are skipped. Note that an *archived* item drops off the board
// entirely and so is absent here — see WaitForMemberLanded for why that
// matters.
func fetchBoardItems(env *Env) ([]boardItem, error) {
	var items []boardItem
	after := ""
	// The board is small (tens of items); the bound is a runaway guard, not a
	// real limit — 20 pages is 2,000 items.
	for page := 0; page < 20; page++ {
		args := []string{"api", "graphql", "-f", "query=" + boardItemsQuery,
			"-f", "login=" + env.ProjectOwner, "-F", fmt.Sprintf("num=%d", env.ProjectNumber)}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		out, err := ghOutput(env, args...)
		if err != nil {
			return nil, fmt.Errorf("project board query: %w\n%s", err, out)
		}
		var resp struct {
			Data struct {
				RepositoryOwner struct {
					ProjectV2 struct {
						ID    string `json:"id"`
						Items struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
								Content struct {
									Number     int `json:"number"`
									Repository struct {
										NameWithOwner string `json:"nameWithOwner"`
									} `json:"repository"`
								} `json:"content"`
								FieldValueByName struct {
									Name string `json:"name"`
								} `json:"fieldValueByName"`
							} `json:"nodes"`
						} `json:"items"`
					} `json:"projectV2"`
				} `json:"repositoryOwner"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			return nil, fmt.Errorf("parse project board query: %w\n%s", err, out)
		}
		if resp.Data.RepositoryOwner.ProjectV2.ID == "" {
			return nil, fmt.Errorf("project %s/#%d not found or not visible to this token "+
				"(GitHub returns projectV2:null without a top-level error for a wrong project number)\n%s",
				env.ProjectOwner, env.ProjectNumber, out)
		}
		batch := resp.Data.RepositoryOwner.ProjectV2.Items
		for _, n := range batch.Nodes {
			if n.Content.Number == 0 {
				continue
			}
			items = append(items, boardItem{
				Repo:   n.Content.Repository.NameWithOwner,
				Number: n.Content.Number,
				Status: n.FieldValueByName.Name,
			})
		}
		if !batch.PageInfo.HasNextPage {
			return items, nil
		}
		after = batch.PageInfo.EndCursor
	}
	return items, nil
}

// findBoardItem returns the board item for repo#issueNumber and whether it was
// found. Repo comparison is case-insensitive, matching GitHub's own handling.
func findBoardItem(items []boardItem, repo string, issueNumber int) (boardItem, bool) {
	for _, it := range items {
		if it.Number == issueNumber && strings.EqualFold(it.Repo, repo) {
			return it, true
		}
	}
	return boardItem{}, false
}

// statusOption is one selectable column of the board's Status field.
type statusOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// statusField carries the IDs `gh project item-edit` needs to move an item,
// plus the full set of Status columns.
type statusField struct {
	ProjectID string
	FieldID   string
	Options   []statusOption
}

// fetchStatusField resolves the project's node ID and its Status single-select
// field in one query, replacing a `gh project view` + `gh project field-list`
// pair.
func fetchStatusField(env *Env) (statusField, error) {
	out, err := ghOutput(env, "api", "graphql", "-f", "query="+statusFieldQuery,
		"-f", "login="+env.ProjectOwner, "-F", fmt.Sprintf("num=%d", env.ProjectNumber))
	if err != nil {
		return statusField{}, fmt.Errorf("status field query: %w\n%s", err, out)
	}
	var resp struct {
		Data struct {
			RepositoryOwner struct {
				ProjectV2 struct {
					ID     string `json:"id"`
					Fields struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID      string         `json:"id"`
							Name    string         `json:"name"`
							Options []statusOption `json:"options"`
						} `json:"nodes"`
					} `json:"fields"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return statusField{}, fmt.Errorf("parse status field query: %w\n%s", err, out)
	}
	p := resp.Data.RepositoryOwner.ProjectV2
	if p.ID == "" {
		return statusField{}, fmt.Errorf("project %s/#%d not found or not visible to this token\n%s",
			env.ProjectOwner, env.ProjectNumber, out)
	}
	for _, f := range p.Fields.Nodes {
		if f.Name == "Status" {
			return statusField{ProjectID: p.ID, FieldID: f.ID, Options: f.Options}, nil
		}
	}
	if p.Fields.PageInfo.HasNextPage {
		return statusField{}, fmt.Errorf("project %s/#%d has no Status field among its first 50 fields, "+
			"and it has more — the query needs pagination", env.ProjectOwner, env.ProjectNumber)
	}
	return statusField{}, fmt.Errorf("project %s/#%d has no Status field", env.ProjectOwner, env.ProjectNumber)
}

func resolveStatusOption(t *testing.T, env *Env, columnName string) (projectID, statusFieldID, optionID string) {
	t.Helper()
	sf, err := fetchStatusField(env)
	if err != nil {
		t.Fatalf("resolve Status field: %v", err)
	}
	for _, o := range sf.Options {
		if o.Name == columnName {
			return sf.ProjectID, sf.FieldID, o.ID
		}
	}
	t.Fatalf("could not resolve Status option %q", columnName)
	return "", "", ""
}

// assertSentinelCheckRequired skips the test if "ci-fix-sentinel" is not a
// required status check on the repo's main branch. Skips (never fatals) so
// the test suite remains green before the sentinel CI job is enrolled.
func assertSentinelCheckRequired(t *testing.T, env *Env, repo string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/branches/main/protection", owner, name),
		"--jq", ".required_status_checks.contexts[]")
	if err != nil {
		t.Skipf("ci-fix-sentinel not enrolled as required check on %s/main (branch protection API error: %v)", repo, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "ci-fix-sentinel" {
			return
		}
	}
	t.Skipf("ci-fix-sentinel not in required status checks for %s/main — enroll it before running this test", repo)
}

// tryLinkedPRNumber returns the PR number for the given issue's feature branch,
// or (0, err) if not yet created or on transient error.
func tryLinkedPRNumber(env *Env, repo string, issueNumber int) (int, error) {
	branch := fmt.Sprintf("fabrik/issue-%d", issueNumber)
	out, err := ghOutput(env, "pr", "list", "-R", repo, "-H", branch,
		"--json", "number", "--jq", ".[0].number")
	if err != nil {
		return 0, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 0, fmt.Errorf("no PR for branch %s", branch)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse PR number %q: %w", out, err)
	}
	return n, nil
}

// LinkedPRNumber polls until the linked PR for the issue is created, then
// returns its PR number. Fails the test if no PR appears within 5 minutes.
func LinkedPRNumber(t *testing.T, env *Env, repo string, issueNumber int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		n, err := tryLinkedPRNumber(env, repo, issueNumber)
		if err == nil && n > 0 {
			return n
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for linked PR on %s#%d", repo, issueNumber)
	return 0
}

// tryPRComments returns the bodies of all comments on the given PR (using the
// issues comments API, which covers both issues and PRs).
func tryPRComments(env *Env, repo string, prNumber int) ([]string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, prNumber),
		"--jq", "[.[].body]")
	if err != nil {
		return nil, err
	}
	var bodies []string
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &bodies); uerr != nil {
		return nil, fmt.Errorf("parse PR comments: %w", uerr)
	}
	return bodies, nil
}

// WaitForPRCommentContaining polls PR comments every 15s until one contains
// the given substring, or timeout expires.
func WaitForPRCommentContaining(t *testing.T, env *Env, repo string, prNumber int, substring string, timeout time.Duration) {
	t.Helper()
	WaitForPRCommentContainingAny(t, env, repo, prNumber, []string{substring}, timeout)
}

// WaitForPRCommentContainingAny is WaitForPRCommentContaining for assertions
// that have more than one legitimate wording. It returns as soon as some
// comment contains ANY of substrings, so a caller can accept several equally
// correct engine messages instead of pinning the test to whichever one the
// current bed configuration happens to produce.
//
// Matching is case-sensitive, like WaitForPRCommentContaining. Callers wanting
// case-insensitivity should pass the variants explicitly rather than have this
// helper fold case silently — some engine messages embed deliberately
// capitalized GitHub enums (e.g. CHANGES_REQUESTED), and folding would make an
// assertion that means to pin the enum quietly stop doing so.
func WaitForPRCommentContainingAny(t *testing.T, env *Env, repo string, prNumber int, substrings []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		bodies, err := tryPRComments(env, repo, prNumber)
		if err != nil {
			t.Logf("WaitForPRCommentContainingAny: transient error on %s#%d: %v (will retry)", repo, prNumber, err)
			pollSleep(pollBase())
			continue
		}
		for _, b := range bodies {
			for _, substring := range substrings {
				if strings.Contains(b, substring) {
					return
				}
			}
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for PR comment containing any of %q on %s#%d", substrings, repo, prNumber)
}

// tryPRCheckRunConclusions resolves the head SHA of the PR and returns the
// conclusion for every check run on that SHA. In-progress or queued runs map
// to "pending" so callers can detect that checks are not yet settled.
// Returns (nil, err) on any failure.
func tryPRCheckRunConclusions(env *Env, repo string, prNumber int) ([]string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil, fmt.Errorf("bad repo: %q", repo)
	}
	shaOut, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, prNumber),
		"--jq", ".head.sha")
	if err != nil {
		return nil, fmt.Errorf("resolve head SHA: %w", err)
	}
	sha := strings.TrimSpace(shaOut)
	if sha == "" || sha == "null" {
		return nil, fmt.Errorf("empty head SHA for %s#%d", repo, prNumber)
	}
	checkOut, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", owner, name, sha),
		"--jq", "[.check_runs[] | .conclusion // \"pending\"]")
	if err != nil {
		return nil, fmt.Errorf("check runs: %w", err)
	}
	var conclusions []string
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(checkOut)), &conclusions); uerr != nil {
		return nil, fmt.Errorf("parse check runs: %w", uerr)
	}
	return conclusions, nil
}

// PRCheckRunConclusions returns the check-run conclusions for the PR's head SHA
// (in-progress runs appear as "pending"). Fails the test on error.
func PRCheckRunConclusions(t *testing.T, env *Env, repo string, prNumber int) []string {
	t.Helper()
	conclusions, err := tryPRCheckRunConclusions(env, repo, prNumber)
	if err != nil {
		t.Fatalf("PRCheckRunConclusions %s#%d: %v", repo, prNumber, err)
	}
	return conclusions
}

// tryNamedCheckConclusion returns the latest conclusion of the named check run
// on the PR's head SHA ("pending" if the run exists but has not completed, ""
// if no such check is present yet). Errors are transient (callers retry).
func tryNamedCheckConclusion(env *Env, repo string, prNumber int, checkName string) (string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("bad repo: %q", repo)
	}
	shaOut, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, prNumber),
		"--jq", ".head.sha")
	if err != nil {
		return "", fmt.Errorf("resolve head SHA: %w", err)
	}
	sha := strings.TrimSpace(shaOut)
	if sha == "" || sha == "null" {
		return "", fmt.Errorf("empty head SHA for %s#%d", repo, prNumber)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", owner, name, sha),
		"--jq", fmt.Sprintf("[.check_runs[] | select(.name==%q) | .conclusion // \"pending\"] | last // \"\"", checkName))
	if err != nil {
		return "", fmt.Errorf("check runs: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// WaitForCheckConclusion polls until the named check run on the PR reaches the
// wanted conclusion, or fails after timeout. Use it to establish a precondition
// (e.g. confirm ci-fix-sentinel has genuinely FAILED) before asserting on gate
// behavior — fabrik:awaiting-ci alone does not prove CI is failing.
func WaitForCheckConclusion(t *testing.T, env *Env, repo string, prNumber int, checkName, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		c, err := tryNamedCheckConclusion(env, repo, prNumber, checkName)
		if err == nil {
			last = c
			if c == want {
				return
			}
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for check %q to reach conclusion %q on %s#%d (last observed %q)",
		checkName, want, repo, prNumber, last)
}

// AssertPRDidNotTouchWorkflows fails if the PR modifies any file under .github/.
// The CI workflow (including the ci-fix-sentinel job) is immutable test
// infrastructure; an agent editing it invalidates CI-gating scenarios.
func AssertPRDidNotTouchWorkflows(t *testing.T, env *Env, repo string, prNumber int) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/pulls/%d/files", owner, name, prNumber),
		"--jq", ".[].filename")
	if err != nil {
		t.Fatalf("list PR files for %s#%d: %v", repo, prNumber, err)
	}
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(f), ".github/") {
			t.Fatalf("PR #%d modified protected path %q — the CI workflow is immutable test infrastructure and must not be edited by the agent",
				prNumber, strings.TrimSpace(f))
		}
	}
}

// tryPRCommitCount returns the number of commits on the PR, or (0, err) on
// failure.
func tryPRCommitCount(env *Env, repo string, prNumber int) (int, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return 0, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/commits", owner, name, prNumber),
		"--jq", "length")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse commit count %q: %w", out, err)
	}
	return n, nil
}

// PRCommitCount returns the number of commits on the PR. Fails the test on error.
func PRCommitCount(t *testing.T, env *Env, repo string, prNumber int) int {
	t.Helper()
	n, err := tryPRCommitCount(env, repo, prNumber)
	if err != nil {
		t.Fatalf("PRCommitCount %s#%d: %v", repo, prNumber, err)
	}
	return n
}

// readEnvFileMaxCiFixCycles reads FABRIK_MAX_CI_FIX_CYCLES from the test bed's
// .env file. Returns 5 (the engine default) if the key is absent. Fails the
// test if the key is present but not a valid integer.
func readEnvFileMaxCiFixCycles(t *testing.T, env *Env) int {
	t.Helper()
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	val, err := readEnvFileValue(envFile, "FABRIK_MAX_CI_FIX_CYCLES")
	if err != nil {
		// Key absent — return engine default.
		return 5
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		t.Fatalf("FABRIK_MAX_CI_FIX_CYCLES in %s is not an integer: %q", envFile, val)
	}
	return n
}

// readEnvFileMaxReviewCycles reads FABRIK_MAX_REVIEW_CYCLES from the test
// bed's .env file. Returns 5 (the engine default) if the key is absent.
// Fails the test if the key is present but not a valid integer.
func readEnvFileMaxReviewCycles(t *testing.T, env *Env) int {
	t.Helper()
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	val, err := readEnvFileValue(envFile, "FABRIK_MAX_REVIEW_CYCLES")
	if err != nil {
		// Key absent — return engine default.
		return 5
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		t.Fatalf("FABRIK_MAX_REVIEW_CYCLES in %s is not an integer: %q", envFile, val)
	}
	return n
}

// writeEnvFileValue replaces (or appends) a KEY=value line in a .env-style
// file, preserving unrelated lines, blank lines, and comments as-is. Creates
// the file if it doesn't exist yet. Unlike readEnvFileValue, this does a
// literal "key=" line match rather than trimming/quote-stripping the value —
// it owns line construction on the write side, so there's nothing to strip.
func writeEnvFileValue(path, key, value string) error {
	var lines []string
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		lines = strings.Split(string(data), "\n")
		// strings.Split on a trailing newline yields a spurious final empty
		// element; drop it so re-joining doesn't accumulate blank lines.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	case os.IsNotExist(err):
		// No file yet — start from an empty line set.
	default:
		return err
	}

	newLine := key + "=" + value
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == key {
			lines[i] = newLine
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, newLine)
	}

	// 0o600: .env files carry secrets (FABRIK_TOKEN). Only applies if the file
	// doesn't already exist — os.WriteFile does not chmod an existing file.
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// normalizeTrainMode validates and lowercases a mode string, accepting only
// an exact case-insensitive "on" or "off" (surrounding whitespace tolerated).
// Pulled out of resolveTrainMode as a pure function so its validation logic
// is unit-testable without a *testing.T (a t.Fatalf-driven parent test
// can't distinguish "the fatal path fired as designed" from "the test
// itself failed" — Go propagates a failed subtest's failure to its parent).
func normalizeTrainMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on":
		return "on", nil
	case "off":
		return "off", nil
	default:
		return "", fmt.Errorf("invalid mode %q (must be on or off)", v)
	}
}

// resolveTrainMode determines the e2e suite's merge-train mode for the
// current process. E2E_TRAIN_MODE (set by scripts/e2e/run.sh's two-mode
// validation gate — see TestSwitchTrainMode) takes precedence when set,
// satisfying FR-2: mode must be explicit and legible at the suite level, not
// an ambient property of the bed's .env that a reader has to go look up. An
// explicit-but-invalid E2E_TRAIN_MODE is a hard t.Fatalf — unlike the lenient
// .env fallback below, a wrong explicit signal should be loud, not silently
// defaulted.
//
// When E2E_TRAIN_MODE is unset (ad-hoc/manual bed usage — running go test
// directly against a bed someone configured by hand), falls back to
// readEnvFileMergeTrainMode's lenient read of the bed's own .env file.
func resolveTrainMode(t *testing.T, env *Env) string {
	t.Helper()
	if v := os.Getenv("E2E_TRAIN_MODE"); v != "" {
		mode, err := normalizeTrainMode(v)
		if err != nil {
			t.Fatalf("E2E_TRAIN_MODE=%q is invalid (must be on or off)", v)
		}
		return mode
	}
	return readEnvFileMergeTrainMode(t, env)
}

// readEnvFileMergeTrainMode reads FABRIK_MERGE_TRAIN from the test bed's .env
// file, normalizing exactly like cmd/root.go's mergeTrainMode: missing/empty
// or unrecognized values default to "off" (logged via t.Logf, not stderr),
// and only an exact case-insensitive "on" returns "on". This is the fallback
// path behind resolveTrainMode, used when E2E_TRAIN_MODE is unset.
func readEnvFileMergeTrainMode(t *testing.T, env *Env) string {
	t.Helper()
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	val, err := readEnvFileValue(envFile, "FABRIK_MERGE_TRAIN")
	if err != nil {
		return "off"
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "on":
		return "on"
	case "off", "":
		return "off"
	default:
		t.Logf("FABRIK_MERGE_TRAIN=%q in %s is invalid (must be on or off); using default off", val, envFile)
		return "off"
	}
}

// assertTrainPoisonGuardRequired skips the test if "train-poison-guard" is not
// a required status check on the repo's main branch. Accepts repo as a
// parameter so it works for both Alpha and Beta repos. Skips (never fatals) so
// the test suite remains green before the guard CI job is enrolled.
func assertTrainPoisonGuardRequired(t *testing.T, env *Env, repo string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/branches/main/protection", owner, name),
		"--jq", ".required_status_checks.contexts[]")
	if err != nil {
		t.Skipf("train-poison-guard not enrolled as required check on %s/main (branch protection API error: %v)", repo, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "train-poison-guard" {
			return
		}
	}
	t.Skipf("train-poison-guard not in required status checks for %s/main — enroll it before running this test", repo)
}

// assertSlowGateRequired skips the test if "slow-gate" is not a required
// status check on the repo's main branch. Skips (never fatals) so the test
// suite remains green before the slow-gate CI job is enrolled.
func assertSlowGateRequired(t *testing.T, env *Env, repo string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/branches/main/protection", owner, name),
		"--jq", ".required_status_checks.contexts[]")
	if err != nil {
		t.Skipf("slow-gate not enrolled as required check on %s/main (branch protection API error: %v)", repo, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "slow-gate" {
			return
		}
	}
	t.Skipf("slow-gate not in required status checks for %s/main — enroll it before running this test", repo)
}

// CommentOnPR posts a comment on the PR using the test bed's PAT identity.
// PR numbers differ from issue numbers; use this helper (not CommentOnIssue)
// when targeting a PR's comment thread.
func CommentOnPR(t *testing.T, env *Env, repo string, prNumber int, body string) {
	t.Helper()
	if _, err := ghOutput(env, "pr", "comment", fmt.Sprint(prNumber), "-R", repo, "--body", body); err != nil {
		t.Fatalf("post comment on %s PR #%d: %v", repo, prNumber, err)
	}
}

// readEnvFileReviewerToken reads FABRIK_REVIEWER_TOKEN from the test bed's
// .env file. Returns "" if absent — callers branch on the empty string to
// decide whether to run the approval path or the review-timeout fallback.
func readEnvFileReviewerToken(t *testing.T, env *Env) string {
	t.Helper()
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	val, err := readEnvFileValue(envFile, "FABRIK_REVIEWER_TOKEN")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// readEnvFileReviewWaitTimeout reads FABRIK_REVIEW_WAIT_TIMEOUT (minutes)
// from the test bed's .env file. Returns 15 (the engine default) if absent.
// Fails the test if the key is present but not a valid integer.
func readEnvFileReviewWaitTimeout(t *testing.T, env *Env) int {
	t.Helper()
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	val, err := readEnvFileValue(envFile, "FABRIK_REVIEW_WAIT_TIMEOUT")
	if err != nil {
		return 15
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		t.Fatalf("FABRIK_REVIEW_WAIT_TIMEOUT in %s is not an integer: %q", envFile, val)
	}
	return n
}

// ghOutputWithToken runs gh with a caller-supplied token (overrides GH_TOKEN).
// Used by SubmitPRReview where the reviewer identity differs from the bot's
// main PAT stored in *Env.
func ghOutputWithToken(token string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SubmitPRReview submits a review on the PR using reviewerToken (a PAT for a
// non-author identity — GitHub forbids the PR author from approving their own
// PR). action must be "APPROVE" or "REQUEST_CHANGES" (GitHub API event values).
// GitHub's API requires a non-empty body for REQUEST_CHANGES/COMMENT (only
// APPROVE may omit it), so a fixed body is always sent — harmless for APPROVE.
// Fails the test on API error (e.g. 422 if reviewerToken == env.GHToken).
func SubmitPRReview(t *testing.T, env *Env, reviewerToken string, repo string, prNumber int, action string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, name, prNumber)
	out, err := ghOutputWithToken(reviewerToken, "api", "-X", "POST", path,
		"-f", "event="+action,
		"-f", "body=e2e harness review ("+action+")")
	if err != nil {
		t.Fatalf("SubmitPRReview %s on %s PR #%d: %v\n%s", action, repo, prNumber, err, out)
	}
}

// TokenLogin returns the GitHub login that owns the given token. Used to resolve
// the reviewer identity so it can be requested as a PR reviewer.
func TokenLogin(t *testing.T, token string) string {
	t.Helper()
	out, err := ghOutputWithToken(token, "api", "user", "--jq", ".login")
	if err != nil {
		t.Fatalf("resolve token login: %v\n%s", err, out)
	}
	login := strings.TrimSpace(out)
	if login == "" {
		t.Fatalf("empty login for token")
	}
	return login
}

// RequestPRReviewer requests reviewerLogin as a reviewer on the PR, using the
// harness's primary token (the PR-author identity). The engine's review gate
// (fabrik:awaiting-review) only engages when an outstanding reviewer request
// exists on the PR, so review-gate tests must establish one before the gated
// stage completes.
func RequestPRReviewer(t *testing.T, env *Env, repo string, prNumber int, reviewerLogin string) {
	t.Helper()
	out, err := ghOutput(env, "pr", "edit", fmt.Sprint(prNumber), "-R", repo, "--add-reviewer", reviewerLogin)
	if err != nil {
		t.Fatalf("RequestPRReviewer %s on %s PR #%d: %v\n%s", reviewerLogin, repo, prNumber, err, out)
	}
}

// AssertPRAuthorIsExpectedIdentity fails the test fast if the PR's actual
// author (as observed on GitHub) does not match the login that env.GHToken
// resolves to. The engine authenticates with the same token precedence as
// this harness's env.GHToken (FABRIK_TOKEN, falling back to GITHUB_TOKEN);
// if the engine process's shell shadowed FABRIK_TOKEN with a different
// identity, PRs come out authored by that other identity instead — the
// exact confound (handarbeit/fabrik#925) that silently breaks
// RequestPRReviewer (GitHub forbids requesting/approving a review from the
// PR author) and wastes a full 60-100 min run before failing downstream.
// Checking PR authorship directly against GitHub (not the test bed's .env)
// catches either possible shadowing mechanism within seconds.
func AssertPRAuthorIsExpectedIdentity(t *testing.T, env *Env, repo string, prNumber int) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "pr", "view", fmt.Sprint(prNumber), "-R", repo, "--json", "author", "--jq", ".author.login")
	if err != nil {
		t.Fatalf("read author of %s/%s PR #%d: %v\n%s", owner, name, prNumber, err, out)
	}
	actual := strings.TrimSpace(out)
	expected := TokenLogin(t, env.GHToken)
	if actual != expected {
		t.Fatalf("engine identity mismatch: PR #%d on %s was authored by %q but the test bed's token resolves to %q — "+
			"the engine process is very likely authenticating as a different identity than FABRIK_TOKEN "+
			"(e.g. FABRIK_TOKEN itself shadowed by an export in the launching shell; see handarbeit/fabrik#925 Confound 1)",
			prNumber, repo, actual, expected)
	}
}

// WaitForPRCommentReaction polls PR comments (via the issues comments API)
// every 15s until a comment containing commentSubstring has at least one
// reaction of type reactionContent (e.g. "eyes" for 👀, "rocket" for 🚀).
// Returns as soon as the count is > 0; fails the test on timeout.
func WaitForPRCommentReaction(t *testing.T, env *Env, repo string, prNumber int, commentSubstring string, reactionContent string, timeout time.Duration) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	// jq: sum the named reaction count across all comments whose body contains
	// the substring; default 0 when no match.
	escaped := strings.ReplaceAll(commentSubstring, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	filter := fmt.Sprintf(`[.[] | select(.body | contains("%s")) | .reactions.%s] | add // 0`,
		escaped, reactionContent)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := ghOutput(env, "api",
			fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, prNumber),
			"--jq", filter)
		if err != nil {
			t.Logf("WaitForPRCommentReaction: transient error on %s PR #%d: %v (will retry)", repo, prNumber, err)
			pollSleep(pollBase())
			continue
		}
		count, parseErr := strconv.Atoi(strings.TrimSpace(out))
		if parseErr == nil && count > 0 {
			return
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out waiting for %q reaction on comment containing %q on %s PR #%d",
		reactionContent, commentSubstring, repo, prNumber)
}
