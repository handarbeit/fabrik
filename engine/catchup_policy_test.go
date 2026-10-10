package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// Neutralisation (#2065). Each test fails with the branch it names removed:
//   - Classify* / fixtures        : loosen classifyCatchUpPushRejection (drop a veto, the marker
//     requirement or the secrets exclusion).
//   - PolicyRejectionSkips*       : remove the catchUpPolicyBackoff consult in trySingletonCatchUp
//     (and PolicyMemoSkipDisabled* proves it by switching the consult off).
//   - NonPolicy*                  : record a memo for a fallback without policyReason.
//   - MemoExpires*                : drop the expiry check in catchUpPolicyBackoff.
//   - OneLogLine*                 : drop the isNew gate on the operator line.

const catchUpFixtureDir = "testdata/catchup_push_rejections"

func readCatchUpFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(catchUpFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(b), "\n---\n")
	if !ok {
		t.Fatalf("fixture %s has no provenance header terminator", name)
	}
	return body
}

func TestClassifyCatchUpPushRejection_Fixtures(t *testing.T) {
	cases := []struct {
		file   string
		policy bool
		reason string // substring of the extracted reason, for policy cases
	}{
		{"linear-history-gh013.txt", true, "must not contain merge commits"},
		{"ruleset-merge-commit-gh013.txt", true, "Merge commits are not allowed"},
		{"protected-branch-gh006.txt", true, "GH006"},
		{"required-signatures-gh013.txt", true, "verified signatures"},
		{"secret-scanning-gh013.txt", false, ""},
		{"lease-stale-info.txt", false, ""},
		{"transport-unable-to-access.txt", false, ""},
		{"transport-502.txt", false, ""},
		{"permission-denied-403.txt", false, ""},
		{"bare-protected-branch-prose.txt", false, ""},
		{"veto-beats-marker.txt", false, ""},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			reason, policy := classifyCatchUpPushRejection(readCatchUpFixture(t, c.file))
			if policy != c.policy {
				t.Fatalf("policy = %v, want %v (reason %q)", policy, c.policy, reason)
			}
			if !c.policy {
				if reason != "" {
					t.Errorf("a non-policy rejection must carry no reason, got %q", reason)
				}
				return
			}
			if !strings.Contains(reason, c.reason) {
				t.Errorf("reason %q lacks %q", reason, c.reason)
			}
			if strings.ContainsAny(reason, "\n\r\t") {
				t.Errorf("reason is not one line: %q", reason)
			}
			if strings.Contains(reason, "remote:") {
				t.Errorf("reason still carries the remote: prefix: %q", reason)
			}
		})
	}
}

func TestCatchUpPolicyReason_IsSanitisedAndCapped(t *testing.T) {
	out := "remote: error: GH013: Repository rule violations found.\n" +
		"remote: - bad\x1b[31m\x00 text\twith controls\n" +
		"remote: - " + strings.Repeat("x", 1000) + "\n"
	reason, policy := classifyCatchUpPushRejection(out)
	if !policy {
		t.Fatal("expected a policy rejection")
	}
	for _, r := range reason {
		if r < ' ' || r == 0x7f {
			t.Fatalf("control character %q in %q", r, reason)
		}
	}
	if n := len([]rune(reason)); n > catchUpPolicyReasonMax+1 {
		t.Errorf("reason is %d runes, cap is %d", n, catchUpPolicyReasonMax)
	}
	if strings.Contains(reason, "\n") {
		t.Error("reason spans lines")
	}
}

// fakeClock is a settable Clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

const policyTestReason = "GH013: Repository rule violations found; This branch must not contain merge commits."

func policyOutcome() catchUpGitOutcome {
	return catchUpGitOutcome{kind: catchUpFallback, reason: catchUpPolicyAttemptReason, policyReason: policyTestReason}
}

// policyLogLines returns the operator memo lines drained from the engine's event channel.
func policyLogLines(events chan tui.Event) []string {
	var lines []string
	for {
		select {
		case ev := <-events:
			if le, ok := ev.(tui.LogEvent); ok && strings.Contains(le.Message, "rejects catch-up pushes by repository policy") {
				lines = append(lines, le.Message)
			}
		default:
			return lines
		}
	}
}

func newPolicyWorld(t *testing.T) (*catchUpWorld, *fakeClock, chan tui.Event) {
	t.Helper()
	w := newCatchUpWorld(t)
	clk := &fakeClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	w.eng.SetClock(clk)
	events := make(chan tui.Event, 64)
	w.eng.events = events
	w.gitOut = policyOutcome()
	return w, clk, events
}

func TestSingletonCatchUp_PolicyRejectionSkipsTheNextSingletonOnThatRepo(t *testing.T) {
	w, _, events := newPolicyWorld(t)

	if got, decided := w.run(); decided || got.headSHA != cuMemberHead {
		t.Fatalf("a policy rejection must hand the untouched member to the trial, got (%+v, %v)", got, decided)
	}
	if w.gitCalls.Load() != 1 {
		t.Fatalf("first singleton: git calls = %d, want 1", w.gitCalls.Load())
	}

	// Another member on the same repo (and another partition of it).
	w.m.item.Number = 10
	w.p.trainKey = "owner/repo\x00release"
	behindCalls := 0
	w.client.mu.Lock()
	inner := w.client.fetchCommitsBehindFn
	w.client.fetchCommitsBehindFn = func(o, r, b, h string) (int, error) { behindCalls++; return inner(o, r, b, h) }
	w.client.mu.Unlock()

	if got, decided := w.run(); decided || got.headSHA != cuMemberHead {
		t.Fatalf("a memoised repo must hand the member to the trial, got (%+v, %v)", got, decided)
	}
	if w.gitCalls.Load() != 1 {
		t.Errorf("the catch-up was attempted despite the memo (git calls = %d)", w.gitCalls.Load())
	}
	if behindCalls != 0 {
		t.Errorf("a skipped singleton must not even ask GitHub whether it is behind (%d calls)", behindCalls)
	}
	w.assertNothingLandedOrCharged()
	if n := len(policyLogLines(events)); n != 1 {
		t.Errorf("operator lines = %d, want exactly 1 across both singletons", n)
	}
}

func TestSingletonCatchUp_PolicyMemoIsPerRepo(t *testing.T) {
	w, _, _ := newPolicyWorld(t)
	w.run()

	w.p.repo = "other"
	w.run()
	if w.gitCalls.Load() != 2 {
		t.Errorf("a different repo must still be probed (git calls = %d, want 2)", w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_NonPolicyFallbackSetsNoMemo(t *testing.T) {
	w, _, events := newPolicyWorld(t)
	// A lease failure / transport error: fallback without a policy reason.
	w.gitOut = catchUpGitOutcome{kind: catchUpFallback, reason: "pushing catch-up to fabrik/issue-9: stale info"}

	for i := 0; i < 3; i++ {
		if _, decided := w.run(); decided {
			t.Fatal("a fallback must hand the member to the trial")
		}
	}
	if w.gitCalls.Load() != 3 {
		t.Errorf("git calls = %d, want 3 — an ambiguous rejection must be re-attempted every poll", w.gitCalls.Load())
	}
	if n := len(policyLogLines(events)); n != 0 {
		t.Errorf("operator lines = %d, want 0", n)
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_PolicyMemoExpiresAndReprobes(t *testing.T) {
	w, clk, events := newPolicyWorld(t)
	w.run()
	clk.t = clk.t.Add(catchUpPolicyMemoTTL - time.Minute)
	w.run()
	if w.gitCalls.Load() != 1 {
		t.Fatalf("before expiry the repo must stay skipped (git calls = %d)", w.gitCalls.Load())
	}

	clk.t = clk.t.Add(2 * time.Minute)
	w.run()
	if w.gitCalls.Load() != 2 {
		t.Fatalf("after expiry the repo must be re-probed (git calls = %d, want 2)", w.gitCalls.Load())
	}
	// The re-probe was rejected again: a fresh memo, a fresh line.
	if n := len(policyLogLines(events)); n != 2 {
		t.Errorf("operator lines = %d, want 2 (one per memo)", n)
	}
	w.run()
	if w.gitCalls.Load() != 2 {
		t.Errorf("the new memo must skip again (git calls = %d)", w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_PolicyMemoOneLogLinePerMemo(t *testing.T) {
	w, _, events := newPolicyWorld(t)
	w.run()
	// A second rejection racing in while the memo is active: ignored.
	if w.eng.recordCatchUpPolicyRejection("owner/repo", "again") {
		t.Error("a rejection during an active memo must not report as new")
	}
	for i := 0; i < 3; i++ {
		w.run()
	}
	lines := policyLogLines(events)
	if len(lines) != 1 {
		t.Fatalf("operator lines = %d, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{"owner/repo", policyTestReason, "singleton_catch_up: off", "--singleton-catch-up", "FABRIK_SINGLETON_CATCH_UP"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("operator line lacks %q: %s", want, lines[0])
		}
	}
	if strings.Count(strings.TrimSuffix(lines[0], "\n"), "\n") != 0 {
		t.Errorf("operator line spans lines: %q", lines[0])
	}
}

func TestSingletonCatchUp_PolicyMemoDoesNotOverrideOff(t *testing.T) {
	w, _, _ := newPolicyWorld(t)
	w.run()
	w.eng.cfg.SingletonCatchUp = "off"
	if _, decided := w.run(); decided || w.gitCalls.Load() != 1 {
		t.Errorf("off must stay off (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
}

// Neutralisation: with the consult disabled the memo-skip scenario attempts the push again,
// so the skip assertions above are not vacuous.
func TestSingletonCatchUp_PolicyMemoSkipDisabledReattempts(t *testing.T) {
	w, _, events := newPolicyWorld(t)
	w.eng.SetCatchUpPolicyMemoSkipDisabledForTest(true)

	w.run()
	w.run()
	if w.gitCalls.Load() != 2 {
		t.Fatalf("git calls = %d, want 2 — with the skip disabled every singleton attempts the push", w.gitCalls.Load())
	}
	if n := len(policyLogLines(events)); n != 1 {
		t.Errorf("operator lines = %d, want 1 (the memo is still set once)", n)
	}
}
