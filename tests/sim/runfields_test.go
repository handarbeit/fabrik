package sim

import (
	"regexp"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// This file is the sim twin of #2049 (ADR 2049): the display-only Last
// activity (date) and Last run (text) project fields, written from the stage
// and comment-review run sites. There is no live e2e twin — what a date write
// does to a real board's updatedAt and the real webhook payload cannot be
// proven here (see the ADR).

const (
	lastActivityName = "Last activity"
	lastRunName      = "Last run"
)

var (
	isoDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	lastRunLine = regexp.MustCompile(`^(\w+) · (completed|turn-limited|blocked on input|failed|incomplete)`)
)

func runFieldsEnv(t *testing.T, mutate func(*EnvOptions)) *Env {
	t.Helper()
	opts := EnvOptions{
		Stages:            smokeStages(),
		LastActivityField: lastActivityName,
		LastRunField:      lastRunName,
	}
	if mutate != nil {
		mutate(&opts)
	}
	env := NewEnv(t, opts)
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	return env
}

// fieldWrites returns, in order, the values written to issue n's cards by the
// given mutation method.
func fieldWrites(t *testing.T, env *Env, n int, method string) []string {
	t.Helper()
	itemID := projectItem(t, env, n).ItemID
	var out []string
	for _, e := range env.Sim.Log().Mutations() {
		if e.Err == nil && e.Method == method && len(e.Args.Values) == 3 && e.Args.Values[0] == itemID {
			out = append(out, e.Args.Values[2])
		}
	}
	return out
}

// A full pipeline: Last activity ends on an ISO date and Last run on the
// Validate run's line; both survive Done (nothing clears them); and no
// consecutive repeats are written. That the writes cause no drift or deep fetch
// is pinned where it can fail: the github parser tests (board and probe), the
// simgh projection test and the boardcache webhook test.
func TestRunFields_Sim_FullPipelineLeavesValuesAtDone(t *testing.T) {
	t.Parallel()
	env := runFieldsEnv(t, nil)

	num := FileIssue(t, env, "Run fields: full pipeline", "Prove the display fields.", "Specify")
	WaitForProjectStatus(t, env, num, "Done", 80)
	WaitForIssueClosed(t, env, num, 80)

	sim := env.Sim.Sim()
	date := sim.DateFieldValue(env.OwnerRepo, num, lastActivityName)
	if !isoDate.MatchString(date) {
		t.Errorf("Last activity after Done = %q, want a YYYY-MM-DD date", date)
	}
	run := sim.TextFieldValue(env.OwnerRepo, num, lastRunName)
	m := lastRunLine.FindStringSubmatch(run)
	if m == nil || m[1] != "Validate" {
		t.Errorf("Last run after Done = %q, want the Validate run's line (the Done cleanup is not a run)", run)
	}

	runs := fieldWrites(t, env, num, "UpdateProjectItemTextField")
	if len(runs) == 0 || runs[len(runs)-1] != run {
		t.Errorf("Last run writes = %q, want them to end on the value left at Done %q", runs, run)
	}
	for i := 1; i < len(runs); i++ {
		if runs[i] == runs[i-1] {
			t.Errorf("Last run written twice in a row: %q", runs)
		}
	}
	dates := fieldWrites(t, env, num, "UpdateProjectItemDateField")
	if len(dates) != 1 {
		t.Errorf("Last activity writes = %q, want exactly one (every event fell on one day)", dates)
	}
	for _, e := range env.Sim.Log().Entries() {
		if e.Method == "ClearProjectItemField" {
			t.Errorf("unexpected ClearProjectItemField — the run fields are never cleared")
		}
	}
}

// A board without the fields — or with Last activity of the wrong type: the
// pipeline is unaffected and no field write is ever attempted.
func TestRunFields_Sim_MissingFieldsWriteNothing(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*EnvOptions){
		"both missing":    func(o *EnvOptions) { o.LastActivityFieldMissing = true; o.LastRunFieldMissing = true },
		"wrong type date": func(o *EnvOptions) { o.LastActivityFieldWrongType = true; o.LastRunFieldMissing = true },
	} {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := runFieldsEnv(t, mutate)
			num := FileIssue(t, env, "Run fields: missing", "No fields on the board.", "Specify")
			WaitForProjectStatus(t, env, num, "Done", 80)

			for _, e := range env.Sim.Log().Entries() {
				switch e.Method {
				case "UpdateProjectItemDateField", "UpdateProjectItemTextField", "ClearProjectItemField":
					t.Errorf("unexpected %s on a board without the field(s)", e.Method)
				}
			}
		})
	}
}

// Off by default: with no field configured no field call is ever made.
func TestRunFields_Sim_OffByDefaultMakesNoCalls(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	num := FileIssue(t, env, "Run fields: off", "Feature unset.", "Specify")
	WaitForProjectStatus(t, env, num, "Done", 80)

	for _, e := range env.Sim.Log().Entries() {
		if strings.Contains(e.Method, "DateField") || e.Method == "FetchTextField" || e.Method == "UpdateProjectItemTextField" {
			t.Errorf("unexpected %s with the run fields unset", e.Method)
		}
	}
}
