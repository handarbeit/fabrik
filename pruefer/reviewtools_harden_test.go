package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Hardening of the review_tools validator (#2370, ADR 2308 amendment).

// issueNamedTools are the executables #2370 requires to be refused.
var issueNamedTools = []string{"tar", "less", "vim", "vi", "stdbuf", "flock", "watch", "busybox", "command", "rsync", "java"}

// FR-001/FR-004/SC-001: every named executable is refused in lower case, mixed
// case, upper case, with an absolute-path prefix and with a version/.exe suffix.
func TestValidateReviewToolRule_RefusesSideEffectToolsInEverySpelling(t *testing.T) {
	for _, tool := range issueNamedTools {
		spellings := []string{
			tool, strings.ToUpper(tool), strings.ToUpper(tool[:1]) + tool[1:],
			"/usr/bin/" + tool, "/usr/bin/" + strings.ToUpper(tool), tool + ".exe", strings.ToUpper(tool) + ".EXE", tool + "9",
		}
		for _, exe := range spellings {
			for _, shape := range []string{"Bash(%s x)", "Bash(%s *)", "Bash(%s:*)", "Bash(%s)"} {
				rule := strings.Replace(shape, "%s", exe, 1)
				if got, err := validateReviewToolRule(rule); err == nil {
					t.Errorf("%q: expected refusal, accepted as %q", rule, got)
				}
			}
		}
	}
}

// FR-002: the additional tools in the same class are refused too.
func TestValidateReviewToolRule_RefusesTheWiderSideEffectClass(t *testing.T) {
	for _, tool := range reviewToolsSideEffectTools {
		if tool != strings.ToLower(tool) {
			t.Errorf("list entry %q must be lower case", tool)
		}
		if _, err := validateReviewToolRule("Bash(" + tool + " x)"); err == nil {
			t.Errorf("%q: expected refusal", tool)
		}
	}
	for _, tool := range issueNamedTools {
		if tool == "rsync" {
			continue // refused as a network client
		}
		if !slices.Contains(reviewToolsSideEffectTools, tool) {
			t.Errorf("%q named in #2370 is missing from reviewToolsSideEffectTools", tool)
		}
	}
	// Messages say why: tar is not "arbitrary code" in the interpreter sense.
	_, err := validateReviewToolRule("Bash(tar *)")
	if err == nil || !strings.Contains(err.Error(), "write files") || !strings.Contains(err.Error(), "Bash(tar *)") {
		t.Errorf("tar error = %v, want it to name the rule and the reason", err)
	}
}

// FR-004: case and path prefix cannot bypass the pre-existing lists either.
func TestValidateReviewToolRule_CaseFoldsEveryExecutableList(t *testing.T) {
	for _, in := range []string{
		"Bash(Python3 x)", "Bash(PYTHON3 -m foo)", "Bash(Node x.js)", "Bash(/usr/bin/Bash -c x)", "Bash(SH:*)", "Bash(Curl -s *)", "Bash(GIT log)",
		// launchers, test runners, repo-code runners and implicit script launchers
		"Bash(Go:*)", "Bash(Go test:*)", "Bash(/usr/local/go/bin/GO test ./...)", "Bash(NPM *)", "Bash(PyTest)", "Bash(MAKE lint)", "Bash(Yarn lint)", "Bash(PNPM lint)", "Bash(Pip wheel x)", "Bash(Cargo build)", "Bash(Mvn verify)", "Bash(DOTNET build)", "Bash(Uv sync)",
	} {
		if got, err := validateReviewToolRule(in); err == nil {
			t.Errorf("%q: expected refusal, accepted as %q", in, got)
		}
	}
}

// A leading environment assignment must not hide the executable from the lists.
func TestValidateReviewToolRule_RefusesLeadingEnvAssignment(t *testing.T) {
	for _, in := range []string{"Bash(A=1 vim *)", "Bash(X=y tar *)", "Bash(PATH=/x python3 -c *)", "Bash(FOO=1 bash -c *)", "Bash(A=1 shellcheck *)", "Bash(A=1)", "Bash(A=1 B=2 less x)"} {
		_, err := validateReviewToolRule(in)
		if err == nil || !strings.Contains(err.Error(), "environment assignment") {
			t.Errorf("%q: error = %v, want an environment-assignment refusal", in, err)
		}
	}
}

// The returned rule keeps the case as typed, so repo-versus-operator equality
// and deduplication are unaffected by the folding.
func TestValidateReviewToolRule_KeepsTypedCase(t *testing.T) {
	for _, in := range []string{"Bash(ShellCheck *)", "Bash(/opt/Tools/Lint --Strict)"} {
		got, err := validateReviewToolRule(in)
		if err != nil || got != in {
			t.Errorf("%q: got %q err %v, want it accepted unchanged", in, got, err)
		}
	}
}

// Substring safety: matching is on the whole executable name.
func TestValidateReviewToolRule_NearMissesAreAccepted(t *testing.T) {
	for _, in := range []string{"Bash(lesspipe x)", "Bash(tarpit *)", "Bash(viewer x)", "Bash(javac-lint x)", "Bash(commander *)", "Bash(shellcheck *)", "Bash(jq .)", "Bash(sort x)", "Bash(golangci-lint:*)"} {
		if _, err := validateReviewToolRule(in); err != nil {
			t.Errorf("%q: unexpected refusal %v", in, err)
		}
	}
}

// FR-003: git stays refused in every form, including the --output spellings.
func TestValidateReviewToolRule_GitOutputFormsStayRefused(t *testing.T) {
	for _, in := range []string{"Bash(git diff *)", "Bash(git diff --output=out.txt *)", "Bash(git diff --output out.txt *)", "Bash(git log *)", "Bash(/usr/bin/git diff --output=x)"} {
		if got, err := validateReviewToolRule(in); err == nil {
			t.Errorf("%q: expected refusal, accepted as %q", in, got)
		}
	}
}

// FR-005/SC-003: a rule injected past LoadConfig is refused where the arguments
// are built, with an error naming the rule.
func TestBuildReviewArgs_RefusesInvalidExtrasInjectedPastConfig(t *testing.T) {
	for _, bad := range []string{"Bash(tar *)", "Bash(Vim x)", "Bash(Python3 x)", "Bash(git diff --output=x *)", "Bash(*)", "Edit"} {
		req := reviewTestScope(t)
		req.Grant = reviewGrant{Extras: []string{"Bash(shellcheck *)", bad}}
		args, err := buildReviewArgs(req)
		if err == nil {
			t.Errorf("%q: buildReviewArgs succeeded with %v", bad, args)
			continue
		}
		if args != nil {
			t.Errorf("%q: args must be nil on error", bad)
		}
		if !strings.Contains(err.Error(), "review_tools entry") {
			t.Errorf("%q: error %v does not name the rule", bad, err)
		}
	}
}

// A rule that is valid but not in normalised form is refused too: the scoped
// check would reject it, and the grant must hold exactly what config produced.
func TestBuildReviewArgs_RefusesNonNormalisedExtra(t *testing.T) {
	req := reviewTestScope(t)
	req.Grant = reviewGrant{Extras: []string{"Bash(shellcheck  *)"}}
	_, err := buildReviewArgs(req)
	if err == nil || !strings.Contains(err.Error(), "normalised") {
		t.Errorf("error = %v, want a normalisation refusal", err)
	}
}

// Valid and empty extras build exactly as before.
func TestBuildReviewArgs_ValidAndEmptyExtrasUnchanged(t *testing.T) {
	req := reviewTestScope(t)
	baseline := mustReviewArgs(t, req)
	req.Grant = reviewGrant{Extras: []string{}}
	if got := mustReviewArgs(t, req); !slices.Equal(got, baseline) {
		t.Errorf("empty extras changed the args")
	}
	req.Grant = reviewGrant{Extras: []string{"Bash(shellcheck *)"}}
	if err := validateGrantExtras(req.Grant.Extras); err != nil {
		t.Errorf("valid extras refused: %v", err)
	}
	mustReviewArgs(t, req)
}

// FR-006: Review stops before any reviewer process starts and returns an error,
// which ReviewPR turns into "post nothing".
func TestReview_InvalidInjectedExtraStartsNoProcess(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selNeverInvoked, defStream)
	req, _ := selReqOp(t, "mine:deep")
	req.Grant = reviewGrant{Extras: []string{"Bash(tar *)"}}
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "Bash(tar *)") {
		t.Fatalf("res = %+v err = %v, want an error naming the rule", res, err)
	}
	if res.Text != "" {
		t.Errorf("result must be zero-value, got %+v", res)
	}
	if b, rerr := os.ReadFile(rec); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Errorf("claude was started: %q", b)
	}
}

func TestReviewPR_InvalidInjectedGrantPostsNothing(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selNeverInvoked, defStream)
	client := newFakeReviewer()
	clone, _ := fakeClone(t, nil)
	cfg := toolsCfg("Bash(tar *)") // bypasses LoadConfig validation
	pr := gh.PRDetails{Number: 1, Author: "alice", AuthorAssociation: "MEMBER", HeadSHA: "head-sha", BaseRef: "main"}
	out := ReviewPR(context.Background(), client, &RealClaudeInvoker{}, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if out.Err == nil || out.Reviewed {
		t.Errorf("outcome = %+v, want an error and no review", out)
	}
	if len(client.submitCalls) != 0 {
		t.Errorf("submitted %d reviews, want none", len(client.submitCalls))
	}
	if b, rerr := os.ReadFile(rec); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Errorf("claude was started: %q", b)
	}
}
