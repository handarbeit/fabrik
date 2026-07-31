package pruefer

import (
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
