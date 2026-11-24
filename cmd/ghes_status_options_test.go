package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
)

// #2616 / ADR 2616: Status option replacement on a server whose option input
// has no id (GHES 3.19.10). The fakes are strict: on a ghes-shaped board an
// option carrying an `id` fails with the server's real validation error.

func twoStatusOptions() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "PURPLE", "description": "specify desc"},
		{"id": "OPT_2", "name": "Research", "color": "BLUE", "description": ""},
	}
}

func optionMaps(t *testing.T, vars map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := vars["options"].([]interface{})
	if !ok {
		t.Fatalf("options missing from payload: %+v", vars)
	}
	out := make([]map[string]interface{}, len(raw))
	for i, o := range raw {
		out[i] = o.(map[string]interface{})
	}
	return out
}

func runRepair(t *testing.T, ff *fakeFieldBoard, apply bool) (string, error) {
	t.Helper()
	srv := repairBoardTestServerFields(t, "organization", twoStatusOptions(), nil, ff)
	t.Cleanup(srv.Close)
	var buf bytes.Buffer
	err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, apply, &buf)
	return buf.String(), err
}

func TestRepairBoardGHES_EmptyBoardSendsNameOnlyOptions(t *testing.T) {
	ff := allDisplayFieldsPresent()
	ff.ghes = true
	out, err := runRepair(t, ff, true)
	if err != nil {
		t.Fatalf("repairBoardCore: %v\n%s", err, out)
	}
	if ff.statusMutationCount() != 1 || ff.introspections != 1 || ff.populationReads != 1 {
		t.Fatalf("mutations=%d introspections=%d populationReads=%d, want 1/1/1", ff.statusMutationCount(), ff.introspections, ff.populationReads)
	}
	opts := optionMaps(t, ff.statusMutations[0])
	if len(opts) != 5 {
		t.Fatalf("options = %+v, want 5 (2 existing + Implement, Queued, Done)", opts)
	}
	for i, o := range opts {
		if _, has := o["id"]; has {
			t.Errorf("option %d sent an id: %+v", i, o)
		}
		if _, ok := o["color"].(string); !ok {
			t.Errorf("option %d has no color: %+v", i, o)
		}
		if _, ok := o["description"].(string); !ok {
			t.Errorf("option %d has no description (must be \"\" when none): %+v", i, o)
		}
	}
	// Existing options keep name, color and description, in order.
	if opts[0]["name"] != "Specify" || opts[0]["color"] != "PURPLE" || opts[0]["description"] != "specify desc" {
		t.Errorf("existing option 0 not preserved: %+v", opts[0])
	}
	if opts[1]["name"] != "Research" || opts[1]["color"] != "BLUE" {
		t.Errorf("existing option 1 not preserved: %+v", opts[1])
	}
	if !strings.Contains(out, "added 3 column(s)") {
		t.Errorf("output should report the columns added:\n%s", out)
	}
}

func TestRepairBoardGHES_PopulatedBoardRefusesAndSendsNothing(t *testing.T) {
	ff := allDisplayFieldsPresent()
	ff.ghes = true
	ff.statusPopulated = true
	out, err := runRepair(t, ff, true)
	if err == nil {
		t.Fatalf("a refusal must be a failing outcome, output:\n%s", out)
	}
	if ff.statusMutationCount() != 0 {
		t.Fatalf("no option mutation may be sent, got %d", ff.statusMutationCount())
	}
	msg := err.Error()
	for _, want := range []string{"Implement, Queued, Done", "board's UI", "Nothing was changed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should contain %q, got: %s", want, msg)
		}
	}
	if strings.Contains(out, "added") {
		t.Errorf("must not claim columns were added:\n%s", out)
	}
}

func TestRepairBoardGHES_RefusalDoesNotBlockDisplayFields(t *testing.T) {
	ff := missingLastRunBoard()
	ff.ghes = true
	ff.statusPopulated = true
	out, err := runRepair(t, ff, true)
	if err == nil {
		t.Fatalf("expected the Status refusal as an error, output:\n%s", out)
	}
	if ff.statusMutationCount() != 0 {
		t.Fatalf("no option mutation may be sent")
	}
	if got := ff.createdNames(); len(got) != 1 || got[0] != "Last run:TEXT" {
		t.Errorf("display field creation must still run, creates = %v", got)
	}
}

func TestRepairBoardGHES_NothingMissingIsUnaffected(t *testing.T) {
	ff := allDisplayFieldsPresent()
	ff.ghes = true
	ff.statusPopulated = true
	srv := repairBoardTestServerFields(t, "organization", allStatusOptions(), nil, ff)
	defer srv.Close()
	var buf bytes.Buffer
	if err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf); err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("want nothing to do, got:\n%s", buf.String())
	}
	if ff.introspections != 0 || ff.populationReads != 0 || ff.statusMutationCount() != 0 {
		t.Errorf("no capability/population read or mutation when nothing is missing: %d/%d/%d", ff.introspections, ff.populationReads, ff.statusMutationCount())
	}
}

func TestRepairBoardGHES_PopulationReadFailureFailsClosed(t *testing.T) {
	ff := allDisplayFieldsPresent()
	ff.ghes = true
	ff.populationFails = true
	_, err := runRepair(t, ff, true)
	if err == nil || ff.statusMutationCount() != 0 {
		t.Fatalf("err=%v mutations=%d, want a refusal with no mutation", err, ff.statusMutationCount())
	}
}

func TestRepairBoard_UnknownCapability(t *testing.T) {
	// Introspection denied: never send an id; replace only an empty board.
	t.Run("populated refuses", func(t *testing.T) {
		ff := allDisplayFieldsPresent()
		ff.introspection = "denied"
		ff.statusPopulated = true
		_, err := runRepair(t, ff, true)
		if err == nil || ff.statusMutationCount() != 0 {
			t.Fatalf("err=%v mutations=%d, want a refusal with no mutation", err, ff.statusMutationCount())
		}
	})
	t.Run("empty goes name-only", func(t *testing.T) {
		ff := allDisplayFieldsPresent()
		ff.introspection = "null"
		out, err := runRepair(t, ff, true)
		if err != nil {
			t.Fatalf("repairBoardCore: %v\n%s", err, out)
		}
		for i, o := range optionMaps(t, ff.statusMutations[0]) {
			if _, has := o["id"]; has {
				t.Errorf("option %d sent an id with unknown capability: %+v", i, o)
			}
		}
	})
}

// github.com shape: ids echoed exactly as before, even on a populated board,
// and the population read is never made.
func TestRepairBoardGitHubCom_PopulatedBoardStillEchoesIDs(t *testing.T) {
	ff := allDisplayFieldsPresent()
	ff.statusPopulated = true
	out, err := runRepair(t, ff, true)
	if err != nil {
		t.Fatalf("repairBoardCore: %v\n%s", err, out)
	}
	if ff.populationReads != 0 {
		t.Errorf("github.com path must not read board population, got %d reads", ff.populationReads)
	}
	opts := optionMaps(t, ff.statusMutations[0])
	if opts[0]["id"] != "OPT_1" || opts[1]["id"] != "OPT_2" {
		t.Errorf("existing ids not echoed: %+v", opts[:2])
	}
	for _, o := range opts[2:] {
		if _, has := o["id"]; has {
			t.Errorf("new option must omit id: %+v", o)
		}
	}
}

func TestRepairBoardGHES_DryRun(t *testing.T) {
	t.Run("populated reports the limitation and no apply hint for columns", func(t *testing.T) {
		ff := allDisplayFieldsPresent()
		ff.ghes = true
		ff.statusPopulated = true
		out, err := runRepair(t, ff, false)
		if err != nil {
			t.Fatalf("dry run must not fail: %v", err)
		}
		if ff.statusMutationCount() != 0 {
			t.Fatal("dry run must not mutate")
		}
		if !strings.Contains(out, "cannot add Status column(s) Implement, Queued, Done") || !strings.Contains(out, "board's UI") {
			t.Errorf("dry run should report the limitation and remedy:\n%s", out)
		}
		if strings.Contains(out, "Re-run with --apply") {
			t.Errorf("must not suggest --apply for columns it cannot add:\n%s", out)
		}
	})
	t.Run("empty board suggests apply", func(t *testing.T) {
		ff := allDisplayFieldsPresent()
		ff.ghes = true
		out, err := runRepair(t, ff, false)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if ff.statusMutationCount() != 0 || !strings.Contains(out, "Re-run with --apply to add the missing column(s).") {
			t.Errorf("mutations=%d output:\n%s", ff.statusMutationCount(), out)
		}
	})
	t.Run("populated plus missing field still suggests apply for the field only", func(t *testing.T) {
		ff := missingLastRunBoard()
		ff.ghes = true
		ff.statusPopulated = true
		out, _ := runRepair(t, ff, false)
		if !strings.Contains(out, "Re-run with --apply to add the missing display field(s).") {
			t.Errorf("output:\n%s", out)
		}
	})
}

// create-board on a GHES-shaped server: the new board's default options are
// replaced with name/color/description only, with no capability query.
func TestCreateBoardCore_GHESShapedServerSendsNoIDs(t *testing.T) {
	ff := newFakeFieldBoard()
	ff.ghes = true
	srv := createBoardTestServerFields(t, "Organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Todo", "color": "GRAY", "description": ""},
		{"id": "OPT_2", "name": "In Progress", "color": "BLUE", "description": ""},
		{"id": "OPT_3", "name": "Done", "color": "GREEN", "description": ""},
	}, nil, ff)
	defer srv.Close()

	stagesDir := t.TempDir()
	writeMinimalStage(t, stagesDir, "Specify", 0)
	writeMinimalStage(t, stagesDir, "Implement", 1)

	project, _, fieldsErr, err := createBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", "", stagesDir, config.ProjectFieldsConfig{})
	if err != nil {
		t.Fatalf("createBoardCore: %v", err)
	}
	if project != "42" || fieldsErr != nil {
		t.Errorf("project=%q fieldsErr=%v", project, fieldsErr)
	}
	if ff.statusMutationCount() != 1 {
		t.Fatalf("status mutations = %d, want 1", ff.statusMutationCount())
	}
	opts := optionMaps(t, ff.statusMutations[0])
	if len(opts) != 2 || opts[0]["name"] != "Specify" || opts[1]["name"] != "Implement" {
		t.Fatalf("options = %+v", opts)
	}
	for i, o := range opts {
		if _, has := o["id"]; has {
			t.Errorf("option %d sent an id: %+v", i, o)
		}
		if o["color"] != "GRAY" || o["description"] != "" {
			t.Errorf("option %d = %+v, want GRAY and empty description", i, o)
		}
	}
	if ff.introspections != 0 || ff.populationReads != 0 {
		t.Errorf("a new board needs no capability or population read: %d/%d", ff.introspections, ff.populationReads)
	}
}
