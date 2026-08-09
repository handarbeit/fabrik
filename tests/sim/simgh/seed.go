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
// Most Seed* methods construct *static initial state*: the board as it stands
// when the scenario begins. Alongside them are the Seed*At / Seed*After
// methods, which enqueue a mutation for a future instant on the injected clock
// — "this SHA goes red at T", "the reviewer responds forty minutes in". They
// are the same seeding idiom (sticky error, chainable) and write into the same
// objects; what differs is *when* the write lands. See schedule.go for why
// sequencing is clock-driven rather than read-count-driven, and FIDELITY.md
// for what a scheduled surface lets a scenario construct that real GitHub
// never would.
//
// Fault injection and the mutation log are deliberately *not* here. They are
// cross-cutting concerns over every interface method, and live in the
// instrumentation layer (instrumented.go, fault.go, mutationlog.go) which
// decorates a Sim rather than modifying it. Wrap a seeded Sim with Instrument
// to get them.

// IssueSeed describes an issue to create.
type IssueSeed struct {
	// Number is 0 to auto-assign from the repo's shared issue-and-PR sequence.
	// An explicit number a PR already holds is a seeding error — GitHub numbers
	// both kinds from one counter.
	Number    int
	Title     string
	Body      string
	Author    string
	Labels    []string
	Assignees []string
	State     string // "OPEN" (default) or "CLOSED"

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

	// Serialise repo creation end to end. Checking "already seeded" under mu,
	// releasing mu for the git init, then publishing is not enough on its own:
	// concurrent callers for one key all clear the check before any publishes,
	// and they then race inside initBare against one directory with no gitMu in
	// existence yet to serialise them. This is the one seeding call that does
	// I/O before publishing state, so it takes its own lock for the whole
	// sequence — cheap, since seeding is setup, not a hot path.
	//
	// Ordering: seedRepoMu is only ever taken here, at the outermost level, and
	// mu is taken and released inside it. Nothing acquires it while holding mu
	// or gitMu, so it cannot participate in a cycle.
	s.seedRepoMu.Lock()
	defer s.seedRepoMu.Unlock()

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
		worktreeRoot:      dir + ".worktrees",
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
	sha, err := r.headSHA(branch)
	if err != nil {
		return "", err
	}
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
	switch {
	case num == 0:
		num = r.allocNumber()
	case num < 0:
		// reserveNumber is a no-op below nextNumber, so a negative number would
		// be accepted silently and then embedded in node IDs and every board
		// projection — a number GitHub could never assign.
		s.fail("simgh: issue number %d is not valid; use 0 to auto-assign", num)
		return s
	case r.numberTaken(num):
		// Shared number space: #N may already be a PR, not just an issue.
		s.fail("simgh: %s#%d already exists", ownerRepo, num)
		return s
	}
	// Only GitHub's own enum, not merely "non-empty". Every downstream read
	// compares this by exact string (board projections, IsClosed, the PR
	// auto-close path), so "closed" or "Open" would be stored verbatim and then
	// silently misclassify the issue as open forever.
	state := seed.State
	switch state {
	case "":
		state = "OPEN"
	case "OPEN", "CLOSED":
	default:
		s.fail("simgh: issue state %q is not valid; use \"OPEN\" or \"CLOSED\"", state)
		return s
	}
	r.reserveNumber(num)
	now := s.now()
	iss := &issueRecord{
		number:         num,
		title:          seed.Title,
		body:           seed.Body,
		state:          state,
		author:         seed.Author,
		labels:         cloneStrings(seed.Labels),
		assignees:      cloneStrings(seed.Assignees),
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
	// GitHub refuses a PR whose head is its base ("No commits between ..."), and
	// the shape is degenerate here too: the trial merge collapses to the
	// nothing-to-merge path.
	if seed.Head == base {
		s.fail("simgh: PR head and base are both %q; GitHub refuses a PR with no commits between them", base)
		return s
	}

	num := seed.Number
	switch {
	case num == 0:
		num = r.allocNumber()
	case num < 0:
		s.fail("simgh: PR number %d is not valid; use 0 to auto-assign", num)
		return s
	case r.numberTaken(num):
		// Shared number space: #N may already be an issue, not just a PR.
		s.fail("simgh: PR %s#%d already exists", ownerRepo, num)
		return s
	}

	// Refuse shapes GitHub cannot produce. A merged PR is always closed, and a
	// draft can never have been merged — accepting either would let a scenario
	// drive the engine from a state production can never deliver, and pass.
	//
	// The state string is checked against GitHub's REST enum rather than merely
	// for emptiness, for the reason SeedIssue states: downstream reads compare
	// it exactly, so "Open" would be stored and then never match.
	state := seed.State
	switch state {
	case "", "open", "closed":
	default:
		s.fail("simgh: PR state %q is not valid; use \"open\" or \"closed\"", state)
		return s
	}
	if seed.Merged {
		if seed.Draft {
			s.fail("simgh: PR %s#%d cannot be both merged and draft", ownerRepo, num)
			return s
		}
		if state == "open" {
			s.fail("simgh: PR %s#%d cannot be merged and open; a merged PR is closed", ownerRepo, num)
			return s
		}
		// Unspecified means "whatever merging implies", which is closed.
		state = "closed"
	}
	if state == "" {
		state = "open"
	}
	r.reserveNumber(num)
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
	if isPR {
		// Board projections resolve a card's content as an issue
		// (buildProjectItem), so a PR card would read back as nothing at all —
		// every board fetch would silently omit it, with no error. Refuse it at
		// seed time instead: a loud failure here beats a scenario quietly
		// asserting against a board the model never built. Recorded in
		// FIDELITY.md as absent.
		s.fail("simgh: PR cards on a project board are not modelled (%s#%d); see FIDELITY.md", ownerRepo, number)
		return
	}
	// The card must point at something that exists. buildProjectItem resolves a
	// card's content through r.issues and returns nil when it finds nothing, so
	// a card for a mistyped or not-yet-seeded number is recorded here and then
	// silently omitted from every board read — the scenario believes it built a
	// board it did not. Checked after the PR refusal above, which gives a
	// clearer diagnosis for the one case a valid number can still fail.
	r, ok := s.repos[ownerRepo]
	if !ok {
		s.fail("simgh: repo %s not seeded; cannot place %s#%d on project %s", ownerRepo, ownerRepo, number, p.id)
		return
	}
	if _, ok := r.issues[number]; !ok {
		s.fail("simgh: no issue %s#%d to place on project %s", ownerRepo, number, p.id)
		return
	}
	id := itemNodeID(p.owner, p.num, ownerRepo, number)
	if existing, ok := p.items[id]; ok {
		// Unlike AddProjectV2ItemById this *does* move the card, so a column
		// change counts as a change alongside an un-archive. Re-seeding a card
		// into the column it already occupies is a no-op and must not bump.
		changed := existing.archived || existing.status != status
		existing.status = status
		if changed {
			s.reviveCardLocked(p, existing)
		}
		return
	}
	now := s.now()
	p.items[id] = &itemState{
		itemID:    id,
		ownerRepo: ownerRepo,
		number:    number,
		isPR:      isPR,
		status:    status,
		updatedAt: now,
	}
	p.itemOrder = append(p.itemOrder, id)
	p.updatedAt = now
}

// SeedCheckRun attaches a check run to a commit SHA. Name and Conclusion are
// what the mergeable-state derivation reads; an empty ID is auto-assigned.
//
// IDs are unique across the whole sim, as GitHub's are, and an explicitly
// seeded ID is reserved against later auto-assignment — the same rule
// reserveNumber enforces for the shared issue-and-PR sequence. This is
// load-bearing, not tidiness: production's latestCheckRunsByName
// (github/checkruns.go) reduces same-named runs by keeping the highest ID as
// the most recent rerun, so two runs sharing an ID make it keep whichever it
// saw first. A scenario seeding "check failed, then the rerun passed" would
// then be classified off the failed run, with nothing to indicate why.
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
	} else if s.seededCheckRunIDs[run.ID] {
		s.fail("simgh: check run ID %d already used in %s; IDs are unique across the sim", run.ID, ownerRepo)
		return s
	} else if run.ID >= s.nextCheckRunID {
		// Keep the auto-assign counter ahead of every explicit ID, so a later
		// ID: 0 seed can never collide with one already handed out by hand.
		s.nextCheckRunID = run.ID + 1
	}
	s.seededCheckRunIDs[run.ID] = true
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
//
// n must be positive. Seeding zero would make FetchPRReviewDecision report
// APPROVED on a PR with no reviews at all — the approvals-satisfied branch is
// reached vacuously — which is not a reading real GitHub produces. "No review
// requirement" is already representable, and is the *absence* of this call:
// leaving the branch unseeded returns "" (GraphQL's null), the case whose
// engine-side fallback ADR-1250 makes load-bearing. Refusing here keeps those
// two states from collapsing into one silently.
func (s *Sim) SeedRequiredApprovals(ownerRepo, branch string, n int) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 1 {
		s.fail("simgh: SeedRequiredApprovals(%s, %q, %d): count must be positive; "+
			"omit the call entirely to model a branch with no review requirement", ownerRepo, branch, n)
		return s
	}
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
	now := s.now()
	iss.comments = append(iss.comments, &commentRecord{
		databaseID: id,
		author:     author,
		body:       body,
		createdAt:  now,
		reactions:  make(map[string]int),
	})
	iss.updatedAt = now
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
	// Refused on both paths, for the reason AddBlockedByIssue states: a
	// self-block is unsatisfiable, so the dependency gate holds forever.
	if blockerOwnerRepo == ownerRepo && blockerNumber == issueNumber {
		s.fail("simgh: issue %s#%d cannot block itself", ownerRepo, issueNumber)
		return s
	}
	// Validate the blocker for the same reason AddBlockedByIssue does: an
	// unresolvable blocker resolves to State "OPEN", leaving the dependent
	// issue permanently blocked with no diagnostic. The seeding and runtime
	// paths must agree about what is representable.
	br, ok := s.repos[blockerOwnerRepo]
	if !ok {
		s.fail("simgh: blocker repo %s not seeded", blockerOwnerRepo)
		return s
	}
	if _, ok := br.issues[blockerNumber]; !ok {
		s.fail("simgh: blocker issue %s#%d not found", blockerOwnerRepo, blockerNumber)
		return s
	}
	// Dedupe and stamp exactly as AddBlockedByIssue does. GitHub silently
	// ignores a duplicate dependency rather than erroring, and the seeding path
	// diverging from the runtime path about what a repeated link means is the
	// asymmetry this package has had to correct repeatedly.
	for _, dep := range iss.blockedBy {
		depRepo := dep.Repo
		if depRepo == "" {
			depRepo = ownerRepo
		}
		if dep.Number == blockerNumber && depRepo == blockerOwnerRepo {
			return s
		}
	}
	repoField := ""
	if blockerOwnerRepo != ownerRepo {
		repoField = blockerOwnerRepo
	}
	iss.blockedBy = append(iss.blockedBy, gh.Dependency{Number: blockerNumber, Repo: repoField})
	iss.updatedAt = s.now()
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

// SeedRateLimits overrides the static budgets RateLimitStats reports, after
// construction and chainably (WithClock's sibling WithRateLimits does the same
// at construction).
//
// This is the controllability the rate-limit surface actually needs.
// RateLimitStats is the one interface method with no error return, so fault
// injection cannot fail it — registering a fault against it panics rather than
// sitting silently inert (see fault.go). What the engine consumes from it is
// budget *ratios* (engine/backoff.go), so scripting the numbers is what makes
// its thresholds reachable. Recorded in FIDELITY.md.
func (s *Sim) SeedRateLimits(restLimit, restRemaining, graphqlLimit, graphqlRemaining int) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	if restRemaining > restLimit || graphqlRemaining > graphqlLimit {
		s.fail("simgh: SeedRateLimits: remaining (%d rest, %d graphql) cannot exceed the limit (%d rest, %d graphql)",
			restRemaining, graphqlRemaining, restLimit, graphqlLimit)
		return s
	}
	s.restRate = rateBudget{limit: restLimit, remaining: restRemaining, used: restLimit - restRemaining, resetIn: time.Hour}
	s.graphqlRate = rateBudget{limit: graphqlLimit, remaining: graphqlRemaining, used: graphqlLimit - graphqlRemaining, resetIn: time.Hour}
	return s
}

// --- scheduled seeding -----------------------------------------------------
//
// The Seed*At / Seed*After methods enqueue a mutation for a future instant on
// the injected clock instead of applying it now. The step lands on the first
// read of that surface at or after its time, and the write is permanent — see
// schedule.go for why that shape rather than a read filter, and why the clock
// rather than a read count.
//
// Every one of them refuses an empty step: a scheduled mutation that changes
// nothing could never be observed, so a scenario built on one would pass
// without its sequence ever having existed.

// SeedCheckRunsAt schedules check runs to land on a SHA at instant at.
//
// A run whose ID matches one already recorded for that SHA **supersedes it in
// place**, which is how a scenario models one check *transitioning* (queued →
// in_progress → failure): pass the same explicit ID each time. A run with a
// new or auto-assigned ID is appended, which models a *rerun* — the shape
// production's latestCheckRunsByName reduces by keeping the highest ID.
//
// Unlike SeedCheckRun, reusing an existing ID here is allowed rather than a
// seeding error, precisely because the transition case needs it.
func (s *Sim) SeedCheckRunsAt(ownerRepo, sha string, at time.Time, runs ...gh.CheckRun) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(runs) == 0 {
		s.fail("simgh: SeedCheckRunsAt(%s, %s): no check runs given; an empty step is unobservable", ownerRepo, sha)
		return s
	}
	prepared := make([]gh.CheckRun, 0, len(runs))
	for _, run := range runs {
		prepared = append(prepared, s.reserveCheckRunID(run))
	}
	r.ciSchedule.add(at, func(rs *repoState) {
		for _, run := range prepared {
			rs.checkRuns[sha] = upsertCheckRun(rs.checkRuns[sha], run)
		}
	})
	return s
}

// SeedCheckRunsAfter is SeedCheckRunsAt relative to the clock's current
// reading.
func (s *Sim) SeedCheckRunsAfter(ownerRepo, sha string, d time.Duration, runs ...gh.CheckRun) *Sim {
	return s.SeedCheckRunsAt(ownerRepo, sha, s.now().Add(d), runs...)
}

// SeedCommitStatusesAt schedules classic commit statuses to land on a SHA at
// instant at.
//
// Statuses are appended rather than superseded by context, because that is
// already how the classic Statuses API behaves and how contextsForSHA reduces
// them: the last one posted for a context wins. Posting a second status for
// the same context is therefore the transition, with no ID bookkeeping needed.
func (s *Sim) SeedCommitStatusesAt(ownerRepo, sha string, at time.Time, statuses ...gh.CommitStatus) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(statuses) == 0 {
		s.fail("simgh: SeedCommitStatusesAt(%s, %s): no statuses given; an empty step is unobservable", ownerRepo, sha)
		return s
	}
	pending := make([]gh.CommitStatus, len(statuses))
	copy(pending, statuses)
	r.ciSchedule.add(at, func(rs *repoState) {
		rs.commitStatuses[sha] = append(rs.commitStatuses[sha], pending...)
	})
	return s
}

// SeedCommitStatusesAfter is SeedCommitStatusesAt relative to the clock's
// current reading.
func (s *Sim) SeedCommitStatusesAfter(ownerRepo, sha string, d time.Duration, statuses ...gh.CommitStatus) *Sim {
	return s.SeedCommitStatusesAt(ownerRepo, sha, s.now().Add(d), statuses...)
}

// SeedReviewsAt schedules submitted reviews to land on a PR at instant at —
// "the reviewer responds forty minutes in".
//
// Reviews append, matching SeedReview and real GitHub: a submission never
// erases an earlier one, and latestReviewsByAuthor does the collapsing on
// read. A review whose SubmittedAt is zero is stamped with the step's instant
// rather than with the clock's reading when the step happens to be drained, so
// the value does not depend on which read triggered it.
func (s *Sim) SeedReviewsAt(ownerRepo string, prNumber int, at time.Time, reviews ...gh.PRReview) *Sim {
	pr, ok := s.prForSeed(ownerRepo, prNumber)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(reviews) == 0 {
		s.fail("simgh: SeedReviewsAt(%s#%d): no reviews given; an empty step is unobservable", ownerRepo, prNumber)
		return s
	}
	pending := make([]gh.PRReview, 0, len(reviews))
	for _, rev := range reviews {
		if rev.SubmittedAt.IsZero() {
			rev.SubmittedAt = at
		}
		pending = append(pending, rev)
	}
	pr.reviewSchedule.add(at, func(p *prRecord) {
		p.reviews = append(p.reviews, pending...)
		p.updatedAt = laterOf(p.updatedAt, at)
	})
	return s
}

// SeedReviewsAfter is SeedReviewsAt relative to the clock's current reading.
func (s *Sim) SeedReviewsAfter(ownerRepo string, prNumber int, d time.Duration, reviews ...gh.PRReview) *Sim {
	return s.SeedReviewsAt(ownerRepo, prNumber, s.now().Add(d), reviews...)
}

// SeedReviewRequestsAt schedules reviewer requests to appear on a PR at
// instant at.
//
// Requests are added, not replaced, and an already-outstanding login is
// skipped — the same rule AddReviewRequest applies, so a step and an engine
// re-request cannot produce a duplicate entry between them. The step is not
// authoritative over the list: a reviewer the engine has since withdrawn
// (DeleteReviewRequest, the bot re-prompt ladder's own mutation) is genuinely
// gone, and this will re-add them only if its own time had not yet come.
func (s *Sim) SeedReviewRequestsAt(ownerRepo string, prNumber int, at time.Time, requests ...gh.ReviewRequest) *Sim {
	pr, ok := s.prForSeed(ownerRepo, prNumber)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(requests) == 0 {
		s.fail("simgh: SeedReviewRequestsAt(%s#%d): no requests given; an empty step is unobservable", ownerRepo, prNumber)
		return s
	}
	pending := make([]gh.ReviewRequest, 0, len(requests))
	for _, req := range requests {
		if !req.IsBot {
			req.IsBot = gh.IsBotLogin(req.Login)
		}
		pending = append(pending, req)
	}
	pr.reviewSchedule.add(at, func(p *prRecord) {
		changed := false
		for _, req := range pending {
			already := false
			for _, existing := range p.reviewRequests {
				if existing.Login == req.Login {
					already = true
					break
				}
			}
			if already {
				continue
			}
			p.reviewRequests = append(p.reviewRequests, req)
			changed = true
		}
		if changed {
			p.updatedAt = laterOf(p.updatedAt, at)
		}
	})
	return s
}

// SeedReviewRequestsAfter is SeedReviewRequestsAt relative to the clock's
// current reading.
func (s *Sim) SeedReviewRequestsAfter(ownerRepo string, prNumber int, d time.Duration, requests ...gh.ReviewRequest) *Sim {
	return s.SeedReviewRequestsAt(ownerRepo, prNumber, s.now().Add(d), requests...)
}

// reserveCheckRunID assigns an ID when unset and keeps the auto-assign counter
// ahead of every explicit one, so a later ID: 0 seed cannot collide with an ID
// chosen by hand.
//
// Unlike SeedCheckRun's inline version it tolerates an ID that already exists,
// because reusing one is exactly how a scheduled step models a run
// transitioning rather than a rerun. Caller must hold mu.
func (s *Sim) reserveCheckRunID(run gh.CheckRun) gh.CheckRun {
	if run.ID == 0 {
		run.ID = s.nextCheckRunID
		s.nextCheckRunID++
	} else if run.ID >= s.nextCheckRunID {
		s.nextCheckRunID = run.ID + 1
	}
	s.seededCheckRunIDs[run.ID] = true
	if run.Status == "" {
		if run.Conclusion == "" {
			run.Status = "in_progress"
		} else {
			run.Status = "completed"
		}
	}
	return run
}

// upsertCheckRun replaces the entry sharing run's ID, or appends when there is
// none. Replacing in place preserves position, so a transition does not
// reorder the collection under a caller reading it raw.
func upsertCheckRun(list []gh.CheckRun, run gh.CheckRun) []gh.CheckRun {
	for i := range list {
		if list[i].ID == run.ID {
			list[i] = run
			return list
		}
	}
	return append(list, run)
}

// prForSeed looks up a PR for a chained Seed* call, recording a sticky error
// and reporting false when the repo or the PR is missing.
func (s *Sim) prForSeed(ownerRepo string, prNumber int) (*prRecord, bool) {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.fail("simgh: PR %s#%d not found", ownerRepo, prNumber)
		return nil, false
	}
	return pr, true
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
