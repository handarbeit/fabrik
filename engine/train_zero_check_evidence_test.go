package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// #2593: a zero-check trial or landing head is "CI not started yet", not "no CI", when a
// member's own PR head carries check runs. Every held test has a neutralised twin that
// must land with SetZeroCheckMemberEvidenceDisabledForTest(true).

var ciOK = []gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}}

func evidenceMembers() []trainMember {
	return []trainMember{{prNum: 11, headSHA: "m11"}, {prNum: 12, headSHA: "m12"}}
}

// zeroCheckClient: the trial/landing head "sha123" has zero check runs; members read by
// their own head SHA from memberRuns.
func zeroCheckClient(memberRuns map[string][]gh.CheckRun, memberErr error) *mockGitHubClient {
	return &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) { return nil, "clean", nil },
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, MergeableState: "clean", HeadSHA: "sha123"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			if sha == "sha123" {
				return nil, nil
			}
			if memberErr != nil {
				return nil, memberErr
			}
			return memberRuns[sha], nil
		},
		mergePRAtHeadSHAFn: func(owner, repo string, n int, sha string) error { return nil },
	}
}

func evalOnce(t *testing.T, eng *Engine, members []trainMember) (TrainCIResult, bool) {
	t.Helper()
	st := trialCIState{members: members}
	res, _, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st)
	return res, decided
}

func TestMemberCIEvidence(t *testing.T) {
	cases := []struct {
		name     string
		runs     map[string][]gh.CheckRun
		err      error
		members  []trainMember
		wantHold bool
	}{
		{"member with runs", map[string][]gh.CheckRun{"m12": ciOK}, nil, evidenceMembers(), true},
		{"no member has runs (no-CI repo)", nil, nil, evidenceMembers(), false},
		{"read error, no evidence", nil, errors.New("boom"), evidenceMembers(), true},
		{"nil members", nil, nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng := trainTestEngine(t, zeroCheckClient(c.runs, c.err), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
			hold, why := eng.memberCIEvidence("owner", "repo", c.members)
			if hold != c.wantHold {
				t.Fatalf("hold=%v (%s), want %v", hold, why, c.wantHold)
			}
			if hold && why == "" {
				t.Fatal("a hold must say why")
			}
			eng.SetZeroCheckMemberEvidenceDisabledForTest(true)
			if hold, _ := eng.memberCIEvidence("owner", "repo", c.members); hold {
				t.Fatal("the seam must disable the rule")
			}
		})
	}
}

func TestEvalTrialCI_ZeroChecks_MemberHasChecks_Held(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(map[string][]gh.CheckRun{"m11": ciOK}, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	if res, decided := evalOnce(t, eng, evidenceMembers()); decided || res != TrainCIPending {
		t.Fatalf("res=%v decided=%v, want pending/undecided while members carry check runs", res, decided)
	}
}

func TestEvalTrialCI_ZeroChecks_MemberHasChecks_NeutralisedLands(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(map[string][]gh.CheckRun{"m11": ciOK}, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.SetZeroCheckMemberEvidenceDisabledForTest(true)
	if res, decided := evalOnce(t, eng, evidenceMembers()); !decided || res != TrainCIGreen {
		t.Fatalf("res=%v decided=%v, want green with the rule neutralised (the pre-fix defect)", res, decided)
	}
}

func TestEvalTrialCI_ZeroChecks_NoCIRepoStillLands(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(nil, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	if res, decided := evalOnce(t, eng, evidenceMembers()); !decided || res != TrainCIGreen {
		t.Fatalf("res=%v decided=%v, want green for a repository whose members carry no check runs", res, decided)
	}
}

func TestEvalTrialCI_ZeroChecks_EvidenceReadErrorHolds(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(nil, errors.New("503")), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	if res, decided := evalOnce(t, eng, evidenceMembers()); decided || res != TrainCIPending {
		t.Fatalf("res=%v decided=%v, want pending when the evidence cannot be read", res, decided)
	}
}

func TestEvalTrialCI_ZeroChecks_RealCIThenDecides(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []gh.CheckRun
		want TrainCIResult
	}{
		{"passes", ciOK, TrainCIGreen},
		{"fails", []gh.CheckRun{{ID: 2, Name: "build", Status: "completed", Conclusion: "failure"}}, TrainCIRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			appeared := false
			c := zeroCheckClient(map[string][]gh.CheckRun{"m11": ciOK}, nil)
			base := c.fetchCheckRunsFn
			c.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) {
				mu.Lock()
				defer mu.Unlock()
				if sha == "sha123" && appeared {
					return tc.runs, nil
				}
				return base(owner, repo, sha)
			}
			eng := trainTestEngine(t, c, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
			// Red re-runs once before judging; with no Actions run behind it, it is red at once.
			st := trialCIState{members: evidenceMembers()}
			if _, _, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st); decided {
				t.Fatal("must be held before CI appears")
			}
			mu.Lock()
			appeared = true
			mu.Unlock()
			res, _, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st)
			if !decided || res != tc.want {
				t.Fatalf("res=%v decided=%v, want %v once real CI appears", res, decided, tc.want)
			}
		})
	}
}

func TestEvalTrialCI_ZeroChecks_HoldLogsOneLine(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(map[string][]gh.CheckRun{"m12": ciOK}, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	var lines []string
	eng.SetLogSinkForTest(func(_ int, _ string, msg string) { lines = append(lines, msg) })
	evalOnce(t, eng, evidenceMembers())
	n := 0
	for _, l := range lines {
		if containsAll(l, "trial sha123", "member PR #12", "CI not started yet", "waiting") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one hold line, got %d in %q", n, lines)
	}
}

// ── landing settle ──────────────────────────────────────────────────────────

func settleZero(t *testing.T, eng *Engine, members []trainMember) landingResult {
	t.Helper()
	eng.cfg.CIBackstopTimeout = 80 * time.Millisecond
	eng.SetTrainCIPollIntervalForTest(5 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return eng.settleAndMerge(ctx, "owner", "repo", 42, "", members)
}

func TestSettleAndMerge_ZeroChecks_MemberHasChecks_HeldToBackstop(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(map[string][]gh.CheckRun{"m11": ciOK}, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	res := settleZero(t, eng, evidenceMembers())
	if res.outcome != outcomeBackstopExpired {
		t.Fatalf("outcome=%v, want backstop expiry (held, not merged)", res.outcome)
	}
	if !strings.Contains(res.reason, "CI had not started") {
		t.Fatalf("reason %q should say CI had not started", res.reason)
	}
}

func TestSettleAndMerge_ZeroChecks_MemberHasChecks_NeutralisedMerges(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(map[string][]gh.CheckRun{"m11": ciOK}, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.SetZeroCheckMemberEvidenceDisabledForTest(true)
	if res := settleZero(t, eng, evidenceMembers()); res.outcome != outcomeMerged {
		t.Fatalf("outcome=%v, want merged with the rule neutralised (the pre-fix defect)", res.outcome)
	}
}

func TestSettleAndMerge_ZeroChecks_NoCIRepoMerges(t *testing.T) {
	eng := trainTestEngine(t, zeroCheckClient(nil, nil), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	if res := settleZero(t, eng, evidenceMembers()); res.outcome != outcomeMerged {
		t.Fatalf("outcome=%v, want merged for a repository with no check-run CI", res.outcome)
	}
}
