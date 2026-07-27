package pruefer

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestEligible(t *testing.T) {
	baseReviews := []gh.PRReview{
		{Author: "pruefer-bot[bot]", CommitID: "old-sha"},
	}

	tests := []struct {
		name       string
		in         EligibilityInput
		wantOK     bool
		wantReason SkipReason
	}{
		{
			name: "eligible: no exclusions, never reviewed",
			in: EligibilityInput{
				PR: gh.PRDetails{Author: "alice", HeadSHA: "sha1", Draft: false},
			},
			wantOK: true,
		},
		{
			name: "draft PR is skipped",
			in: EligibilityInput{
				PR: gh.PRDetails{Author: "alice", HeadSHA: "sha1", Draft: true},
			},
			wantOK:     false,
			wantReason: SkipDraft,
		},
		{
			name: "self-authored PR is skipped",
			in: EligibilityInput{
				PR:       gh.PRDetails{Author: "pruefer-bot[bot]", HeadSHA: "sha1"},
				BotLogin: "pruefer-bot[bot]",
			},
			wantOK:     false,
			wantReason: SkipSelfAuthored,
		},
		{
			name: "self-authored check is case-insensitive",
			in: EligibilityInput{
				PR:       gh.PRDetails{Author: "Pruefer-Bot[bot]", HeadSHA: "sha1"},
				BotLogin: "pruefer-bot[bot]",
			},
			wantOK:     false,
			wantReason: SkipSelfAuthored,
		},
		{
			name: "excluded author is skipped",
			in: EligibilityInput{
				PR:              gh.PRDetails{Author: "dependabot[bot]", HeadSHA: "sha1"},
				ExcludedAuthors: []string{"dependabot[bot]"},
			},
			wantOK:     false,
			wantReason: SkipExcludedAuthor,
		},
		{
			name: "non-excluded author is not skipped",
			in: EligibilityInput{
				PR:              gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedAuthors: []string{"dependabot[bot]"},
			},
			wantOK: true,
		},
		{
			name: "excluded label (any match) is skipped",
			in: EligibilityInput{
				PR:             gh.PRDetails{Author: "alice", HeadSHA: "sha1", Labels: []string{"docs", "skip-review"}},
				ExcludedLabels: []string{"skip-review"},
			},
			wantOK:     false,
			wantReason: SkipExcludedLabel,
		},
		{
			name: "non-matching labels are not skipped",
			in: EligibilityInput{
				PR:             gh.PRDetails{Author: "alice", HeadSHA: "sha1", Labels: []string{"docs"}},
				ExcludedLabels: []string{"skip-review"},
			},
			wantOK: true,
		},
		{
			name: "excluded path: all touched paths match is skipped",
			in: EligibilityInput{
				PR:            gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedPaths: []string{"docs/*"},
				ChangedPaths:  []string{"docs/a.md", "docs/b.md"},
			},
			wantOK:     false,
			wantReason: SkipExcludedPath,
		},
		{
			name: "excluded path: partial match is NOT skipped",
			in: EligibilityInput{
				PR:            gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedPaths: []string{"docs/*"},
				ChangedPaths:  []string{"docs/a.md", "engine/claude.go"},
			},
			wantOK: true,
		},
		{
			name: "excluded path: unknown changed paths never trigger exclusion",
			in: EligibilityInput{
				PR:            gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedPaths: []string{"docs/*"},
				ChangedPaths:  nil,
			},
			wantOK: true,
		},
		{
			name: "excluded path: ** matches nested paths (README's documented vendor/** example)",
			in: EligibilityInput{
				PR:            gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedPaths: []string{"vendor/**"},
				ChangedPaths:  []string{"vendor/pkg/foo/bar.go"},
			},
			wantOK:     false,
			wantReason: SkipExcludedPath,
		},
		{
			name: "excluded path: ** does not match outside the prefixed dir",
			in: EligibilityInput{
				PR:            gh.PRDetails{Author: "alice", HeadSHA: "sha1"},
				ExcludedPaths: []string{"vendor/**"},
				ChangedPaths:  []string{"engine/claude.go"},
			},
			wantOK: true,
		},
		{
			name: "already reviewed at this head SHA is skipped",
			in: EligibilityInput{
				PR:              gh.PRDetails{Author: "alice", HeadSHA: "old-sha"},
				BotLogin:        "pruefer-bot[bot]",
				ExistingReviews: baseReviews,
			},
			wantOK:     false,
			wantReason: SkipAlreadyReviewed,
		},
		{
			name: "new commit (different head SHA) is eligible again",
			in: EligibilityInput{
				PR:              gh.PRDetails{Author: "alice", HeadSHA: "new-sha"},
				BotLogin:        "pruefer-bot[bot]",
				ExistingReviews: baseReviews,
			},
			wantOK: true,
		},
		{
			name: "forced re-review bypasses already-reviewed-at-SHA",
			in: EligibilityInput{
				PR:              gh.PRDetails{Author: "alice", HeadSHA: "old-sha"},
				BotLogin:        "pruefer-bot[bot]",
				ExistingReviews: baseReviews,
				ForceReview:     true,
			},
			wantOK: true,
		},
		{
			name: "forced re-review does NOT bypass draft skip",
			in: EligibilityInput{
				PR:          gh.PRDetails{Author: "alice", HeadSHA: "sha1", Draft: true},
				ForceReview: true,
			},
			wantOK:     false,
			wantReason: SkipDraft,
		},
		{
			name: "forced re-review does NOT bypass self-authored skip",
			in: EligibilityInput{
				PR:          gh.PRDetails{Author: "pruefer-bot[bot]", HeadSHA: "sha1"},
				BotLogin:    "pruefer-bot[bot]",
				ForceReview: true,
			},
			wantOK:     false,
			wantReason: SkipSelfAuthored,
		},
		{
			name: "review by a different author at the same SHA does not count",
			in: EligibilityInput{
				PR:       gh.PRDetails{Author: "alice", HeadSHA: "old-sha"},
				BotLogin: "pruefer-bot[bot]",
				ExistingReviews: []gh.PRReview{
					{Author: "some-human", CommitID: "old-sha"},
				},
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Eligible(tt.in)
			if ok != tt.wantOK {
				t.Errorf("Eligible() ok = %v, want %v (reason=%q)", ok, tt.wantOK, reason)
			}
			if !ok && reason != tt.wantReason {
				t.Errorf("Eligible() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestParseChangedPaths(t *testing.T) {
	diff := `diff --git a/engine/claude.go b/engine/claude.go
index abc..def 100644
--- a/engine/claude.go
+++ b/engine/claude.go
@@ -1,1 +1,1 @@
-old
+new
diff --git a/docs/README.md b/docs/README.md
index 111..222 100644
--- a/docs/README.md
+++ b/docs/README.md
@@ -1,1 +1,1 @@
-old doc
+new doc
`
	got := ParseChangedPaths(diff)
	want := []string{"engine/claude.go", "docs/README.md"}
	if len(got) != len(want) {
		t.Fatalf("ParseChangedPaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ParseChangedPaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseChangedPaths_Empty(t *testing.T) {
	if got := ParseChangedPaths(""); got != nil {
		t.Errorf("ParseChangedPaths(\"\") = %v, want nil", got)
	}
	if got := ParseChangedPaths("not a diff"); got != nil {
		t.Errorf("ParseChangedPaths(garbage) = %v, want nil", got)
	}
}

func TestParseChangedPaths_Rename(t *testing.T) {
	diff := `diff --git a/old_name.go b/new_name.go
similarity index 100%
rename from old_name.go
rename to new_name.go
`
	got := ParseChangedPaths(diff)
	if len(got) != 1 || got[0] != "new_name.go" {
		t.Errorf("ParseChangedPaths(rename) = %v, want [new_name.go]", got)
	}
}
