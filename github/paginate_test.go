package github

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPaginateGraphQL_SinglePage(t *testing.T) {
	calls := 0
	nodes, totalCount, err := paginateGraphQL("test op", func(cursor string) ([]int, bool, string, int, error) {
		calls++
		if cursor != "" {
			t.Errorf("expected empty cursor on first call, got %q", cursor)
		}
		return []int{1, 2, 3}, false, "", 3, nil
	})
	if err != nil {
		t.Fatalf("paginateGraphQL: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(nodes) != 3 {
		t.Errorf("nodes = %v, want 3 items", nodes)
	}
	if totalCount != 3 {
		t.Errorf("totalCount = %d, want 3", totalCount)
	}
}

func TestPaginateGraphQL_MultiPageAccumulatesAndTracksMaxTotalCount(t *testing.T) {
	pages := [][]int{{1, 2}, {3}}
	cursors := []string{"", "cursor-1"}
	call := 0
	nodes, totalCount, err := paginateGraphQL("test op", func(cursor string) ([]int, bool, string, int, error) {
		if cursor != cursors[call] {
			t.Errorf("call %d: cursor = %q, want %q", call, cursor, cursors[call])
		}
		page := pages[call]
		hasNext := call < len(pages)-1
		endCursor := ""
		if hasNext {
			endCursor = "cursor-1"
		}
		call++
		return page, hasNext, endCursor, 3, nil
	})
	if err != nil {
		t.Fatalf("paginateGraphQL: %v", err)
	}
	if call != 2 {
		t.Errorf("expected 2 page fetches, got %d", call)
	}
	if len(nodes) != 3 || nodes[0] != 1 || nodes[1] != 2 || nodes[2] != 3 {
		t.Errorf("nodes = %v, want [1 2 3]", nodes)
	}
	if totalCount != 3 {
		t.Errorf("totalCount = %d, want 3", totalCount)
	}
}

func TestPaginateGraphQL_HasNextPageWithEmptyEndCursorErrors(t *testing.T) {
	_, _, err := paginateGraphQL("fetching widgets", func(cursor string) ([]int, bool, string, int, error) {
		return []int{1}, true, "", 1, nil
	})
	if err == nil {
		t.Fatal("expected error for hasNextPage=true with empty endCursor")
	}
	want := "fetching widgets: hasNextPage=true but endCursor is empty"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestPaginateGraphQL_FetchPageErrorPropagates(t *testing.T) {
	fetchErr := errors.New("boom")
	_, _, err := paginateGraphQL("test op", func(cursor string) ([]int, bool, string, int, error) {
		return nil, false, "", 0, fetchErr
	})
	if !errors.Is(err, fetchErr) {
		t.Fatalf("expected wrapped fetchErr, got %v", err)
	}
}

func TestRetryOnEmpty_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	result, err := retryOnEmpty("test op", func(attempt int) (string, int, int, error) {
		calls++
		return "ok", 5, 5, nil
	})
	if err != nil {
		t.Fatalf("retryOnEmpty: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (should not retry a healthy response)", calls)
	}
	if result != "ok" {
		t.Errorf("result = %q, want ok", result)
	}
}

func TestRetryOnEmpty_RetriesOnEmptyThenSucceeds(t *testing.T) {
	calls := 0
	result, err := retryOnEmpty("test op", func(attempt int) (string, int, int, error) {
		calls++
		if attempt < 2 {
			return "", 0, 0, nil // empty + zero totalCount: inconsistent, retry
		}
		return "recovered", 3, 3, nil
	})
	if err != nil {
		t.Fatalf("retryOnEmpty: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if result != "recovered" {
		t.Errorf("result = %q, want recovered", result)
	}
}

func TestRetryOnEmpty_ExhaustsAttemptsReturnsLastResult(t *testing.T) {
	calls := 0
	result, err := retryOnEmpty("test op", func(attempt int) (string, int, int, error) {
		calls++
		return "empty-result", 0, 0, nil
	})
	if err != nil {
		t.Fatalf("retryOnEmpty: %v", err)
	}
	if calls != projectBoardFetchAttempts {
		t.Errorf("calls = %d, want %d", calls, projectBoardFetchAttempts)
	}
	if result != "empty-result" {
		t.Errorf("result = %q, want empty-result", result)
	}
}

func TestRetryOnEmpty_ErrorReturnsImmediatelyWithoutRetry(t *testing.T) {
	calls := 0
	fetchErr := errors.New("transport failure")
	_, err := retryOnEmpty("test op", func(attempt int) (string, int, int, error) {
		calls++
		return "", 0, 0, fetchErr
	})
	if !errors.Is(err, fetchErr) {
		t.Fatalf("expected fetchErr, got %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (errors must not retry)", calls)
	}
}

func TestRetryOnEmpty_NonZeroRawCountAcceptedEvenWithZeroTotalCount(t *testing.T) {
	calls := 0
	result, err := retryOnEmpty("test op", func(attempt int) (string, int, int, error) {
		calls++
		return "legacy-shape", 2, 0, nil // rawCount > 0 alone is sufficient
	})
	if err != nil {
		t.Fatalf("retryOnEmpty: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if result != "legacy-shape" {
		t.Errorf("result = %q, want legacy-shape", result)
	}
}

// captureGHLogs routes the package logger to a slice for the test.
func captureGHLogs(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	old := Logf
	Logf = func(_ int, _, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = old })
	return &lines
}

// A zero that exhausts every attempt is silent: only the caller can say
// whether it is a plausible empty board (#1894).
func TestRetryOnEmpty_ExhaustedZeroIsSilent(t *testing.T) {
	lines := captureGHLogs(t)
	_, err := retryOnEmptyN("test op", 2, func(attempt int) (string, int, int, error) {
		return "", 0, 0, nil
	})
	if err != nil {
		t.Fatalf("retryOnEmptyN: %v", err)
	}
	if len(*lines) != 0 {
		t.Errorf("exhausted zero logged %v, want nothing", *lines)
	}
}

// A retry that recovers items logs exactly one line.
func TestRetryOnEmpty_RecoveryLogsOneLine(t *testing.T) {
	lines := captureGHLogs(t)
	_, err := retryOnEmptyN("test op", 3, func(attempt int) (string, int, int, error) {
		if attempt < 2 {
			return "", 0, 0, nil
		}
		return "ok", 4, 4, nil
	})
	if err != nil {
		t.Fatalf("retryOnEmptyN: %v", err)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "recovered 4 items after 2 attempts") {
		t.Errorf("lines = %v, want one recovery line", *lines)
	}
}

// A first-attempt success logs nothing.
func TestRetryOnEmpty_FirstAttemptSuccessIsSilent(t *testing.T) {
	lines := captureGHLogs(t)
	if _, err := retryOnEmptyN("test op", 3, func(int) (string, int, int, error) { return "ok", 1, 1, nil }); err != nil {
		t.Fatal(err)
	}
	if len(*lines) != 0 {
		t.Errorf("lines = %v, want none", *lines)
	}
}

func TestRetryOnEmptyN_SingleAttemptMakesOneCall(t *testing.T) {
	for _, attempts := range []int{1, 0, -3} {
		calls := 0
		start := time.Now()
		_, err := retryOnEmptyN("test op", attempts, func(int) (string, int, int, error) {
			calls++
			return "", 0, 0, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("attempts=%d: calls = %d, want 1", attempts, calls)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("attempts=%d: single attempt slept", attempts)
		}
	}
}
