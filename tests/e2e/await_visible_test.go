//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/inconclusive"
)

// fakeGH puts a `gh` on PATH that forwards its argv (one arg per line) to an
// httptest server, and answers with whatever handler returns. A non-2xx status
// makes curl, and therefore the fake gh, exit non-zero — a failed read. The
// harness reaches GitHub only through the gh CLI, so this is the seam that lets
// the wrapper layer be exercised against an httptest fake GitHub without
// depending on the real gh speaking plain HTTP to a local host.
func fakeGH(t *testing.T, handler func(args []string, call int) (int, string)) *Env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a POSIX shell script")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available for the fake gh")
	}
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		status, body := handler(strings.Split(strings.TrimRight(string(b), "\n"), "\n"), n)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" | curl -sSf --data-binary @- \"$FAKE_GH_URL\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_URL", srv.URL)
	return &Env{ProjectOwner: "acme", ProjectNumber: 2, GHToken: "tok"}
}

// fastClock replaces the wrapper seams with a clock that advances only on sleep.
func fastClock(t *testing.T) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	oldNow, oldSleep := awaitNow, awaitSleep
	awaitNow = func() time.Time { return now }
	awaitSleep = func(d time.Duration) { now = now.Add(d) }
	t.Cleanup(func() { awaitNow, awaitSleep = oldNow, oldSleep })
}

func has(args []string, s string) bool {
	for _, a := range args {
		if strings.Contains(a, s) {
			return true
		}
	}
	return false
}

func boardJSON(items ...string) string {
	return fmt.Sprintf(`{"data":{"repositoryOwner":{"projectV2":{"id":"P1","items":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}}}}}`, strings.Join(items, ","))
}

func boardNode(repo string, n int, status string) string {
	return fmt.Sprintf(`{"content":{"number":%d,"repository":{"nameWithOwner":%q}},"fieldValueByName":{"name":%q}}`, n, repo, status)
}

func TestAwaitBoardItemVisibleAfterLagWithTransientError(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func(args []string, call int) (int, string) {
		switch call {
		case 1:
			return 502, "bad gateway" // transient read error: retried, not "not visible"
		case 2:
			return 200, boardJSON(boardNode("acme/r", 1, "Specify")) // lagging listing
		default:
			return 200, boardJSON(boardNode("acme/r", 1, "Specify"), boardNode("acme/r", 7, "Queued"))
		}
	})
	res := awaitBoardItem(t, env, "acme/r", 7, "Queued", time.Minute)
	if !res.Visible || res.Attempts != 3 {
		t.Fatalf("want visible on the 3rd read, got %+v", res)
	}
}

func TestAwaitBoardItemStatusMustMatch(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func([]string, int) (int, string) {
		return 200, boardJSON(boardNode("acme/r", 7, "Specify"))
	})
	res := awaitBoardItem(t, env, "acme/r", 7, "Queued", 30*time.Second)
	if !res.TimedOut() || !strings.Contains(res.Reason(), `Status "Specify"`) {
		t.Fatalf("want a timeout that names the observed Status, got %+v", res)
	}
	if res := awaitBoardItem(t, env, "acme/r", 7, "", 30*time.Second); !res.Visible {
		t.Fatalf("with no Status required the item is visible, got %+v", res)
	}
}

func TestAwaitClosingLinkageNeedsBothSides(t *testing.T) {
	fastClock(t)
	const issueOnly = `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":12}]}},"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`
	const both = `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":12}]}},"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":5}]}}}}}`
	env := fakeGH(t, func(args []string, call int) (int, string) {
		if !has(args, "closingIssuesReferences") {
			t.Errorf("unexpected query: %v", args)
		}
		if call < 3 {
			return 200, issueOnly
		}
		return 200, both
	})
	res := awaitClosingLinkage(t, env, "acme/r", 5, 12, time.Minute)
	if !res.Visible || res.Attempts != 3 {
		t.Fatalf("want visible only once both sides show, on read 3, got %+v", res)
	}
}

func TestAwaitClosingLinkageNeverVisibleTimesOut(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func([]string, int) (int, string) {
		return 200, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[]}},"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`
	})
	res := awaitClosingLinkage(t, env, "acme/r", 5, 12, time.Minute)
	if !res.TimedOut() || !strings.Contains(res.Reason(), "issue side=false, PR side=false") {
		t.Fatalf("want a timeout naming both sides, got %+v", res)
	}
}

func TestAwaitPRMergeableComputedVersusSettled(t *testing.T) {
	fastClock(t)
	states := []string{"unknown", "unknown", "blocked", "clean"}
	env := fakeGH(t, func(args []string, call int) (int, string) {
		if !has(args, "pulls/9") {
			t.Errorf("unexpected call: %v", args)
		}
		return 200, states[min(call-1, len(states)-1)] + "\n"
	})
	// Computed: "blocked" is already computed (read 3), so the primitive returns.
	if res := awaitPRMergeable(t, env, "acme/r", 9, time.Minute, false); !res.Visible || res.Attempts != 3 {
		t.Fatalf("computed: want visible on read 3, got %+v", res)
	}
}

func TestAwaitPRMergeableSettledKeepsBlockedWaitingAndAcceptsClean(t *testing.T) {
	fastClock(t)
	states := []string{"unknown", "blocked", "unstable"}
	env := fakeGH(t, func(args []string, call int) (int, string) {
		return 200, states[min(call-1, len(states)-1)]
	})
	res := awaitPRMergeable(t, env, "acme/r", 9, time.Minute, true)
	if !res.Visible || res.Attempts != 3 {
		t.Fatalf("want settled on the unstable read (3), got %+v", res)
	}
}

func TestAwaitPRMergeableSettledFailsFastOnDirtyAndBehind(t *testing.T) {
	for _, state := range []string{"dirty", "behind"} {
		fastClock(t)
		env := fakeGH(t, func([]string, int) (int, string) { return 200, state })
		res := awaitPRMergeable(t, env, "acme/r", 9, time.Hour, true)
		if res.Aborted == nil || res.Attempts != 1 || res.TimedOut() {
			t.Fatalf("%s: want an immediate abort (a scenario assertion, not lag), got %+v", state, res)
		}
	}
}

func TestAwaitPRForBranchVisible(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func(args []string, call int) (int, string) {
		if !has(args, "head=acme:fabrik/issue-5") {
			t.Errorf("unexpected call: %v", args)
		}
		if call < 2 {
			return 200, "null"
		}
		return 200, "31\n"
	})
	pr, res := awaitPRForBranch(t, env, "acme/r", 5, time.Minute)
	if !res.Visible || pr != 31 {
		t.Fatalf("want PR 31, got %d %+v", pr, res)
	}
}

func TestAwaitLabelVisible(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func(args []string, call int) (int, string) {
		if call < 3 {
			return 200, `["bug"]`
		}
		return 200, `["bug","stage:Validate:complete"]`
	})
	AwaitLabelVisible(t, env, "acme/r", 5, "stage:Validate:complete", time.Minute)
}

// recordingTB stands in for *testing.T so the Result→outcome mapping can be
// observed: Skip ends the "test" with runtime.Goexit exactly as t.Skip does.
type recordingTB struct {
	testing.TB
	name    string
	skipMsg string
	fatal   string
}

func (r *recordingTB) Name() string        { return r.name }
func (r *recordingTB) Helper()             {}
func (r *recordingTB) Logf(string, ...any) {}
func (r *recordingTB) Skip(args ...any) {
	r.skipMsg = fmt.Sprint(args...)
	runtime.Goexit()
}
func (r *recordingTB) Fatalf(f string, args ...any) {
	r.fatal = fmt.Sprintf(f, args...)
	runtime.Goexit()
}

func runRecorded(name string, fn func(tb *recordingTB)) *recordingTB {
	tb := &recordingTB{name: name}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	return tb
}

func TestFinishAwaitMapsTimeoutToInconclusiveNeverAFailure(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func([]string, int) (int, string) { return 200, boardJSON() })
	tb := runRecorded("TestSeeded", func(tb *recordingTB) {
		finishAwait(tb, awaitBoardItem(tb, env, "acme/r", 7, "", time.Minute))
	})
	if tb.fatal != "" || !inconclusive.IsMarked(tb.skipMsg) {
		t.Fatalf("a timed-out wait must end Inconclusive, got fatal=%q skip=%q", tb.fatal, tb.skipMsg)
	}
	if !strings.Contains(tb.skipMsg, "board item acme/r#7") {
		t.Errorf("the reason must name what was waited for: %q", tb.skipMsg)
	}
}

func TestFinishAwaitFromSubtestIsALoudFatalf(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func([]string, int) (int, string) { return 200, boardJSON() })
	tb := runRecorded("TestSeeded/sub", func(tb *recordingTB) {
		finishAwait(tb, awaitBoardItem(tb, env, "acme/r", 7, "", time.Minute))
	})
	if tb.skipMsg != "" || !strings.Contains(tb.fatal, "subtest") {
		t.Fatalf("a subtest caller must fail loudly, got fatal=%q skip=%q", tb.fatal, tb.skipMsg)
	}
}

func TestFinishAwaitAbortIsAFailureNotInconclusive(t *testing.T) {
	fastClock(t)
	env := fakeGH(t, func([]string, int) (int, string) { return 200, "dirty" })
	tb := runRecorded("TestSeeded", func(tb *recordingTB) {
		finishAwait(tb, awaitPRMergeable(tb, env, "acme/r", 9, time.Hour, true))
	})
	if tb.skipMsg != "" || !strings.Contains(tb.fatal, "will not settle by itself") {
		t.Fatalf("dirty must stay a real failure, got fatal=%q skip=%q", tb.fatal, tb.skipMsg)
	}
}
