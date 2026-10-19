package github

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// dateHeader is a fixed GitHub-clock Date header, deliberately far from the host
// clock so a fallback to time.Now() is unmistakable.
const (
	dateHeader = "Mon, 19 Oct 2026 04:00:07 GMT"
)

var dateHeaderTime = time.Date(2026, 10, 19, 4, 0, 7, 0, time.UTC)

func stampServer(t *testing.T, status int, body string, withDate bool) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withDate {
			w.Header().Set("Date", dateHeader)
		} else {
			w.Header()["Date"] = nil
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL("token", srv.URL)
}

func TestWriteStamp_AddCommentPrefersBodyTimestamp(t *testing.T) {
	c := stampServer(t, 201, `{"id":7,"created_at":"2026-10-19T03:59:58Z","updated_at":"2026-10-19T03:59:58Z"}`, true)
	if _, err := c.AddComment("o", "r", 5, "hi"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	want := time.Date(2026, 10, 19, 3, 59, 58, 0, time.UTC)
	if got := c.IssueWriteStamp("o/r", 5); !got.Equal(want) {
		t.Errorf("stamp = %v; want the comment's own %v (not Date %v)", got, want, dateHeaderTime)
	}
	if got := c.IssueWriteStamp("o/r", 6); !got.IsZero() {
		t.Errorf("another issue has a stamp: %v", got)
	}
}

func TestWriteStamp_UpdateIssueBodyUsesUpdatedAt(t *testing.T) {
	c := stampServer(t, 200, `{"number":5,"updated_at":"2026-10-19T03:59:50Z"}`, true)
	if err := c.UpdateIssueBody("o", "r", 5, "b"); err != nil {
		t.Fatalf("UpdateIssueBody: %v", err)
	}
	if got, want := c.IssueWriteStamp("o/r", 5), time.Date(2026, 10, 19, 3, 59, 50, 0, time.UTC); !got.Equal(want) {
		t.Errorf("stamp = %v; want %v", got, want)
	}
}

func TestWriteStamp_UpdateIssueBodyFallsBackToDate(t *testing.T) {
	c := stampServer(t, 200, `{}`, true)
	if err := c.UpdateIssueBody("o", "r", 5, "b"); err != nil {
		t.Fatalf("UpdateIssueBody: %v", err)
	}
	if got := c.IssueWriteStamp("o/r", 5); !got.Equal(dateHeaderTime) {
		t.Errorf("stamp = %v; want Date %v", got, dateHeaderTime)
	}
}

func TestWriteStamp_LabelWritesUseDateHeader(t *testing.T) {
	c := stampServer(t, 200, `[]`, true)
	if err := c.AddLabelToIssue("o", "r", 5, "fabrik:paused"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if got := c.IssueWriteStamp("o/r", 5); !got.Equal(dateHeaderTime) {
		t.Errorf("add: stamp = %v; want %v", got, dateHeaderTime)
	}
	c2 := stampServer(t, 200, `[]`, true)
	if err := c2.RemoveLabelFromIssue("o", "r", 6, "fabrik:paused"); err != nil {
		t.Fatalf("RemoveLabelFromIssue: %v", err)
	}
	if got := c2.IssueWriteStamp("o/r", 6); !got.Equal(dateHeaderTime) {
		t.Errorf("remove: stamp = %v; want %v", got, dateHeaderTime)
	}
}

// With no Date header and no timestamp in the body nothing is recorded: the
// client never substitutes the host clock (FR-003, SC-005).
func TestWriteStamp_NoGitHubTimeRecordsNothing(t *testing.T) {
	c := stampServer(t, 200, `[]`, false)
	if err := c.RemoveLabelFromIssue("o", "r", 5, "x"); err != nil {
		t.Fatalf("RemoveLabelFromIssue: %v", err)
	}
	if got := c.IssueWriteStamp("o/r", 5); !got.IsZero() {
		t.Errorf("stamp = %v; want zero", got)
	}
}

func TestWriteStamp_FailedWriteRecordsNothing(t *testing.T) {
	c := stampServer(t, 422, `{"message":"no"}`, true)
	if _, err := c.AddComment("o", "r", 5, "hi"); err == nil {
		t.Fatal("expected error")
	}
	if err := c.UpdateIssueBody("o", "r", 5, "b"); err == nil {
		t.Fatal("expected error")
	}
	if got := c.IssueWriteStamp("o/r", 5); !got.IsZero() {
		t.Errorf("a failed write recorded %v", got)
	}
}

func TestWriteStamp_StatusMoveUsesProjectItemUpdatedAt(t *testing.T) {
	c := stampServer(t, 200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1","updatedAt":"2026-10-19T03:59:40Z"}}}}`, true)
	if err := c.UpdateProjectItemStatus("PVT_1", "PVTI_1", "F", "O"); err != nil {
		t.Fatalf("UpdateProjectItemStatus: %v", err)
	}
	if got, want := c.ProjectItemWriteStamp("PVTI_1"), time.Date(2026, 10, 19, 3, 59, 40, 0, time.UTC); !got.Equal(want) {
		t.Errorf("stamp = %v; want %v", got, want)
	}
	// A status move bumps the project item, not the issue.
	if got := c.IssueWriteStamp("o/r", 5); !got.IsZero() {
		t.Errorf("issue stamp = %v", got)
	}
}

func TestWriteStamp_StatusMoveWithoutUpdatedAtRecordsNothing(t *testing.T) {
	c := stampServer(t, 200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`, true)
	if err := c.UpdateProjectItemStatus("PVT_1", "PVTI_1", "F", "O"); err != nil {
		t.Fatalf("UpdateProjectItemStatus: %v", err)
	}
	if got := c.ProjectItemWriteStamp("PVTI_1"); !got.IsZero() {
		t.Errorf("stamp = %v; want zero", got)
	}
}

// Display-field writes must never anchor the baseline (#2180).
func TestWriteStamp_DisplayFieldWritesRecordNothing(t *testing.T) {
	c := stampServer(t, 200, `{"data":{}}`, true)
	_ = c.UpdateProjectItemTextField("PVT_1", "PVTI_1", "F", "text")
	_ = c.ClearProjectItemField("PVT_1", "PVTI_1", "F")
	if got := c.ProjectItemWriteStamp("PVTI_1"); !got.IsZero() {
		t.Errorf("display-field write recorded %v", got)
	}
}

func TestWriteStamp_RaiseOnlyAndRaceSafe(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused")
	hi := time.Unix(2000, 0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.recordIssueWrite("o", "r", 1, time.Unix(int64(1000+i), 0))
			_ = c.IssueWriteStamp("o/r", 1)
		}(i)
	}
	wg.Wait()
	c.recordIssueWrite("o", "r", 1, hi)
	c.recordIssueWrite("o", "r", 1, time.Unix(10, 0))
	c.recordIssueWrite("o", "r", 1, time.Time{})
	if got := c.IssueWriteStamp("o/r", 1); !got.Equal(hi) {
		t.Errorf("stamp = %v; want %v", got, hi)
	}
}
