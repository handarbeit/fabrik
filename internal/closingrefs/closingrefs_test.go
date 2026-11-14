package closingrefs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const self = "handarbeit/fabrik"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keys(refs []Ref) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

func TestFindFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		opts    Options
		want    []string
	}{
		{"negated sentence still matches (#2556 shape)", "negated_2556.md", Options{}, []string{"handarbeit/fabrik#2497"}},
		{"bold keyword and bold with colon", "bold_keyword.md", Options{}, []string{"handarbeit/fabrik#41", "handarbeit/fabrik#42"}},
		{"owner/repo#N, case-insensitive repo", "cross_repo.md", Options{}, []string{"acme/widgets#7"}},
		{"issue URL", "issue_url.md", Options{}, []string{"handarbeit/fabrik#55"}},
		{"clean body", "clean.md", Options{}, []string{}},
		{"normal Fabrik work PR", "fabrik_work_pr.md", Options{}, []string{"handarbeit/fabrik#2576"}},
		{"only the first target after a keyword", "multi_target.md", Options{}, []string{"handarbeit/fabrik#1", "handarbeit/fabrik#3"}},
		{"case, italics, newline and link variants", "variants.md", Options{},
			[]string{"handarbeit/fabrik#21", "handarbeit/fabrik#22", "handarbeit/fabrik#23", "handarbeit/fabrik#24", "handarbeit/fabrik#25"}},
		// UNVERIFIED against GitHub: GH-N is an issue-reference form; matched to stay safe.
		{"GH-N reference form (UNVERIFIED)", "gh_style.md", Options{}, []string{"handarbeit/fabrik#2497", "handarbeit/fabrik#2498"}},
		// UNVERIFIED against GitHub: code is treated as a match (see testdata/README.md).
		{"code span matches by default (UNVERIFIED)", "code_span.md", Options{}, []string{"handarbeit/fabrik#61"}},
		{"fenced block matches by default (UNVERIFIED)", "fenced_block.md", Options{}, []string{"handarbeit/fabrik#62"}},
		{"unclosed fence matches by default (UNVERIFIED)", "unclosed_fence.md", Options{}, []string{"handarbeit/fabrik#63"}},
		{"code span exempt with ExemptCode", "code_span.md", Options{ExemptCode: true}, []string{}},
		{"fenced block exempt with ExemptCode", "fenced_block.md", Options{ExemptCode: true}, []string{}},
		{"unclosed fence exempt with ExemptCode", "unclosed_fence.md", Options{ExemptCode: true}, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keys(Find(fixture(t, tc.fixture), nil, self, tc.opts))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindLocationsAndPhrases(t *testing.T) {
	refs := Find(fixture(t, "negated_2556.md"), nil, self, Options{})
	if len(refs) != 1 || len(refs[0].Locations) != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	loc := refs[0].Locations[0]
	if loc.Where != "PR body" || loc.Phrase != "close #2497" {
		t.Fatalf("location = %+v", loc)
	}
}

func TestFindCommitMessage(t *testing.T) {
	commits := []Commit{
		{SHA: "aaa111", Message: "feat: add thing\n\nbody"},
		{SHA: "bbb222", Message: "fix: stop the crash\n\nFixes #2497"},
	}
	refs := Find("Closes #2576", commits, self, Options{})
	if got, want := keys(refs), []string{"handarbeit/fabrik#2576", "handarbeit/fabrik#2497"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if loc := refs[1].Locations[0]; loc.Where != "commit bbb222" || loc.Phrase != "Fixes #2497" {
		t.Fatalf("location = %+v", loc)
	}
}

func TestFindDedupesAcrossBodyAndCommits(t *testing.T) {
	commits := []Commit{{SHA: "ccc333", Message: "Resolves #9"}}
	refs := Find("closes #9 and also closes #9", commits, self, Options{})
	if len(refs) != 1 {
		t.Fatalf("want one ref, got %+v", refs)
	}
	if n := len(refs[0].Locations); n != 2 {
		t.Fatalf("want 2 locations (body once, commit), got %+v", refs[0].Locations)
	}
}

func TestFindForeignRepoIsNotConfusedWithSelf(t *testing.T) {
	refs := Find("Closes other/repo#5 and Closes #5", nil, self, Options{})
	if got, want := keys(refs), []string{"other/repo#5", "handarbeit/fabrik#5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFindNoMatches(t *testing.T) {
	for _, s := range []string{
		"", "Closes #", "Closes", "closest #1", "unfixed #2", "prefixes #3",
		"fix(engine): thing (#2498)", "Mentioned for context: #5", "#7 is out of scope here",
	} {
		if refs := Find(s, nil, self, Options{}); len(refs) != 0 {
			t.Errorf("%q: unexpected %+v", s, refs)
		}
	}
}

func TestInlineTripleBacktickIsNotAFence(t *testing.T) {
	// ```Closes #1``` on one line is an inline span, not an opening fence; the next line is live.
	body := "```Closes #1```\nCloses #2"
	got := keys(Find(body, nil, self, Options{ExemptCode: true}))
	if want := []string{"handarbeit/fabrik#2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSorted(t *testing.T) {
	in := []Ref{{Repo: "b/x", Number: 1}, {Repo: "a/x", Number: 9}, {Repo: "a/x", Number: 2}}
	if got, want := keys(Sorted(in)), []string{"a/x#2", "a/x#9", "b/x#1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The PR title can land in a merge or squash commit message, so it is scanned too (ADR 2576).
func TestFindTextsScansTitleWithLocation(t *testing.T) {
	refs := FindTexts(self, Options{},
		Text{Where: "PR title", Body: "fix: handle X (closes #2497)"},
		Text{Where: "PR body", Body: "Closes #2497"})
	if len(refs) != 1 || refs[0].Number != 2497 || len(refs[0].Locations) != 2 ||
		refs[0].Locations[0].Where != "PR title" || refs[0].Locations[1].Where != "PR body" {
		t.Fatalf("got %+v", refs)
	}
}
