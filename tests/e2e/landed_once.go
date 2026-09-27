//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exactly-once landing assertion (#1874, regression coverage for #1871 /
// ADR-1871). #1871's defect: right after the merge train lands a member, the
// next batch formation could still see it in Queued from a stale snapshot and
// run its whole landing again — a second Done move, a second close and a second
// "Landed via …" comment. A scenario that only checks "the member landed" is
// satisfied by a double landing too, so this asserts the member landed ONCE.
//
// Split into a pure half (parsing + verdict, unit-tested in landed_once_test.go
// without the bed) and a thin I/O half (gh api reads + the settle wait).

// landingCommentPattern is the COUNTING pattern: it matches every landing
// comment the engine posts on a member's own PR, on all three landing paths
// (engine/merge_train.go):
//
//	landMergeTrainBatch:            "Landed via batch PR #%d."
//	finishSingletonFastPathLanding: "Landed via singleton fast path PR #%d. …"
//	landSingleton:                  "Landed one-at-a-time via singleton PR #%d."
//
// It is deliberately looser than landedPRPattern (which requires "PR #N." and
// captures the number, for extraction): counting must never miss a form because
// a suffix changed, and extraction must never be loosened by counting's needs.
// The post-merge guard reply (#1862) says "comment not applied" and cannot match.
var landingCommentPattern = regexp.MustCompile(`Landed (?:one-at-a-time )?via `)

// landingRetryWindow is the created_at gap under which two matching comments are
// treated as ONE landing. addLandedCommentWithRetry (engine/merge_train.go) retries
// a transient error within ~0.6s (200ms then 400ms backoff), so when the server
// stored a comment but the response failed, a legitimate identical duplicate
// appears within about a second. A real #1871 re-landing happens in a LATER poll —
// the bed polls every bedPollSeconds() (60s by default) — so it is far outside this
// window and still counts.
const landingRetryWindow = 10 * time.Second

// issueComment is the slice of a GitHub issue comment the assertion needs.
type issueComment struct {
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

// decodeStream decodes every JSON value in raw into a []T, accepting both
// shapes `gh api --paginate --jq` can produce: concatenated arrays (one per
// page) and a stream of bare objects (one per line). Whitespace between values
// is ignored.
func decodeStream[T any](raw []byte) ([]T, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var out []T
	for {
		var rm json.RawMessage
		if err := dec.Decode(&rm); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("decoding JSON stream: %w", err)
		}
		trimmed := strings.TrimSpace(string(rm))
		if strings.HasPrefix(trimmed, "[") {
			var many []T
			if err := json.Unmarshal(rm, &many); err != nil {
				return nil, fmt.Errorf("decoding JSON array: %w", err)
			}
			out = append(out, many...)
			continue
		}
		var one T
		if err := json.Unmarshal(rm, &one); err != nil {
			return nil, fmt.Errorf("decoding JSON object: %w", err)
		}
		out = append(out, one)
	}
}

// parseIssueComments parses `gh api --paginate` comment output (see decodeStream).
func parseIssueComments(raw []byte) ([]issueComment, error) {
	return decodeStream[issueComment](raw)
}

// landingComments returns the comments that are landing comments, in input order.
func landingComments(cs []issueComment) []issueComment {
	var out []issueComment
	for _, c := range cs {
		if landingCommentPattern.MatchString(c.Body) {
			out = append(out, c)
		}
	}
	return out
}

// collapseRetryDuplicates groups comments (any order) into clusters where each
// comment is within window of the previous one by created_at. One cluster is one
// landing; a cluster of >1 is a retried post (see landingRetryWindow).
func collapseRetryDuplicates(cs []issueComment, window time.Duration) [][]issueComment {
	sorted := append([]issueComment(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	var clusters [][]issueComment
	for _, c := range sorted {
		if n := len(clusters); n > 0 {
			last := clusters[n-1]
			if c.CreatedAt.Sub(last[len(last)-1].CreatedAt) <= window {
				clusters[n-1] = append(last, c)
				continue
			}
		}
		clusters = append(clusters, []issueComment{c})
	}
	return clusters
}

// timelineEvent is the one field of an issue timeline event the assertion reads.
type timelineEvent struct {
	Event string `json:"event"`
}

// countLifecycleEvents counts `closed` and `reopened` events in `gh api
// --paginate` issue-timeline output.
func countLifecycleEvents(raw []byte) (closed, reopened int, err error) {
	evs, err := decodeStream[timelineEvent](raw)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range evs {
		switch e.Event {
		case "closed":
			closed++
		case "reopened":
			reopened++
		}
	}
	return closed, reopened, nil
}

// describeComments renders matching comments (URL + body) for a failure message.
func describeComments(cs []issueComment) string {
	if len(cs) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "  - %s (%s)\n    %s\n", c.URL, c.CreatedAt.Format(time.RFC3339), strings.ReplaceAll(strings.TrimSpace(c.Body), "\n", "\n    "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// checkLandedExactlyOnce is the pure verdict. member names the member in every
// message. comments is the member PR's full comment list (non-landing comments
// are ignored); closed/reopened are the member ISSUE's timeline counts.
//
// It fails when there is not exactly one landing (zero: the best-effort comment
// post failed, #1275; more than one: the #1871 double landing), when the issue
// has not been closed exactly once, or when it was ever reopened. notes carries
// non-fatal observations (a collapsed retry duplicate) for the caller to log.
func checkLandedExactlyOnce(member string, comments []issueComment, closed, reopened int) (notes []string, err error) {
	matches := landingComments(comments)
	clusters := collapseRetryDuplicates(matches, landingRetryWindow)
	for _, cl := range clusters {
		if len(cl) > 1 {
			notes = append(notes, fmt.Sprintf("%s: %d identical landing comments within %s of each other — treated as one landing (retried post, addLandedCommentWithRetry)\n%s",
				member, len(cl), landingRetryWindow, describeComments(cl)))
		}
	}
	var problems []string
	switch {
	case len(clusters) == 0:
		problems = append(problems, fmt.Sprintf("found 0 landing comments (want exactly 1); the engine's best-effort landed-comment post (#1275) may have failed — check the bed log for %q", "could not post landed comment"))
	case len(clusters) > 1:
		problems = append(problems, fmt.Sprintf("found %d landing comments spread across separate polls (want exactly 1) — the member was landed more than once (#1871):\n%s", len(clusters), describeComments(matches)))
	}
	if closed != 1 {
		problems = append(problems, fmt.Sprintf("issue timeline has %d closed events (want exactly 1)", closed))
	}
	if reopened != 0 {
		problems = append(problems, fmt.Sprintf("issue timeline has %d reopened events (want 0)", reopened))
	}
	if len(problems) > 0 {
		return notes, fmt.Errorf("%s did not land exactly once: %s", member, strings.Join(problems, "; "))
	}
	return notes, nil
}

// ---------------------------------------------------------------------------
// I/O half
// ---------------------------------------------------------------------------

// landedMember identifies one member a scenario landed. Counting is always scoped
// to these numbers — never repo-wide — so it stays correct under t.Parallel().
type landedMember struct {
	Name  string
	Issue int
	PR    int
}

// landingSettleWait is how long to wait after every member has been observed
// landed before counting. The #1871 defect re-lands a member in the poll AFTER
// the landing (a stale Queued snapshot), so counting at the moment of landing
// passes vacuously. The bed engine polls every bedPollSeconds() (60s default,
// E2E_BED_POLL_SECONDS); two full polls past the landing is 2×, and a third
// absorbs cadence jitter and rate-limit backoff. The bed log has no per-poll
// line to count polls from, so the wait is derived from the configured cadence.
func landingSettleWait() time.Duration {
	secs, err := strconv.Atoi(bedPollSeconds())
	if err != nil || secs <= 0 {
		secs = 60
	}
	return time.Duration(3*secs) * time.Second
}

// fetchIssueCommentsDetailed reads every comment on an issue or PR (the same
// endpoint serves both) with body, html URL and created_at, following pagination.
func fetchIssueCommentsDetailed(env *Env, repo string, n int) ([]issueComment, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, n),
		"--jq", `.[] | {body: .body, url: .html_url, created_at: .created_at}`)
	if err != nil {
		return nil, fmt.Errorf("reading comments on %s#%d: %w\n%s", repo, n, err, out)
	}
	return parseIssueComments([]byte(out))
}

// fetchIssueLifecycleEvents counts the issue's closed/reopened timeline events.
func fetchIssueLifecycleEvents(env *Env, repo string, n int) (closed, reopened int, err error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return 0, 0, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/%d/timeline", owner, name, n),
		"--jq", `.[] | select(.event == "closed" or .event == "reopened") | {event: .event}`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading timeline of %s#%d: %w\n%s", repo, n, err, out)
	}
	return countLifecycleEvents([]byte(out))
}

// retryGH runs fn up to three times, 10s apart, so one transient gh error after a
// multi-minute settle wait does not fail a scenario that landed correctly.
func retryGH[T any](fn func() (T, error)) (T, error) {
	var v T
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		time.Sleep(10 * time.Second)
	}
	return v, err
}

// AssertMembersLandedExactlyOnce asserts each member landed exactly once (#1874):
// its PR carries exactly one landing comment (all three landing forms counted)
// and its issue has exactly one closed and no reopened timeline event.
//
// Call it only after every member has been observed landed (Done/closed). It
// sleeps landingSettleWait() ONCE, then counts all members, so it adds a fixed
// ~3 poll intervals per scenario regardless of member count.
//
// A1 (the comment count) is the load-bearing assertion: a #1871 re-landing posts
// a second landing comment. A2 (closed/reopened) is secondary — closing an
// already-closed issue creates no new timeline event, so it mainly catches a
// reopen-and-re-close (e.g. a false landing-verification reversal, ADR-1616).
//
// Counting is by comment body, never by author: in PAT mode the landing comment
// is authored by Fabrik's own account and in GitHub App mode by "<slug>[bot]",
// so an author filter would break one auth leg. logStart is the bed-log offset
// from before the scenario queued anything; on a zero count it is scanned for the
// engine's "could not post landed comment" warning to make the failure legible.
func AssertMembersLandedExactlyOnce(t *testing.T, env *Env, repo string, members []landedMember, logStart int64) {
	t.Helper()
	wait := landingSettleWait()
	t.Logf("exactly-once landing check: settling %s (3 × bed poll interval) before counting %d member(s)", wait, len(members))
	time.Sleep(wait)

	for _, m := range members {
		comments, err := retryGH(func() ([]issueComment, error) { return fetchIssueCommentsDetailed(env, repo, m.PR) })
		if err != nil {
			t.Fatalf("exactly-once landing check for %s: %v", m.Name, err)
		}
		type counts struct{ closed, reopened int }
		c, err := retryGH(func() (counts, error) {
			cl, re, err := fetchIssueLifecycleEvents(env, repo, m.Issue)
			return counts{cl, re}, err
		})
		if err != nil {
			t.Fatalf("exactly-once landing check for %s: %v", m.Name, err)
		}
		member := fmt.Sprintf("member %s (issue #%d, PR #%d on %s)", m.Name, m.Issue, m.PR, repo)
		notes, verr := checkLandedExactlyOnce(member, comments, c.closed, c.reopened)
		for _, n := range notes {
			t.Logf("exactly-once landing check note: %s", n)
		}
		if verr != nil {
			msg := verr.Error()
			if len(landingComments(comments)) == 0 {
				want := fmt.Sprintf("could not post landed comment on PR #%d", m.PR)
				for _, line := range readLogLinesFrom(t, env, logStart) {
					if strings.Contains(line, want) {
						msg += "\nbed log confirms the engine's best-effort post failed (known gap, #1275): " + strings.TrimSpace(line)
						break
					}
				}
			}
			t.Fatal(msg)
		}
		t.Logf("member %s landed exactly once: 1 landing comment on PR #%d, issue #%d closed once, never reopened", m.Name, m.PR, m.Issue)
	}
}
