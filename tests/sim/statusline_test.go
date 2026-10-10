package sim

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// This file is the sim twin of #2048 (ADR 2048): the display-only status-line
// project field, with the merge train as its first writer. There is no live
// e2e twin — what a text-field write does to a real board's updatedAt and the
// real webhook payload shape cannot be proven here (see the ADR), and nothing
// the sim checks needs a real GitHub.

const statusLineName = "Fabrik"

// statusLineWrites returns, in order, the lines written to issue n's status-line
// field by completed mutations: the text of each set, "<cleared>" for a clear.
func statusLineWrites(t *testing.T, env *Env, n int) []string {
	t.Helper()
	itemID := projectItem(t, env, n).ItemID
	var out []string
	for _, e := range env.Sim.Log().Mutations() {
		if e.Err != nil || len(e.Args.Values) == 0 || e.Args.Values[0] != itemID {
			continue
		}
		switch e.Method {
		case "UpdateProjectItemTextField":
			out = append(out, e.Args.Values[2])
		case "ClearProjectItemField":
			out = append(out, "<cleared>")
		}
	}
	return out
}

func statusLineEnv(t *testing.T) *Env {
	t.Helper()
	return mergeTrainEnv(t, mergeTrainEnvOptions{StatusLineField: statusLineName})
}

var trialCILine = regexp.MustCompile(`^trial #\d+ · CI running$`)

func assertNoRepeats(t *testing.T, who string, seq []string) {
	t.Helper()
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			t.Errorf("%s wrote %q twice in a row: %q", who, seq[i], seq)
		}
	}
}

// A clean batch of three: each member shows queued → trial CI → landing and the
// field is cleared when it reaches Done; every line is written exactly once.
func TestStatusLine_Sim_GreenTrainLifecycle(t *testing.T) {
	t.Parallel()
	env := statusLineEnv(t)
	var nums []int
	for _, m := range []string{"sl-a", "sl-b", "sl-c"} {
		n, _ := QueueMember(t, env, m, map[string]string{m + ".txt": m + "\n"})
		nums = append(nums, n)
	}
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	for _, n := range nums {
		seq := statusLineWrites(t, env, n)
		if got := projectItem(t, env, n).Status; got != "Done" {
			t.Fatalf("#%d is %q, want Done (lines %q)", n, got, seq)
		}
		if len(seq) != 4 || seq[0] != "queued · batch of 3" || !trialCILine.MatchString(seq[1]) || seq[2] != "landing" || seq[3] != "<cleared>" {
			t.Errorf("#%d lines = %q, want [queued · batch of 3, trial #N · CI running, landing, <cleared>]", n, seq)
		}
		assertNoRepeats(t, fmt.Sprintf("#%d", n), seq)
		if v := env.Sim.Sim().TextFieldValue(env.OwnerRepo, n, statusLineName); v != "" {
			t.Errorf("#%d field value after Done = %q, want empty", n, v)
		}
	}
}

// A red batch is bisected: the isolating validations are shown as
// "bisecting · step i of n"; the poisoner is ejected and stays Queued.
func TestStatusLine_Sim_BisectShowsSteps(t *testing.T) {
	t.Parallel()
	env := statusLineEnv(t)
	var nums []int
	for _, m := range []string{"slb-a", "slb-b", "slb-c", "slb-d"} {
		n, _ := QueueMember(t, env, m, map[string]string{m + ".txt": m + "\n"})
		nums = append(nums, n)
	}
	startTrialVerdictSeeder(t, env, poisonVerdict(nums[1]))

	RunPoll(t, env)

	var sawBisect bool
	for _, n := range nums {
		seq := statusLineWrites(t, env, n)
		assertNoRepeats(t, fmt.Sprintf("#%d", n), seq)
		for _, l := range seq {
			if strings.HasPrefix(l, "bisecting · step ") {
				sawBisect = true
			}
		}
	}
	if !sawBisect {
		t.Fatalf("no member showed a bisecting line; #%d lines = %q", nums[0], statusLineWrites(t, env, nums[0]))
	}
	// The ejected poisoner stays Queued and shows it.
	seq := statusLineWrites(t, env, nums[1])
	if last := seq[len(seq)-1]; last != "queued" {
		t.Errorf("ejected poisoner #%d last line = %q, want %q (lines %q)", nums[1], last, "queued", seq)
	}
}

// A board without the field: the train behaves as before, nothing is written
// and no error surfaces.
func TestStatusLine_Sim_MissingFieldWritesNothing(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{StatusLineField: statusLineName, StatusLineFieldMissing: true})
	a, _ := QueueMember(t, env, "slm-a", map[string]string{"a.txt": "a\n"})
	b, _ := QueueMember(t, env, "slm-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	for _, n := range []int{a, b} {
		if got := projectItem(t, env, n).Status; got != "Done" {
			t.Errorf("#%d is %q, want Done — a missing display field must not change the train", n, got)
		}
		if seq := statusLineWrites(t, env, n); len(seq) != 0 {
			t.Errorf("#%d lines = %q, want none", n, seq)
		}
	}
	for _, e := range env.Sim.Log().Entries() {
		if e.Method == "UpdateProjectItemTextField" || e.Method == "ClearProjectItemField" {
			t.Errorf("unexpected %s call on a board without the field", e.Method)
		}
	}
}

// Feature off (the default): no text-field call is ever made.
func TestStatusLine_Sim_OffByDefaultMakesNoCalls(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	QueueMember(t, env, "slo-a", map[string]string{"a.txt": "a\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	for _, e := range env.Sim.Log().Entries() {
		if e.Method == "FetchTextField" || e.Method == "UpdateProjectItemTextField" || e.Method == "ClearProjectItemField" {
			t.Errorf("unexpected %s call with the status line off", e.Method)
		}
	}
}
