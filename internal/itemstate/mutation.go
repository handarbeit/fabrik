package itemstate

import (
	"strconv"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
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

// FromProbe wraps a single-item mutation applied by the per-poll board probe
// loop (#2080). The Store applies Inner unchanged and tags the resulting Change
// with OriginProbe, which the wake observer ignores: the probe runs at the start
// of the poll that dispatches, so its own writes must not request another poll.
// Do not wrap multi-item mutations (BoardReconciled, ProjectV2ItemEdited,
// CheckRunCompleted); Apply routes those by concrete type and would not unwrap.
type FromProbe struct {
	Inner Mutation
}

func (FromProbe) isMutation()       {}
func (m FromProbe) itemKey() string { return m.Inner.itemKey() }

// FromLiveRead wraps a single-item mutation that a gate applies after reading
// GitHub live inside the poll (#2350). The Store applies Inner unchanged and tags
// the resulting Change with OriginLiveRead, which the wake observer ignores: the
// read happens inside a poll that is already deciding on the item, so its own
// write must not request another poll.
type FromLiveRead struct {
	Inner Mutation
}

func (FromLiveRead) isMutation()       {}
func (m FromLiveRead) itemKey() string { return m.Inner.itemKey() }

// LabelsRead applies a full label set read live from GitHub (FetchLabels) by a
// gate (#2350). FetchedAt is the instant the request was issued; the read is
// ordered like a listing (fetch start minus the listing-lag allowance) and goes
// through the engine-write protection, so a label the engine wrote after the
// read began is not cleared by it. A failed read is never applied.
type LabelsRead struct {
	Repo      string
	Number    int
	Labels    []string
	FetchedAt time.Time
}

func (LabelsRead) isMutation()       {}
func (m LabelsRead) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRReviewsRead applies the review requests and reviews of the item's linked PR
// read live from GitHub (FetchPRReviews + FetchPRReviewRequests) by a gate
// (#2350). Both halves must have been read: a partial read is never applied.
// FetchedAt is the request start; the read is ordered like a live deep fetch.
type PRReviewsRead struct {
	Repo      string
	Number    int // issue number
	PRNumber  int
	Reviews   []gh.PRReview
	Requests  []gh.ReviewRequest
	FetchedAt time.Time
}

func (PRReviewsRead) isMutation()       {}
func (m PRReviewsRead) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LinkedPRStateObserved sets only LinkedPR.State and Merged from a live PR read
// (FetchPRMerged) by a gate (#2350). Unlike PRDetailsUpdated it never touches
// Title or Draft. At is the request start: the read is judged by it as is (a
// direct GET does not lag), and the baseline it records gets the listing-lag
// allowance like every other fetch.
type LinkedPRStateObserved struct {
	Repo     string
	Number   int // issue number
	PRNumber int
	State    string
	Merged   bool
	At       time.Time
}

func (LinkedPRStateObserved) isMutation()       {}
func (m LinkedPRStateObserved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Inbound webhook deltas ----

// IssueOpened is emitted when a new issue is created or first observed.
// Item is a gh.ProjectItem because there is no separate gh.Issue type; ProjectItem
// is the full per-item representation used throughout Fabrik.
type IssueOpened struct {
	// Partial is set for a payload that does not carry the whole item (an
	// issues.opened or projects_v2_item.created webhook): only the fields it
	// carries are written, labels and milestone are ordered against their
	// records, and an item that is already populated keeps everything else
	// (#2348, R3). Listing callers leave it false.
	Partial bool
	// At is the payload's observation time for a Partial item (see IssueLabeled.At).
	At   time.Time
	Item gh.ProjectItem

	// PreserveBlockedBy is set by callers whose Item.BlockedBy is known to be
	// incomplete rather than genuinely empty — specifically, a GitHub
	// "issues.opened" webhook payload, which structurally never carries Issue
	// Dependency data. When true, applyToItem keeps the existing item's
	// BlockedBy instead of overwriting it with Item's (always-empty) value —
	// protecting a dependency edge written synchronously via
	// BlockedByEdgeAdded (#1783) moments earlier for a newly-created item
	// from being wiped by an out-of-order "opened" webhook delivery. Callers
	// whose Item.BlockedBy comes from a genuine deep-fetch (authoritative)
	// must leave this false so a since-removed dependency is still cleared.
	PreserveBlockedBy bool
}

func (IssueOpened) isMutation() {}
func (m IssueOpened) itemKey() string {
	return itemKeyFor(m.Item.Repo, m.Item.Number)
}

// IssueMilestoneUpdated is emitted for the "issues" webhook actions
// "milestoned" and "demilestoned" (#1967 R10). A nil Milestone means the issue
// now has no milestone (demilestoned); either way the value is authoritative,
// so the item's MilestoneKnown becomes true.
type IssueMilestoneUpdated struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At        time.Time
	Repo      string
	Number    int
	Milestone *gh.Milestone
}

func (IssueMilestoneUpdated) isMutation()       {}
func (m IssueMilestoneUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueLabeled is emitted when a label is added to an issue.
type IssueLabeled struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At     time.Time
	Repo   string
	Number int
	Label  string
	// Sender is the webhook's sender.login ("" when unknown).
	Sender string
	// EchoOfEngine is true when the engine's echo registry matched this
	// webhook, i.e. it is the echo of a label write Fabrik made itself.
	EchoOfEngine bool
	// Requester is the sanitised overseer subscriber behind the write (#2125);
	// "" for an engine-autonomous write. A webhook mutation carries it only when
	// the engine's echo registry matched and recorded one.
	Requester string
}

func (IssueLabeled) isMutation()       {}
func (m IssueLabeled) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueUnlabeled is emitted when a label is removed from an issue.
type IssueUnlabeled struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At     time.Time
	Repo   string
	Number int
	Label  string
	// Sender is the webhook's sender.login ("" when unknown).
	Sender string
	// EchoOfEngine is true when the engine's echo registry matched this
	// webhook, i.e. it is the echo of a label write Fabrik made itself.
	EchoOfEngine bool
	// Requester is the sanitised overseer subscriber behind the write (#2125);
	// "" for an engine-autonomous write. A webhook mutation carries it only when
	// the engine's echo registry matched and recorded one.
	Requester string
}

func (IssueUnlabeled) isMutation()       {}
func (m IssueUnlabeled) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueClosed is emitted when an issue is closed.
type IssueClosed struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At     time.Time
	Repo   string
	Number int
	// Sender is the GitHub login that closed the issue, from the webhook
	// payload; empty when unknown (#2123).
	Sender string
	// There is no echo flag: the engine registers no issues/closed echo, so a
	// close Fabrik made is attributable only through its LocalIssueClosed
	// write-through (OriginEngine), never through this webhook mutation.
}

func (IssueClosed) isMutation()       {}
func (m IssueClosed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueReopened is emitted when a previously closed issue is reopened.
type IssueReopened struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At     time.Time
	Repo   string
	Number int
}

func (IssueReopened) isMutation()       {}
func (m IssueReopened) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueEdited is emitted when an issue's title or body is changed.
type IssueEdited struct {
	Repo   string
	Number int
	Title  string
	Body   string
}

func (IssueEdited) isMutation()       {}
func (m IssueEdited) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueAssigneesUpdated is emitted when the assignee list changes (assigned or unassigned).
// Assignees is the full post-mutation list from the webhook payload.
type IssueAssigneesUpdated struct {
	Repo      string
	Number    int
	Assignees []string
}

func (IssueAssigneesUpdated) isMutation()       {}
func (m IssueAssigneesUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRReviewRequested is emitted when reviewers are added to the linked PR.
// Reviewers is the full post-mutation list from the webhook payload.
type PRReviewRequested struct {
	Repo      string
	Number    int // issue number
	Reviewers []gh.ReviewRequest
}

func (PRReviewRequested) isMutation()       {}
func (m PRReviewRequested) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRReviewRequestRemoved is emitted when one reviewer is removed from the linked PR.
type PRReviewRequestRemoved struct {
	Repo   string
	Number int // issue number
	Login  string
}

func (PRReviewRequestRemoved) isMutation()       {}
func (m PRReviewRequestRemoved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueCommentCreated is emitted when a new comment is added to an issue.
type IssueCommentCreated struct {
	Repo    string
	Number  int
	Comment gh.Comment
}

func (IssueCommentCreated) isMutation()       {}
func (m IssueCommentCreated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ProjectV2ItemEdited is emitted when the project board status field is changed.
type ProjectV2ItemEdited struct {
	// ItemID is the GitHub Project item node ID (matches ItemState.ItemID).
	ItemID    string
	NewStatus string
	// Sender is the GitHub login behind a webhook-delivered move; empty for a
	// sweep- or poll-delivered one (#2123).
	Sender string
	// EchoOfEngine is true when the webhook matched the engine's own echo
	// registry for this write (#2123).
	EchoOfEngine bool
	// Requester is the overseer subscriber recorded with the engine echo.
	Requester string
	// At is the GitHub time of the move when known; zero otherwise.
	At time.Time
	// Observed is the ordering time (#2348): At when the payload carried one,
	// otherwise the receipt time. Zero means no ordering information, and At is
	// used for ordering instead.
	Observed time.Time
	// ObservedIsFetchStart marks Observed as the start of a bulk read (the Layer 2
	// status sweep) rather than a webhook's own time: the listing-lag allowance
	// applies to it like any other read (#2348).
	ObservedIsFetchStart bool
}

func (ProjectV2ItemEdited) isMutation() {}
func (m ProjectV2ItemEdited) itemKey() string {
	// ItemID-based lookup is resolved by Store using its itemIDToKey index.
	return ""
}

// PRReviewSubmitted is emitted when a reviewer submits a review on the linked PR.
type PRReviewSubmitted struct {
	// At is the observation time of the fact: the payload's GitHub time where
	// one exists, otherwise the receipt time (#2348). Zero means no ordering
	// information: the write applies and records nothing.
	At time.Time
	// Revision marks an "edited" or "dismissed" delivery: it concerns an earlier
	// submission and carries no submission time of its own, so At is its receipt
	// time. It is not ordered against, and does not advance, the author's record
	// of submissions, so a later-processed "submitted" event for a newer review
	// by the same author is never dropped behind it (#2348).
	Revision bool
	Repo     string
	Number   int
	Review   gh.PRReview
}

func (PRReviewSubmitted) isMutation()       {}
func (m PRReviewSubmitted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRReviewCommentCreated is emitted when a new inline review comment is added.
type PRReviewCommentCreated struct {
	Repo     string
	PRNumber int
	Comment  gh.Comment
}

func (PRReviewCommentCreated) isMutation()       {}
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

// ---- Cache enrichment mutations (populated from read-side lookups) ----

// ItemIDRegistered sets the project-item node ID for an existing Store entry that
// was added without one (e.g., via issues.opened before projects_v2_item.created).
// This is triggered by the Layer 1 fallback GraphQL lookup, not by a Fabrik-initiated
// GitHub mutation. Returns ChangeFlags(0) — the itemIDToKey reverse index is updated
// via reflect.DeepEqual detection in applySingleItem regardless of the returned flags.
// Dispatch is triggered only by the subsequent UpdateItemStatus call, not by this.
type ItemIDRegistered struct {
	Repo   string
	Number int
	ItemID string
}

func (ItemIDRegistered) isMutation()       {}
func (m ItemIDRegistered) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Self-mutations (write-through from fabrik's own GitHub mutations) ----

// LocalStatusUpdated is applied after fabrik calls UpdateProjectItemStatus.
type LocalStatusUpdated struct {
	Repo      string
	Number    int
	NewStatus string
	// Requester is the sanitised overseer subscriber that requested the move
	// (#2123); empty for an engine-autonomous advance.
	Requester string
}

func (LocalStatusUpdated) isMutation()       {}
func (m LocalStatusUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalIssueClosed is applied after fabrik closes an issue. Unlike the webhook
// IssueClosed it records an engine write so a reconcile listing fetched before
// the close cannot reopen the cached item (#2159). Its Change has OriginEngine
// (#2123).
type LocalIssueClosed struct {
	Repo   string
	Number int
}

func (LocalIssueClosed) isMutation()       {}
func (m LocalIssueClosed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalIssueBodyUpdated is applied after fabrik rewrites an issue body on
// GitHub (#2346). It sets Body only — never Title — so the cached copy that
// feeds .fabrik-context/issue.md matches what the engine just wrote, with no
// webhook echo needed. An identical body is a no-op. Its Change has OriginEngine.
type LocalIssueBodyUpdated struct {
	Repo   string
	Number int
	Body   string
}

func (LocalIssueBodyUpdated) isMutation()       {}
func (m LocalIssueBodyUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLabelAdded is applied after fabrik adds a label to an issue.
type LocalLabelAdded struct {
	Repo   string
	Number int
	Label  string
	// Requester is the sanitised overseer subscriber that requested the write
	// (#2125); "" for an engine-autonomous write.
	Requester string
}

func (LocalLabelAdded) isMutation()       {}
func (m LocalLabelAdded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLabelRemoved is applied after fabrik removes a label from an issue.
type LocalLabelRemoved struct {
	Repo   string
	Number int
	Label  string
	// Requester: see LocalLabelAdded.
	Requester string
}

func (LocalLabelRemoved) isMutation()       {}
func (m LocalLabelRemoved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalCommentAdded is applied after fabrik posts a comment on an issue.
type LocalCommentAdded struct {
	Repo    string
	Number  int
	Comment gh.Comment
}

func (LocalCommentAdded) isMutation()       {}
func (m LocalCommentAdded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLockAcquired is applied after fabrik adds the fabrik:locked:<user> label.
type LocalLockAcquired struct {
	Repo       string
	Number     int
	User       string
	Worker     *WorkerHandle
	AcquiredAt time.Time // caller-provided time; enables idempotent/no-op detection
}

func (LocalLockAcquired) isMutation()       {}
func (m LocalLockAcquired) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LocalLockReleased is applied after fabrik removes the fabrik:locked:<user> label.
type LocalLockReleased struct {
	Repo   string
	Number int
}

func (LocalLockReleased) isMutation()       {}
func (m LocalLockReleased) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// SelfWriteObserved advances an item's probe staleness baseline
// (LastSeenSourceUpdatedAt) to At, a time GitHub itself reported for the write,
// without performing a deep fetch and without touching any other field. Applied
// at every call site where Fabrik performs a self-write (label add/remove,
// comment post, issue body edit, board status move) that is known to bump the
// item's real GitHub updatedAt but whose cache write-through already reflects
// the resulting state (#1090).
//
// At is GitHub's clock, never the daemon's (#1945, ADR 1945): the baseline is
// compared with GitHub's second-granular updatedAt, and a host-clock value would
// make that comparison depend on the skew between the two machines. A zero At
// means the write yielded no GitHub timestamp: the baseline is not advanced (the
// next probe may then cost one deep fetch, never a masked external write).
// Store.applyToItem enforces monotonicity: the baseline only ever advances,
// never moves backward relative to a prior deep-fetch or self-write, so a
// concurrent DeepFetchInvalidated (which zeroes the baseline) can never be
// "un-done" by a stale SelfWriteObserved.
type SelfWriteObserved struct {
	Repo   string
	Number int
	// At is GitHub's timestamp for the write (zero = none; do not advance).
	At time.Time
}

func (SelfWriteObserved) isMutation()       {}
func (m SelfWriteObserved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Periodic reconciliation ----

// BoardReconciled is submitted by the periodic poll loop after a full board fetch.
// The Store computes per-item deltas and applies them as focused mutations.
// itemKey returns "" because this mutation affects all items, not a single one.
type BoardReconciled struct {
	Items []gh.ProjectItem
}

func (BoardReconciled) isMutation()       {}
func (m BoardReconciled) itemKey() string { return "" }

// ItemDeepFetched is applied after a single-item deep fetch from the GitHub API.
type ItemDeepFetched struct {
	Repo       string
	Number     int
	FreshState gh.ProjectItem
}

func (ItemDeepFetched) isMutation()       {}
func (m ItemDeepFetched) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// BlockedByEdgeAdded appends a single dependency edge to an item's BlockedBy
// list, deduplicated by (Repo, Number) — a no-op (returns ChangeFlags(0)) if an
// edge to the same blocker is already present. Applied synchronously by
// spawnChildren (engine/spawn.go) the instant each child is linked via
// AddBlockedByIssue, so the Store's own copy of BlockedBy reflects the new
// edge immediately, without waiting for a subsequent deep-fetch or depending
// on cycleSet wake timing (#1783). Unlike ItemDeepFetched, this touches only
// BlockedBy — no other deep field (Comments, Body, Assignees, etc.) is reset
// or overwritten.
type BlockedByEdgeAdded struct {
	Repo   string // parent's "owner/repo"
	Number int    // parent's issue number
	Dep    gh.Dependency
}

func (BlockedByEdgeAdded) isMutation()       {}
func (m BlockedByEdgeAdded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// BlockedByEdgeRemoved deletes a single dependency edge from an item's
// BlockedBy list — a no-op (returns ChangeFlags(0)) if no edge to that blocker
// is present. Applied synchronously by the fabrik_set_blocked_by overseer
// action (#2385) the instant RemoveBlockedByIssue succeeds. GitHub's removal
// does not bump the issue's updatedAt (#977), so this write-through is the only
// thing that keeps the cached list right until the next live read.
//
// Dep.Repo equality ignores the "empty means the item's own repo" convention
// (see sameDependency): a fetched same-repo edge and a full "owner/repo" one
// name the same blocker.
type BlockedByEdgeRemoved struct {
	Repo   string // dependent's "owner/repo"
	Number int    // dependent's issue number
	Dep    gh.Dependency
}

func (BlockedByEdgeRemoved) isMutation()       {}
func (m BlockedByEdgeRemoved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// sameDependency reports whether two edges of an item in itemRepo name the same
// blocker. gh.Dependency.Repo is empty for a same-repo blocker (as the GraphQL
// fetch reports it), while writers such as spawnChildren and
// fabrik_set_blocked_by record the full "owner/repo"; both must compare equal
// or the list would hold a duplicate. Repository names are case-insensitive on
// GitHub, so the comparison is too: a blocker typed "Owner/Repo#N" must match the
// edge GitHub reports under its canonical casing.
func sameDependency(itemRepo string, a, b gh.Dependency) bool {
	return a.Number == b.Number && strings.EqualFold(depRepo(itemRepo, a), depRepo(itemRepo, b))
}

func depRepo(itemRepo string, d gh.Dependency) string {
	if d.Repo == "" {
		return itemRepo
	}
	return d.Repo
}

// DeepFetchInvalidated clears LastDeepFetchAt so the next FetchItemDetails call
// re-fetches deep fields from GitHub. Used by delta handlers that detect stale
// deep state (e.g., new PR linkage discovered, new review thread comment added).
type DeepFetchInvalidated struct {
	Repo   string
	Number int
}

func (DeepFetchInvalidated) isMutation()       {}
func (m DeepFetchInvalidated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// DepBlockedCooldown is the CooldownAt key the engine records for an item it has
// left labelled fabrik:blocked (engine/item.go). itemMayNeedWork suppresses the
// item until it expires.
const DepBlockedCooldown = "dep-blocked"

// DependenciesInvalidated is applied when a dependency change (an
// issue_dependencies webhook delivery, or a blocker closing) means an item's
// cached BlockedBy can no longer be trusted (#1995, ADR 1995). It does what
// DeepFetchInvalidated does and, in addition, makes the item re-evaluable
// before the next poll: it returns DependenciesInvalidatedChanged (a wake and
// cycle-set flag) and, for an item carrying fabrik:blocked, drops the
// dep-blocked admission cooldown so itemMayNeedWork admits it. It never touches
// labels: whether the item is (un)blocked is still decided by checkDependencies'
// live BlockedBy read (ADR 1419). Idempotent: applying it to an item whose deep
// cache is already invalid and that has no dep-blocked cooldown is a no-op.
type DependenciesInvalidated struct {
	Repo   string
	Number int
}

func (DependenciesInvalidated) isMutation()       {}
func (m DependenciesInvalidated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRHeadSHAUpdated sets LinkedPR.HeadSHA for the given item and triggers the
// Store's shaToKey index to be updated. Used by delta handlers when a push event
// updates the PR's head commit.
// LinkedPRNum, if non-zero, also sets LinkedPR.Number (used in auto-heal paths
// where the PR↔issue linkage is being established for the first time).
type PRHeadSHAUpdated struct {
	// At is the observation time of the head or PR read. AtIsFetchStart marks it
	// as the start of a read (a fetch, a listing), which gets the listing-lag
	// allowance; otherwise it is the fact's own time (a webhook's GitHub time or
	// an engine write). Zero means no ordering information (#2348).
	At             time.Time
	AtIsFetchStart bool
	Repo           string
	Number         int // issue number
	LinkedPRNum    int // PR number to set (0 = leave unchanged)
	SHA            string
	// FillOnly makes the write set the head only while the Store holds none: with
	// a head already recorded it only links the PR. For a producer that learned
	// the SHA from something that says nothing about whether it is still the
	// head, such as a check_run for an old commit (#2348, FR-008).
	FillOnly bool
}

func (PRHeadSHAUpdated) isMutation()       {}
func (m PRHeadSHAUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRDetailsUpdated sets LinkedPR.Title, State, Merged, and Draft for the given item.
// These four fields were previously stored only in CacheImpl.linkedPRs; this mutation
// moves them into the Store so PR-detail changes are observable via LinkedPRChanged.
// PRNumber also ensures the prToKey reverse index is current.
type PRDetailsUpdated struct {
	// At is the observation time of the head or PR read. AtIsFetchStart marks it
	// as the start of a read (a fetch, a listing), which gets the listing-lag
	// allowance; otherwise it is the fact's own time (a webhook's GitHub time or
	// an engine write). Zero means no ordering information (#2348).
	At             time.Time
	AtIsFetchStart bool
	Repo           string
	Number         int // issue number
	PRNumber       int
	Title          string
	State          string
	Merged         bool
	Draft          bool
}

func (PRDetailsUpdated) isMutation()       {}
func (m PRDetailsUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewThreadCommentAdded appends an inline review-thread comment to an item's
// LinkedPR.ThreadComments, idempotent by NodeID. Uses the ISSUE number (not the
// PR number) as the routing key. Used by boardcache delta handlers.
type ReviewThreadCommentAdded struct {
	Repo        string
	IssueNumber int
	Comment     gh.Comment
}

func (ReviewThreadCommentAdded) isMutation()       {}
func (m ReviewThreadCommentAdded) itemKey() string { return itemKeyFor(m.Repo, m.IssueNumber) }

// ShallowBoardItemUpdated updates only the shallow fields of an existing item
// during a Reconcile pass. Unlike ItemDeepFetched or IssueOpened, it does NOT
// overwrite deep fields (Comments, Body, Assignees, BlockedBy, LinkedPRReviews,
// etc.). Used exclusively by CacheImpl.Reconcile.
//
// WARNING: do not use with probe data — the Labels field will be set to empty,
// wiping the cached label set. Use ProbeBoardItemUpdated for probe-loop updates.
type ShallowBoardItemUpdated struct {
	Repo   string
	Number int
	Item   gh.ProjectItem
	// FetchedAt is the instant the listing was requested (#2159). Fields the
	// engine wrote more recently than FetchedAt (plus a grace margin) keep their
	// local value. Zero means unknown: no protection, the listing wins.
	FetchedAt time.Time
}

func (ShallowBoardItemUpdated) isMutation()       {}
func (m ShallowBoardItemUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ProbeBoardItemUpdated updates only the probe-visible fields of an existing item
// from a ProbeProjectBoard result. Unlike ShallowBoardItemUpdated, it does NOT
// touch Labels (the probe query fetches no labels). Updates ID, ItemID, State,
// IsClosed, IsPR, Status, and UpdatedAt only. Status and closed state go through
// the engine-write protection (protectStatusClosed, #2178).
type ProbeBoardItemUpdated struct {
	Repo   string
	Number int
	Item   gh.BoardProbeItem
	// FetchedAt is the instant the probe listing was requested, taken before the
	// query was issued (#2178). Status and closed state the engine wrote more
	// recently than FetchedAt (plus a grace margin) keep their local value. Zero
	// means unknown: no protection, the listing wins.
	FetchedAt time.Time
}

func (ProbeBoardItemUpdated) isMutation()       {}
func (m ProbeBoardItemUpdated) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StatusRefreshed applies a Status the engine read live from GitHub for one item
// (the Layer 1 status refresh, #2347). It is a GitHub read, not an engine write:
// it goes through the same engine-write protection as a listing, is dropped when
// it predates a move the Store has already observed, and never records an
// engine write, marks an engine move or sets an Origin.
type StatusRefreshed struct {
	Repo   string
	Number int
	Status string
	// FetchedAt is the wall-clock instant the read was started, taken before the
	// GitHub call. Zero means unknown: no protection, the read wins (but it still
	// records no engine write).
	FetchedAt time.Time
}

func (StatusRefreshed) isMutation()       {}
func (m StatusRefreshed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ---- Engine internals ----

// StageAttempted records the start of a new Claude invocation for a stage.
type StageAttempted struct {
	Repo      string
	Number    int
	StageName string
	At        time.Time
	// Slice marks the invocation as a time slice (turn-cap or max_wall_time
	// stop): the retry backoff is skipped for the next dispatch (#2232).
	Slice bool
}

func (StageAttempted) isMutation()       {}
func (m StageAttempted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageRetryIncremented increments the attempt counter for a stage.
type StageRetryIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (StageRetryIncremented) isMutation()       {}
func (m StageRetryIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageRetryCleared resets the attempt counter (and the sibling
// SliceRetries/ToolsDeniedRetries counters) for a stage.
type StageRetryCleared struct {
	Repo      string
	Number    int
	StageName string
}

func (StageRetryCleared) isMutation()       {}
func (m StageRetryCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// SliceRetryIncremented increments the turn-cap preemption ("slice") counter for
// a stage. Applied instead of StageRetryIncremented when the invocation ended in
// a turn-cap exit (CLI subtype error_max_turns) — a resumable time-slice, not a
// failure — so it is bounded independently by MaxSliceRetries rather than
// counting against MaxRetries (#1199).
type SliceRetryIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (SliceRetryIncremented) isMutation()       {}
func (m SliceRetryIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ToolsDeniedRetryIncremented increments the tool-permission-denial retry
// counter for a stage. Applied instead of StageRetryIncremented when the
// invocation ended in a tools-denied exit (claudeToolsDeniedError) — an
// environmental permission misconfiguration, not a stage failure — so it is
// bounded independently by MaxToolsDeniedRetries rather than counting against
// MaxRetries (#1523). Mirrors SliceRetryIncremented structurally.
type ToolsDeniedRetryIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ToolsDeniedRetryIncremented) isMutation()       {}
func (m ToolsDeniedRetryIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// NoOpCommentCycleIncremented increments the success-agnostic, never-pruned
// no-progress counter for a stage's comment-processing loop (#1555). Applied
// exactly once per processComments cycle — at whichever of its five mutually
// exclusive exit points was reached — when that cycle produced no observable
// progress (no commit, no issue-body update, no FABRIK_STAGE_COMPLETE),
// regardless of whether the invocation itself exited successfully. See
// StageState.NoOpCommentCycles.
type NoOpCommentCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (NoOpCommentCycleIncremented) isMutation()       {}
func (m NoOpCommentCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// NoOpCommentCycleReset zeroes the no-op comment cycle counter for a stage
// after a cycle makes genuine progress (R2) — a commit lands, the issue body
// updates, or FABRIK_STAGE_COMPLETE is emitted — or after a manual unpause
// (via EngineCyclesCleared, mirroring ReviewCycles/ReviewBlockedCycles).
type NoOpCommentCycleReset struct {
	Repo      string
	Number    int
	StageName string
}

func (NoOpCommentCycleReset) isMutation()       {}
func (m NoOpCommentCycleReset) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewCycleIncremented increments the review cycle counter for a stage.
type ReviewCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ReviewCycleIncremented) isMutation()       {}
func (m ReviewCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewBlockedCycleIncremented increments the never-refunded non-convergence
// counter for a stage's review gate (ADR-1518). Applied alongside
// ReviewCycleIncremented, but only when the reinvoke it accompanies is
// dispatched while the gate is still blocked/timed-out — a reinvoke
// dispatched with the gate already clear (the #1045 junk-overview shape)
// never increments this counter. Unlike ReviewCycleIncremented, this counter
// is not refunded for a no-op on HEAD: a reinvoke that turns out to be a
// no-op on HEAD while the gate was blocking is still genuine evidence the
// loop failed to converge, not an attempt that "didn't count." The one
// exception is a reinvoke that provably never ran at all (#1812, see
// ReviewBlockedCycleDecremented).
type ReviewBlockedCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ReviewBlockedCycleIncremented) isMutation()       {}
func (m ReviewBlockedCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewCycleDecremented compensates a prior ReviewCycleIncremented when a
// review reinvoke lands no new commit (#1045) — a cycle that changed nothing
// was not an attempt, so it must not count against MaxReviewCycles. Applied
// post-hoc from dispatchReviewReinvoke's after hook, which runs synchronously
// inside the reinvoke goroutine before the deferred WorkerExited fires, so
// the decrement is always visible before any poll can next read the counter.
// Floored at 0 by the store — never goes negative even if applied more than
// once for the same increment (defensive; the call site is idempotency-safe
// by construction, this is a second line of defense).
type ReviewCycleDecremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ReviewCycleDecremented) isMutation()       {}
func (m ReviewCycleDecremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewBlockedCycleDecremented compensates a prior ReviewBlockedCycleIncremented
// when the reinvoke it accompanied provably never ran (#1812) — a usage-limit,
// api_error or apiKeyHelper exit. Unlike ReviewCycleDecremented's #1045
// no-op-on-HEAD refund, this is applied ONLY for did-not-run exits, so
// ADR-1518's guarantee that a genuinely no-op blocked cycle is never refunded
// is preserved. Floored at 0 by the store.
type ReviewBlockedCycleDecremented struct {
	Repo      string
	Number    int
	StageName string
}

func (ReviewBlockedCycleDecremented) isMutation()       {}
func (m ReviewBlockedCycleDecremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// DidNotRunReinvokeRecorded increments the never-refunded tally of reinvokes
// (review, CI-fix, rebase) whose Claude invocation provably never ran (#1812).
// It exists purely as evidence for the cycle-limit pause messages: once a
// did-not-run cycle has been refunded, the counters no longer show it, so this
// tally is the only way to tell an operator that a burst of cycles was an
// outage rather than a reviewer that never converged. Cleared by
// EngineCyclesCleared.
type DidNotRunReinvokeRecorded struct {
	Repo      string
	Number    int
	StageName string
}

func (DidNotRunReinvokeRecorded) isMutation()       {}
func (m DidNotRunReinvokeRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// DidNotRunReinvokesReset zeroes the DidNotRunReinvokes tally for a stage (#1812).
// Applied whenever a reinvoke stays charged (it ran, or is ambiguous), so the
// tally only ever describes the streak of never-ran reinvokes since the last
// genuine cycle — never an outage from days earlier. A no-op (no Change) when
// the tally is already zero.
type DidNotRunReinvokesReset struct {
	Repo      string
	Number    int
	StageName string
}

func (DidNotRunReinvokesReset) isMutation()       {}
func (m DidNotRunReinvokesReset) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIFixCycleIncremented increments the CI-fix cycle counter for a stage.
type CIFixCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (CIFixCycleIncremented) isMutation()       {}
func (m CIFixCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIFixCycleDecremented compensates a prior CIFixCycleIncremented when the
// CI-fix reinvoke provably never ran (#1812). Floored at 0 by the store.
type CIFixCycleDecremented struct {
	Repo      string
	Number    int
	StageName string
}

func (CIFixCycleDecremented) isMutation()       {}
func (m CIFixCycleDecremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// RebaseCycleIncremented increments the rebase cycle counter for a stage.
type RebaseCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (RebaseCycleIncremented) isMutation()       {}
func (m RebaseCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// RebaseCycleDecremented compensates a prior RebaseCycleIncremented when the
// rebase reinvoke provably never ran (#1812). Floored at 0 by the store.
type RebaseCycleDecremented struct {
	Repo      string
	Number    int
	StageName string
}

func (RebaseCycleDecremented) isMutation()       {}
func (m RebaseCycleDecremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// EnqueueCycleIncremented increments the merge-queue re-enqueue cycle counter for a
// stage (ADR-058 D4 FR-3). Applied on each fresh enqueue trip in the convergence
// monitor; bounds a queue-thrash loop independently of the rebase/CI-fix caps.
type EnqueueCycleIncremented struct {
	Repo      string
	Number    int
	StageName string
}

func (EnqueueCycleIncremented) isMutation()       {}
func (m EnqueueCycleIncremented) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRCreationFailedRecorded records that Claude completed a stage but the draft PR
// could not be created. In-memory only — does not survive restart. Cleared by
// StageRetryCleared when the PR is eventually created or the stage succeeds.
type PRCreationFailedRecorded struct {
	Repo      string
	Number    int
	StageName string
}

func (PRCreationFailedRecorded) isMutation()       {}
func (m PRCreationFailedRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// EnginePaused records that the engine has paused work on a stage — either
// due to repeated failures (escalateFailedStage/escalatePRCreationFailure) or
// a turn-cap slice budget exceeded (pauseForSliceLimit, #1199), both of which
// need the same "a human manually removed fabrik:paused" detection to reset
// their respective counters via clearFailedStage. (The design doc listed this
// as "EngineEnginePaused" — typo.)
type EnginePaused struct {
	Repo      string
	Number    int
	StageName string
}

func (EnginePaused) isMutation()       {}
func (m EnginePaused) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CooldownRecorded sets a cooldown expiry for a given reason key.
type CooldownRecorded struct {
	Repo   string
	Number int
	Reason string
	Until  time.Time
}

func (CooldownRecorded) isMutation()       {}
func (m CooldownRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ArchiveEligibleCooldown is the CooldownAt key that holds an absolute
// "eligible since" timestamp rather than a gate expiry. ExpiredCooldownsConsumed
// never deletes it: past its instant it means "eligible", not "expired".
const ArchiveEligibleCooldown = "archive-eligible-at"

// ExpiredCooldownsConsumed deletes every CooldownAt entry that is non-zero and
// has expired relative to Now (!Now.Before(entry)), except ArchiveEligibleCooldown
// (#2096, ADR 2096). Applied when poll admission acts on an item's expired
// cooldown, so that expiry admits the item exactly once instead of for ever:
// HasExpiredCooldown then reports only expiries not yet acted on. Reports
// CooldownChanged only when it deleted something.
type ExpiredCooldownsConsumed struct {
	Repo   string
	Number int
	Now    time.Time
}

func (ExpiredCooldownsConsumed) isMutation()       {}
func (m ExpiredCooldownsConsumed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LabelAppliedAtRecorded records the time the engine itself applied Label to
// this issue (record-at-write, #1314). Always overwrites any prior entry for
// the same label — a genuine re-application (applied → removed → re-applied)
// must yield the latest timestamp, not the first. Callers are expected to
// only record here on a genuine new application (guarded by a "not already
// present" check at the write site), never as a defensive idempotent no-op.
type LabelAppliedAtRecorded struct {
	Repo   string
	Number int
	Label  string
	At     time.Time
}

func (LabelAppliedAtRecorded) isMutation()       {}
func (m LabelAppliedAtRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerHeartbeat updates the LastSignAt timestamp for the active worker.
type WorkerHeartbeat struct {
	Repo   string
	Number int
	At     time.Time
}

func (WorkerHeartbeat) isMutation()       {}
func (m WorkerHeartbeat) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerPIDSet records the Claude subprocess PID once cmd.Start() returns.
// No-op if Worker is nil (e.g., WorkerExited arrived first).
type WorkerPIDSet struct {
	Repo   string
	Number int
	PID    int
}

func (WorkerPIDSet) isMutation()       {}
func (m WorkerPIDSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerEntered sets a non-nil Worker placeholder immediately before the goroutine is
// launched. This makes snap.Worker() != nil an effective dispatch guard from the instant
// the goroutine is started, closing the window between goroutine launch and
// LocalLockAcquired (which fires only after the GitHub lock label is acquired).
// LocalLockAcquired overwrites the placeholder with full details.
//
// Queued (#2121) marks a worker that has been dispatched but holds no worker slot
// yet; QueuedSince is when it began waiting. WorkerSlotAcquired clears it. The
// handle is non-nil either way: it is the claim on the item.
type WorkerEntered struct {
	Repo        string
	Number      int
	StageName   string
	StartedAt   time.Time
	Kind        string
	Trigger     string
	Queued      bool
	QueuedSince time.Time
}

func (WorkerEntered) isMutation()       {}
func (m WorkerEntered) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerSlotAcquired records that a queued worker now holds a worker slot (#2121):
// Queued is cleared and StartedAt/LastSignAt are re-stamped to At, so the PID<=0
// stale-worker detector and the reported start time measure from the slot, not
// from dispatch. It claims no lock (Lock mirrors the fabrik:locked:<id> label,
// which only stage dispatch applies, #2179, ADR 2179). It carries WorkerChanged
// only — never WorkerLifecycleChanged, so it cannot wake the poll. A no-op when
// the worker has already exited.
type WorkerSlotAcquired struct {
	Repo   string
	Number int
	At     time.Time
	// Reserved is true when the slot taken is the reserved fix-round slot
	// (#2490), not a general-pool slot.
	Reserved bool
}

func (WorkerSlotAcquired) isMutation()       {}
func (m WorkerSlotAcquired) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerKindSet sets the live worker's kind (and trigger) (#2121). It only
// replaces an empty, stage-run or comment-review kind, so a comment review
// started inside a reinvoke worker never overrides the reinvoke kind. A no-op
// when there is no worker.
type WorkerKindSet struct {
	Repo    string
	Number  int
	Kind    string
	Trigger string
}

func (WorkerKindSet) isMutation()       {}
func (m WorkerKindSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerTriggerSet records what the live worker is addressing (#2121). A no-op
// when there is no worker.
type WorkerTriggerSet struct {
	Repo    string
	Number  int
	Trigger string
}

func (WorkerTriggerSet) isMutation()       {}
func (m WorkerTriggerSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerTurnsRecorded records the live worker's turn count so far and the turn
// budget of the invocation it belongs to (#2121). Turns == 0 marks an invocation
// start: the previous invocation's count is forgotten (turns unknown again) and
// Max is the new budget. A no-op when there is no worker, or nothing changed.
type WorkerTurnsRecorded struct {
	Repo   string
	Number int
	Turns  int
	Max    int
}

func (WorkerTurnsRecorded) isMutation()       {}
func (m WorkerTurnsRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// RetryScheduled records a retry the engine has scheduled (#2121). It has no
// explicit clearing mutation: the retry ends with the next stage-run WorkerEntered
// (dispatch), and with a pause, a stage:*:complete label, a close or a column move
// (clearStatusFacts). Every path that gives a retry up escalates to a pause.
type RetryScheduled struct {
	Repo   string
	Number int
	Retry  PendingRetry
}

func (RetryScheduled) isMutation()       {}
func (m RetryScheduled) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// GateHoldRecorded records a gate hold (#2121). A repeat of the same gate keeps
// the original Since and refreshes the rest.
type GateHoldRecorded struct {
	Repo   string
	Number int
	Hold   GateHold
}

func (GateHoldRecorded) isMutation()       {}
func (m GateHoldRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// GateHoldCleared clears the gate hold (#2121). When Gate is non-empty only a
// hold of that gate is cleared.
type GateHoldCleared struct {
	Repo   string
	Number int
	Gate   string
}

func (GateHoldCleared) isMutation()       {}
func (m GateHoldCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ConflictResolutionSet marks (or, with Active false, clears) a merge-train
// conflict resolution for a member (#2121).
type ConflictResolutionSet struct {
	Repo   string
	Number int
	Active bool
	Queued bool
	Since  time.Time
	// GeneralSlot: the resolution holds a general-pool slot (not the reserved
	// merge-train slot).
	GeneralSlot bool
}

func (ConflictResolutionSet) isMutation()       {}
func (m ConflictResolutionSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// RebaseCauseSet records why fabrik:rebase-needed was applied; an empty Cause
// clears it (#2121).
type RebaseCauseSet struct {
	Repo   string
	Number int
	Cause  string
}

func (RebaseCauseSet) isMutation()       {}
func (m RebaseCauseSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRFactsObserved records the linked PR's overseer-facing facts (#2122). It is
// quiet: it returns PRFactsChanged, which is not a wake flag, and it never
// calls SelfWriteObserved or moves LastSeenPRUpdatedAt/LinkedPRUpdatedAt. A
// merged or closed PR clears the CI verdict (no open-PR verdict applies).
type PRFactsObserved struct {
	Repo   string
	Number int // issue number
	// PRNumber, if non-zero, is the PR the facts describe; the facts are dropped
	// when it differs from the item's linked PR number.
	PRNumber int
	Facts    PRFacts
}

func (PRFactsObserved) isMutation()       {}
func (m PRFactsObserved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIVerdictObserved records the CI verdict computed for a PR head (#2122). It
// is dropped when HeadSHA is not the item's current linked-PR head, and never
// touches LastCIProgressAt (ADR-2059).
type CIVerdictObserved struct {
	Repo    string
	Number  int // issue number
	Verdict CIVerdictRecord
}

func (CIVerdictObserved) isMutation()       {}
func (m CIVerdictObserved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// WorkerExited clears the Worker handle when a Claude invocation finishes.
type WorkerExited struct {
	Repo   string
	Number int
}

func (WorkerExited) isMutation()       {}
func (m WorkerExited) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// InvocationRecorded captures the outcome of a completed Claude invocation for TUI display.
type InvocationRecorded struct {
	Repo      string
	Number    int
	Completed bool
	Blocked   bool
	// Errored is true when the Claude process exited non-zero AND the exit was not
	// classified as a turn-limit hit (see TurnLimited). Recorded independently of
	// Completed — the completion marker is authoritative for whether the stage
	// completed; Errored only records that the process didn't exit cleanly for a
	// genuine fault.
	Errored bool
	// TurnLimited is true when the Claude invocation exited because it exhausted
	// its configured turn budget (CLI subtype error_max_turns), as opposed to a
	// genuine failure. A turn-limited run is incomplete but resumable, not an
	// error — see claudeTurnLimitError in engine/claude.go and ADR-1178.
	TurnLimited bool
	Usage       TokenUsage
	// RequestedModel is the effective requested model (label override > stage
	// config, as written); empty when no --model flag was passed. See ADR-1411.
	RequestedModel string
	// IsComment is true when the invocation processed a user comment rather than
	// running a stage. Stored in ItemState.LastInvocationIsComment so that the
	// InvocationObserver can forward the correct flag to the TUI.
	IsComment bool
	// Duration is the wall-clock time from invocation start to completion.
	// Zero when not set (e.g., comment-processing paths that don't track start time).
	Duration time.Duration
}

func (InvocationRecorded) isMutation()       {}
func (m InvocationRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// DeepFetchFailed records that a deep-fetch attempt for this item failed.
type DeepFetchFailed struct {
	Repo   string
	Number int
	At     time.Time
}

func (DeepFetchFailed) isMutation()       {}
func (m DeepFetchFailed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// BaseBranchWarnRecorded marks a base-branch override as having been warned about.
type BaseBranchWarnRecorded struct {
	Repo   string
	Number int
	Branch string
}

func (BaseBranchWarnRecorded) isMutation()       {}
func (m BaseBranchWarnRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// TerminalFlagSet sets or clears the Terminal flag on an item. When Terminal is
// true, the poll loop skips deep-fetch for this item as long as its status
// remains in a cleanup (Done) stage. When Terminal is false, normal deep-fetch
// evaluation resumes.
type TerminalFlagSet struct {
	Repo     string
	Number   int
	Terminal bool
}

func (TerminalFlagSet) isMutation()       {}
func (m TerminalFlagSet) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIMergePendingStarted records that CI-gate merge polling has begun for the item's
// linked PR. Sets LinkedPRState.CIMergePendingSince to At.
// Replaces engine.ciMergePendingSince[iKey] = time.Now().
type CIMergePendingStarted struct {
	Repo   string
	Number int
	At     time.Time
}

func (CIMergePendingStarted) isMutation()       {}
func (m CIMergePendingStarted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIMergePendingCleared records that CI-gate merge polling has ended for the item's
// linked PR. Zeros LinkedPRState.CIMergePendingSince.
// Replaces delete(engine.ciMergePendingSince, iKey).
type CIMergePendingCleared struct {
	Repo   string
	Number int
}

func (CIMergePendingCleared) isMutation()       {}
func (m CIMergePendingCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ValidateCompletedAtSHA records the HEAD SHA of the linked PR at the moment
// stage:Validate:complete was applied. The SHA-invalidation scan uses this to
// detect force-pushes or external commits after Validate finished.
type ValidateCompletedAtSHA struct {
	Repo   string
	Number int
	SHA    string
}

func (ValidateCompletedAtSHA) isMutation()       {}
func (m ValidateCompletedAtSHA) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ValidateCompletedAtSHACleared zeroes LinkedPRState.ValidateCompletedSHA after
// the SHA-invalidation scan fires and removes the stale completion labels.
type ValidateCompletedAtSHACleared struct {
	Repo   string
	Number int
}

func (ValidateCompletedAtSHACleared) isMutation()       {}
func (m ValidateCompletedAtSHACleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PRChecksObserved records that the linked PR has had at least one CI check run
// returned by FetchCheckRuns (REST path). Sets LinkedPRState.HasHadChecks = true.
// Replaces engine.prHasHadChecks[iKey] = true.
type PRChecksObserved struct {
	Repo   string
	Number int
}

func (PRChecksObserved) isMutation()       {}
func (m PRChecksObserved) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// PREnqueueRecorded sets LinkedPRState.LastEnqueuedSHA to the head SHA at which the
// engine last enqueued (or re-enqueued) the PR into GitHub's merge queue (ADR-058
// D4 FR-3). Lets the convergence monitor distinguish a genuine post-resolution
// re-enqueue (SHA changed) from the post-enqueue consistency window (SHA unchanged).
type PREnqueueRecorded struct {
	Repo   string
	Number int
	SHA    string
}

func (PREnqueueRecorded) isMutation()       {}
func (m PREnqueueRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CIFixNoOpRecorded sets LinkedPRState.LastCIFixNoOpSHA to the head SHA at which a
// CI-fix reinvoke (engine.dispatchCIFixReinvoke) completed without pushing a new
// commit (#958 leg 2). While the head SHA stays at this value, handleMergeAndCIGates
// skips further CI-fix dispatch/cycle-increment for it — settleAwaitingCIScan's
// CIBackstopTimeout remains the backstop if CI never resolves on this SHA
// (ADR-1410 — a confirmed CI failure never consults CIWaitTimeout).
type CIFixNoOpRecorded struct {
	Repo   string
	Number int
	SHA    string
}

func (CIFixNoOpRecorded) isMutation()       {}
func (m CIFixNoOpRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CommentProcessed records that a comment has been processed by the engine.
// Prevents reprocessing the same comment on subsequent polls or restarts
// (backed by rocket reactions on GitHub for cross-restart durability).
type CommentProcessed struct {
	Repo      string
	Number    int
	CommentID string
	At        time.Time
}

func (CommentProcessed) isMutation()       {}
func (m CommentProcessed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageLastAttemptCleared zeroes LastAttemptAt for a stage so it re-runs promptly.
// Used by unblockAwaitingInput to reset the dispatch cooldown after user input.
type StageLastAttemptCleared struct {
	Repo      string
	Number    int
	StageName string
}

func (StageLastAttemptCleared) isMutation()       {}
func (m StageLastAttemptCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// EngineUnpaused clears PausedByEngine for a stage, used when a user comment
// triggers an unpause or when clearFailedStage resets engine-managed pause state.
type EngineUnpaused struct {
	Repo      string
	Number    int
	StageName string
}

func (EngineUnpaused) isMutation()       {}
func (m EngineUnpaused) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// EngineCyclesCleared zeroes ReviewCycles, ReviewBlockedCycles, CIFixCycles,
// RebaseCycles, EnqueueCycles, NoOpCommentCycles, and DidNotRunReinvokes for a stage. Called by
// clearFailedStage on unpause/success to prevent stale counters from
// triggering premature max-cycle pauses on the next run.
type EngineCyclesCleared struct {
	Repo      string
	Number    int
	StageName string
}

func (EngineCyclesCleared) isMutation()       {}
func (m EngineCyclesCleared) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// LinkageHealAttempted records that the engine attempted to auto-heal missing
// PR↔issue linkage for the given stage. Keyed by stage name → PR head SHA.
// In-memory only — does not survive restart. A force-push (new SHA) clears the
// guard naturally by virtue of the SHA not matching the stored value.
type LinkageHealAttempted struct {
	Repo      string
	Number    int
	StageName string
	PRSHA     string
}

func (LinkageHealAttempted) isMutation()       {}
func (m LinkageHealAttempted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StageTurnUsageRecorded records the TurnsUsed/capped/clean status of the
// invocation that just finalized as incomplete for a stage (#1146). Applied once
// per incomplete invocation, always overwriting the prior value. Clean records
// whether this attempt stopped cleanly (err == nil or a turn-cap exit) rather than
// erroring out partway through; a non-clean attempt is recorded with Clean=false
// regardless of its TurnsUsed, which invalidates the arming precondition for the
// next comparison (see StageState.LastTurnsClean). Prior to #1767, arming rode on
// Capped instead: the very attempt that triggered detection was itself uncapped,
// which cleared the precondition for a subsequent re-arm as a side effect. Arming
// now instead requires Clean, which the arming attempt also satisfies — so that
// implicit self-limiting no longer applies; see StallHintArmed/StallEpisodeArmed
// for the explicit guard that replaces it.
type StageTurnUsageRecorded struct {
	Repo      string
	Number    int
	StageName string
	TurnsUsed int
	Capped    bool
	Clean     bool
}

func (StageTurnUsageRecorded) isMutation()       {}
func (m StageTurnUsageRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StallHintArmed records that a stall was detected for a stage (a clean incomplete
// predecessor — capped or not — followed by an incomplete, uncapped attempt using
// strictly fewer turns, #1146/#1767) and the next invocation of that stage should
// receive a corrective hint. Also sets StageState.StallEpisodeArmed, the explicit
// R4 guard that keeps arming to at most once per retry episode now that the old
// prevCapped-driven self-limiting no longer applies (see StageTurnUsageRecorded).
type StallHintArmed struct {
	Repo      string
	Number    int
	StageName string
}

func (StallHintArmed) isMutation()       {}
func (m StallHintArmed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// StallHintConsumed clears a pending stall hint once it has been injected into an
// invocation's prompt (#1146), so the hint fires exactly once per detected episode.
type StallHintConsumed struct {
	Repo      string
	Number    int
	StageName string
}

func (StallHintConsumed) isMutation()       {}
func (m StallHintConsumed) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CommentBreakerInvocationRecorded appends a timestamp (and records the
// triggering comment's author) to ItemState.CommentBreaker for the
// runaway-loop circuit breaker (#1089). Applied once per comment-processing
// invocation that actually reaches Claude. Cutoff, when non-zero, additionally
// prunes any InvocationsAt entries older than Cutoff before appending — the
// caller (engine) computes it from its own window setting so an issue that
// receives invocations sparser than the window doesn't grow InvocationsAt
// without bound; a zero Cutoff performs no pruning (mirrors zero-means-default
// elsewhere, but here zero means "caller opted out of pruning").
type CommentBreakerInvocationRecorded struct {
	Repo   string
	Number int
	At     time.Time
	Author string
	Cutoff time.Time
}

func (CommentBreakerInvocationRecorded) isMutation()       {}
func (m CommentBreakerInvocationRecorded) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// CommentBreakerReset clears ItemState.CommentBreaker.InvocationsAt (and
// LastAuthor) after forward progress is observed: a stage:*:complete
// transition, a new commit on the branch, a PR state change, an issue-body
// update, or a manual human unpause (#1089).
type CommentBreakerReset struct {
	Repo   string
	Number int
}

func (CommentBreakerReset) isMutation()       {}
func (m CommentBreakerReset) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

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
