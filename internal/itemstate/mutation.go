package itemstate

import (
	"strconv"
	"time"

	gh "github.com/verveguy/fabrik/github"
)

// Mutation is a discriminated union of every possible state change.
// Every code path that wants to update ItemState expresses it as a Mutation
// and calls Store.Apply. There is no other write path.
type Mutation interface {
	// isMutation is a marker method that prevents non-Mutation types from
	// accidentally satisfying the interface.
	isMutation()
	// itemKey returns the "owner/repo#N" key identifying the affected item.
	// BoardReconciled returns "" because it affects multiple items.
	itemKey() string
}

// ---- Inbound webhook deltas ----

// IssueOpened is emitted when a new issue is created or first observed.
// Item is a gh.ProjectItem because there is no separate gh.Issue type; ProjectItem
// is the full per-item representation used throughout Fabrik.
type IssueOpened struct {
	Item gh.ProjectItem
}

func (IssueOpened) isMutation() {}
func (m IssueOpened) itemKey() string {
	return itemKeyFor(m.Item.Repo, m.Item.Number)
}

// IssueLabeled is emitted when a label is added to an issue.
type IssueLabeled struct {
	Repo   string
	Number int
	Label  string
}

func (IssueLabeled) isMutation() {}
func (m IssueLabeled) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueUnlabeled is emitted when a label is removed from an issue.
type IssueUnlabeled struct {
	Repo   string
	Number int
	Label  string
}

func (IssueUnlabeled) isMutation() {}
func (m IssueUnlabeled) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueClosed is emitted when an issue is closed.
type IssueClosed struct {
	Repo   string
	Number int
}

func (IssueClosed) isMutation() {}
func (m IssueClosed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueReopened is emitted when a previously closed issue is reopened.
type IssueReopened struct {
	Repo   string
	Number int
}

func (IssueReopened) isMutation() {}
func (m IssueReopened) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueCommentCreated is emitted when a new comment is added to an issue.
type IssueCommentCreated struct {
	Repo    string
	Number  int
	Comment gh.Comment
}

func (IssueCommentCreated) isMutation() {}
func (m IssueCommentCreated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ProjectV2ItemEdited is emitted when the project board status field is changed.
type ProjectV2ItemEdited struct {
	// ItemID is the GitHub Project item node ID (matches ItemState.ItemID).
	ItemID    string
	NewStatus string
}

func (ProjectV2ItemEdited) isMutation() {}
func (m ProjectV2ItemEdited) itemKey() string {
	// ItemID-based lookup is resolved by Store using its itemIDToKey index.
	return ""
}

// PRReviewSubmitted is emitted when a reviewer submits a review on the linked PR.
type PRReviewSubmitted struct {
	Repo   string
	Number int
	Review gh.PRReview
}

func (PRReviewSubmitted) isMutation() {}
func (m PRReviewSubmitted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRReviewCommentCreated is emitted when a new inline review comment is added.
type PRReviewCommentCreated struct {
	Repo     string
	PRNumber int
	Comment  gh.Comment
}

func (PRReviewCommentCreated) isMutation() {}
func (m PRReviewCommentCreated) itemKey() string { return itemKeyFor(m.Repo, m.PRNumber) }

// CheckRunCompleted is emitted when a CI check run reaches a terminal state.
// The SHA field is used to route the run to the correct LinkedPRState.
type CheckRunCompleted struct {
	Repo string
	SHA  string
	Run  gh.CheckRun
}

func (CheckRunCompleted) isMutation() {}
func (m CheckRunCompleted) itemKey() string {
	// SHA-based lookup is resolved by Store using its shaToKey index.
	return ""
}

// ---- Self-mutations (write-through from fabrik's own GitHub mutations) ----

// LocalStatusUpdated is applied after fabrik calls UpdateProjectItemStatus.
type LocalStatusUpdated struct {
	Repo      string
	Number    int
	NewStatus string
}

func (LocalStatusUpdated) isMutation() {}
func (m LocalStatusUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLabelAdded is applied after fabrik adds a label to an issue.
type LocalLabelAdded struct {
	Repo   string
	Number int
	Label  string
}

func (LocalLabelAdded) isMutation() {}
func (m LocalLabelAdded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLabelRemoved is applied after fabrik removes a label from an issue.
type LocalLabelRemoved struct {
	Repo   string
	Number int
	Label  string
}

func (LocalLabelRemoved) isMutation() {}
func (m LocalLabelRemoved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalCommentAdded is applied after fabrik posts a comment on an issue.
type LocalCommentAdded struct {
	Repo    string
	Number  int
	Comment gh.Comment
}

func (LocalCommentAdded) isMutation() {}
func (m LocalCommentAdded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLockAcquired is applied after fabrik adds the fabrik:locked:<user> label.
type LocalLockAcquired struct {
	Repo       string
	Number     int
	User       string
	Worker     *WorkerHandle
	AcquiredAt time.Time // caller-provided time; enables idempotent/no-op detection
}

func (LocalLockAcquired) isMutation() {}
func (m LocalLockAcquired) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLockReleased is applied after fabrik removes the fabrik:locked:<user> label.
type LocalLockReleased struct {
	Repo   string
	Number int
}

func (LocalLockReleased) isMutation() {}
func (m LocalLockReleased) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Periodic reconciliation ----

// BoardReconciled is submitted by the periodic poll loop after a full board fetch.
// The Store computes per-item deltas and applies them as focused mutations.
// itemKey returns "" because this mutation affects all items, not a single one.
type BoardReconciled struct {
	Items []gh.ProjectItem
}

func (BoardReconciled) isMutation() {}
func (m BoardReconciled) itemKey() string { return "" }

// ItemDeepFetched is applied after a single-item deep fetch from the GitHub API.
type ItemDeepFetched struct {
	Repo       string
	Number     int
	FreshState gh.ProjectItem
}

func (ItemDeepFetched) isMutation() {}
func (m ItemDeepFetched) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Engine internals ----

// StageAttempted records the start of a new Claude invocation for a stage.
type StageAttempted struct {
	Repo      string
	Number    int
	StageName string
	At        time.Time
}

func (StageAttempted) isMutation() {}
func (m StageAttempted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageRetryIncremented increments the attempt counter for a stage.
type StageRetryIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (StageRetryIncremented) isMutation() {}
func (m StageRetryIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageRetryCleared resets the attempt counter for a stage.
type StageRetryCleared struct {
	Repo      string
	Number    int
	StageName string
}

func (StageRetryCleared) isMutation() {}
func (m StageRetryCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewCycleIncremented increments the review cycle counter for a stage.
type ReviewCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ReviewCycleIncremented) isMutation() {}
func (m ReviewCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIFixCycleIncremented increments the CI-fix cycle counter for a stage.
type CIFixCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (CIFixCycleIncremented) isMutation() {}
func (m CIFixCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// RebaseCycleIncremented increments the rebase cycle counter for a stage.
type RebaseCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (RebaseCycleIncremented) isMutation() {}
func (m RebaseCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// EnginePaused records that the engine has paused work on a stage due to
// repeated failures. (The design doc listed this as "EngineEnginePaused" — typo.)
type EnginePaused struct {
	Repo      string
	Number    int
	StageName string
}

func (EnginePaused) isMutation() {}
func (m EnginePaused) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CooldownRecorded sets a cooldown expiry for a given reason key.
type CooldownRecorded struct {
	Repo   string
	Number int
	Reason string
	Until  time.Time
}

func (CooldownRecorded) isMutation() {}
func (m CooldownRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerHeartbeat updates the LastSignAt timestamp for the active worker.
type WorkerHeartbeat struct {
	Repo   string
	Number int
	At     time.Time
}

func (WorkerHeartbeat) isMutation() {}
func (m WorkerHeartbeat) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerExited clears the Worker handle when a Claude invocation finishes.
type WorkerExited struct {
	Repo   string
	Number int
}

func (WorkerExited) isMutation() {}
func (m WorkerExited) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// InvocationRecorded captures the outcome of a completed Claude invocation for TUI display.
type InvocationRecorded struct {
	Repo      string
	Number    int
	Completed bool
	Blocked   bool
	Usage     TokenUsage
}

func (InvocationRecorded) isMutation() {}
func (m InvocationRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// DeepFetchFailed records that a deep-fetch attempt for this item failed.
type DeepFetchFailed struct {
	Repo   string
	Number int
	At     time.Time
}

func (DeepFetchFailed) isMutation() {}
func (m DeepFetchFailed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// BaseBranchWarnRecorded marks a base-branch override as having been warned about.
type BaseBranchWarnRecorded struct {
	Repo   string
	Number int
	Branch string
}

func (BaseBranchWarnRecorded) isMutation() {}
func (m BaseBranchWarnRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// itemKeyFor constructs the canonical "owner/repo#N" item key used throughout the Store.
// Returns "" when repo is empty (indicating an invalid or unroutable mutation).
func itemKeyFor(repo string, number int) string {
	if repo == "" {
		return ""
	}
	return repo + "#" + strconv.Itoa(number)
}

// itoa is an alias for strconv.Itoa used by test helpers in this package.
func itoa(n int) string { return strconv.Itoa(n) }
