package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// repairBoardTestServer fakes the GraphQL surface repairBoardCore drives:
// FetchProjectBoard (org variant only — repair always resolves an
// org-owned board in these tests, with a separate user-owned test using its
// own server), FetchStatusField, and — only when a test actually applies a
// change — SetStatusFieldOptions. setStatusCalled records whether the
// mutation fired, letting AC4/no-op tests assert it never does.
func repairBoardTestServer(t *testing.T, ownerType string, statusOptions []map[string]interface{}, setStatusCalled *bool) *httptest.Server {
	t.Helper()
	return repairBoardTestServerFields(t, ownerType, statusOptions, setStatusCalled, allDisplayFieldsPresent())
}

// repairBoardTestServerFields is repairBoardTestServer with an explicit
// display-field board (#2110), so a test can start with missing or
// wrong-typed fields and inspect the create calls.
func repairBoardTestServerFields(t *testing.T, ownerType string, statusOptions []map[string]interface{}, setStatusCalled *bool, ff *fakeFieldBoard) *httptest.Server {
	t.Helper()
	orgOrUserKey := "organization"
	if ownerType == "user" {
		orgOrUserKey = "user"
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)

		var resp map[string]interface{}
		if fieldResp, ok := ff.handle(body.Query, body.Variables); ok {
			json.NewEncoder(w).Encode(fieldResp)
			return
		}
		switch {
		case strings.Contains(body.Query, "projectV2(number: $projectNum)") && strings.Contains(body.Query, "closedByPullRequestsReferences"):
			// fetchProjectBoardQueryTemplate (rendered for either
			// "organization" or "user" — both contain this marker).
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					orgOrUserKey: map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id":    "PVT_EXISTING1",
							"title": "Fabrik Pipeline",
							"items": map[string]interface{}{
								"totalCount": 1,
								"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes": []interface{}{
									map[string]interface{}{
										"id":               "ITEM_1",
										"updatedAt":        "2020-01-01T00:00:00Z",
										"fieldValueByName": nil,
										"content": map[string]interface{}{
											"__typename":                     "Issue",
											"id":                             "ISSUE_1",
											"number":                         1,
											"title":                          "dummy",
											"state":                          "OPEN",
											"updatedAt":                      "2020-01-01T00:00:00Z",
											"repository":                     map[string]interface{}{"nameWithOwner": "acme/widgets"},
											"labels":                         map[string]interface{}{"nodes": []interface{}{}},
											"closedByPullRequestsReferences": map[string]interface{}{"nodes": []interface{}{}},
										},
									},
								},
							},
						},
					},
				},
			}
		case strings.Contains(body.Query, "field(name: \"Status\")"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"field": map[string]interface{}{
							"id":      "FIELD_STATUS",
							"options": statusOptions,
						},
					},
				},
			}
		case strings.Contains(body.Query, "updateProjectV2Field(input:"):
			if setStatusCalled != nil {
				*setStatusCalled = true
			}
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"updateProjectV2Field": map[string]interface{}{
						"projectV2Field": map[string]interface{}{"id": "FIELD_STATUS"},
					},
				},
			}
		default:
			t.Fatalf("unexpected GraphQL query: %s", body.Query)
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func testStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 0},
		{Name: "Research", Order: 1},
		{Name: "Implement", Order: 2},
		{Name: "Queued", Order: 3, HoldingStage: true},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

func TestRepairBoardCore_DryRunNoMutation(t *testing.T) {
	var mutated bool
	srv := repairBoardTestServer(t, "organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
		{"id": "OPT_2", "name": "Research", "color": "GRAY", "description": ""},
	}, &mutated)
	defer srv.Close()

	client := gh.NewClientWithBaseURL("token", srv.URL)
	var buf bytes.Buffer
	err := repairBoardCore(client, "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, false, &buf)
	if err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	if mutated {
		t.Fatal("dry run must never call SetStatusFieldOptions (AC4)")
	}
	out := buf.String()
	if !strings.Contains(out, "Implement") || !strings.Contains(out, "Queued") {
		t.Errorf("dry-run output should name missing columns, got:\n%s", out)
	}
	if !strings.Contains(out, "Dry run") {
		t.Errorf("dry-run output should say no changes were made, got:\n%s", out)
	}
}

func TestRepairBoardCore_NoMissingColumnsIsNoOpEvenWithApply(t *testing.T) {
	var mutated bool
	srv := repairBoardTestServer(t, "organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
		{"id": "OPT_2", "name": "Research", "color": "GRAY", "description": ""},
		{"id": "OPT_3", "name": "Implement", "color": "GRAY", "description": ""},
		{"id": "OPT_4", "name": "Queued", "color": "GRAY", "description": ""},
		{"id": "OPT_5", "name": "Done", "color": "GRAY", "description": ""},
	}, &mutated)
	defer srv.Close()

	client := gh.NewClientWithBaseURL("token", srv.URL)
	var buf bytes.Buffer
	// apply=true, but nothing is missing — must still be a no-op.
	err := repairBoardCore(client, "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	if mutated {
		t.Fatal("no missing columns must never call SetStatusFieldOptions")
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("expected a nothing-to-do message, got:\n%s", buf.String())
	}
}

// TestRepairBoardCore_ApplyPreservesExistingIDsAndEchoesFields is the R4/R5
// safeguard this issue exists to add: it proves that repairBoardCore's
// --apply path sends every existing option's id/color/description back
// unchanged, and appends only the missing columns with a nil id — the
// payload shape the issue's own empirical 2026-09-13 finding established as
// safe (an omitted id on an existing option regenerates it server-side and
// silently clears every item's Status).
func TestRepairBoardCore_ApplyPreservesExistingIDsAndEchoesFields(t *testing.T) {
	var gotVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)

		var resp map[string]interface{}
		switch {
		case strings.Contains(body.Query, "projectV2(number: $projectNum)"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"organization": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id":    "PVT_EXISTING1",
							"title": "Fabrik Pipeline",
							"items": map[string]interface{}{
								"totalCount": 1,
								"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes": []interface{}{
									map[string]interface{}{
										"id":               "ITEM_1",
										"updatedAt":        "2020-01-01T00:00:00Z",
										"fieldValueByName": nil,
										"content": map[string]interface{}{
											"__typename":                     "Issue",
											"id":                             "ISSUE_1",
											"number":                         1,
											"title":                          "dummy",
											"state":                          "OPEN",
											"updatedAt":                      "2020-01-01T00:00:00Z",
											"repository":                     map[string]interface{}{"nameWithOwner": "acme/widgets"},
											"labels":                         map[string]interface{}{"nodes": []interface{}{}},
											"closedByPullRequestsReferences": map[string]interface{}{"nodes": []interface{}{}},
										},
									},
								},
							},
						},
					},
				},
			}
		case strings.Contains(body.Query, "field(name: \"Status\")"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"field": map[string]interface{}{
							"id": "FIELD_STATUS",
							"options": []map[string]interface{}{
								{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": "specify desc"},
								{"id": "OPT_2", "name": "Research", "color": "BLUE", "description": ""},
							},
						},
					},
				},
			}
		case strings.Contains(body.Query, "fields(first: 100"):
			resp, _ = allDisplayFieldsPresent().handle(body.Query, body.Variables)
		case strings.Contains(body.Query, "updateProjectV2Field(input:"):
			gotVars = body.Variables
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"updateProjectV2Field": map[string]interface{}{
						"projectV2Field": map[string]interface{}{"id": "FIELD_STATUS"},
					},
				},
			}
		default:
			t.Fatalf("unexpected GraphQL query: %s", body.Query)
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := gh.NewClientWithBaseURL("token", srv.URL)
	var buf bytes.Buffer
	err := repairBoardCore(client, "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}

	rawOptions, ok := gotVars["options"].([]interface{})
	// Specify and Research echoed; Implement, Queued and Done appended — Done
	// is the cleanup stage's column, which a repaired board must have.
	if !ok || len(rawOptions) != 5 {
		t.Fatalf("options sent = %+v", gotVars["options"])
	}
	if last := rawOptions[4].(map[string]interface{}); last["name"] != "Done" {
		t.Errorf("last appended option = %+v, want Done", last)
	}

	opt0 := rawOptions[0].(map[string]interface{})
	if opt0["id"] != "OPT_1" || opt0["color"] != "GRAY" || opt0["description"] != "specify desc" {
		t.Errorf("existing option 0 not echoed unchanged: %+v", opt0)
	}
	opt1 := rawOptions[1].(map[string]interface{})
	if opt1["id"] != "OPT_2" || opt1["color"] != "BLUE" {
		t.Errorf("existing option 1 not echoed unchanged: %+v", opt1)
	}
	// The two missing columns (Implement, Queued) must be appended with no id.
	for _, raw := range rawOptions[2:] {
		opt := raw.(map[string]interface{})
		if _, hasID := opt["id"]; hasID {
			t.Errorf("new option must omit id entirely, got: %+v", opt)
		}
	}
	names := map[string]bool{}
	for _, raw := range rawOptions {
		opt := raw.(map[string]interface{})
		names[opt["name"].(string)] = true
	}
	for _, want := range []string{"Specify", "Research", "Implement", "Queued"} {
		if !names[want] {
			t.Errorf("options missing expected column %q: %+v", want, rawOptions)
		}
	}
}

func TestRepairBoardCore_RefusesUserOwnedBoard(t *testing.T) {
	var mutated bool
	srv := repairBoardTestServer(t, "user", nil, &mutated)
	defer srv.Close()

	client := gh.NewClientWithBaseURL("token", srv.URL)
	var buf bytes.Buffer
	err := repairBoardCore(client, "someuser", "widgets", 1, "user", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err == nil {
		t.Fatal("expected refusal for user-owned board")
	}
	if !strings.Contains(err.Error(), "organization-only") {
		t.Errorf("unexpected error: %v", err)
	}
	if mutated {
		t.Fatal("refusal must happen before any mutation")
	}
}

func TestRunRepairBoard_RequiresConfig(t *testing.T) {
	dir := t.TempDir()
	restoreDir := chdirTemp(t, dir)
	defer restoreDir()

	if err := runRepairBoard([]string{}); err == nil {
		t.Fatal("expected error when .fabrik/config.yaml is absent")
	}
}

// chdirTemp is a small local helper mirroring the chdir-and-restore pattern
// used throughout init_test.go, kept local to this file to avoid coupling.
func chdirTemp(t *testing.T, dir string) func() {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chdir(orig) } //nolint
}

// allStatusOptions satisfies testStages() so a test can isolate display
// fields from the Status repair.
func allStatusOptions() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
		{"id": "OPT_2", "name": "Research", "color": "GRAY", "description": ""},
		{"id": "OPT_3", "name": "Implement", "color": "GRAY", "description": ""},
		{"id": "OPT_4", "name": "Queued", "color": "GRAY", "description": ""},
		{"id": "OPT_5", "name": "Done", "color": "GRAY", "description": ""},
	}
}

func missingLastRunBoard() *fakeFieldBoard {
	return newFakeFieldBoard(fakeField("F1", "Fabrik", "TEXT"), fakeField("F2", "Last activity", "DATE"))
}

func TestRepairBoardCore_DisplayFieldDryRunListsMissingAndMakesNoMutation(t *testing.T) {
	var statusMutated bool
	ff := missingLastRunBoard()
	srv := repairBoardTestServerFields(t, "organization", allStatusOptions(), &statusMutated, ff)
	defer srv.Close()

	var buf bytes.Buffer
	err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, false, &buf)
	if err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	if len(ff.createdNames()) != 0 || statusMutated {
		t.Fatalf("dry run must not mutate: creates=%v status=%v", ff.createdNames(), statusMutated)
	}
	out := buf.String()
	if !strings.Contains(out, `"Last run"`) || !strings.Contains(out, "missing") || !strings.Contains(out, "Dry run") {
		t.Errorf("dry-run output should list Last run as missing, got:\n%s", out)
	}
	if strings.Contains(out, "nothing to do") {
		t.Errorf("must not claim nothing to do, got:\n%s", out)
	}
}

func TestRepairBoardCore_ApplyCreatesMissingFieldThenSecondRunIsNoOp(t *testing.T) {
	ff := missingLastRunBoard()
	srv := repairBoardTestServerFields(t, "organization", allStatusOptions(), nil, ff)
	defer srv.Close()
	client := gh.NewClientWithBaseURL("token", srv.URL)

	var buf bytes.Buffer
	if err := repairBoardCore(client, "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if got := ff.createdNames(); len(got) != 1 || got[0] != "Last run:TEXT" {
		t.Fatalf("creates = %v, want [Last run:TEXT]", got)
	}
	if !strings.Contains(buf.String(), "created") {
		t.Errorf("report should say the field was created, got:\n%s", buf.String())
	}

	buf.Reset()
	if err := repairBoardCore(client, "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := ff.createdNames(); len(got) != 1 {
		t.Fatalf("second apply must make no create call, creates = %v", got)
	}
	if !strings.Contains(buf.String(), "nothing to do") || !strings.Contains(buf.String(), "3 enabled display field(s)") {
		t.Errorf("second apply should report all fields present, got:\n%s", buf.String())
	}
}

func TestRepairBoardCore_TypeMismatchReportedAndNeverWritten(t *testing.T) {
	ff := newFakeFieldBoard(fakeField("F1", "Fabrik", "SINGLE_SELECT"), fakeField("F2", "Last activity", "DATE"))
	var statusMutated bool
	srv := repairBoardTestServerFields(t, "organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
	}, &statusMutated, ff)
	defer srv.Close()

	var buf bytes.Buffer
	err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err != nil {
		t.Fatalf("a type mismatch must not fail the command: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "expected TEXT, found SINGLE_SELECT") {
		t.Errorf("report should name the mismatch, got:\n%s", out)
	}
	// The other field and the Status columns are still repaired.
	if got := ff.createdNames(); len(got) != 1 || got[0] != "Last run:TEXT" {
		t.Errorf("creates = %v, want only Last run:TEXT (never Fabrik)", got)
	}
	if !statusMutated {
		t.Error("Status repair must still run alongside a field mismatch")
	}
}

func TestRepairBoardCore_DisabledFieldNeitherReportedNorCreated(t *testing.T) {
	ff := newFakeFieldBoard() // no fields at all
	srv := repairBoardTestServerFields(t, "organization", allStatusOptions(), nil, ff)
	defer srv.Close()

	off := "off"
	pf := config.ProjectFieldsConfig{LastRun: &off}
	var buf bytes.Buffer
	if err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), pf, true, &buf); err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	got := strings.Join(ff.createdNames(), ",")
	if got != "Fabrik:TEXT,Last activity:DATE" {
		t.Errorf("creates = %s", got)
	}
	if strings.Contains(buf.String(), "Last run") {
		t.Errorf("a disabled field must not be reported, got:\n%s", buf.String())
	}
}

func TestRepairBoardCore_BothStatusAndFieldsRepairedInOneInvocation(t *testing.T) {
	ff := newFakeFieldBoard()
	var statusMutated bool
	srv := repairBoardTestServerFields(t, "organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
	}, &statusMutated, ff)
	defer srv.Close()

	var buf bytes.Buffer
	if err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf); err != nil {
		t.Fatalf("repairBoardCore: %v", err)
	}
	if !statusMutated || len(ff.createdNames()) != 3 {
		t.Errorf("status=%v creates=%v", statusMutated, ff.createdNames())
	}
}

func TestRepairBoardCore_FieldCreateFailureIsIsolatedAndNamed(t *testing.T) {
	ff := newFakeFieldBoard()
	ff.failName = "Last activity"
	var statusMutated bool
	srv := repairBoardTestServerFields(t, "organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Specify", "color": "GRAY", "description": ""},
	}, &statusMutated, ff)
	defer srv.Close()

	var buf bytes.Buffer
	err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "acme", "widgets", 1, "", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err == nil || !strings.Contains(err.Error(), "Last activity") {
		t.Fatalf("expected an error naming Last activity, got %v", err)
	}
	if strings.Contains(err.Error(), `"Fabrik"`) || strings.Contains(err.Error(), `"Last run"`) {
		t.Errorf("only the failed field should be named, got %v", err)
	}
	if got := strings.Join(ff.createdNames(), ","); got != "Fabrik:TEXT,Last activity:DATE,Last run:TEXT" {
		t.Errorf("all fields must still be attempted, creates = %s", got)
	}
	if !statusMutated {
		t.Error("Status repair must still run when a field fails")
	}
}

func TestRepairBoardCore_UserOwnedBoardRefusedBeforeAnyFieldMutation(t *testing.T) {
	ff := newFakeFieldBoard()
	srv := repairBoardTestServerFields(t, "user", nil, nil, ff)
	defer srv.Close()

	var buf bytes.Buffer
	err := repairBoardCore(gh.NewClientWithBaseURL("token", srv.URL), "someuser", "widgets", 1, "user", testStages(), config.ProjectFieldsConfig{}, true, &buf)
	if err == nil {
		t.Fatal("expected refusal for user-owned board")
	}
	if len(ff.createdNames()) != 0 {
		t.Fatalf("refusal must precede every field mutation, creates = %v", ff.createdNames())
	}
}
