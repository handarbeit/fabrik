//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Helpers for the App self-recognition scenarios (#1877). The pure ones are
// unit-tested in self_recognition_helpers_test.go, which also pins every
// engine-derived string below against the engine's own format strings.

// blockedCommentPrefix / blockedCommentWaitingHeader mirror
// engine/dependencies.go's blockedCommentPrefix and buildBlockedComment
// ("<prefix>\n\nWaiting for the following issues to close: #12, #13").
const (
	blockedCommentPrefix        = "🏭 **Fabrik — blocked on dependencies**"
	blockedCommentWaitingHeader = "Waiting for the following issues to close: "
)

// blockedCommentDeps parses the dependency list out of a blocked comment body.
// ok is false when body is not a blocked comment or carries no list. The list
// is returned as parsed tokens ("#12", "owner/repo#13") in sorted order, so
// callers compare sets — never substrings, since "#1" is a substring of "#12".
func blockedCommentDeps(body string) (deps []string, ok bool) {
	if !strings.HasPrefix(body, blockedCommentPrefix) {
		return nil, false
	}
	_, tail, found := strings.Cut(body, blockedCommentWaitingHeader)
	if !found {
		return nil, false
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return nil, false
	}
	for _, d := range strings.Split(tail, ",") {
		if d = strings.TrimSpace(d); d != "" {
			deps = append(deps, d)
		}
	}
	sort.Strings(deps)
	return deps, len(deps) > 0
}

// sameDepSet reports whether got (as returned by blockedCommentDeps) is exactly
// the same-repo set want, order-insensitively.
func sameDepSet(got []string, want ...int) bool {
	w := make([]string, len(want))
	for i, n := range want {
		w[i] = fmt.Sprintf("#%d", n)
	}
	sort.Strings(w)
	if len(got) != len(w) {
		return false
	}
	g := append([]string(nil), got...)
	sort.Strings(g)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// Log-line matchers. Engine log lines are "<RFC3339> [#N tag] msg" with no repo,
// so every matcher is keyed on the scenario's own issue number; "[#1 skip]" can
// never match "[#12 skip]" because the closing bracket follows the number.

// noHumanSkipLine matches the engine's "paused/awaiting-input item evaluated,
// no comment was human-authored, pause kept" line for issue n
// (engine/item.go, itemNeedsWork's two pause branches). A paused item that also
// carries fabrik:awaiting-input takes the awaiting-input branch first.
func noHumanSkipLine(line string, n int) bool {
	return (strings.Contains(line, fmt.Sprintf("[#%d skip] awaiting-input: ", n)) ||
		strings.Contains(line, fmt.Sprintf("[#%d skip] paused: ", n))) &&
		strings.Contains(line, "none human-authored")
}

// resumeLine matches the engine's "a human comment lifted the pause" lines for
// issue n (engine/item.go: the paused resume and the awaiting-input resume).
func resumeLine(line string, n int) bool {
	return strings.Contains(line, fmt.Sprintf("[#%d unpause] user commented on paused issue", n)) ||
		strings.Contains(line, fmt.Sprintf("[#%d unblock] user comment received", n))
}

// blockedLogPrefix is the engine's "waiting for" line for issue n
// (engine/dependencies.go's checkDependencies).
func blockedLogPrefix(n int) string {
	return fmt.Sprintf("[#%d blocked] waiting for ", n)
}

// reviewReinvokePrefix is the review-reinvoke dispatch line for issue n
// (engine/reinvoke.go's dispatchReinvoke, tag "review-reinvoke").
func reviewReinvokePrefix(n int) string {
	return fmt.Sprintf("[#%d review-reinvoke] re-invoking stage", n)
}

var reviewBodyRefRe = regexp.MustCompile(`review-body:\d+`)

// dispatchesReview reports whether a reinvoke dispatch line names the review
// with the given database id. Extract-then-compare, never substring: "review-body:12" is a
// prefix of "review-body:123".
func dispatchesReview(line string, reviewID int) bool {
	want := fmt.Sprintf("review-body:%d", reviewID)
	for _, ref := range reviewBodyRefRe.FindAllString(line, -1) {
		if ref == want {
			return true
		}
	}
	return false
}

// reviewAddressedMarkerComment is a PR comment body shaped like the one the
// engine posts after addressing review feedback (engine/pr.go's
// formatReviewFeedbackComment): the "🏭 **Fabrik" prefix plus the
// machine-readable marker engine/reviews.go's parseReviewIDsAddressedMarker
// reads. The prefix keeps it out of findNewComments, so the only path that can
// react to it is durablyAddressedReviewIDs — which honours it solely for
// comments authored by the engine's own identity.
func reviewAddressedMarkerComment(reviewID int) string {
	return fmt.Sprintf("🏭 **Fabrik — stage: Review (review feedback addressed)**\n\n"+
		"e2e harness stand-in for an engine-posted review-feedback comment (#1877).\n\n"+
		"<!-- fabrik:review-ids-addressed: %d -->", reviewID)
}

// bedPollInterval is the bed engine's poll cadence as a Duration.
func bedPollInterval() time.Duration {
	d, err := time.ParseDuration(bedPollSeconds() + "s")
	if err != nil || d <= 0 {
		return time.Minute
	}
	return d
}

// restComment is one issue/PR comment as the REST API reports it.
type restComment struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Login  string `json:"login"`
	Type   string `json:"type"`
	Eyes   int    `json:"eyes"`
	Rocket int    `json:"rocket"`
}

// listComments returns every comment on an issue or PR over REST (the endpoint
// is shared; GitHub numbers issues and PRs in one space). REST, so the author is
// the "<slug>[bot]" form and no GraphQL budget is spent.
func listComments(env *Env, repo string, number int) ([]restComment, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, number),
		"--jq", `.[] | {id, body, login: .user.login, type: .user.type, eyes: .reactions.eyes, rocket: .reactions.rocket}`)
	if err != nil {
		return nil, fmt.Errorf("listing comments on %s#%d: %v\n%s", repo, number, err, out)
	}
	return parseComments(out)
}

// parseComments parses listComments' jq output: one JSON object per line.
func parseComments(out string) ([]restComment, error) {
	var comments []restComment
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c restComment
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("parsing comment %q: %w", line, err)
		}
		comments = append(comments, c)
	}
	return comments, nil
}

// blockedComments returns the comments on the issue that start with the
// blocked-on-dependencies prefix (HasPrefix, not substring: a human quoting the
// prefix must not count).
func blockedComments(env *Env, repo string, number int) ([]restComment, error) {
	all, err := listComments(env, repo, number)
	if err != nil {
		return nil, err
	}
	var out []restComment
	for _, c := range all {
		if strings.HasPrefix(c.Body, blockedCommentPrefix) {
			out = append(out, c)
		}
	}
	return out, nil
}

// waitForLogLineWhere polls fabrik.log from offset until a line beginning with
// (containing) prefix also satisfies keep, or fails after timeout. Unlike
// WaitForLogLine it can require a second condition on the same line.
func waitForLogLineWhere(t *testing.T, env *Env, prefix string, keep func(string) bool, offset int64, timeout time.Duration, what string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		lines, err := allLogLinesContaining(env, prefix, offset)
		if err != nil {
			t.Fatalf("scanning %s from offset %d: %v", env.LogPath, offset, err)
		}
		for _, l := range lines {
			if keep(l) {
				return strings.TrimSpace(l)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (lines containing %q from offset %d)", timeout, what, prefix, offset)
		}
		time.Sleep(10 * time.Second)
	}
}

// anyLogLineWhere reports the first line containing prefix that satisfies keep,
// scanning once from offset ("" when none).
func anyLogLineWhere(t *testing.T, env *Env, prefix string, keep func(string) bool, offset int64) string {
	t.Helper()
	lines, err := allLogLinesContaining(env, prefix, offset)
	if err != nil {
		t.Fatalf("scanning %s from offset %d: %v", env.LogPath, offset, err)
	}
	for _, l := range lines {
		if keep(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// holdFor runs check every interval until d has elapsed (and once at the end),
// so an assertion of absence spans a stated wall-clock window rather than a
// single instant.
func holdFor(d, interval time.Duration, check func()) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		check()
	}
	check()
}

// decodeJSONObject decodes the first JSON object in out into v. ghOutputWithToken
// merges stderr into out (CombinedOutput), so a successful call can still carry
// gh noise (an upgrade notice, a deprecation warning) before or after the JSON;
// decoding from the first '{' and ignoring what follows keeps that environmental
// noise from failing a scenario as an unparseable response.
func decodeJSONObject(out string, v any) error {
	i := strings.Index(out, "{")
	if i < 0 {
		return fmt.Errorf("no JSON object in output")
	}
	return json.NewDecoder(strings.NewReader(out[i:])).Decode(v)
}

// createPendingReview opens a PENDING (unsubmitted) review with token and
// returns its id. A pending review is invisible to everyone else, and keeps its
// id when later submitted, so a scenario can act on the review's id before the
// engine can possibly see the review.
func createPendingReview(t *testing.T, token, repo string, prNumber int, body string) int {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutputWithToken(token, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, name, prNumber),
		"-f", "body="+body)
	if err != nil {
		t.Fatalf("creating pending review on %s PR #%d: %v\n%s", repo, prNumber, err, redactSecret(out, token))
	}
	var resp struct {
		ID    int    `json:"id"`
		State string `json:"state"`
	}
	if uerr := decodeJSONObject(out, &resp); uerr != nil || resp.ID == 0 {
		t.Fatalf("creating pending review on %s PR #%d: unparseable response (%v): %q", repo, prNumber, uerr, redactSecret(out, token))
	}
	if resp.State != "PENDING" {
		t.Fatalf("review %d on %s PR #%d was created in state %q, want PENDING", resp.ID, repo, prNumber, resp.State)
	}
	return resp.ID
}

// submitPendingReview submits a review created by createPendingReview.
func submitPendingReview(t *testing.T, token, repo string, prNumber, reviewID int, event, body string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutputWithToken(token, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d/events", owner, name, prNumber, reviewID),
		"-f", "event="+event, "-f", "body="+body)
	if err != nil {
		t.Fatalf("submitting review %d on %s PR #%d: %v\n%s", reviewID, repo, prNumber, err, redactSecret(out, token))
	}
}
