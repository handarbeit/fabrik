package simgh

import (
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// This file is the instrumentation layer: a decorator around *Sim that adds
// fault injection (fault.go) and the mutation log (mutationlog.go) without
// changing a line of the model.
//
// # Why a decorator rather than inline instrumentation
//
// Threading a log write and a fault check through ~60 existing method bodies
// would perturb every one of them, and nonvacuity.sh's mutations are
// line-pattern-sensitive perl edits that the churn would break wholesale. The
// wrapper isolates the new code entirely, and the compile-time interface
// assertion below makes it grow in lockstep with engine.GitHubClient.
//
// # Why sim is a named field and never an embedded one
//
// This is the load-bearing detail of the whole design, and it is a one-word
// change away from being destroyed.
//
// An *embedded* *Sim would promote all sixty methods, so
// `var _ engine.GitHubClient = (*Instrumented)(nil)` would be satisfied *by
// promotion* — a wrapper method you forgot to write, or one added to the
// interface later, would compile cleanly and silently bypass both the fault
// table and the log. The wrapper would appear to work and would quietly not
// instrument whatever was missed, which is worse than no wrapper at all: every
// scenario built on it would pass for the wrong reason.
//
// With a named field, the same omission is a compile error. That is the entire
// reason to prefer a decorator here. Do not embed this field, however much
// shorter it would make the file.
//
// # Locking
//
// Both new mutexes are acquired and released *before* control enters Sim, and
// neither is ever held across a Sim call. They are therefore leaves in the
// package's lock ordering and add no edge to the mu-before-gitMu invariant
// documented in sim.go.

// Instrumented wraps a Sim with fault injection and a mutation log. It
// satisfies engine.GitHubClient, so the engine can be pointed at it in place
// of the bare Sim.
var _ engine.GitHubClient = (*Instrumented)(nil)

// Instrumented is the controllable GitHub stand-in: a Sim plus the two
// cross-cutting test instruments.
type Instrumented struct {
	// sim is deliberately a NAMED field, never embedded. See this file's doc
	// comment — embedding it would satisfy the interface assertion above by
	// method promotion and make a forgotten wrapper compile silently.
	sim *Sim

	faults *FaultTable
	log    *MutationLog
}

// Instrument wraps a Sim. The Sim remains usable directly — Seed* calls go
// through Sim() — but only calls made through the wrapper are faulted and
// logged.
func Instrument(s *Sim) *Instrumented {
	return &Instrumented{
		sim:    s,
		faults: NewFaultTable(),
		log:    NewMutationLog(),
	}
}

// Sim returns the wrapped model, for the Seed* API and for Snapshot/Restore.
func (in *Instrumented) Sim() *Sim { return in.sim }

// Faults returns the fault table.
func (in *Instrumented) Faults() *FaultTable { return in.faults }

// Log returns the mutation log.
func (in *Instrumented) Log() *MutationLog { return in.log }

// --- interception helpers --------------------------------------------------
//
// Every wrapper below is one call to one of these. Keeping them mechanically
// uniform is deliberate: a wrong method-name string is caught by
// TestEveryInterfaceMethodIsInstrumented, but a wrong Args field is caught
// only by review, and review of sixty near-identical lines only works if they
// really are near-identical.

// do0 intercepts a method returning only an error.
func do0(in *Instrumented, method string, mutation bool, args Args, fn func() error) error {
	seq := in.log.reserve(method, mutation, args, in.sim.now())
	if err := in.faults.next(method, args); err != nil {
		in.log.complete(seq, err, true)
		return err
	}
	err := fn()
	in.log.complete(seq, err, false)
	return err
}

// do1 intercepts a method returning one value and an error.
func do1[T any](in *Instrumented, method string, mutation bool, args Args, fn func() (T, error)) (T, error) {
	seq := in.log.reserve(method, mutation, args, in.sim.now())
	if err := in.faults.next(method, args); err != nil {
		var zero T
		in.log.complete(seq, err, true)
		return zero, err
	}
	v, err := fn()
	in.log.complete(seq, err, false)
	return v, err
}

// do2 intercepts a method returning two values and an error.
func do2[A, B any](in *Instrumented, method string, mutation bool, args Args, fn func() (A, B, error)) (A, B, error) {
	seq := in.log.reserve(method, mutation, args, in.sim.now())
	if err := in.faults.next(method, args); err != nil {
		var zeroA A
		var zeroB B
		in.log.complete(seq, err, true)
		return zeroA, zeroB, err
	}
	a, b, err := fn()
	in.log.complete(seq, err, false)
	return a, b, err
}

// There is deliberately no do3: no method on engine.GitHubClient returns three
// values plus an error. An unused helper could not be exercised, so the
// non-vacuity sweep could not prove it does anything.

// --- board -----------------------------------------------------------------

func (in *Instrumented) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return do1(in, "FetchProjectBoard", false, Args{Owner: owner, Repo: repo, Values: []string{ownerType}},
		func() (*gh.ProjectBoard, error) { return in.sim.FetchProjectBoard(owner, repo, projectNum, ownerType) })
}

func (in *Instrumented) ProbeProjectBoard(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
	return do2(in, "ProbeProjectBoard", false, Args{Owner: owner, Repo: repo, Values: []string{ownerType}},
		func() ([]gh.BoardProbeItem, string, error) {
			return in.sim.ProbeProjectBoard(owner, repo, projectNum, ownerType)
		})
}

func (in *Instrumented) FetchItemDetails(item *gh.ProjectItem) error {
	args := Args{}
	if item != nil {
		args = Args{Repo: item.Repo, Number: item.Number, ID: item.ItemID}
	}
	return do0(in, "FetchItemDetails", false, args, func() error { return in.sim.FetchItemDetails(item) })
}

func (in *Instrumented) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return do1(in, "FetchStatusField", false, Args{ID: projectID},
		func() (*gh.StatusField, error) { return in.sim.FetchStatusField(projectID) })
}

func (in *Instrumented) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	return do0(in, "UpdateProjectItemStatus", true, Args{ID: projectID, Values: []string{itemID, statusFieldID, statusOptionID}},
		func() error { return in.sim.UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID) })
}

func (in *Instrumented) ArchiveProjectItem(projectID, itemID string) error {
	return do0(in, "ArchiveProjectItem", true, Args{ID: projectID, Values: []string{itemID}},
		func() error { return in.sim.ArchiveProjectItem(projectID, itemID) })
}

func (in *Instrumented) FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error) {
	return do1(in, "FetchProjectItem", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() (*gh.ProjectItem, error) { return in.sim.FetchProjectItem(owner, repo, issueNumber) })
}

func (in *Instrumented) FetchProjectItemStatus(itemID string) (string, error) {
	return do1(in, "FetchProjectItemStatus", false, Args{ID: itemID},
		func() (string, error) { return in.sim.FetchProjectItemStatus(itemID) })
}

func (in *Instrumented) FetchProjectItemStatusBatch(projectID string) (map[string]string, error) {
	return do1(in, "FetchProjectItemStatusBatch", false, Args{ID: projectID},
		func() (map[string]string, error) { return in.sim.FetchProjectItemStatusBatch(projectID) })
}

func (in *Instrumented) FetchProjectUpdatedAt(projectID string) (time.Time, error) {
	return do1(in, "FetchProjectUpdatedAt", false, Args{ID: projectID},
		func() (time.Time, error) { return in.sim.FetchProjectUpdatedAt(projectID) })
}

func (in *Instrumented) LookupIssueProjectItem(projectID, repo string, issueNumber int) (string, string, error) {
	return do2(in, "LookupIssueProjectItem", false, Args{Repo: repo, Number: issueNumber, ID: projectID},
		func() (string, string, error) { return in.sim.LookupIssueProjectItem(projectID, repo, issueNumber) })
}

func (in *Instrumented) AddProjectV2ItemById(projectID, contentNodeID string) (string, error) {
	return do1(in, "AddProjectV2ItemById", true, Args{ID: projectID, Values: []string{contentNodeID}},
		func() (string, error) { return in.sim.AddProjectV2ItemById(projectID, contentNodeID) })
}

// --- labels ----------------------------------------------------------------

func (in *Instrumented) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	return do1(in, "FetchLabels", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() ([]string, error) { return in.sim.FetchLabels(owner, repo, issueNumber) })
}

func (in *Instrumented) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	return do0(in, "AddLabelToIssue", true, Args{Owner: owner, Repo: repo, Number: issueNumber, Label: labelName},
		func() error { return in.sim.AddLabelToIssue(owner, repo, issueNumber, labelName) })
}

func (in *Instrumented) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	return do0(in, "RemoveLabelFromIssue", true, Args{Owner: owner, Repo: repo, Number: issueNumber, Label: labelName},
		func() error { return in.sim.RemoveLabelFromIssue(owner, repo, issueNumber, labelName) })
}

func (in *Instrumented) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	return do1(in, "FetchLabelAppliedAt", false, Args{Owner: owner, Repo: repo, Number: issueNumber, Label: labelName},
		func() (time.Time, error) { return in.sim.FetchLabelAppliedAt(owner, repo, issueNumber, labelName) })
}

func (in *Instrumented) SeedLabels(owner, repo string, stageNames []string, lockedUser string) error {
	return do0(in, "SeedLabels", true, Args{Owner: owner, Repo: repo, Values: append(cloneStrings(stageNames), lockedUser)},
		func() error { return in.sim.SeedLabels(owner, repo, stageNames, lockedUser) })
}

// --- comments --------------------------------------------------------------

func (in *Instrumented) AddComment(owner, repo string, issueNumber int, body string) (int, error) {
	return do1(in, "AddComment", true, Args{Owner: owner, Repo: repo, Number: issueNumber, Body: body},
		func() (int, error) { return in.sim.AddComment(owner, repo, issueNumber, body) })
}

func (in *Instrumented) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return do0(in, "AddCommentReaction", true, Args{Owner: owner, Repo: repo, Number: commentDatabaseID, Values: []string{content}},
		func() error { return in.sim.AddCommentReaction(owner, repo, commentDatabaseID, content) })
}

func (in *Instrumented) AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return do0(in, "AddPRReviewCommentReaction", true, Args{Owner: owner, Repo: repo, Number: commentDatabaseID, Values: []string{content}},
		func() error { return in.sim.AddPRReviewCommentReaction(owner, repo, commentDatabaseID, content) })
}

func (in *Instrumented) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	return do0(in, "UpdateComment", true, Args{Owner: owner, Repo: repo, Number: commentDatabaseID, Body: body},
		func() error { return in.sim.UpdateComment(owner, repo, commentDatabaseID, body) })
}

func (in *Instrumented) ResolveReviewThread(threadID string) error {
	return do0(in, "ResolveReviewThread", true, Args{ID: threadID},
		func() error { return in.sim.ResolveReviewThread(threadID) })
}

// --- issues ----------------------------------------------------------------

func (in *Instrumented) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	return do1(in, "GetIssueBody", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() (string, error) { return in.sim.GetIssueBody(owner, repo, issueNumber) })
}

func (in *Instrumented) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	return do0(in, "UpdateIssueBody", true, Args{Owner: owner, Repo: repo, Number: issueNumber, Body: body},
		func() error { return in.sim.UpdateIssueBody(owner, repo, issueNumber, body) })
}

func (in *Instrumented) CloseIssue(owner, repo string, issueNumber int) error {
	return do0(in, "CloseIssue", true, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() error { return in.sim.CloseIssue(owner, repo, issueNumber) })
}

func (in *Instrumented) CreateIssue(owner, repo, title, body string, assignees []string) (int, string, error) {
	return do2(in, "CreateIssue", true, Args{Owner: owner, Repo: repo, Body: body, Values: append([]string{title}, assignees...)},
		func() (int, string, error) { return in.sim.CreateIssue(owner, repo, title, body, assignees) })
}

func (in *Instrumented) FetchIssue(owner, repo string, issueNumber int) (*gh.IssueData, error) {
	return do1(in, "FetchIssue", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() (*gh.IssueData, error) { return in.sim.FetchIssue(owner, repo, issueNumber) })
}

func (in *Instrumented) AddBlockedByIssue(issueNodeID, blockerNodeID string) error {
	return do0(in, "AddBlockedByIssue", true, Args{ID: issueNodeID, Values: []string{blockerNodeID}},
		func() error { return in.sim.AddBlockedByIssue(issueNodeID, blockerNodeID) })
}

// --- pull requests ---------------------------------------------------------

func (in *Instrumented) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	return do1(in, "FindPRForIssue", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() (int, error) { return in.sim.FindPRForIssue(owner, repo, issueNumber) })
}

func (in *Instrumented) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	return do1(in, "FetchLinkedPR", false, Args{Owner: owner, Repo: repo, Number: issueNumber},
		func() (*gh.PRDetails, error) { return in.sim.FetchLinkedPR(owner, repo, issueNumber) })
}

func (in *Instrumented) FetchPRMergeableFields(owner, repo string, prNumber int) (*bool, string, error) {
	return do2(in, "FetchPRMergeableFields", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (*bool, string, error) { return in.sim.FetchPRMergeableFields(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return do1(in, "FetchPRMergeable", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (*bool, error) { return in.sim.FetchPRMergeable(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRMerged(owner, repo string, prNumber int) (bool, error) {
	return do1(in, "FetchPRMerged", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (bool, error) { return in.sim.FetchPRMerged(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	return do1(in, "FetchPRMergeableState", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (string, error) { return in.sim.FetchPRMergeableState(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRDetails(owner, repo string, prNumber int) (*gh.PRDetails, error) {
	return do1(in, "FetchPRDetails", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (*gh.PRDetails, error) { return in.sim.FetchPRDetails(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	return do1(in, "FetchPRClosingIssues", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() ([]int, error) { return in.sim.FetchPRClosingIssues(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	return do1(in, "FetchPRsForSHA", false, Args{Owner: owner, Repo: repo, SHA: sha},
		func() ([]int, error) { return in.sim.FetchPRsForSHA(owner, repo, sha) })
}

func (in *Instrumented) GetPRBase(owner, repo string, prNumber int) (string, error) {
	return do1(in, "GetPRBase", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (string, error) { return in.sim.GetPRBase(owner, repo, prNumber) })
}

func (in *Instrumented) UpdatePRBase(owner, repo string, prNumber int, newBase string) error {
	return do0(in, "UpdatePRBase", true, Args{Owner: owner, Repo: repo, Number: prNumber, Values: []string{newBase}},
		func() error { return in.sim.UpdatePRBase(owner, repo, prNumber, newBase) })
}

func (in *Instrumented) CreateDraftPR(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
	return do1(in, "CreateDraftPR", true, Args{Owner: owner, Repo: repo, Number: issueNumber, Body: body, Values: []string{title, head, base}},
		func() (int, error) { return in.sim.CreateDraftPR(owner, repo, title, head, base, body, issueNumber) })
}

func (in *Instrumented) CreatePR(owner, repo, title, head, base, body string) (int, error) {
	return do1(in, "CreatePR", true, Args{Owner: owner, Repo: repo, Body: body, Values: []string{title, head, base}},
		func() (int, error) { return in.sim.CreatePR(owner, repo, title, head, base, body) })
}

func (in *Instrumented) ListPRs(owner, repo string) ([]gh.PRDetails, error) {
	return do1(in, "ListPRs", false, Args{Owner: owner, Repo: repo},
		func() ([]gh.PRDetails, error) { return in.sim.ListPRs(owner, repo) })
}

func (in *Instrumented) MarkPRReady(owner, repo string, prNumber int) error {
	return do0(in, "MarkPRReady", true, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() error { return in.sim.MarkPRReady(owner, repo, prNumber) })
}

func (in *Instrumented) MergePR(owner, repo string, prNumber int) error {
	return do0(in, "MergePR", true, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() error { return in.sim.MergePR(owner, repo, prNumber) })
}

func (in *Instrumented) EnablePullRequestAutoMerge(owner, repo string, prNumber int, strategy string) error {
	return do0(in, "EnablePullRequestAutoMerge", true, Args{Owner: owner, Repo: repo, Number: prNumber, Values: []string{strategy}},
		func() error { return in.sim.EnablePullRequestAutoMerge(owner, repo, prNumber, strategy) })
}

func (in *Instrumented) DisablePullRequestAutoMerge(owner, repo string, prNumber int) error {
	return do0(in, "DisablePullRequestAutoMerge", true, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() error { return in.sim.DisablePullRequestAutoMerge(owner, repo, prNumber) })
}

func (in *Instrumented) EnqueuePullRequest(owner, repo string, prNumber int, expectedHeadOID string) error {
	return do0(in, "EnqueuePullRequest", true, Args{Owner: owner, Repo: repo, Number: prNumber, SHA: expectedHeadOID},
		func() error { return in.sim.EnqueuePullRequest(owner, repo, prNumber, expectedHeadOID) })
}

func (in *Instrumented) DequeuePullRequest(owner, repo string, prNumber int) error {
	return do0(in, "DequeuePullRequest", true, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() error { return in.sim.DequeuePullRequest(owner, repo, prNumber) })
}

// --- reviews ---------------------------------------------------------------

func (in *Instrumented) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	return do1(in, "FetchPRReviews", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() ([]gh.PRReview, error) { return in.sim.FetchPRReviews(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRReviewRequests(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
	return do1(in, "FetchPRReviewRequests", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() ([]gh.ReviewRequest, error) { return in.sim.FetchPRReviewRequests(owner, repo, prNumber) })
}

func (in *Instrumented) FetchPRReviewDecision(owner, repo string, prNumber int) (string, error) {
	return do1(in, "FetchPRReviewDecision", false, Args{Owner: owner, Repo: repo, Number: prNumber},
		func() (string, error) { return in.sim.FetchPRReviewDecision(owner, repo, prNumber) })
}

func (in *Instrumented) AddReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	return do0(in, "AddReviewRequest", true, Args{Owner: owner, Repo: repo, Number: prNumber, Values: cloneStrings(reviewers)},
		func() error { return in.sim.AddReviewRequest(owner, repo, prNumber, reviewers) })
}

func (in *Instrumented) DeleteReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	return do0(in, "DeleteReviewRequest", true, Args{Owner: owner, Repo: repo, Number: prNumber, Values: cloneStrings(reviewers)},
		func() error { return in.sim.DeleteReviewRequest(owner, repo, prNumber, reviewers) })
}

// --- CI --------------------------------------------------------------------

func (in *Instrumented) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	return do1(in, "FetchCheckRuns", false, Args{Owner: owner, Repo: repo, SHA: sha},
		func() ([]gh.CheckRun, error) { return in.sim.FetchCheckRuns(owner, repo, sha) })
}

func (in *Instrumented) FetchCombinedStatus(owner, repo, ref string) ([]gh.CommitStatus, error) {
	return do1(in, "FetchCombinedStatus", false, Args{Owner: owner, Repo: repo, SHA: ref},
		func() ([]gh.CommitStatus, error) { return in.sim.FetchCombinedStatus(owner, repo, ref) })
}

func (in *Instrumented) FetchCommitsBehind(owner, repo, base, head string) (int, error) {
	return do1(in, "FetchCommitsBehind", false, Args{Owner: owner, Repo: repo, Values: []string{base, head}},
		func() (int, error) { return in.sim.FetchCommitsBehind(owner, repo, base, head) })
}

// --- misc ------------------------------------------------------------------

func (in *Instrumented) FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error) {
	return do1(in, "FetchLatestRelease", false, Args{Owner: owner, Repo: repo},
		func() (*gh.LatestRelease, error) { return in.sim.FetchLatestRelease(owner, repo) })
}

func (in *Instrumented) FetchRepoAccess(owner, repo string) (gh.RepoAccess, error) {
	return do1(in, "FetchRepoAccess", false, Args{Owner: owner, Repo: repo},
		func() (gh.RepoAccess, error) { return in.sim.FetchRepoAccess(owner, repo) })
}

func (in *Instrumented) DeleteForwardingHooks(owner, repo string) error {
	return do0(in, "DeleteForwardingHooks", true, Args{Owner: owner, Repo: repo},
		func() error { return in.sim.DeleteForwardingHooks(owner, repo) })
}

// RateLimitStats is logged but never faulted: it is the one interface method
// with no error return, so there is no channel through which a fault could
// surface. Registering one panics (see fault.go). This wrapper still exists —
// and must — because leaving it out would fall through to nothing at all: with
// sim held as a named field there is no promotion to fall back on, so the
// omission would not compile. That is the property this design is built for.
func (in *Instrumented) RateLimitStats() (gh.RateLimitStats, gh.RateLimitStats) {
	seq := in.log.reserve("RateLimitStats", false, Args{}, in.sim.now())
	rest, graphql := in.sim.RateLimitStats()
	in.log.complete(seq, nil, false)
	return rest, graphql
}
