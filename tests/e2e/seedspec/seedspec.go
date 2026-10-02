// Package seedspec is the pure description of a live-e2e seed (#1992, ADR-1992):
// the state a test wants the item to be in when the engine first sees it, as data.
//
// A live test that is about something late in the pipeline (a Validate-time gate,
// a landing decision, a post-merge behaviour) must not walk Specify → Research →
// Plan → Implement → Review just to reach it. It seeds the state directly —
// through the GitHub API, at no Claude cost — via seedAtStage in tests/e2e. That
// executor does only I/O; *what* to create is decided here, by Build, a pure
// function. Keeping the decision pure is what lets the fidelity check run in plain
// `go test ./...` with no bed, no network and no build tag (the same untagged-core
// split as tests/e2e/awaitvisible and tests/e2e/registry).
//
// Fidelity (R4). A seeded state must be one the engine could actually have
// produced, or a test can pass from an impossible state. CheckFidelity compares a
// Plan against a Fixture — the labels, board column and PR shape of an item
// parked after a real traversal, recorded from the engine itself by
// tests/sim's TestSeedFixturesMatchEngineTraversal. The rule is "may omit, never
// invent": a seed may leave out labels a traversal would carry, but every label
// the seed adds must be one the traversal carries, and the column and the PR's
// existence, base, linkage and draft state must match exactly.
package seedspec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Stages is the pipeline in order. Seeds cover Research..Validate: Specify is the
// state a test files an item at, not one it seeds.
var Stages = []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"}

// firstPRStage is the first stage whose completion implies a linked PR: Implement
// creates the draft PR and marks it ready on completion.
const firstPRStage = "Implement"

// DefaultBase is the base branch used when Spec.BaseBranch is empty.
const DefaultBase = "main"

// closesKeyword is the PR-body linkage the engine's own discovery relies on.
const closesKeyword = "Closes"

// DefaultPRBodyPrefix is the harness member PR's body ahead of its closing line
// (createMemberPR in tests/e2e).
const DefaultPRBodyPrefix = "e2e merge-train member.\n\n"

// DeviationDraft marks a deliberate departure from the real-traversal PR shape: a
// draft PR the bed's real review bot never sees (#1312).
const DeviationDraft = "draft-pr"

// PathMode says how Spec.Path becomes the file written on the PR branch.
type PathMode int

const (
	// PathExact uses Spec.Path as given.
	PathExact PathMode = iota
	// PathUnique inserts "-<issue>" before the extension so each run's file is
	// unique (landed files persist on the base branch).
	PathUnique
)

// StageComment is a prior-stage comment to post on the issue, in the format the
// engine's findStageComment reads (engine/context.go).
type StageComment struct {
	Stage string
	Body  string
}

// Spec describes the state to seed.
type Spec struct {
	// Column is the board column the item is placed at: Research, Plan, Implement,
	// Review or Validate. By default the seed represents that stage's completion
	// (stage:<Column>:complete, the state an item parks in after the stage ran).
	Column string
	// RunColumn instead leaves Column's own stage for the engine to run: only the
	// stages before Column are marked complete, so the engine dispatches one real
	// invocation of Column (the state an item is in the moment it arrives). Use it
	// when the subject needs that stage's real output — a Validate run that sets the
	// CI gate, a CI-fix reinvoke — but not the stages before it.
	RunColumn bool
	// BaseBranch is the PR's base; empty means DefaultBase.
	BaseBranch string
	// Title and IssueBody are the filed issue's.
	Title, IssueBody string
	// ExtraLabels are applied when the issue is filed (autonomy labels such as
	// fabrik:yolo, review-authority:*). They are the test's choice, not engine
	// state, and are not fidelity-checked.
	ExtraLabels []string
	// Path, PathMode and Content shape the one file written on the PR branch. Only
	// used when the column implies a PR.
	Path     string
	PathMode PathMode
	Content  string
	// PRBodySuffix is appended to the PR body after the closing line (the
	// slow-ci-required* and ci-fix-sentinel markers CI reads).
	PRBodySuffix string
	// Draft opens the PR as a draft. A real traversal's PR is ready from Implement
	// onward, so this is a recorded deviation (DeviationDraft).
	Draft bool
	// Minimal labels only stage:<Column>:complete instead of the whole chain a real
	// traversal would carry. It preserves the engine inputs of the seeds that
	// predate #1992; new seeds label the full chain.
	Minimal bool
	// DeferStatus leaves the board Status unset so the caller decides when the
	// engine may see the item (a comment posted first is then deterministically
	// present at the engine's first evaluation).
	DeferStatus bool
	// Comments are prior-stage comments to post. Default none: the engine tolerates
	// their absence, and no converted test reads one.
	Comments []StageComment
}

// Plan is what Build decides: every field is an exact input to the executor.
type Plan struct {
	Column string
	// RunsColumn: the engine runs Column's stage itself; Column is not marked complete.
	RunsColumn  bool
	Status      string
	DeferStatus bool
	IssueLabels []string
	StageLabels []string
	CreatePR    bool
	Branch      string
	BaseBranch  string
	Path        string
	Content     string
	PRDraft     bool
	PRBody      string
	// Comments are fully formatted issue-comment bodies.
	Comments []string
	// Deviations names deliberate departures from the real-traversal shape.
	Deviations []string
}

func stageIndex(name string) int {
	for i, s := range Stages {
		if s == name {
			return i
		}
	}
	return -1
}

// doneIndex is the index of the last stage the seed marks complete.
func (s Spec) doneIndex() int {
	if s.RunColumn {
		return stageIndex(s.Column) - 1
	}
	return stageIndex(s.Column)
}

// Validate reports a malformed spec.
func (s Spec) Validate() error {
	ci := stageIndex(s.Column)
	if ci < 1 {
		return fmt.Errorf("seed column %q: want one of %v (Specify is filed at, never seeded)", s.Column, Stages[1:])
	}
	if strings.TrimSpace(s.Title) == "" {
		return fmt.Errorf("seed has no title")
	}
	for _, c := range s.Comments {
		i := stageIndex(c.Stage)
		if i < 0 || i > ci {
			return fmt.Errorf("prior-stage comment for %q: want a stage at or before %s", c.Stage, s.Column)
		}
	}
	if s.doneIndex() < stageIndex(firstPRStage) {
		switch {
		case s.Draft:
			return fmt.Errorf("seed at %s carries no PR, so Draft cannot apply", s.Column)
		case s.PRBodySuffix != "":
			return fmt.Errorf("seed at %s carries no PR, so PRBodySuffix cannot apply", s.Column)
		}
		return nil
	}
	if s.Path == "" {
		return fmt.Errorf("seed at %s carries a PR but has no Path", s.Column)
	}
	return nil
}

// UniquePath inserts "-<num>" before path's extension: "e2e/x/a.txt" + 42 →
// "e2e/x/a-42.txt".
func UniquePath(path string, num int) string {
	slash := strings.LastIndex(path, "/")
	dot := strings.LastIndex(path, ".")
	if dot <= slash { // no extension in the basename
		return fmt.Sprintf("%s-%d", path, num)
	}
	return fmt.Sprintf("%s-%d%s", path[:dot], num, path[dot:])
}

// StageCommentBody formats a prior-stage comment so engine/context.go's
// findStageComment matches it.
func StageCommentBody(stage, body string) string {
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s", stage, body)
}

// Build turns spec into the Plan for the issue number the executor was given. It is
// pure.
func Build(spec Spec, issue int) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	ci := spec.doneIndex()
	p := Plan{
		Column:      spec.Column,
		RunsColumn:  spec.RunColumn,
		Status:      spec.Column,
		DeferStatus: spec.DeferStatus,
		IssueLabels: append([]string(nil), spec.ExtraLabels...),
		Branch:      fmt.Sprintf("fabrik/issue-%d", issue),
		BaseBranch:  spec.BaseBranch,
	}
	if p.BaseBranch == "" {
		p.BaseBranch = DefaultBase
	}
	first := 0
	if spec.Minimal {
		first = ci
	}
	for _, s := range Stages[first : ci+1] {
		p.StageLabels = append(p.StageLabels, "stage:"+s+":complete")
	}
	if ci >= stageIndex(firstPRStage) {
		p.CreatePR = true
		p.Path = spec.Path
		if spec.PathMode == PathUnique {
			p.Path = UniquePath(spec.Path, issue)
		}
		p.Content = spec.Content
		p.PRDraft = spec.Draft
		p.PRBody = fmt.Sprintf("%s%s #%d\n%s", DefaultPRBodyPrefix, closesKeyword, issue, spec.PRBodySuffix)
		if spec.Draft {
			p.Deviations = append(p.Deviations, DeviationDraft)
		}
	}
	for _, c := range spec.Comments {
		p.Comments = append(p.Comments, StageCommentBody(c.Stage, c.Body))
	}
	return p, nil
}

// FixtureVersion is the recorded-fixture schema version.
const FixtureVersion = 1

// Fixture is the state of an item parked after a real traversal to Column, as the
// engine itself produced it. Recorded by tests/sim; see tests/e2e/README.md for the
// recapture command.
type Fixture struct {
	Version int `json:"version"`
	// Column is the stage the item parked at (its Status, and the stage whose
	// completion label is present).
	Column string `json:"column"`
	// Status is the board column at park time.
	Status string `json:"status"`
	// Labels are every label the item carries at park time, sorted. The recorded
	// issue number is normalised out of any label that embeds it.
	Labels []string `json:"labels"`
	// PR is nil when the traversal had not yet produced a PR.
	PR *FixturePR `json:"pr,omitempty"`
	// Provenance says how the fixture was produced ("sim-engine").
	Provenance string `json:"provenance"`
	// EngineSHA is the engine commit the fixture was recorded at; informational.
	EngineSHA string `json:"engine_sha,omitempty"`
}

// FixturePR is a recorded PR's shape.
type FixturePR struct {
	Draft bool   `json:"draft"`
	Base  string `json:"base"`
	// HeadIsIssueBranch records that the head ref is fabrik/issue-<N>.
	HeadIsIssueBranch bool `json:"head_is_issue_branch"`
	// ClosesIssue records that the body carries "Closes #<N>".
	ClosesIssue bool `json:"closes_issue"`
}

// Marshal renders f in the stable on-disk form.
func (f Fixture) Marshal() ([]byte, error) {
	sort.Strings(f.Labels)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseFixture strictly decodes a recorded fixture.
func ParseFixture(data []byte) (Fixture, error) {
	var f Fixture
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("decoding seed fixture: %w", err)
	}
	if f.Version != FixtureVersion {
		return Fixture{}, fmt.Errorf("seed fixture version %d, want %d", f.Version, FixtureVersion)
	}
	return f, nil
}

// CheckFidelity compares plan with the recorded real-traversal fixture for the same
// column and returns every way the seed could not have been produced by the engine
// (nil when faithful). "May omit, never invent": see the package comment.
func CheckFidelity(plan Plan, fx Fixture) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// A seed that leaves Column for the engine to run is the state an item is in on
	// arrival: the board has advanced to Column but nothing else has changed, so it
	// is compared with the previous stage's parked state, at Column's Status.
	wantFixtureColumn, wantStatus := plan.Column, fx.Status
	if plan.RunsColumn {
		if i := stageIndex(plan.Column); i > 0 {
			wantFixtureColumn = Stages[i-1]
		}
		wantStatus = plan.Column
	}
	if fx.Column != wantFixtureColumn {
		add("plan for column %q (runs column: %v) needs the %q fixture, got %q", plan.Column, plan.RunsColumn, wantFixtureColumn, fx.Column)
		return problems
	}
	if plan.Status != wantStatus {
		add("seeded board Status %q, but a real traversal parks at %q", plan.Status, wantStatus)
	}
	have := toSet(fx.Labels)
	for _, l := range plan.StageLabels {
		if !have[l] {
			add("seeded label %q is never carried by a real traversal at %s (carries %v) — an invented label", l, fx.Column, fx.Labels)
		}
	}
	if want := "stage:" + fx.Column + ":complete"; !contains(plan.StageLabels, want) {
		add("seed omits %q, the label that says the stage the item is parked at has completed", want)
	}
	if plan.RunsColumn && contains(plan.StageLabels, "stage:"+plan.Column+":complete") {
		add("seed marks %s complete although the engine is to run it", plan.Column)
	}
	switch {
	case plan.CreatePR && fx.PR == nil:
		add("seed opens a PR but a real traversal has none yet at %s", fx.Column)
	case !plan.CreatePR && fx.PR != nil:
		add("seed opens no PR but a real traversal has one at %s", fx.Column)
	case plan.CreatePR:
		if plan.PRDraft != fx.PR.Draft && !contains(plan.Deviations, DeviationDraft) {
			add("seeded PR draft=%v but a real traversal's is draft=%v (an undeclared deviation)", plan.PRDraft, fx.PR.Draft)
		}
		if contains(plan.Deviations, DeviationDraft) && plan.PRDraft == fx.PR.Draft {
			add("seed declares %s but its draft flag already matches the traversal", DeviationDraft)
		}
		if plan.BaseBranch == "" {
			add("seeded PR has no base branch")
		}
		if fx.PR.HeadIsIssueBranch && !strings.HasPrefix(plan.Branch, "fabrik/issue-") {
			add("seeded PR head %q is not the fabrik/issue-<N> branch the engine resolves by convention", plan.Branch)
		}
		if fx.PR.ClosesIssue && !hasClosesLine(plan.PRBody) {
			add("seeded PR body has no %q line, which the engine's PR discovery relies on", closesKeyword+" #<N>")
		}
	}
	return problems
}

func hasClosesLine(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), closesKeyword+" #") {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
