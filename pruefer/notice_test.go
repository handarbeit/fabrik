package pruefer

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestDiffTooLargeMarker_KeyedToHeadSHA(t *testing.T) {
	m1 := diffTooLargeMarker("sha1")
	m2 := diffTooLargeMarker("sha2")
	if m1 == m2 {
		t.Fatalf("markers for different SHAs must differ: %q == %q", m1, m2)
	}
	if !strings.Contains(m1, "sha1") {
		t.Errorf("marker %q does not contain the head SHA", m1)
	}
}

func TestAlreadyNoticedTooLarge_NoComments(t *testing.T) {
	if alreadyNoticedTooLarge(nil, "sha1") {
		t.Error("expected false for no comments")
	}
}

func TestAlreadyNoticedTooLarge_MarkerPresent(t *testing.T) {
	comments := []gh.Comment{
		{Body: "unrelated"},
		{Body: buildDiffUnavailableNoticeBody("sha1")},
	}
	if !alreadyNoticedTooLarge(comments, "sha1") {
		t.Error("expected true when the marker for this SHA is present")
	}
}

func TestAlreadyNoticedTooLarge_MarkerForDifferentSHA(t *testing.T) {
	comments := []gh.Comment{
		{Body: buildDiffUnavailableNoticeBody("sha1")},
	}
	if alreadyNoticedTooLarge(comments, "sha2") {
		t.Error("a notice for a different (stale) head SHA must not suppress a fresh notice")
	}
}

func TestBuildDiffUnavailableNoticeBody_ContainsMarkerAndSHA(t *testing.T) {
	body := buildDiffUnavailableNoticeBody("deadbeef")
	if !strings.Contains(body, "deadbeef") {
		t.Errorf("body = %q, want it to mention the head SHA", body)
	}
	if !strings.Contains(body, diffTooLargeMarker("deadbeef")) {
		t.Errorf("body = %q, want it to embed the idempotency marker", body)
	}
}

func TestPostDiffUnavailableNoticeOnce_PostsWhenAbsent(t *testing.T) {
	f := &fakeCommenter{}
	if err := postDiffUnavailableNoticeOnce(f, "owner", "repo", 1, "sha1"); err != nil {
		t.Fatalf("postDiffUnavailableNoticeOnce: %v", err)
	}
	if f.addCommentCount() != 1 {
		t.Fatalf("addCommentCount = %d, want 1", f.addCommentCount())
	}
	if !strings.Contains(f.addedBodies[0], "sha1") {
		t.Errorf("posted body = %q, want it to reference sha1", f.addedBodies[0])
	}
}

func TestPostDiffUnavailableNoticeOnce_SkipsWhenAlreadyNoticed(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{Body: buildDiffUnavailableNoticeBody("sha1")},
	}}
	if err := postDiffUnavailableNoticeOnce(f, "owner", "repo", 1, "sha1"); err != nil {
		t.Fatalf("postDiffUnavailableNoticeOnce: %v", err)
	}
	if f.addCommentCount() != 0 {
		t.Errorf("addCommentCount = %d, want 0 (notice already posted for this SHA)", f.addCommentCount())
	}
}

func TestPostDiffUnavailableNoticeOnce_RepeatedCallsPostExactlyOnce(t *testing.T) {
	f := &fakeCommenter{}
	for i := 0; i < 3; i++ {
		if err := postDiffUnavailableNoticeOnce(f, "owner", "repo", 1, "sha1"); err != nil {
			t.Fatalf("call %d: postDiffUnavailableNoticeOnce: %v", i, err)
		}
	}
	if f.addCommentCount() != 1 {
		t.Fatalf("addCommentCount = %d, want exactly 1 across 3 calls", f.addCommentCount())
	}
}

func TestPostDiffUnavailableNoticeOnce_FetchErrorFailsClosed(t *testing.T) {
	f := &fakeCommenter{fetchErr: fmt.Errorf("rate limited")}
	if err := postDiffUnavailableNoticeOnce(f, "owner", "repo", 1, "sha1"); err == nil {
		t.Fatal("expected fetch error to propagate")
	}
	if f.addCommentCount() != 0 {
		t.Error("expected no comment posted when the idempotency fetch fails")
	}
}
