package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Covers the fifth self-write category (#1090): issue body edit via
// publishCommentOutput's FABRIK_ISSUE_UPDATE handling. Unlike the mutate.go
// call sites, this one has no cache write-through to piggyback on, so the
// SelfWriteObserved call is standalone — these tests verify it fires only on
// a successful UpdateIssueBody call.

// bodyOnlyOutput contains nothing but a FABRIK_ISSUE_UPDATE block: after
// publishCommentOutput strips it, the remaining output is empty, so the
// function's later stage-comment posting (which would independently advance
// the staleness baseline via postComment — see mutate_test.go) is skipped.
// This isolates the assertions below to the issue-body-edit call site only.
const bodyOnlyOutput = "FABRIK_ISSUE_UPDATE_BEGIN\nupdated spec body\nFABRIK_ISSUE_UPDATE_END\n"

func TestPublishCommentOutput_BodyEditAdvancesStaleness(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 50, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Specify"}
	before := time.Now()

	eng.publishCommentOutput("owner", "repo", item, stage, nil, bodyOnlyOutput, "", "main")

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 50); got.Before(before) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want advanced to >= %v after a successful issue body edit", got, before)
	}
}

func TestPublishCommentOutput_BodyEditErrorDoesNotAdvanceStaleness(t *testing.T) {
	client := &mockGitHubClient{
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 51, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Specify"}

	eng.publishCommentOutput("owner", "repo", item, stage, nil, bodyOnlyOutput, "", "main")

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 51); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want unchanged (zero) when UpdateIssueBody fails", got)
	}
}
