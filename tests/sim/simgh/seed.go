package simgh

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// This file is the seeding API: how a scenario constructs a board in a given
// state in a few lines.
//
// Every Seed* method returns *Sim and records the first failure in a sticky
// error, so setup reads as a chain and is checked once with Err():
//
//	s := simgh.New(t.TempDir(), simgh.WithClock(clk)).
//		SeedRepo("acme/widgets").
//		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Done"}).
//		SeedIssue("acme/widgets", simgh.IssueSeed{Number: 7, Title: "…", Status: "Implement"})
//	if err := s.Err(); err != nil { t.Fatal(err) }
//
// Seeding constructs *static initial state only*. There is deliberately no way
// here to script a sequence of future responses, inject a fault, or replay a
// mutation log — those are a separate layer (#1457) that will build on these
// same objects. Keeping seeding static is what lets that layer be added
// without breaking this API.

// IssueSeed describes an issue to create.
type IssueSeed struct {
	// Number is 0 to auto-assign from the repo's shared issue-and-PR sequence.
	// An explicit number a PR already holds is a seeding error — GitHub numbers
	// both kinds from one counter.
	Number int
	Title  string
	Body   string
	Author string
	Labels []string
	State  string // "OPEN" (default) or "CLOSED"

	// Status, when non-empty, also places the issue on the default project
	// (the first one seeded) in that column.
	Status string
}

// PRSeed describes a pull request to create. Head and Base are branch names in
// the backing repository; both must already exist (seed them with SeedCommit
// or SeedBranch first) so that every git-derived answer about the PR is real.
type PRSeed struct {
	// Number is 0 to auto-assign from the repo's shared issue-and-PR sequence.
	// An explicit number an issue already holds is a seeding error.
	Number int
	Title  string
	Body   string
	Author string
	Head   string
	Base   string // defaults to the repo's default branch
	Draft  bool
	Merged bool
	State  string // "open" (default) or "closed"

	// IssueNumber links the PR to an issue the way CreateDraftPR does, feeding
	// FindPRForIssue and FetchPRClosingIssues.
	IssueNumber int

	// MergeableRecomputeReads seeds GitHub's recompute window: for this many
	// reads, mergeable reports null and mergeableState reports "unknown"
	// before the real git-derived answer surfaces. See FIDELITY.md.
	MergeableRecomputeReads int
}

// SeedRepo creates a repo and its backing bare git repository, with a root
// commit on the default branch. defaultBranch may be empty for "main".
func (s *Sim) SeedRepo(ownerRepo string, defaultBranch ...string) *Sim {
	branch := "main"
	if len(defaultBranch) > 0 && defaultBranch[0] != "" {
		branch = defaultBranch[0]
	}
	owner, repo, err := splitOwnerRepo(ownerRepo)
	if err != nil {
		s.mu.Lock()
		s.fail("%v", err)
		s.mu.Unlock()
		return s
	}

	s.mu.Lock()
	if _, exists := s.repos[ownerRepo]; exists {
		s.fail("simgh: repo %s already seeded", ownerRepo)
		s.mu.Unlock()
		return s
	}
	dir := s.repoDir(owner, repo)
	s.mu.Unlock()

	if err := s.ensureBaseDir(); err != nil {
		s.mu.Lock()
		s.fail("%v", err)
		s.mu.Unlock()
		return s
	}
	// No repoState exists yet, so no gitMu to take; nothing else can reach
	// this directory until the state is published below.
	if err := initBare(dir, branch); err != nil {
		s.mu.Lock()
		s.fail("%v", err)
		s.mu.Unlock()
		return s
	}

	s.mu.Lock()
	s.repos[ownerRepo] = &repoState{
		owner:             owner,
		repo:              repo,
		bareDir:           dir,
		defaultBranch:     branch,
		issues:            make(map[int]*issueRecord),
		prs:               make(map[int]*prRecord),
		checkRuns:         make(map[string][]gh.CheckRun),
		commitStatuses:    make(map[string][]gh.CommitStatus),
		requiredContexts:  make(map[string][]string),
		requireUpToDate:   make(map[string]bool),
		requiredApprovals: make(map[string]int),
		labelVocab:        make(map[string]bool),
		access:            gh.RepoAccess{AllowAutoMerge: true, CanPush: true},
		nextNumber:        1,
	}
	s.mu.Unlock()
	return s
}

// SeedProject creates a Projects v2 board owned by owner. The first project
// seeded becomes the default that SeedIssue's Status field places cards on.
// Column order is significant: the first is the leftmost.
func (s *Sim) SeedProject(owner string, num int, title string, columns []string) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := projectKey(owner, num)
	if _, exists := s.projects[key]; exists {
		s.fail("simgh: project %s already seeded", key)
		return s
	}
	p := &projectState{
		owner:              owner,
		num:                num,
		title:              title,
		statusOptions:      make(map[string]string),
		orderedStatusNames: cloneStrings(columns),
		items:              make(map[string]*itemState),
		updatedAt:          s.now(),
	}
	p.id = p.nodeID()
	p.statusFieldID = "field:" + p.id + ":Status"
	for _, c := range columns {
		p.statusOptions[c] = fmt.Sprintf("opt:%s:%s", p.id, c)
	}
	s.projects[key] = p
	if s.defaultProject == nil {
		s.defaultProject = p
	}
	return s
}

// SeedCommit writes files onto branch and commits them, creating the branch
// from the repo's default branch if it does not exist. files may be nil for an
// empty commit.
func (s *Sim) SeedCommit(ownerRepo, branch string, files map[string]string, msg string) *Sim {
	return s.seedCommitFrom(ownerRepo, branch, "", files, msg)
}

// SeedCommitFrom is SeedCommit with an explicit parent branch, used when
// forking a feature branch from somewhere other than the default branch.
func (s *Sim) SeedCommitFrom(ownerRepo, branch, fromBranch string, files map[string]string, msg string) *Sim {
	return s.seedCommitFrom(ownerRepo, branch, fromBranch, files, msg)
}

func (s *Sim) seedCommitFrom(ownerRepo, branch, fromBranch string, files map[string]string, msg string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	r.gitMu.Lock()
	_, err := r.commitFiles(branch, fromBranch, files, msg)
	r.gitMu.Unlock()
	if err != nil {
		s.mu.Lock()
		s.fail("%v", err)
		s.mu.Unlock()
	}
	return s
}

// SeedBranch points a new branch at fromBranch's tip (the repo's default
// branch when fromBranch is empty).
func (s *Sim) SeedBranch(ownerRepo, branch, fromBranch string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	r.gitMu.Lock()
	err := r.createBranch(branch, fromBranch)
	r.gitMu.Unlock()
	if err != nil {
		s.mu.Lock()
		s.fail("%v", err)
		s.mu.Unlock()
	}
	return s
}

// HeadSHA returns the commit SHA at a branch tip. Useful when seeding check
// runs, which GitHub keys by SHA.
func (s *Sim) HeadSHA(ownerRepo, branch string) (string, error) {
	r, err := s.repoByKey(ownerRepo)
	if err != nil {
		return "", err
	}
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	sha := r.headSHA(branch)
	if sha == "" {
		return "", fmt.Errorf("simgh: branch %q not found in %s", branch, ownerRepo)
	}
	return sha, nil
}

// SeedIssue creates an issue, optionally placing it on the default project.
func (s *Sim) SeedIssue(ownerRepo string, seed IssueSeed) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	num := seed.Number
	if num == 0 {
		num = r.allocNumber()
	} else if r.numberTaken(num) {
		// Shared number space: #N may already be a PR, not just an issue.
		s.fail("simgh: %s#%d already exists", ownerRepo, num)
		return s
	}
	r.reserveNumber(num)
	state := seed.State
	if state == "" {
		state = "OPEN"
	}
	now := s.now()
	iss := &issueRecord{
		number:         num,
		title:          seed.Title,
		body:           seed.Body,
		state:          state,
		author:         seed.Author,
		labels:         cloneStrings(seed.Labels),
		labelAppliedAt: make(map[string]time.Time),
		createdAt:      now,
		updatedAt:      now,
	}
	for _, l := range iss.labels {
		iss.labelAppliedAt[l] = now
		r.labelVocab[l] = true
	}
	r.issues[num] = iss

	if seed.Status != "" {
		s.placeOnProjectLocked(s.defaultProject, ownerRepo, num, false, seed.Status)
	}
	return s
}

// SeedPR creates a pull request. Its head and base branches must already exist
// in the backing repository — every git-derived answer about the PR is
// computed from them rather than declared here.
func (s *Sim) SeedPR(ownerRepo string, seed PRSeed) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}

	base := seed.Base
	if base == "" {
		base = r.defaultBranch
	}

	// Validate branch existence under gitMu, before taking mu.
	r.gitMu.Lock()
	headOK := r.branchExists(seed.Head)
	baseOK := r.branchExists(base)
	r.gitMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !headOK {
		s.fail("simgh: PR head branch %q does not exist in %s (seed it first)", seed.Head, ownerRepo)
		return s
	}
	if !baseOK {
		s.fail("simgh: PR base branch %q does not exist in %s", base, ownerRepo)
		return s
	}

	num := seed.Number
	if num == 0 {
		num = r.allocNumber()
	} else if r.numberTaken(num) {
		// Shared number space: #N may already be an issue, not just a PR.
		s.fail("simgh: PR %s#%d already exists", ownerRepo, num)
		return s
	}
	r.reserveNumber(num)
	state := seed.State
	if state == "" {
		state = "open"
	}
	now := s.now()
	r.prs[num] = &prRecord{
		number:                  num,
		title:                   seed.Title,
		body:                    seed.Body,
		head:                    seed.Head,
		base:                    base,
		state:                   state,
		draft:                   seed.Draft,
		merged:                  seed.Merged,
		author:                  seed.Author,
		issueNumber:             seed.IssueNumber,
		mergeableRecomputeReads: seed.MergeableRecomputeReads,
		createdAt:               now,
		updatedAt:               now,
	}
	return s
}

// SeedProjectItem places an existing issue or PR on a project board explicitly,
// for scenarios with more than one project.
func (s *Sim) SeedProjectItem(owner string, projectNum int, ownerRepo string, number int, isPR bool, status string) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectKey(owner, projectNum)]
	if !ok {
		s.fail("simgh: project %s not seeded", projectKey(owner, projectNum))
		return s
	}
	s.placeOnProjectLocked(p, ownerRepo, number, isPR, status)
	return s
}

// placeOnProjectLocked adds or moves a card. Caller must hold mu.
func (s *Sim) placeOnProjectLocked(p *projectState, ownerRepo string, number int, isPR bool, status string) {
	if p == nil {
		s.fail("simgh: no project seeded; call SeedProject before placing items")
		return
	}
	if _, ok := p.statusOptions[status]; !ok {
		s.fail("simgh: project %s has no column %q", p.id, status)
		return
	}
	id := itemNodeID(p.owner, p.num, ownerRepo, number)
	if existing, ok := p.items[id]; ok {
		existing.status = status
		existing.updatedAt = s.now()
		p.updatedAt = s.now()
		return
	}
	p.items[id] = &itemState{
		itemID:    id,
		ownerRepo: ownerRepo,
		number:    number,
		isPR:      isPR,
		status:    status,
		updatedAt: s.now(),
	}
	p.itemOrder = append(p.itemOrder, id)
	p.updatedAt = s.now()
}

// SeedCheckRun attaches a check run to a commit SHA. Name and Conclusion are
// what the mergeable-state derivation reads; an empty ID is auto-assigned.
func (s *Sim) SeedCheckRun(ownerRepo, sha string, run gh.CheckRun) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.ID == 0 {
		run.ID = s.nextCheckRunID
		s.nextCheckRunID++
	}
	if run.Status == "" {
		if run.Conclusion == "" {
			run.Status = "in_progress"
		} else {
			run.Status = "completed"
		}
	}
	r.checkRuns[sha] = append(r.checkRuns[sha], run)
	return s
}

// SeedCommitStatus attaches a classic commit status to a SHA. Kept genuinely
// distinct from check runs because production distinguishes them (ADR-933).
func (s *Sim) SeedCommitStatus(ownerRepo, sha string, st gh.CommitStatus) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.commitStatuses[sha] = append(r.commitStatuses[sha], st)
	return s
}

// SeedRequiredContexts declares which check/status context names branch
// protection requires on a branch. This is what makes "blocked" distinguishable
// from "unstable": a red required context blocks the merge, a red non-required
// one only makes the PR unstable. Modelled on ADR-933's RequiredStatusContexts.
func (s *Sim) SeedRequiredContexts(ownerRepo, branch string, contexts []string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.requiredContexts[branch] = cloneStrings(contexts)
	return s
}

// SeedRequireUpToDate turns on branch protection's "require branches to be up
// to date before merging" for a branch. Only with this on does a PR whose base
// has advanced report the "behind" mergeable state; without it, real GitHub
// reports such a PR as "clean", and so does the model.
func (s *Sim) SeedRequireUpToDate(ownerRepo, branch string, required bool) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.requireUpToDate[branch] = required
	return s
}

// SeedRequiredApprovals declares how many approving reviews branch protection
// requires on a branch, backing FetchPRReviewDecision.
func (s *Sim) SeedRequiredApprovals(ownerRepo, branch string, n int) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.requiredApprovals[branch] = n
	return s
}

// SeedMergeableRecomputePending puts a PR into GitHub's recompute window for
// the next reads reads: mergeable reports null and mergeableState "unknown"
// until the counter drains. Also settable at construction via
// PRSeed.MergeableRecomputeReads.
func (s *Sim) SeedMergeableRecomputePending(ownerRepo string, prNumber, reads int) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.fail("simgh: PR %s#%d not found", ownerRepo, prNumber)
		return s
	}
	pr.mergeableRecomputeReads = reads
	return s
}

// SeedReview records a submitted review on a PR.
func (s *Sim) SeedReview(ownerRepo string, prNumber int, review gh.PRReview) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.fail("simgh: PR %s#%d not found", ownerRepo, prNumber)
		return s
	}
	if review.SubmittedAt.IsZero() {
		review.SubmittedAt = s.now()
	}
	pr.reviews = append(pr.reviews, review)
	return s
}

// SeedReviewRequest records an outstanding reviewer request on a PR. IsBot is
// derived from the login when not set explicitly, using the same classifier
// production uses.
func (s *Sim) SeedReviewRequest(ownerRepo string, prNumber int, req gh.ReviewRequest) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.fail("simgh: PR %s#%d not found", ownerRepo, prNumber)
		return s
	}
	if !req.IsBot {
		req.IsBot = gh.IsBotLogin(req.Login)
	}
	pr.reviewRequests = append(pr.reviewRequests, req)
	return s
}

// SeedReviewThreadComment attaches an inline review-thread comment to a PR.
func (s *Sim) SeedReviewThreadComment(ownerRepo string, prNumber int, author, body, path string, line int) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.fail("simgh: PR %s#%d not found", ownerRepo, prNumber)
		return s
	}
	id := s.nextCommentDatabaseID
	s.nextCommentDatabaseID++
	pr.reviewThreadComment = append(pr.reviewThreadComment, &commentRecord{
		databaseID:     id,
		author:         author,
		body:           body,
		createdAt:      s.now(),
		reactions:      make(map[string]int),
		fromPR:         prNumber,
		reviewThreadID: fmt.Sprintf("thread:%s#%d:%d", ownerRepo, prNumber, id),
		path:           path,
		line:           line,
	})
	return s
}

// SeedComment adds a comment to an issue. Returns the Sim for chaining; use
// FetchProjectItem or FetchLinkedPR to read the assigned database ID back.
func (s *Sim) SeedComment(ownerRepo string, issueNumber int, author, body string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	id := s.nextCommentDatabaseID
	s.nextCommentDatabaseID++
	iss.comments = append(iss.comments, &commentRecord{
		databaseID: id,
		author:     author,
		body:       body,
		createdAt:  s.now(),
		reactions:  make(map[string]int),
	})
	iss.updatedAt = s.now()
	return s
}

// SeedBlockedBy records that an issue is blocked by another. The blocker's
// state is resolved live on read, so closing the blocker unblocks the issue
// the way GitHub's dependency graph does.
func (s *Sim) SeedBlockedBy(ownerRepo string, issueNumber int, blockerOwnerRepo string, blockerNumber int) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	repoField := ""
	if blockerOwnerRepo != ownerRepo {
		repoField = blockerOwnerRepo
	}
	iss.blockedBy = append(iss.blockedBy, gh.Dependency{Number: blockerNumber, Repo: repoField})
	return s
}

// SeedRepoAccess overrides the repo's reported access flags.
func (s *Sim) SeedRepoAccess(ownerRepo string, access gh.RepoAccess) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.access = access
	return s
}

// SeedLatestRelease sets what FetchLatestRelease reports for the repo.
func (s *Sim) SeedLatestRelease(ownerRepo string, rel gh.LatestRelease) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.latestRelease = &rel
	return s
}

// SeedMergeQueueEnabled toggles the repo-level merge-queue feature flag.
func (s *Sim) SeedMergeQueueEnabled(ownerRepo string, enabled bool) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.mergeQueueEnabled = enabled
	return s
}

// repoForSeed looks up a repo for a chained Seed* call, recording a sticky
// error and reporting false when it is missing.
func (s *Sim) repoForSeed(ownerRepo string) (*repoState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[ownerRepo]
	if !ok {
		s.fail("simgh: repo %s not seeded", ownerRepo)
		return nil, false
	}
	return r, true
}

// repoByKey looks up a repo by "owner/repo" for non-seeding callers.
func (s *Sim) repoByKey(ownerRepo string) (*repoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[ownerRepo]
	if !ok {
		return nil, fmt.Errorf("simgh: repo %s not seeded", ownerRepo)
	}
	return r, nil
}
