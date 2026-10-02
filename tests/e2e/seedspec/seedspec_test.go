package seedspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, column string) Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", strings.ToLower(column)+".json"))
	if err != nil {
		t.Fatalf("recorded fixture for %s: %v (recapture: see tests/e2e/README.md)", column, err)
	}
	fx, err := ParseFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func specFor(column string) Spec {
	return Spec{
		Column: column, Title: "t", IssueBody: "b", Path: "e2e/seed/x.txt", PathMode: PathUnique, Content: "c\n",
	}
}

func mustBuild(t *testing.T, s Spec, issue int) Plan {
	t.Helper()
	p, err := Build(s, issue)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPlanMatchesRecordedTraversal is R4: for every column a seed can target, the
// plan a full-chain seed builds must be a state the engine produces. It runs in
// plain `go test ./...`; the fixtures are re-derived from the real engine by
// tests/sim's TestSeedFixturesMatchEngineTraversal.
func TestPlanMatchesRecordedTraversal(t *testing.T) {
	for _, column := range Stages[1:] {
		t.Run(column, func(t *testing.T) {
			fx := loadFixture(t, column)
			if p := CheckFidelity(mustBuild(t, specFor(column), 7), fx); len(p) != 0 {
				t.Fatalf("full-chain seed at %s is not a state the engine produces:\n  %s", column, strings.Join(p, "\n  "))
			}
		})
	}
}

// TestMinimalPlansMatchRecordedTraversal: the older minimal labelling (only
// stage:<column>:complete) omits labels a traversal carries, which is allowed,
// and invents none.
func TestMinimalPlansMatchRecordedTraversal(t *testing.T) {
	for _, column := range []string{"Review", "Validate"} {
		s := specFor(column)
		s.Minimal = true
		if p := CheckFidelity(mustBuild(t, s, 7), loadFixture(t, column)); len(p) != 0 {
			t.Fatalf("minimal seed at %s: %v", column, p)
		}
	}
}

// TestRunColumnPlansMatchRecordedTraversal: a seed that leaves a column's own stage
// for the engine to run is the arrival state — the previous stage's parked labels and
// PR, at the new column's Status.
func TestRunColumnPlansMatchRecordedTraversal(t *testing.T) {
	for i, column := range Stages[1:] {
		prev := Stages[i]
		t.Run(column, func(t *testing.T) {
			s := specFor(column)
			s.RunColumn = true
			p := mustBuild(t, s, 7)
			if !p.RunsColumn || contains(p.StageLabels, "stage:"+column+":complete") {
				t.Fatalf("plan must leave %s to the engine: %+v", column, p)
			}
			if problems := CheckFidelity(p, loadFixture(t, prev)); len(problems) != 0 {
				t.Fatalf("arrival seed at %s is not a state the engine produces:\n  %s", column, strings.Join(problems, "\n  "))
			}
			// The Implement run creates its own draft PR; every later column inherits one.
			if wantPR := stageIndex(column)-1 >= stageIndex(firstPRStage); p.CreatePR != wantPR {
				t.Fatalf("CreatePR=%v at %s, want %v", p.CreatePR, column, wantPR)
			}
			// ...and it must not pass against the wrong fixture.
			if problems := CheckFidelity(p, loadFixture(t, column)); len(problems) == 0 {
				t.Fatalf("arrival seed at %s passed against its own completed-state fixture", column)
			}
		})
	}
}

func TestDraftSeedIsADeclaredDeviation(t *testing.T) {
	s := specFor("Review")
	s.Draft = true
	p := mustBuild(t, s, 7)
	if len(p.Deviations) != 1 || p.Deviations[0] != DeviationDraft {
		t.Fatalf("deviations = %v", p.Deviations)
	}
	if problems := CheckFidelity(p, loadFixture(t, "Review")); len(problems) != 0 {
		t.Fatalf("declared draft deviation rejected: %v", problems)
	}
}

// TestFidelityRejectsImpossibleSeeds is the mutation self-test: a plan that could
// not have come from the engine must fail, or the comparison proves nothing.
func TestFidelityRejectsImpossibleSeeds(t *testing.T) {
	fx := loadFixture(t, "Validate")
	cases := []struct {
		name string
		mut  func(*Plan)
		want string
	}{
		{"invented label", func(p *Plan) { p.StageLabels = append(p.StageLabels, "stage:Done:complete") }, "invented label"},
		{"later stage's label", func(p *Plan) { p.StageLabels = append(p.StageLabels, "stage:Review:in_progress") }, "invented label"},
		{"missing completion label", func(p *Plan) { p.StageLabels = []string{"stage:Review:complete"} }, "omits"},
		{"wrong column", func(p *Plan) { p.Status = "Review" }, "board Status"},
		{"undeclared draft", func(p *Plan) { p.PRDraft = true }, "undeclared deviation"},
		{"declared draft that is not one", func(p *Plan) { p.Deviations = []string{DeviationDraft} }, "already matches"},
		{"no PR", func(p *Plan) { p.CreatePR = false }, "opens no PR"},
		{"no closing line", func(p *Plan) { p.PRBody = "no linkage here" }, "Closes"},
		{"not the issue branch", func(p *Plan) { p.Branch = "feature/x" }, "fabrik/issue-<N>"},
		{"no base", func(p *Plan) { p.BaseBranch = "" }, "no base branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := mustBuild(t, specFor("Validate"), 7)
			c.mut(&p)
			problems := CheckFidelity(p, fx)
			if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Fatalf("want a problem containing %q, got %v", c.want, problems)
			}
		})
	}
	// A plan for a different column must not pass.
	p := mustBuild(t, specFor("Implement"), 7)
	if problems := CheckFidelity(p, loadFixture(t, "Plan")); len(problems) == 0 {
		t.Fatal("plan for a different column must not pass")
	}
	pp := mustBuild(t, specFor("Plan"), 7)
	pp.CreatePR = true
	if problems := CheckFidelity(pp, loadFixture(t, "Plan")); len(problems) == 0 {
		t.Fatal("a PR before Implement must fail")
	}
}

func TestBuildShapes(t *testing.T) {
	p := mustBuild(t, Spec{
		Column: "Validate", BaseBranch: "release", Title: "t", Path: "e2e/a/b.md", PathMode: PathUnique,
		ExtraLabels: []string{"fabrik:yolo"}, PRBodySuffix: "marker: x\n", DeferStatus: true,
		Comments: []StageComment{{Stage: "Plan", Body: "plan body"}},
	}, 42)
	if p.Branch != "fabrik/issue-42" || p.BaseBranch != "release" || p.Path != "e2e/a/b-42.md" {
		t.Fatalf("branch/base/path = %q %q %q", p.Branch, p.BaseBranch, p.Path)
	}
	wantLabels := "stage:Specify:complete,stage:Research:complete,stage:Plan:complete,stage:Implement:complete,stage:Review:complete,stage:Validate:complete"
	if got := strings.Join(p.StageLabels, ","); got != wantLabels {
		t.Fatalf("stage labels = %s", got)
	}
	if !strings.Contains(p.PRBody, "Closes #42\n") || !strings.HasSuffix(p.PRBody, "marker: x\n") {
		t.Fatalf("PR body = %q", p.PRBody)
	}
	if !p.DeferStatus || p.Status != "Validate" || len(p.IssueLabels) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.Comments) != 1 || !strings.HasPrefix(p.Comments[0], "🏭 **Fabrik — stage: Plan**\n") {
		t.Fatalf("comments = %q", p.Comments)
	}

	r := mustBuild(t, specFor("Research"), 1)
	if r.CreatePR || r.Path != "" || len(r.StageLabels) != 2 {
		t.Fatalf("research plan = %+v", r)
	}
	m := specFor("Review")
	m.Minimal = true
	if got := mustBuild(t, m, 1).StageLabels; len(got) != 1 || got[0] != "stage:Review:complete" {
		t.Fatalf("minimal labels = %v", got)
	}
	exact := specFor("Implement")
	exact.PathMode = PathExact
	if got := mustBuild(t, exact, 5).Path; got != "e2e/seed/x.txt" {
		t.Fatalf("exact path = %q", got)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Spec)
		want string
	}{
		{"specify is filed, not seeded", func(s *Spec) { s.Column = "Specify" }, "never seeded"},
		{"unknown column", func(s *Spec) { s.Column = "Done" }, "want one of"},
		{"no title", func(s *Spec) { s.Title = "" }, "no title"},
		{"comment for a later stage", func(s *Spec) { s.Column = "Implement"; s.Comments = []StageComment{{Stage: "Review"}} }, "at or before"},
		{"draft without a PR", func(s *Spec) { s.Column = "Plan"; s.Draft = true }, "Draft cannot apply"},
		{"suffix without a PR", func(s *Spec) { s.Column = "Plan"; s.PRBodySuffix = "x" }, "PRBodySuffix cannot apply"},
		{"PR without a path", func(s *Spec) { s.Path = "" }, "no Path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := specFor("Validate")
			c.mut(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if _, berr := Build(s, 1); berr == nil {
				t.Fatal("Build must refuse an invalid spec")
			}
		})
	}
}

func TestUniquePath(t *testing.T) {
	for in, want := range map[string]string{
		"e2e/train/entries/clean1.txt": "e2e/train/entries/clean1-42.txt",
		"e2e/dir.d/noext":              "e2e/dir.d/noext-42",
		"plain.md":                     "plain-42.md",
	} {
		if got := UniquePath(in, 42); got != want {
			t.Errorf("UniquePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFixtureRoundTripAndStrictDecode(t *testing.T) {
	fx := loadFixture(t, "Validate")
	data, err := fx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseFixture(data)
	if err != nil || back.Column != "Validate" {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	if _, err := ParseFixture([]byte(`{"version":1,"bogus":1}`)); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if _, err := ParseFixture([]byte(`{"version":99}`)); err == nil {
		t.Fatal("wrong version must be rejected")
	}
}
