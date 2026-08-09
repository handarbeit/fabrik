package simgh

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	gh "github.com/handarbeit/fabrik/github"
)

// Mergeable-state values the model derives. "has_hooks" is deliberately never
// produced; see FIDELITY.md.
const (
	stateClean    = "clean"
	stateUnstable = "unstable"
	stateBlocked  = "blocked"
	stateBehind   = "behind"
	stateDirty    = "dirty"
	stateDraft    = "draft"
	stateUnknown  = "unknown"
)

// issueBranch is the head-branch convention Fabrik uses to link a PR to an
// issue. Production's FetchLinkedPR discovers the linkage by querying the
// pulls list endpoint for exactly this head ref, so the model matches on it
// too rather than on a stored back-reference — a laxer rule here would let a
// test pass against linkage production would never find.
func issueBranch(issueNumber int) string {
	return fmt.Sprintf("fabrik/issue-%d", issueNumber)
}

// FindPRForIssue returns the number of the PR whose head branch is the issue's
// Fabrik branch, or 0 when there is none.
func (s *Sim) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	pr, err := s.FetchLinkedPR(owner, repo, issueNumber)
	if err != nil {
		return 0, err
	}
	if pr == nil {
		return 0, nil
	}
	return pr.Number, nil
}

// FetchLinkedPR returns the PR linked to an issue by head-branch convention,
// or nil when there is none.
//
// When more than one PR shares the head branch, the most recently created one
// wins. Production queries `pulls?head=owner:branch&state=all&per_page=1`, and
// that endpoint defaults to created-descending, so GitHub returns the newest.
// The case is not hypothetical: Fabrik reuses fabrik/issue-<N>, so a PR closed
// without merging leaves a stale record on the branch the next PR reuses.
// Picking the oldest would hand the engine a closed PR that production would
// never have shown it.
//
// It deliberately reports MergeableState as "" and populates no mergeable
// flag. Production reaches this through the pulls *list* endpoint, which omits
// mergeable_state entirely — returning a computed value here would let a test
// pass on a signal production never receives, which is precisely the
// confidently-green failure this layer must not introduce. Callers that need
// the state must go through FetchPRMergeableState, as the engine does.
func (s *Sim) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	branch := issueBranch(issueNumber)

	s.mu.Lock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	found := findPRByHeadLocked(r, branch)
	if found == nil {
		s.mu.Unlock()
		return nil, nil
	}
	snap := *found
	s.mu.Unlock()

	headSHA, err := s.resolveRefSHA(owner, repo, snap.head)
	if err != nil {
		return nil, err
	}

	return &gh.PRDetails{
		Number:              snap.number,
		Title:               snap.title,
		State:               snap.state,
		Merged:              snap.merged,
		Draft:               snap.draft,
		HeadSHA:             headSHA,
		HeadRefName:         snap.head,
		BaseRef:             snap.base,
		Body:                snap.body,
		MergeableState:      "", // omitted by the list endpoint — see doc comment
		AutoMergeEnabled:    snap.autoMergeEnabled,
		IsMergeQueueEnabled: false, // GraphQL-only field; REST leaves it false
		IsInMergeQueue:      false,
		Author:              snap.author,
		Labels:              cloneStrings(snap.labels),
	}, nil
}

// ListPRs returns every PR in the repo. Like production's list-endpoint
// implementation, it leaves MergeableState empty.
func (s *Sim) ListPRs(owner, repo string) ([]gh.PRDetails, error) {
	s.mu.Lock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	nums := sortedPRNumbers(r)
	snaps := make([]prRecord, 0, len(nums))
	for _, n := range nums {
		snaps = append(snaps, *r.prs[n])
	}
	s.mu.Unlock()

	out := make([]gh.PRDetails, 0, len(snaps))
	for i := range snaps {
		headSHA, err := s.resolveRefSHA(owner, repo, snaps[i].head)
		if err != nil {
			return nil, err
		}
		out = append(out, gh.PRDetails{
			Number:      snaps[i].number,
			Title:       snaps[i].title,
			State:       snaps[i].state,
			Merged:      snaps[i].merged,
			Draft:       snaps[i].draft,
			HeadSHA:     headSHA,
			HeadRefName: snaps[i].head,
			BaseRef:     snaps[i].base,
			Body:        snaps[i].body,
			Author:      snaps[i].author,
			Labels:      cloneStrings(snaps[i].labels),
		})
	}
	return out, nil
}

// CreateDraftPR opens a draft pull request. The issueNumber parameter is
// accepted and ignored for linkage, exactly as production does — the head
// branch is what establishes the link.
func (s *Sim) CreateDraftPR(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
	return s.createPR(owner, repo, title, head, base, body, issueNumber, true)
}

// CreatePR opens a ready (non-draft) pull request.
func (s *Sim) CreatePR(owner, repo, title, head, base, body string) (int, error) {
	return s.createPR(owner, repo, title, head, base, body, 0, false)
}

func (s *Sim) createPR(owner, repo, title, head, base, body string, issueNumber int, draft bool) (int, error) {
	r, err := s.repoByKey(repoKey(owner, repo))
	if err != nil {
		return 0, err
	}

	// GitHub refuses to open a PR for a head ref that does not exist; so does
	// the model, since every git-derived answer about the PR depends on it.
	r.gitMu.Lock()
	headOK := r.branchExists(head)
	baseOK := r.branchExists(base)
	r.gitMu.Unlock()
	if !headOK {
		return 0, fmt.Errorf("simgh: cannot create PR: head branch %q does not exist in %s", head, repoKey(owner, repo))
	}
	if !baseOK {
		return 0, fmt.Errorf("simgh: cannot create PR: base branch %q does not exist in %s", base, repoKey(owner, repo))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	num := r.allocNumber()
	now := s.now()
	r.prs[num] = &prRecord{
		number:      num,
		title:       title,
		body:        body,
		head:        head,
		base:        base,
		state:       "open",
		draft:       draft,
		author:      simActor,
		issueNumber: issueNumber,
		createdAt:   now,
		updatedAt:   now,
	}
	return num, nil
}

// MarkPRReady flips a draft PR to ready for review. Idempotent.
func (s *Sim) MarkPRReady(owner, repo string, prNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	pr.draft = false
	pr.updatedAt = s.now()
	return nil
}

// FetchPRMerged reports the authoritative merged flag, as the single-PR
// endpoint does.
func (s *Sim) FetchPRMerged(owner, repo string, prNumber int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return false, err
	}
	return pr.merged, nil
}

// FetchPRMergeable reports whether the PR merges cleanly, from a real trial
// merge in the backing repository. Nil means GitHub has not finished computing
// it — see FetchPRMergeableFields.
func (s *Sim) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	mergeable, _, err := s.FetchPRMergeableFields(owner, repo, prNumber)
	return mergeable, err
}

// FetchPRMergeableState reports GitHub's branch-protection-aware
// mergeable_state. See FetchPRMergeableFields for how it is derived.
func (s *Sim) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	_, state, err := s.FetchPRMergeableFields(owner, repo, prNumber)
	return state, err
}

// FetchPRMergeableFields returns the mergeable flag and mergeable_state
// together, as the single-PR endpoint does.
//
// The state is derived from the whole model, not from the trial merge alone,
// because the engine branches hard on the difference between its values:
// MergeableStateAccepted admits only {clean, unstable}, and ErrNotMergeableCI
// fires specifically on blocked/unknown and must not be routed into the
// rebase-reinvoke path. A model that could only say dirty-or-clean would make
// four of the six states unreachable and ADR-072's merge-safety incident
// unreproducible.
//
// Precedence, applied in this order:
//
//  1. unknown  — the PR is merged or closed, or a recompute window is pending
//  2. draft    — the PR is a draft
//  3. dirty    — the trial merge genuinely conflicts
//  4. behind   — the base branch requires up-to-date heads and has advanced
//  5. blocked  — a required context is missing, pending, or failing
//  6. unstable — a non-required context is pending or failing
//  7. clean    — none of the above
//
// This order is the model's own deliberate choice, not a reverse-engineering
// of GitHub's internal algorithm for every combination. A draft PR that also
// conflicts resolves to "draft", not "dirty". FIDELITY.md states this so a
// scenario author reasons about edge cases rather than discovering them.
func (s *Sim) FetchPRMergeableFields(owner, repo string, prNumber int) (*bool, string, error) {
	s.mu.Lock()
	r, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		s.mu.Unlock()
		return nil, "", err
	}
	// A merged or closed PR has no mergeability to report.
	if pr.merged || pr.state == "closed" {
		s.mu.Unlock()
		return nil, stateUnknown, nil
	}
	// The recompute window: GitHub reports both fields unresolved together
	// while it recomputes after a push. Each read drains one.
	if pr.mergeableRecomputeReads > 0 {
		pr.mergeableRecomputeReads--
		s.mu.Unlock()
		return nil, stateUnknown, nil
	}
	head, base, draft := pr.head, pr.base, pr.draft
	s.mu.Unlock()

	facts, err := s.gitFacts(r, base, head)
	if err != nil {
		return nil, "", err
	}

	mergeable := !facts.conflict
	state := s.deriveMergeableState(r, base, facts, draft)
	return &mergeable, state, nil
}

// gitFactsResult holds everything the derivation needs from git.
type gitFactsResult struct {
	conflict bool
	behind   int
	headSHA  string
}

// gitFacts runs the real-git half of the derivation. Takes gitMu; must not be
// called while holding mu.
func (s *Sim) gitFacts(r *repoState, base, head string) (gitFactsResult, error) {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()

	headSHA := r.headSHA(head)
	if headSHA == "" {
		return gitFactsResult{}, fmt.Errorf("simgh: PR head branch %q no longer exists in %s/%s", head, r.owner, r.repo)
	}
	_, conflict, err := r.tryMerge(base, head, false, "simgh trial merge")
	if err != nil {
		return gitFactsResult{}, err
	}
	behind, err := r.commitsBehind(base, head)
	if err != nil {
		return gitFactsResult{}, err
	}
	return gitFactsResult{conflict: conflict, behind: behind, headSHA: headSHA}, nil
}

// deriveMergeableState applies the precedence order documented on
// FetchPRMergeableFields. Takes mu; must not be called while holding it.
func (s *Sim) deriveMergeableState(r *repoState, base string, facts gitFactsResult, draft bool) string {
	if draft {
		return stateDraft
	}
	if facts.conflict {
		return stateDirty
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// GitHub reports "behind" only where branch protection requires the head
	// to be up to date; without that setting a PR whose base has advanced is
	// still "clean". Gating on the seeded requirement rather than on
	// behind > 0 keeps "clean" reachable in the ordinary case.
	if facts.behind > 0 && r.requireUpToDate[base] {
		return stateBehind
	}

	required := r.requiredContexts[base]
	// One verdict per context name; contextsForSHA owns the reduction rule
	// (latest-wins within a collection), which is load-bearing for reruns.
	byName := r.contextsForSHA(facts.headSHA)

	for _, name := range required {
		v, ok := byName[name]
		if !ok || v != "success" {
			return stateBlocked
		}
	}
	for name, v := range byName {
		if contains(required, name) {
			continue
		}
		if v != "success" {
			return stateUnstable
		}
	}
	return stateClean
}

// FetchPRDetails reads the single-PR endpoint, which — unlike the pulls *list*
// endpoint behind FetchLinkedPR — does report mergeable_state. The state is the
// same derivation FetchPRMergeableState returns, recompute window included: a
// read here drains one, because on real GitHub this is exactly the read that
// would.
//
// The fields production's implementation does not parse from that endpoint
// (HeadRefName, BaseRef, Author, Labels) are deliberately left zero. Populating
// them would let a test pass on a signal the engine never actually receives —
// the same reasoning FetchLinkedPR applies to mergeable_state, in reverse.
func (s *Sim) FetchPRDetails(owner, repo string, prNumber int) (*gh.PRDetails, error) {
	// Taken before the mergeability derivation, which takes mu and gitMu itself.
	s.mu.Lock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	snap := *pr
	s.mu.Unlock()

	state, err := s.FetchPRMergeableState(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}

	headSHA, err := s.resolveRefSHA(owner, repo, snap.head)
	if err != nil {
		return nil, err
	}

	return &gh.PRDetails{
		Number:           snap.number,
		Title:            snap.title,
		State:            snap.state,
		Merged:           snap.merged,
		Draft:            snap.draft,
		Body:             snap.body,
		HeadSHA:          headSHA,
		MergeableState:   state,
		AutoMergeEnabled: snap.autoMergeEnabled,
	}, nil
}

// worseVerdict returns the more severe of two verdicts, ordering
// failure > pending > success.
// worseVerdict returns the more blocking of two verdicts.
//
// Both arguments must already be normalised to exactly "success", "pending", or
// "failure" — checkVerdict and statusVerdict are the only producers, and both
// map onto those three literals. That invariant is what makes the tie-break
// safe: equal rank means the two strings are *identical*, so returning either
// is the same answer, and the caller's map-iteration order cannot affect the
// result. Do not read this as "the first argument wins on a tie" — it preserves
// the rank, not the identity of a particular source.
//
// The invariant also matters because an unrecognised string ranks 0 (a missing
// map key), i.e. it would be silently treated as success. Feeding a raw check
// conclusion in here rather than a normalised verdict would mask a red check.
func worseVerdict(a, b string) string {
	rank := map[string]int{"success": 0, "pending": 1, "failure": 2}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// MergePR merges a pull request, writing a real merge commit onto the base
// branch in the backing repository and only then flipping merged.
//
// It self-gates on the derived mergeable_state before touching git, the way
// GitHub's own merge endpoint refuses a merge branch protection forbids. The
// error kinds mirror production's, so a sim-driven test exercises the same
// branches the engine takes against real GitHub:
//
//   - gh.ErrNotMergeable   — a genuine conflict, or mergeability not yet
//     computed. Callers may route this into the rebase path.
//   - gh.ErrNotMergeableCI — no conflict, but the state is not in
//     {clean, unstable}. Callers must NOT route this into the rebase path.
//
// Without this gate a scenario could merge a PR in a blocked state, which is
// exactly the class of bug ADR-072 records.
func (s *Sim) MergePR(owner, repo string, prNumber int) error {
	mergeable, state, err := s.FetchPRMergeableFields(owner, repo, prNumber)
	if err != nil {
		return err
	}
	if mergeable == nil || !*mergeable {
		return fmt.Errorf("simgh: merging PR #%d (state %q): %w", prNumber, state, gh.ErrNotMergeable)
	}
	if !gh.MergeableStateAccepted(state) {
		return fmt.Errorf("simgh: merging PR #%d (state %q): %w", prNumber, state, gh.ErrNotMergeableCI)
	}

	s.mu.Lock()
	r, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	head, base, title := pr.head, pr.base, pr.title
	s.mu.Unlock()

	r.gitMu.Lock()
	sha, conflict, err := r.tryMerge(base, head, true, fmt.Sprintf("Merge pull request #%d from %s\n\n%s", prNumber, head, title))
	r.gitMu.Unlock()
	if errors.Is(err, errNothingToMerge) {
		// Nothing to merge: the head is already in the base, so git writes no
		// commit. Refuse rather than record a merge that produced nothing —
		// merged=true with no merge commit is the exact silent divergence this
		// package exists to prevent. Reported as ErrNotMergeable so callers
		// using errors.Is classify it the way they classify GitHub's refusal.
		return fmt.Errorf("simgh: merging PR #%d: %v: %w", prNumber, err, gh.ErrNotMergeable)
	}
	if err != nil {
		return err
	}
	if conflict {
		// The gate above said mergeable; if git disagrees now, something
		// changed underneath us. Report it as the conflict it is rather than
		// recording a merge that did not happen.
		return fmt.Errorf("simgh: merging PR #%d: conflict on merge: %w", prNumber, gh.ErrNotMergeable)
	}
	_ = sha

	s.mu.Lock()
	defer s.mu.Unlock()
	pr.merged = true
	pr.state = "closed"
	pr.updatedAt = s.now()

	// GitHub auto-closes issues a merged PR references with a closing keyword,
	// but only when the merge lands on the default branch. The engine relies
	// on this (its awaiting-member-close settle scan treats "closed by
	// GitHub's own Closes #N" as success), so the model reproduces it —
	// including the default-branch restriction, which is why Fabrik has to
	// close issues itself on a non-default base (ADR-1096).
	if base == r.defaultBranch {
		for _, n := range closingIssueNumbers(pr) {
			if iss, ok := r.issues[n]; ok && iss.state != "CLOSED" {
				iss.state = "CLOSED"
				iss.updatedAt = s.now()
			}
		}
	}
	return nil
}

// GetPRBase returns the PR's base branch.
func (s *Sim) GetPRBase(owner, repo string, prNumber int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return "", err
	}
	return pr.base, nil
}

// UpdatePRBase retargets a PR at a different base branch. The branch must
// exist, since every git-derived answer about the PR is computed against it.
func (s *Sim) UpdatePRBase(owner, repo string, prNumber int, newBase string) error {
	r, err := s.repoByKey(repoKey(owner, repo))
	if err != nil {
		return err
	}
	r.gitMu.Lock()
	exists := r.branchExists(newBase)
	r.gitMu.Unlock()
	if !exists {
		return fmt.Errorf("simgh: cannot retarget PR #%d: base branch %q does not exist", prNumber, newBase)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	pr.base = newBase
	pr.updatedAt = s.now()
	return nil
}

// EnablePullRequestAutoMerge turns on GitHub's native auto-merge.
//
// Modelled as a flag only: the sim never spontaneously merges the PR when
// checks later go green. A scenario that wants the merge to happen must call
// MergePR. See FIDELITY.md.
func (s *Sim) EnablePullRequestAutoMerge(owner, repo string, prNumber int, strategy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	if !r.access.AllowAutoMerge {
		return fmt.Errorf("simgh: auto-merge is not enabled for %s", repoKey(owner, repo))
	}
	pr.autoMergeEnabled = true
	pr.autoMergeStrategy = strategy
	pr.updatedAt = s.now()
	return nil
}

// DisablePullRequestAutoMerge turns off native auto-merge.
func (s *Sim) DisablePullRequestAutoMerge(owner, repo string, prNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	pr.autoMergeEnabled = false
	pr.autoMergeStrategy = ""
	pr.updatedAt = s.now()
	return nil
}

// EnqueuePullRequest adds a PR to the repo's merge queue.
//
// Modelled as position bookkeeping only: the queue never advances on its own,
// never runs checks, and never merges anything. See FIDELITY.md.
func (s *Sim) EnqueuePullRequest(owner, repo string, prNumber int, expectedHeadOID string) error {
	r, err := s.repoByKey(repoKey(owner, repo))
	if err != nil {
		return err
	}

	s.mu.Lock()
	pr, ok := r.prs[prNumber]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("simgh: PR %s#%d not found", repoKey(owner, repo), prNumber)
	}
	if !r.mergeQueueEnabled {
		s.mu.Unlock()
		return fmt.Errorf("simgh: merge queue is not enabled for %s", repoKey(owner, repo))
	}
	head := pr.head
	s.mu.Unlock()

	if expectedHeadOID != "" {
		actual, err := s.resolveRefSHA(owner, repo, head)
		if err != nil {
			return err
		}
		// GitHub rejects an enqueue whose expected head OID no longer matches,
		// which is how a race with a concurrent push is caught.
		//
		// Not atomic with the write below: this read takes gitMu, the write
		// takes mu, and the locking invariant forbids holding both. GitHub does
		// this compare-and-swap server-side and can. Recorded in FIDELITY.md
		// under the merge queue rather than closed, because closing it would
		// cost the invariant that keeps the whole package deadlock-free.
		if actual != expectedHeadOID {
			return fmt.Errorf("simgh: enqueue PR #%d: expected head %s but branch is at %s", prNumber, expectedHeadOID, actual)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if pr.inMergeQueue {
		return nil
	}
	pr.inMergeQueue = true
	pr.queueEntry = &gh.MergeQueueEntry{
		State:         "QUEUED",
		Position:      queueDepth(r) + 1,
		EnqueuerLogin: simActor,
	}
	pr.updatedAt = s.now()
	return nil
}

// DequeuePullRequest removes a PR from the merge queue. Positions of the PRs
// behind it are not renumbered; see FIDELITY.md.
func (s *Sim) DequeuePullRequest(owner, repo string, prNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	pr.inMergeQueue = false
	pr.queueEntry = nil
	pr.updatedAt = s.now()
	return nil
}

// queueDepth counts PRs currently in the merge queue. Caller must hold mu.
func queueDepth(r *repoState) int {
	n := 0
	for _, pr := range r.prs {
		if pr.inMergeQueue {
			n++
		}
	}
	return n
}

// closingKeywordRe matches GitHub's closing keywords followed by a same-repo
// issue reference. It covers the common forms only, not the full grammar
// (cross-repo `owner/repo#N` and full URLs are not matched) — see FIDELITY.md.
var closingKeywordRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b[:\s]+#(\d+)`)

// FetchPRClosingIssues returns the issues a PR closes on merge.
func (s *Sim) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	return closingIssueNumbers(pr), nil
}

// closingIssueNumbers scans a PR body for closing keywords, plus the issue
// number CreateDraftPR was called with. Caller must hold mu.
func closingIssueNumbers(pr *prRecord) []int {
	seen := make(map[int]bool)
	var out []int
	add := func(n int) {
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, m := range closingKeywordRe.FindAllStringSubmatch(pr.body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			add(n)
		}
	}
	add(pr.issueNumber)
	sort.Ints(out)
	return out
}

// FetchPRsForSHA returns the numbers of PRs whose head branch currently points
// at the given SHA.
func (s *Sim) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	s.mu.Lock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	nums := sortedPRNumbers(r)
	heads := make([]string, len(nums))
	for i, n := range nums {
		heads[i] = r.prs[n].head
	}
	s.mu.Unlock()

	var out []int
	for i, n := range nums {
		headSHA, err := s.resolveRefSHA(owner, repo, heads[i])
		if err != nil {
			return nil, err
		}
		if headSHA == sha {
			out = append(out, n)
		}
	}
	return out, nil
}

// sortedPRNumbers returns a repo's PR numbers in ascending order, so
// projections built from the map are deterministic. Caller must hold mu.
func sortedPRNumbers(r *repoState) []int {
	out := make([]int, 0, len(r.prs))
	for n := range r.prs {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
