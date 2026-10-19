package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #1945 sim scenarios: the engine's self-write baseline is anchored to GitHub's
// timestamp for the write, never to the daemon's clock, so an external write made
// after an engine write is seen on the next poll whichever way the two clocks are
// skewed. The sim clock is GitHub's clock; the host's time.Now() is the daemon's.
// BoardCache is required: against the live adapter every read is already fresh.
//
// Each scenario: the engine pauses an item (its comment and labels are the
// self-writes), GitHub's clock then moves one second and an external actor writes,
// and the test polls until the engine's Store reflects the write.

const (
	selfWriteExternalBody    = "body edited by a human"
	selfWriteExternalLabel   = "human-added-label"
	selfWriteExternalComment = "comment edited by a human"
	selfWriteNewComment      = "a human reply right after the pause comment"
	selfWriteMaxPolls        = 30
)

// skew is host clock minus GitHub clock: positive = the daemon's clock is ahead.
var selfWriteSkews = []struct {
	name string
	skew time.Duration
}{
	{"host-ahead", 45 * time.Second},
	{"no-skew", 0},
	{"host-behind", -45 * time.Second},
}

type externalWrite struct {
	name string
	do   func(env *Env, num int)
	seen func(snap itemSnapshot) bool
}

// itemSnapshot is the slice of the engine's Store the scenarios read.
type itemSnapshot struct {
	body     string
	labels   []string
	comments []gh.Comment
}

var selfWriteExternalWrites = []externalWrite{
	{
		name: "body-edit",
		do: func(env *Env, num int) {
			env.Sim.Sim().SeedBodyEdit(env.OwnerRepo, num, selfWriteExternalBody)
		},
		seen: func(s itemSnapshot) bool { return s.body == selfWriteExternalBody },
	},
	{
		name: "label-change",
		do: func(env *Env, num int) {
			env.Sim.Sim().SeedLabelAdded(env.OwnerRepo, num, selfWriteExternalLabel)
		},
		seen: func(s itemSnapshot) bool {
			for _, l := range s.labels {
				if l == selfWriteExternalLabel {
					return true
				}
			}
			return false
		},
	},
	{
		name: "comment-edit",
		do: func(env *Env, num int) {
			comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
			if err != nil || len(comments) == 0 {
				panic("self-write scenario: the engine's pause comment is missing")
			}
			env.Sim.Sim().SeedCommentEdit(env.OwnerRepo, num, comments[0].DatabaseID, selfWriteExternalComment)
		},
		seen: func(s itemSnapshot) bool {
			for _, c := range s.comments {
				if c.Body == selfWriteExternalComment {
					return true
				}
			}
			return false
		},
	},
	{
		name: "new-comment",
		do: func(env *Env, num int) {
			env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", selfWriteNewComment)
		},
		seen: func(s itemSnapshot) bool {
			for _, c := range s.comments {
				if strings.Contains(c.Body, selfWriteNewComment) {
					return true
				}
			}
			return false
		},
	},
}

func selfWriteStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// pausedItem brings up an env whose GitHub clock is the host's minus skew, has the
// engine pause issue #N (an engine comment plus label writes), and returns the env
// and the issue number. legacy restores the daemon-clock baseline.
func pausedItem(t *testing.T, skew time.Duration, legacy bool) (*Env, int) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:     selfWriteStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now().Add(-skew).Truncate(time.Second),
		BoardCache: true,
	})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Engine.SetLegacyLocalClockSelfWriteForTest(legacy)
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_BLOCKED_ON_INPUT\nFABRIK_SUMMARY_BEGIN\nwhat is the answer?\nFABRIK_SUMMARY_END\n", false, engine.TokenUsage{}, nil
	})
	env.Claude.ForStageComments("Specify", simclaude.DefaultCommentScript)
	num := FileIssue(t, env, "self-write baseline (#1945)", "original body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 80)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env, num
}

func snapshotOf(env *Env, num int) itemSnapshot {
	st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, num)
	if !ok {
		return itemSnapshot{}
	}
	return itemSnapshot{body: st.Body, labels: st.Labels, comments: st.Comments}
}

// externalWriteSeen runs the scenario and reports whether the engine's Store
// reflected the external write within selfWriteMaxPolls polls.
func externalWriteSeen(t *testing.T, skew time.Duration, legacy bool, w externalWrite) bool {
	t.Helper()
	env, num := pausedItem(t, skew, legacy)
	// One GitHub-second after the engine's last write.
	env.Clock.Advance(time.Second)
	w.do(env, num)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("external write: %v", err)
	}
	for i := 0; i < selfWriteMaxPolls; i++ {
		RunPoll(t, env)
		if w.seen(snapshotOf(env, num)) {
			return true
		}
	}
	return false
}

// SC-001 / SC-002: an external write one second after an engine write is seen on
// a following poll with the daemon clock ahead of, level with and behind GitHub's.
func TestSelfWriteBaseline_ExternalWriteAfterEngineWrite_IsSeen(t *testing.T) {
	t.Parallel()
	for _, sk := range selfWriteSkews {
		for _, w := range selfWriteExternalWrites {
			sk, w := sk, w
			t.Run(w.name+"/"+sk.name, func(t *testing.T) {
				t.Parallel()
				if !externalWriteSeen(t, sk.skew, false, w) {
					t.Errorf("%s one second after the engine's write was never seen (daemon clock skew %v)", w.name, sk.skew)
				}
			})
		}
	}
}

// Non-vacuity: with the daemon-clock baseline restored, a daemon clock ahead of
// GitHub's absorbs the external write — the defect this issue fixes. A comment is
// excluded: the comment-total rule (ADR 2180) catches it with either baseline.
func TestSelfWriteBaseline_NonVacuity_DaemonClockBaselineMasksWriteWhenHostAhead(t *testing.T) {
	t.Parallel()
	for _, w := range selfWriteExternalWrites {
		if w.name == "new-comment" {
			continue
		}
		w := w
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			if externalWriteSeen(t, 45*time.Second, true, w) {
				t.Errorf("%s was seen with the legacy daemon-clock baseline and the host clock ahead; the scenario would not catch the regression", w.name)
			}
		})
	}
}

// SC-003: engine writes with no external activity cost no deep fetch at the
// following probes, whichever way the clocks are skewed (#1090 guarantee).
func TestSelfWriteBaseline_EngineWritesAlone_NoRedundantDeepFetch(t *testing.T) {
	t.Parallel()
	for _, sk := range selfWriteSkews {
		sk := sk
		t.Run(sk.name, func(t *testing.T) {
			t.Parallel()
			env, num := pausedItem(t, sk.skew, false)
			// Let any fetch the pause itself owes happen, then count.
			RunPolls(t, env, 3)
			before := len(env.Sim.Log().ByMethod("FetchItemDetails"))
			RunPolls(t, env, 20)
			if got := len(env.Sim.Log().ByMethod("FetchItemDetails")) - before; got != 0 {
				t.Errorf("#%d: %d deep fetches over 20 polls with no external activity (skew %v), want 0", num, got, sk.skew)
			}
		})
	}
}
