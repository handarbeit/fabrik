package simgh

import (
	"fmt"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// repoState is everything the model knows about one owner/repo, plus the
// mutex serialising git subprocess calls against its backing bare repository.
type repoState struct {
	owner string
	repo  string

	// bareDir is the on-disk bare repository acting as this repo's origin.
	bareDir string

	// defaultBranch is the repo's default base branch (the branch the initial
	// commit lands on).
	defaultBranch string

	// gitMu serialises git subprocess invocations against bareDir. Git's
	// on-disk state is not concurrency-safe, so a model-level mutex is not
	// sufficient. Never acquire Sim.mu while holding this.
	gitMu sync.Mutex

	issues map[int]*issueRecord
	prs    map[int]*prRecord

	// checkRuns and commitStatuses are deliberately separate SHA-keyed
	// collections. Production distinguishes them (ADR-933: a required context
	// may be posted via the classic Statuses API rather than as an Actions
	// check run), so conflating them here would make that path untestable.
	checkRuns      map[string][]gh.CheckRun
	commitStatuses map[string][]gh.CommitStatus

	// requiredContexts maps a branch name to the set of check/status context
	// names branch protection requires on that branch. This is what makes the
	// blocked/unstable distinction reachable: a red *required* context yields
	// "blocked", a red non-required one yields "unstable".
	requiredContexts map[string][]string

	// requireUpToDate maps a branch name to branch protection's "require
	// branches to be up to date before merging" setting. It gates the
	// "behind" mergeable state: real GitHub reports "behind" only where this
	// is on, and reports a conflict-free but out-of-date PR as "clean"
	// otherwise.
	requireUpToDate map[string]bool

	// requiredApprovals maps a branch name to the approving-review count
	// branch protection requires, backing FetchPRReviewDecision.
	requiredApprovals map[string]int

	// labelVocab is the set of labels that exist in the repo, as maintained by
	// SeedLabels. The model does not enforce it on AddLabelToIssue — see
	// FIDELITY.md.
	labelVocab map[string]bool

	access        gh.RepoAccess
	latestRelease *gh.LatestRelease

	mergeQueueEnabled bool

	nextIssueNumber int
	nextPRNumber    int
}

// issueRecord is an issue's mutable state.
type issueRecord struct {
	number int
	title  string
	body   string
	// state is the GitHub enum: "OPEN" or "CLOSED".
	state     string
	author    string
	labels    []string
	assignees []string
	// labelAppliedAt records when each currently-applied label was applied,
	// read back by FetchLabelAppliedAt. Three separate engine mechanisms
	// anchor timeouts on this value, so it is load-bearing, not metadata.
	labelAppliedAt map[string]time.Time
	comments       []*commentRecord
	blockedBy      []gh.Dependency
	createdAt      time.Time
	updatedAt      time.Time
}

// nodeID synthesises the issue's GraphQL node ID. Sim node IDs are readable
// strings rather than opaque base64 blobs; production never parses a node ID
// it receives, only round-trips it. See FIDELITY.md.
func (i *issueRecord) nodeID(ownerRepo string) string {
	return fmt.Sprintf("issue:%s#%d", ownerRepo, i.number)
}

// prRecord is a pull request's mutable state. Anything git can answer about a
// PR (head SHA, mergeability, commits behind) is derived from the backing
// repository rather than stored here.
type prRecord struct {
	number int
	title  string
	body   string
	// head and base are branch names, resolved against the backing repo on
	// every read so a new seeded commit is immediately reflected.
	head string
	base string
	// state is the GitHub REST enum: "open" or "closed".
	state  string
	draft  bool
	merged bool
	author string
	labels []string

	// issueNumber is the issue passed to CreateDraftPR, if any. It seeds
	// FetchPRClosingIssues alongside the body-keyword scan.
	issueNumber int

	comments            []*commentRecord
	reviewThreadComment []*commentRecord

	reviews        []gh.PRReview
	reviewRequests []gh.ReviewRequest

	// resolvedThreads counts review threads marked resolved, surfaced as
	// ProjectItem.LinkedPRResolvedThreadCount for the engine's progress
	// detection.
	resolvedThreads int

	autoMergeEnabled  bool
	autoMergeStrategy string

	inMergeQueue bool
	queueEntry   *gh.MergeQueueEntry

	// mergeableRecomputeReads models GitHub's recompute window, during which
	// it reports mergeable as null and mergeableState as "unknown". While
	// non-zero, each read of a mergeable field returns the unresolved values
	// and decrements this counter by one; at zero, reads return the real
	// git-derived answer. Seeded explicitly rather than raced, because the
	// window is what once made a healthy issue (#1087) look wedged and no
	// scenario could cover that path if the *bool were never nil.
	mergeableRecomputeReads int

	createdAt time.Time
	updatedAt time.Time
}

// commentRecord is a comment on an issue or PR, including inline review-thread
// comments.
type commentRecord struct {
	databaseID int
	author     string
	body       string
	createdAt  time.Time
	reactions  map[string]int

	// fromPR is the PR number when the comment lives on a pull request, zero
	// for issue comments.
	fromPR int

	// Review-thread fields; zero-valued for ordinary comments.
	reviewThreadID string
	path           string
	line           int
	originalLine   int
	diffHunk       string
	isOutdated     bool
	threadResolved bool
}

func (c *commentRecord) nodeID() string { return fmt.Sprintf("comment:%d", c.databaseID) }

// toGH projects a commentRecord into the shape the engine consumes. Built
// fresh on every read so a reaction added a moment ago is always visible.
func (c *commentRecord) toGH() gh.Comment {
	out := gh.Comment{
		ID:             c.nodeID(),
		DatabaseID:     c.databaseID,
		Author:         c.author,
		Body:           c.body,
		CreatedAt:      c.createdAt,
		FromPR:         c.fromPR,
		ReviewThreadID: c.reviewThreadID,
		Path:           c.path,
		Line:           c.line,
		OriginalLine:   c.originalLine,
		DiffHunk:       c.diffHunk,
		IsOutdated:     c.isOutdated,
	}
	// Reaction groups are emitted in a stable order so tests comparing whole
	// projections are not order-flaky; GitHub's own ordering is not
	// contractual.
	for _, content := range sortedKeys(c.reactions) {
		if n := c.reactions[content]; n > 0 {
			out.Reactions = append(out.Reactions, gh.ReactionGroup{Content: content, Count: n})
		}
	}
	return out
}

// projectState is a Projects v2 board. Keyed by (owner, number) because
// Projects v2 is owner-scoped, not repo-scoped.
type projectState struct {
	id    string
	owner string
	num   int
	title string

	statusFieldID string
	// statusOptions maps a column name to its option ID.
	statusOptions map[string]string
	// orderedStatusNames preserves column order; the first is the leftmost.
	orderedStatusNames []string

	// items is keyed by item ID and holds board membership.
	items map[string]*itemState
	// itemOrder preserves insertion order so FetchProjectBoard returns a
	// deterministic sequence.
	itemOrder []string

	updatedAt time.Time
}

// itemState is one card on the board.
type itemState struct {
	itemID    string
	ownerRepo string
	number    int
	isPR      bool
	status    string
	archived  bool
	updatedAt time.Time
}

// contentNodeID is the node ID of the issue or PR the card points at.
func (it *itemState) contentNodeID() string {
	kind := "issue"
	if it.isPR {
		kind = "pr"
	}
	return fmt.Sprintf("%s:%s#%d", kind, it.ownerRepo, it.number)
}

func (p *projectState) nodeID() string { return fmt.Sprintf("project:%s:%d", p.owner, p.num) }

func itemNodeID(owner string, num int, ownerRepo string, number int) string {
	return fmt.Sprintf("item:%s:%d:%s#%d", owner, num, ownerRepo, number)
}
