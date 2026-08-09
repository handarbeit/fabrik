package pruefer

import (
	"fmt"
	"strings"
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
		{
			// AC3 / #1496: the exact production shape (Pruefer never
			// approves — every one of its reviews is COMMENTED) that the
			// prior single-review-per-author fixtures never covered. Its
			// most recent submission's CommitID matches the current head,
			// so it must be skipped even though earlier submissions target
			// stale SHAs.
			name: "comment-only reviewer with multiple submissions is skipped when the most recent matches head",
			in: EligibilityInput{
				PR:       gh.PRDetails{Author: "alice", HeadSHA: "a08167a2"},
				BotLogin: "handarbeit-pruefer[bot]",
				ExistingReviews: []gh.PRReview{
					{Author: "handarbeit-pruefer[bot]", State: "COMMENTED", CommitID: "667bd208"},
					{Author: "handarbeit-pruefer[bot]", State: "COMMENTED", CommitID: "4b333a27"},
					{Author: "handarbeit-pruefer[bot]", State: "COMMENTED", CommitID: "a08167a2"},
				},
			},
			wantOK:     false,
			wantReason: SkipAlreadyReviewed,
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

// TestEligible_LogsComparedAndFoundSHA demonstrates R3/AC5: the head-SHA
// check must log both the SHA it compared against and the SHA it found,
// distinctly, so a healthy skip and a wrongly-pinned skip (the failure mode
// that went unnoticed for three weeks in #1496 — both previously produced
// the identical "already reviewed at this head SHA" line) can be told apart
// from pruefer.log output after the fact.
func TestEligible_LogsComparedAndFoundSHA(t *testing.T) {
	resetLogf(t)
	var lines []string
	Logf = func(prNumber int, tag, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	ok, reason := Eligible(EligibilityInput{
		PR:       gh.PRDetails{Author: "alice", HeadSHA: "a08167a2"},
		BotLogin: "handarbeit-pruefer[bot]",
		ExistingReviews: []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "COMMENTED", CommitID: "a08167a2"},
		},
	})
	if ok || reason != SkipAlreadyReviewed {
		t.Fatalf("Eligible() = (%v, %q), want (false, %q)", ok, reason, SkipAlreadyReviewed)
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line from the head-SHA check, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "compared=a08167a2") {
		t.Errorf("log line %q does not record the compared (head) SHA", lines[0])
	}
	if !strings.Contains(lines[0], "found=a08167a2") {
		t.Errorf("log line %q does not record the found (bot's collapsed) SHA", lines[0])
	}
	if !strings.Contains(lines[0], "skipping") {
		t.Errorf("log line %q does not record the skip outcome", lines[0])
	}
}

// TestEligible_LogsDistinguishesCompareFromFound covers the anomalous case
// directly: a proceed decision where the bot's found SHA differs from the
// compared head SHA must still log both values distinctly, not just the
// binary outcome — this is what would have surfaced the #1496 bug (the
// found SHA stuck at the bot's first review while the compared SHA kept
// advancing) had it existed at the time.
func TestEligible_LogsDistinguishesCompareFromFound(t *testing.T) {
	resetLogf(t)
	var lines []string
	Logf = func(prNumber int, tag, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	ok, _ := Eligible(EligibilityInput{
		PR:       gh.PRDetails{Author: "alice", HeadSHA: "new-sha"},
		BotLogin: "handarbeit-pruefer[bot]",
		ExistingReviews: []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "COMMENTED", CommitID: "stale-sha"},
		},
	})
	if !ok {
		t.Fatal("Eligible() = false, want true (head SHA advanced past the bot's found entry)")
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "compared=new-sha") || !strings.Contains(lines[0], "found=stale-sha") {
		t.Errorf("log line %q must record compared and found as distinct values", lines[0])
	}
	if !strings.Contains(lines[0], "proceeding") {
		t.Errorf("log line %q does not record the proceed outcome", lines[0])
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
