package awaitvisible

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ClosingLinkageQuery is the GraphQL document the harness polls to see a PR's
// "Closes #N" linkage on BOTH sides: the issue's closedByPullRequestsReferences
// (the field the engine's own board fetch and broken-linkage check consult) and
// the PR's closingIssuesReferences (GitHub's computed PR-side field). One call,
// about one GraphQL point per poll.
const ClosingLinkageQuery = `query($owner:String!,$name:String!,$issue:Int!,$pr:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$issue){ closedByPullRequestsReferences(first:20, includeClosedPrs:true){ nodes{ number } } }
    pullRequest(number:$pr){ closingIssuesReferences(first:20){ nodes{ number } } }
  }
}`

// ClosingLinkage is what one ClosingLinkageQuery response shows.
type ClosingLinkage struct {
	IssueSide bool // the issue lists the PR in closedByPullRequestsReferences
	PRSide    bool // the PR lists the issue in closingIssuesReferences
}

// Both reports whether the linkage is visible on both sides.
func (c ClosingLinkage) Both() bool { return c.IssueSide && c.PRSide }

// String names the side(s) still missing, for a timeout reason.
func (c ClosingLinkage) String() string {
	return fmt.Sprintf("issue side=%t, PR side=%t", c.IssueSide, c.PRSide)
}

// ParseClosingLinkage reads a ClosingLinkageQuery response. A response that is
// not JSON, carries GraphQL errors, or has a null repository/issue/PR is an
// error (a failed read, retried by the caller) — never "linkage not visible".
func ParseClosingLinkage(out []byte, issueNum, prNum int) (ClosingLinkage, error) {
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			Repository *struct {
				Issue *struct {
					Closed struct {
						Nodes []struct {
							Number int `json:"number"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"issue"`
				PullRequest *struct {
					Closing struct {
						Nodes []struct {
							Number int `json:"number"`
						} `json:"nodes"`
					} `json:"closingIssuesReferences"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return ClosingLinkage{}, fmt.Errorf("parse closing-linkage response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return ClosingLinkage{}, fmt.Errorf("closing-linkage query error: %s", resp.Errors[0].Message)
	}
	repo := resp.Data.Repository
	if repo == nil || repo.Issue == nil || repo.PullRequest == nil {
		return ClosingLinkage{}, fmt.Errorf("closing-linkage response is missing the repository, issue #%d or PR #%d", issueNum, prNum)
	}
	var c ClosingLinkage
	for _, n := range repo.Issue.Closed.Nodes {
		if n.Number == prNum {
			c.IssueSide = true
		}
	}
	for _, n := range repo.PullRequest.Closing.Nodes {
		if n.Number == issueNum {
			c.PRSide = true
		}
	}
	return c, nil
}

// MergeableVerdict is what a PR's mergeable_state means to a scenario that needs
// the engine's landing decision to run on its first look at the item.
type MergeableVerdict int

const (
	// MergeablePending: GitHub has not computed mergeability yet ("unknown" or
	// an empty state).
	MergeablePending MergeableVerdict = iota
	// MergeableSettled: "clean" or "unstable" — the merge gate can clear (it then
	// classifies the individual checks).
	MergeableSettled
	// MergeableWaitable: computed, but not yet one the gate clears on ("blocked"
	// can still clear as CI or reviews settle; "draft", "has_hooks" likewise).
	MergeableWaitable
	// MergeableFatal: "dirty" or "behind" — will not resolve by waiting in a
	// scenario, so a caller that needs a settled PR fails immediately.
	MergeableFatal
)

// ClassifyMergeable maps a REST /pulls/N mergeable_state to a verdict. It is the
// single place the #1982 semantics live: clean/unstable accepted, dirty/behind
// fail fast, blocked and unknown keep waiting.
func ClassifyMergeable(state string) MergeableVerdict {
	switch strings.TrimSpace(state) {
	case "", "unknown":
		return MergeablePending
	case "clean", "unstable":
		return MergeableSettled
	case "dirty", "behind":
		return MergeableFatal
	default:
		return MergeableWaitable
	}
}

// MergeableComputed reports whether GitHub has computed mergeability at all
// (a non-null `mergeable`, i.e. mergeable_state other than "unknown").
func MergeableComputed(state string) bool { return ClassifyMergeable(state) != MergeablePending }

// HasLabel reports whether labels contains want (exact match).
func HasLabel(labels []string, want string) bool { return slices.Contains(labels, want) }
