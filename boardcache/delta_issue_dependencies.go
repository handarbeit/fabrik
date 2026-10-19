package boardcache

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// lenientInt decodes a JSON number and yields 0 for any other JSON type, for
// the same reason lenientString exists: one mistyped field must not drop the
// whole delivery.
type lenientInt int

// UnmarshalJSON never returns an error.
func (n *lenientInt) UnmarshalJSON(b []byte) error {
	var v int
	if json.Unmarshal(b, &v) != nil {
		*n = 0
		return nil
	}
	*n = lenientInt(v)
	return nil
}

// issueDependenciesIssue declares only what identifies one side of a dependency
// change. The embedded issue_dependencies_summary, sub_issues_summary, state,
// labels and updated_at are deliberately not declared: they reflect the issue
// when GitHub rendered the delivery, which can be after the action, so they say
// nothing reliable about the change (#1995).
type issueDependenciesIssue struct {
	Number  lenientInt    `json:"number"`
	HTMLURL lenientString `json:"html_url"`
}

type issueDependenciesRepo struct {
	FullName lenientString `json:"full_name"`
}

// issueDependenciesPayload is the issue_dependencies webhook. One dependency
// change yields two deliveries, one per side: blocked_by_* carries
// blocking_issue_repo, blocking_* carries blocked_issue_repo. The issue objects
// carry no repository_url, so each side's repository comes from its own
// html_url, cross-checked against that side's repo object when present. The
// top-level `repository` is never read: no capture shows what it means for a
// cross-repository edge.
type issueDependenciesPayload struct {
	Action           lenientString          `json:"action"`
	BlockedIssue     issueDependenciesIssue `json:"blocked_issue"`
	BlockingIssue    issueDependenciesIssue `json:"blocking_issue"`
	BlockedIssueRepo issueDependenciesRepo  `json:"blocked_issue_repo"`
	BlockingRepo     issueDependenciesRepo  `json:"blocking_issue_repo"`
}

// issueDependenciesActions are the four actions GitHub documents for the event.
var issueDependenciesActions = map[string]bool{
	"blocked_by_added":   true,
	"blocked_by_removed": true,
	"blocking_added":     true,
	"blocking_removed":   true,
}

// repoFromIssueURL extracts "owner/repo" from an issue's html_url
// (https://<host>/<owner>/<repo>/issues/<n>).
func repoFromIssueURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// resolveDependencyIssue returns the (repo, number) of one side, or ok=false
// when it cannot be established without guessing: no number, no repository from
// either source, or two sources that disagree.
func resolveDependencyIssue(is issueDependenciesIssue, sideRepo issueDependenciesRepo) (string, int, bool) {
	number := int(is.Number)
	fromURL, urlOK := repoFromIssueURL(string(is.HTMLURL))
	fromSide := string(sideRepo.FullName)
	if number <= 0 {
		return "", 0, false
	}
	switch {
	case urlOK && fromSide != "" && !strings.EqualFold(fromURL, fromSide):
		return "", 0, false
	case urlOK:
		return fromURL, number, true
	case fromSide != "":
		if _, _, ok := splitRepo(fromSide); ok {
			return fromSide, number, true
		}
	}
	return "", 0, false
}

// applyIssueDependenciesDelta handles an issue_dependencies delivery (#1995,
// ADR 1995). It only invalidates: for each of the two named issues that is
// already in the Store it applies DependenciesInvalidated, so the item's next
// evaluation re-reads its blockers from GitHub, and the engine is woken (and, for
// a fabrik:blocked item, its dep-blocked admission cooldown is dropped). It never
// patches BlockedBy from the payload and never touches a label: whether an item
// is blocked or unblocked is decided by checkDependencies' live read (ADR 1419),
// so a stale, partial or reordered delivery cannot unblock anything.
//
// An issue that is not cached is ignored: no fetch, no error. Invalidation is
// idempotent, so the second delivery GitHub sends for the same change costs
// nothing.
func (c *CacheImpl) applyIssueDependenciesDelta(payload []byte, _ time.Time) {
	var p issueDependenciesPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyIssueDependenciesDelta: unmarshal payload: %v\n", err)
		return
	}
	action := string(p.Action)
	if !issueDependenciesActions[action] {
		c.logFn("[cache] issue_dependencies %q ignored: unknown action\n", action)
		return
	}

	type side struct {
		name string
		is   issueDependenciesIssue
		repo issueDependenciesRepo
	}
	for _, s := range []side{
		{"blocked", p.BlockedIssue, p.BlockedIssueRepo},
		{"blocking", p.BlockingIssue, p.BlockingRepo},
	} {
		repo, number, ok := resolveDependencyIssue(s.is, s.repo)
		if !ok {
			c.logFn("[cache] issue_dependencies %s: %s issue not identifiable, skipped\n", action, s.name)
			continue
		}
		if _, cached := c.store.Peek(repo, number); !cached {
			continue
		}
		c.store.Apply(itemstate.DependenciesInvalidated{Repo: repo, Number: number})
		c.logFn("[cache] issue_dependencies %s: invalidated %s#%d (%s)\n", action, repo, number, s.name)
	}
}
