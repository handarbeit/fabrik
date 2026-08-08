package pruefer

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestDiffTooLargeMarker_RoundTrip(t *testing.T) {
	body := buildTooLargeNoticeBody(DiffSizeDetail{MeasuredBytes: 17_139_718, MaxBytes: 500_000}, "eb528d48")
	if !strings.Contains(body, diffTooLargeMarker("eb528d48")) {
		t.Fatalf("notice body does not embed its own marker: %q", body)
	}
	comments := []gh.Comment{{Body: body}}
	if !alreadyNoticedTooLarge(comments, "eb528d48") {
		t.Error("alreadyNoticedTooLarge() = false, want true — the posted notice must be recognized on the next poll")
	}
	if alreadyNoticedTooLarge(comments, "different-sha") {
		t.Error("alreadyNoticedTooLarge() = true for a different head SHA, want false — the marker is per-SHA")
	}
}

func TestAlreadyNoticedTooLarge_NoComments(t *testing.T) {
	if alreadyNoticedTooLarge(nil, "sha1") {
		t.Error("alreadyNoticedTooLarge(nil, ...) = true, want false")
	}
}

func TestAlreadyNoticedTooLarge_UnrelatedComments(t *testing.T) {
	comments := []gh.Comment{{Body: "just a regular comment"}, {Body: "/pruefer review"}}
	if alreadyNoticedTooLarge(comments, "sha1") {
		t.Error("alreadyNoticedTooLarge() = true for unrelated comments, want false")
	}
}

func TestBuildTooLargeNoticeBody_ContainsSizeCapAndDominantPaths(t *testing.T) {
	detail := DiffSizeDetail{
		MeasuredBytes: 17_139_718,
		MaxBytes:      500_000,
		DominantPaths: []PathSize{
			{Path: "packages/evals/corpus/quality-bench-corpus.jsonl", Bytes: 17_054_420},
		},
	}
	body := buildTooLargeNoticeBody(detail, "eb528d48")

	if !strings.Contains(body, "16.3 MB") {
		t.Errorf("body = %q, want the measured size rendered in human-readable form", body)
	}
	if !strings.Contains(body, "488.3 KB") {
		t.Errorf("body = %q, want the cap rendered in human-readable form", body)
	}
	if !strings.Contains(body, "packages/evals/corpus/quality-bench-corpus.jsonl") {
		t.Errorf("body = %q, want the dominant path named", body)
	}
	if !strings.Contains(body, "excluded_paths") {
		t.Errorf("body = %q, want actionable guidance mentioning excluded_paths", body)
	}
}

func TestBuildTooLargeNoticeBody_OmittedPathsSection(t *testing.T) {
	detail := DiffSizeDetail{
		MeasuredBytes: 600_000,
		MaxBytes:      500_000,
		OmittedPaths:  []string{"packages/evals/corpus/quality-bench-corpus.jsonl"},
	}
	body := buildTooLargeNoticeBody(detail, "sha1")
	if !strings.Contains(body, "Already excluded") || !strings.Contains(body, "packages/evals/corpus/quality-bench-corpus.jsonl") {
		t.Errorf("body = %q, want an already-excluded section naming the omitted path", body)
	}
}

func TestBuildTooLargeNoticeBody_TrimAttemptedSection(t *testing.T) {
	detail := DiffSizeDetail{
		MeasuredBytes: 600_000,
		MaxBytes:      500_000,
		TrimAttempted: []string{"pkg/big/generated.go"},
	}
	body := buildTooLargeNoticeBody(detail, "sha1")
	if !strings.Contains(body, "also tried automatically dropping") || !strings.Contains(body, "pkg/big/generated.go") {
		t.Errorf("body = %q, want a section naming the auto-drop attempt and the path it tried to drop", body)
	}
}

func TestBuildTooLargeNoticeBody_NoTrimAttempted_OmitsSection(t *testing.T) {
	detail := DiffSizeDetail{MeasuredBytes: 600_000, MaxBytes: 500_000}
	body := buildTooLargeNoticeBody(detail, "sha1")
	if strings.Contains(body, "also tried automatically dropping") {
		t.Errorf("body = %q, want no trim-attempted section when TrimAttempted is empty", body)
	}
}

// TestBuildTooLargeNoticeBody_ManyOmittedPaths_ListIsBounded proves the
// fix for the unbounded-comment-length gap: an excluded_paths glob that
// matches hundreds of files (e.g. a whole vendor/ tree) must not grow the
// notice body without limit — GitHub caps PR/issue comments at ~65536
// characters, and a comment that fails to post never gets its idempotency
// marker onto the PR, so the same failure would otherwise repeat forever.
func TestBuildTooLargeNoticeBody_ManyOmittedPaths_ListIsBounded(t *testing.T) {
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("vendor/pkg%d/file.go", i)
	}
	detail := DiffSizeDetail{MeasuredBytes: 600_000, MaxBytes: 500_000, OmittedPaths: paths}
	body := buildTooLargeNoticeBody(detail, "sha1")

	if len(body) > 10_000 {
		t.Errorf("body length = %d, want well under GitHub's ~65536-char comment cap for %d omitted paths", len(body), len(paths))
	}
	if !strings.Contains(body, "and 480 more") {
		t.Errorf("body = %q, want a truncation summary naming how many paths were omitted from the list", body)
	}
}

// TestBuildTooLargeNoticeBody_ManyTrimAttemptedPaths_ListIsBounded is the
// TrimAttempted analog of TestBuildTooLargeNoticeBody_ManyOmittedPaths_
// ListIsBounded — a trim pass that drops many small files must not grow
// the notice without limit either.
func TestBuildTooLargeNoticeBody_ManyTrimAttemptedPaths_ListIsBounded(t *testing.T) {
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("pkg/generated/file%d.go", i)
	}
	detail := DiffSizeDetail{MeasuredBytes: 600_000, MaxBytes: 500_000, TrimAttempted: paths}
	body := buildTooLargeNoticeBody(detail, "sha1")

	if len(body) > 10_000 {
		t.Errorf("body length = %d, want well under GitHub's ~65536-char comment cap for %d trim-attempted paths", len(body), len(paths))
	}
	if !strings.Contains(body, "and 480 more") {
		t.Errorf("body = %q, want a truncation summary naming how many trim-attempted paths were omitted from the list", body)
	}
}

func TestBuildAllExcludedNoticeBody_NamesExcludedPaths(t *testing.T) {
	body := buildAllExcludedNoticeBody([]string{"docs/readme.md", "docs/guide.md"})
	if !strings.Contains(body, "excluded_paths") {
		t.Errorf("body = %q, want it to mention excluded_paths", body)
	}
	if !strings.Contains(body, "docs/readme.md") || !strings.Contains(body, "docs/guide.md") {
		t.Errorf("body = %q, want both excluded paths named", body)
	}
}

func TestBuildAllExcludedNoticeBody_ManyPaths_ListIsBounded(t *testing.T) {
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("docs/page%d.md", i)
	}
	body := buildAllExcludedNoticeBody(paths)
	if len(body) > 10_000 {
		t.Errorf("body length = %d, want well under GitHub's ~65536-char comment cap for %d excluded paths", len(body), len(paths))
	}
}
