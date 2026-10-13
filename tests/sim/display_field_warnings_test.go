package sim

import (
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// This file is the sim twin of #2218 (ADR 2218): a board missing the display
// fields starts with exactly one warnings-panel entry carrying the
// repair-board fix, no lookup-failure warning and no retry, and the entry is
// resolved by the next start once the fields exist.
//
// The sim models an absent field as a nil lookup result; GitHub's real
// "could not resolve ... with the name" error is covered by the github
// package's recording test (SC-001), which the sim cannot produce.
//
// Not parallel: it redirects the process-wide warnings file, and parallel sim
// tests resume only after every serial test has finished.

func displayFieldsEnv(t *testing.T, missing bool) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:                   smokeStages(),
		OwnerType:                "organization",
		StatusLineField:          statusLineName,
		StatusLineFieldMissing:   missing,
		LastActivityField:        lastActivityName,
		LastActivityFieldMissing: missing,
		LastRunField:             lastRunName,
		LastRunFieldMissing:      missing,
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	return env
}

func panelByKey(t *testing.T) map[string]warnings.Entry {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("warnings.Load: %v", err)
	}
	m := map[string]warnings.Entry{}
	for _, e := range es {
		m[e.Key] = e
	}
	return m
}

func TestDisplayFieldWarnings_Sim_MissingThenCreated(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })

	// First start: none of the three fields exist.
	env := displayFieldsEnv(t, true)
	RunPolls(t, env, 3)

	got := panelByKey(t)
	if len(got) != 1 {
		t.Fatalf("panel = %v, want exactly one entry", got)
	}
	e, ok := got["display_field:missing"]
	if !ok {
		t.Fatalf("panel = %v, want display_field:missing", got)
	}
	if e.FixAction != "fabrik_command" || e.FixParams["args"] != "repair-board --apply" {
		t.Errorf("fix = %q %v, want fabrik_command repair-board --apply", e.FixAction, e.FixParams)
	}
	for _, name := range []string{statusLineName, lastActivityName, lastRunName} {
		if !strings.Contains(e.Detail, name) {
			t.Errorf("detail %q does not name %q", e.Detail, name)
		}
	}
	// One lookup per field, ever: an absent field is final, not retried.
	lookups := map[string]int{}
	for _, c := range env.Sim.Log().Entries() {
		if c.Method == "FetchTextField" || c.Method == "FetchDateField" {
			lookups[c.Method]++
		}
	}
	if lookups["FetchTextField"] != 2 || lookups["FetchDateField"] != 1 {
		t.Errorf("lookups = %v, want 2 text + 1 date and no retries", lookups)
	}

	// Next start, after `fabrik repair-board --apply`: the fields exist and the
	// entry (still in the panel file) is resolved.
	env2 := displayFieldsEnv(t, false)
	RunPolls(t, env2, 1)
	if got := panelByKey(t); len(got) != 0 {
		t.Fatalf("panel after the fields were created = %v, want empty", got)
	}
}
