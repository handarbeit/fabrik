package simgh

import (
	"fmt"
	"sort"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Board projections are rebuilt from model state on every call and never
// cached. That is what makes "a mutation is observable by every subsequent
// read" a structural property of this package rather than something each test
// has to re-assert.

// FetchProjectBoard returns the whole board. ownerType is accepted and ignored:
// production uses it only to pick between the organization and user GraphQL
// roots, and the model keys projects by owner regardless of which kind it is.
func (s *Sim) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	s.mu.Lock()
	p, ok := s.projects[projectKey(owner, projectNum)]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("simgh: project %s not seeded", projectKey(owner, projectNum))
	}
	board := &gh.ProjectBoard{
		ProjectID: p.id,
		Title:     p.title,
		OwnerType: ownerType,
	}
	if board.OwnerType == "" {
		board.OwnerType = "organization"
	}
	refs := p.liveItemRefs()
	s.mu.Unlock()

	for _, ref := range refs {
		item, err := s.buildProjectItem(p, ref)
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		board.Items = append(board.Items, *item)
	}
	return board, nil
}

// itemRef identifies a card without holding a pointer into model state.
type itemRef struct {
	itemID    string
	ownerRepo string
	number    int
	isPR      bool
	status    string
	updatedAt time.Time
}

// liveItemRefs snapshots the board's non-archived cards in insertion order.
// Caller must hold mu.
func (p *projectState) liveItemRefs() []itemRef {
	out := make([]itemRef, 0, len(p.itemOrder))
	for _, id := range p.itemOrder {
		it, ok := p.items[id]
		if !ok || it.archived {
			continue
		}
		out = append(out, itemRef{
			itemID:    it.itemID,
			ownerRepo: it.ownerRepo,
			number:    it.number,
			isPR:      it.isPR,
			status:    it.status,
			updatedAt: it.updatedAt,
		})
	}
	return out
}

// buildProjectItem assembles the full projection for one card, including its
// linked PR's reviews, review requests, and unresolved review-thread comments.
// Returns nil when the card no longer belongs on the board — its content is
// gone, or it has been archived or removed since the caller snapshotted it.
//
// The ref the caller passes identifies the card; it does not supply the card's
// state. Every caller snapshots refs under one lock acquisition and then builds
// projections under later ones, so a status move or an archive landing in
// between would otherwise be projected from the stale snapshot — reporting an
// old column, or putting an archived card back on a board fetch. That would
// contradict both this file's "never cached" guarantee and R1's "a mutation is
// observable by every subsequent read", in the one place a fake is least likely
// to be caught doing it. So the card's own fields are re-read live here, under
// the same lock the rest of the projection is built from.
func (s *Sim) buildProjectItem(p *projectState, ref itemRef) (*gh.ProjectItem, error) {
	owner, repoName, err := splitOwnerRepo(ref.ownerRepo)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	live, ok := p.items[ref.itemID]
	if !ok || live.archived {
		s.mu.Unlock()
		return nil, nil
	}
	status, isPR, cardUpdatedAt := live.status, live.isPR, live.updatedAt

	r, ok := s.repos[ref.ownerRepo]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("simgh: repo %s not seeded", ref.ownerRepo)
	}
	iss, ok := r.issues[ref.number]
	if !ok {
		s.mu.Unlock()
		return nil, nil
	}

	item := &gh.ProjectItem{
		ID:        iss.nodeID(ref.ownerRepo),
		ItemID:    ref.itemID,
		Number:    iss.number,
		Title:     iss.title,
		Body:      iss.body,
		Status:    status,
		URL:       fmt.Sprintf("https://github.com/%s/issues/%d", ref.ownerRepo, iss.number),
		Repo:      ref.ownerRepo,
		IsPR:      isPR,
		IsClosed:  iss.state == "CLOSED" && !isPR,
		UpdatedAt: laterOf(iss.updatedAt, cardUpdatedAt),
		Labels:    cloneStrings(iss.labels),
		Assignees: cloneStrings(iss.assignees),
		Author:    iss.author,
		BlockedBy: s.resolveDependenciesLocked(ref.ownerRepo, iss.blockedBy),
	}
	for _, c := range iss.comments {
		item.Comments = append(item.Comments, c.toGH())
	}

	// The linked PR is found the same way production finds it: by head branch.
	linked := findPRByHeadLocked(r, issueBranch(iss.number))
	var linkedHead string
	if linked != nil {
		linkedHead = linked.head
		item.LinkedPRNumber = linked.number
		item.LinkedPRNumberShallow = linked.number
		// Collapsed for the same reason FetchPRReviews collapses: production
		// sources this field from GraphQL's latestReviews, which is one entry
		// per author by definition. The two reads must agree.
		item.LinkedPRReviews = latestReviewsByAuthor(linked.reviews)
		item.LinkedPRReviewRequests = append([]gh.ReviewRequest(nil), linked.reviewRequests...)
		item.LinkedPRResolvedThreadCount = linked.resolvedThreads
		item.LinkedPRIsMergeQueueEnabled = r.mergeQueueEnabled
		item.LinkedPRIsInMergeQueue = linked.inMergeQueue
		if linked.queueEntry != nil {
			entry := *linked.queueEntry
			item.LinkedPRMergeQueueEntry = &entry
		}
		for _, c := range linked.comments {
			item.Comments = append(item.Comments, c.toGH())
		}
		// Only unresolved threads surface: the engine treats these as
		// outstanding feedback, and a resolved thread is exactly the signal
		// that the feedback was addressed.
		for _, c := range linked.reviewThreadComment {
			if !c.threadResolved {
				item.LinkedPRReviewThreadComments = append(item.LinkedPRReviewThreadComments, c.toGH())
			}
		}
		item.UpdatedAt = laterOf(item.UpdatedAt, linked.updatedAt)
	}
	s.mu.Unlock()

	if linkedHead != "" {
		sha, err := s.resolveRefSHA(owner, repoName, linkedHead)
		if err != nil {
			return nil, err
		}
		item.LinkedPRHeadSHA = sha
	}
	return item, nil
}

// findPRByHeadLocked returns the most recently created PR with the given head
// branch, or nil. Caller must hold mu.
//
// Newest-wins, not lowest-numbered: Fabrik reuses the branch fabrik/issue-<N>,
// so a branch can carry a stale closed PR alongside the live one, and
// production resolves that to the newest (see FetchLinkedPR). The board
// projection must agree with FetchLinkedPR on which PR is "the" linked PR —
// were they to disagree, two reads of the same model would report two different
// PR numbers, a bug the sim itself would be introducing.
func findPRByHeadLocked(r *repoState, head string) *prRecord {
	nums := sortedPRNumbers(r)
	for i := len(nums) - 1; i >= 0; i-- {
		if r.prs[nums[i]].head == head {
			return r.prs[nums[i]]
		}
	}
	return nil
}

// resolveDependenciesLocked fills in each blocker's live state, so closing a
// blocker unblocks the dependent issue on the next read the way GitHub's
// dependency graph does. Caller must hold mu.
func (s *Sim) resolveDependenciesLocked(ownerRepo string, deps []gh.Dependency) []gh.Dependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]gh.Dependency, 0, len(deps))
	for _, d := range deps {
		blockerRepo := d.Repo
		if blockerRepo == "" {
			blockerRepo = ownerRepo
		}
		resolved := d
		resolved.State = "OPEN"
		if r, ok := s.repos[blockerRepo]; ok {
			if iss, ok := r.issues[d.Number]; ok {
				resolved.State = iss.state
			}
		}
		out = append(out, resolved)
	}
	return out
}

// ProbeProjectBoard returns the scalar-only per-item shape the idle poll uses,
// plus the project's node ID.
func (s *Sim) ProbeProjectBoard(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
	s.mu.Lock()
	p, ok := s.projects[projectKey(owner, projectNum)]
	if !ok {
		s.mu.Unlock()
		return nil, "", fmt.Errorf("simgh: project %s not seeded", projectKey(owner, projectNum))
	}
	projectID := p.id
	refs := p.liveItemRefs()
	s.mu.Unlock()

	out := make([]gh.BoardProbeItem, 0, len(refs))
	for _, ref := range refs {
		probe, err := s.buildProbeItem(p, ref)
		if err != nil {
			return nil, "", err
		}
		if probe == nil {
			continue
		}
		out = append(out, *probe)
	}
	return out, projectID, nil
}

// buildProbeItem assembles the probe shape for one card. It is the scalar-only
// sibling of buildProjectItem and takes the same contract: the ref identifies
// the card, it does not supply the card's state. Returns nil when the card no
// longer belongs on the board.
//
// Split out for the same reason buildProjectItem is a function: it is where the
// live re-read happens, and keeping it callable with a deliberately stale ref is
// what lets a test pin that the re-read actually occurs. Inside a single
// ProbeProjectBoard call the snapshot and the re-read are indistinguishable.
func (s *Sim) buildProbeItem(p *projectState, ref itemRef) (*gh.BoardProbeItem, error) {
	owner, repoName, err := splitOwnerRepo(ref.ownerRepo)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	// Re-read the card live, for the same reason buildProjectItem does: the ref
	// was snapshotted under an earlier lock acquisition, so a status move or an
	// archive landing in between would otherwise be invisible here — a stale
	// column, or an archived card still in the probe output. The probe feeds
	// the engine's idle poll, so a stale answer here is a poll that does not
	// notice work it should.
	live, ok := p.items[ref.itemID]
	if !ok || live.archived {
		s.mu.Unlock()
		return nil, nil
	}
	r, ok := s.repos[ref.ownerRepo]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("simgh: repo %s not seeded", ref.ownerRepo)
	}
	iss, ok := r.issues[ref.number]
	if !ok {
		s.mu.Unlock()
		return nil, nil
	}
	probe := gh.BoardProbeItem{
		ItemID:    ref.itemID,
		ContentID: iss.nodeID(ref.ownerRepo),
		Number:    iss.number,
		IsPR:      live.isPR,
		IsClosed:  iss.state == "CLOSED" && !live.isPR,
		State:     iss.state,
		Repo:      ref.ownerRepo,
		Status:    live.status,
	}
	// effectiveUpdatedAt is max(content, project item, linked PR).
	probe.EffectiveUpdatedAt = laterOf(iss.updatedAt, live.updatedAt)
	linked := findPRByHeadLocked(r, issueBranch(iss.number))
	var linkedHead string
	if linked != nil {
		linkedHead = linked.head
		probe.LinkedPRNumber = linked.number
		probe.LinkedPRUpdatedAt = linked.updatedAt
		probe.EffectiveUpdatedAt = laterOf(probe.EffectiveUpdatedAt, linked.updatedAt)
	}
	s.mu.Unlock()

	if linkedHead != "" {
		sha, err := s.resolveRefSHA(owner, repoName, linkedHead)
		if err != nil {
			return nil, err
		}
		probe.LinkedPRHeadSHA = sha
	}
	return &probe, nil
}

// FetchItemDetails backfills a partially-populated item in place, keyed by its
// content node ID. The engine constructs a bare item from a
// projects_v2_item.created webhook (node ID only) and calls this to fill in
// the rest, so Number and Repo must be populated when they are zero-valued.
func (s *Sim) FetchItemDetails(item *gh.ProjectItem) error {
	if item == nil {
		return fmt.Errorf("simgh: FetchItemDetails called with nil item")
	}

	s.mu.Lock()
	// Search projects in a stable order. s.projects is a map, so Go randomizes
	// iteration; without this the search below could return a different card
	// between two runs on identical state.
	projectKeys := make([]string, 0, len(s.projects))
	for k := range s.projects {
		projectKeys = append(projectKeys, k)
	}
	sort.Strings(projectKeys)

	search := func(pred func(*itemState) bool) (*projectState, itemRef) {
		for _, k := range projectKeys {
			p := s.projects[k]
			for _, id := range p.itemOrder {
				it, ok := p.items[id]
				if !ok || !pred(it) {
					continue
				}
				return p, itemRef{
					itemID:    it.itemID,
					ownerRepo: it.ownerRepo,
					number:    it.number,
					isPR:      it.isPR,
					status:    it.status,
					updatedAt: it.updatedAt,
				}
			}
		}
		return nil, itemRef{}
	}

	// The item ID identifies exactly one card: it embeds the project (see
	// itemNodeID in model.go). A content node ID does not — the same issue can
	// sit on two boards at once, which Fabrik explicitly supports (a private
	// engine board alongside a public triage board). So when the caller has an
	// item ID, it decides; matching either ID interchangeably would let the
	// content ID pick the wrong board and backfill its Status.
	var project *projectState
	var found itemRef
	if item.ItemID != "" {
		project, found = search(func(it *itemState) bool { return it.itemID == item.ItemID })
	}
	if project == nil && item.ID != "" {
		// Fall back to the content ID for the webhook case in the doc comment
		// above, where the engine has a node ID and nothing else.
		project, found = search(func(it *itemState) bool { return it.contentNodeID() == item.ID })
	}
	s.mu.Unlock()

	if project == nil {
		return fmt.Errorf("simgh: no project item found for %q", item.ID)
	}

	full, err := s.buildProjectItem(project, found)
	if err != nil {
		return err
	}
	if full == nil {
		return fmt.Errorf("simgh: project item %q has no content", item.ID)
	}
	*item = *full
	return nil
}

// FetchProjectItem returns the board card for an issue, searching every
// seeded project. Returns nil when the issue is not on any board.
func (s *Sim) FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error) {
	ownerRepo := repoKey(owner, repo)

	s.mu.Lock()
	var project *projectState
	var ref itemRef
	for _, key := range sortedKeys(s.projects) {
		p := s.projects[key]
		for _, id := range p.itemOrder {
			it, ok := p.items[id]
			if !ok || it.archived || it.ownerRepo != ownerRepo || it.number != issueNumber {
				continue
			}
			project = p
			ref = itemRef{it.itemID, it.ownerRepo, it.number, it.isPR, it.status, it.updatedAt}
			break
		}
		if project != nil {
			break
		}
	}
	s.mu.Unlock()

	if project == nil {
		return nil, nil
	}
	return s.buildProjectItem(project, ref)
}

// FetchStatusField returns the project's Status single-select field and its
// options, in column order.
func (s *Sim) FetchStatusField(projectID string) (*gh.StatusField, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return nil, err
	}
	opts := make(map[string]string, len(p.statusOptions))
	for k, v := range p.statusOptions {
		opts[k] = v
	}
	return &gh.StatusField{
		FieldID:            p.statusFieldID,
		Options:            opts,
		OrderedOptionNames: cloneStrings(p.orderedStatusNames),
	}, nil
}

// UpdateProjectItemStatus moves a card to a different column. The option ID
// must belong to the project's Status field — passing a stale or foreign one
// is an error here, as it is on GitHub.
func (s *Sim) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return err
	}
	if statusFieldID != p.statusFieldID {
		return fmt.Errorf("simgh: status field %q does not belong to project %q", statusFieldID, projectID)
	}
	it, ok := p.items[itemID]
	if !ok {
		return fmt.Errorf("simgh: project item %q not found in %q", itemID, projectID)
	}
	for name, optID := range p.statusOptions {
		if optID != statusOptionID {
			continue
		}
		now := s.now()
		it.status = name
		it.updatedAt = now
		p.updatedAt = now
		return nil
	}
	return fmt.Errorf("simgh: no status option %q in project %q", statusOptionID, projectID)
}

// ArchiveProjectItem archives a card. Archived cards disappear from board
// fetches and probes but their underlying issue is untouched.
func (s *Sim) ArchiveProjectItem(projectID, itemID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return err
	}
	it, ok := p.items[itemID]
	if !ok {
		return fmt.Errorf("simgh: project item %q not found in %q", itemID, projectID)
	}
	now := s.now()
	it.archived = true
	it.updatedAt = now
	p.updatedAt = now
	return nil
}

// FetchProjectItemStatus returns a single card's column, including an archived
// card's — deliberately unlike the board-scoped reads, which all skip archived
// cards. Production issues this as a direct node(id:) query, and an archived
// ProjectV2Item is still a node with a field value. Neither side of that
// asymmetry is verified against a recorded response; see FIDELITY.md,
// "Archived cards: board reads hide them, a direct ID read does not".
func (s *Sim) FetchProjectItemStatus(itemID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range sortedKeys(s.projects) {
		if it, ok := s.projects[key].items[itemID]; ok {
			return it.status, nil
		}
	}
	return "", fmt.Errorf("simgh: project item %q not found", itemID)
}

// FetchProjectItemStatusBatch returns every live card's column, keyed by item
// ID.
func (s *Sim) FetchProjectItemStatusBatch(projectID string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(p.items))
	for id, it := range p.items {
		if it.archived {
			continue
		}
		out[id] = it.status
	}
	return out, nil
}

// FetchProjectUpdatedAt returns when the project last changed, from the
// injected clock rather than wall time.
func (s *Sim) FetchProjectUpdatedAt(projectID string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return time.Time{}, err
	}
	return p.updatedAt, nil
}

// LookupIssueProjectItem finds an issue's card in a specific project.
// Returns empty strings with no error when the issue is not on the board —
// "not on this board" is an ordinary answer, not a failure.
func (s *Sim) LookupIssueProjectItem(projectID, repo string, issueNumber int) (string, string, error) {
	if _, _, err := splitOwnerRepo(repo); err != nil {
		return "", "", fmt.Errorf("simgh: LookupIssueProjectItem: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return "", "", err
	}
	for _, id := range p.itemOrder {
		it, ok := p.items[id]
		if !ok || it.archived || it.ownerRepo != repo || it.number != issueNumber {
			continue
		}
		return it.itemID, it.status, nil
	}
	return "", "", nil
}

// AddProjectV2ItemById puts an existing issue on a board and returns the new
// card's ID. Adding one already present returns the existing ID, as the
// GraphQL mutation does.
//
// A PR content node ID is rejected rather than accepted: real GitHub allows PR
// cards, but this model cannot project one (see FIDELITY.md), so accepting it
// would add a card that every board read then silently drops.
func (s *Sim) AddProjectV2ItemById(projectID, contentNodeID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return "", err
	}

	ownerRepo, number, err := parseContentNodeID(contentNodeID)
	if err != nil {
		return "", err
	}
	r, ok := s.repos[ownerRepo]
	if !ok {
		return "", fmt.Errorf("simgh: repo %s not seeded", ownerRepo)
	}
	if _, ok := r.issues[number]; !ok {
		if _, ok := r.prs[number]; !ok {
			return "", fmt.Errorf("simgh: no issue or PR %s#%d to add to project", ownerRepo, number)
		}
		// Board projections resolve a card's content as an issue
		// (buildProjectItem, ProbeProjectBoard), so a PR card recorded here
		// would be silently omitted from every subsequent board read. Refuse
		// it, exactly as the seeding path does (placeOnProjectLocked): the
		// seeding and runtime APIs must not disagree about what the model can
		// represent, or a scenario would get a loud failure one way and a
		// silent no-op the other. Recorded in FIDELITY.md as absent.
		return "", fmt.Errorf("simgh: PR cards on a project board are not modelled (%s#%d); see FIDELITY.md", ownerRepo, number)
	}

	id := itemNodeID(p.owner, p.num, ownerRepo, number)
	if existing, ok := p.items[id]; ok {
		// Deliberately does not touch status: re-adding a card that is already
		// on the board does not move it between columns.
		s.reviveCardLocked(p, existing)
		return existing.itemID, nil
	}
	// A card added by this mutation has no Status until one is set — GitHub
	// leaves the field empty rather than defaulting to the first column.
	now := s.now()
	p.items[id] = &itemState{
		itemID:    id,
		ownerRepo: ownerRepo,
		number:    number,
		isPR:      false,
		status:    "",
		updatedAt: now,
	}
	p.itemOrder = append(p.itemOrder, id)
	p.updatedAt = now
	return id, nil
}

// reviveCardLocked brings an existing card back to a live, freshly-touched
// state: un-archived, with both its own and the project's updated-at bumped.
//
// Both re-add paths funnel through here — SeedProjectItem via
// placeOnProjectLocked, and AddProjectV2ItemById — because they had drifted
// into doing complementary halves of the job. Seeding reset status and the
// timestamps but left `archived` set, so moving an archived card to a new
// column left it excluded from every board read with no error; the runtime API
// cleared `archived` but bumped neither timestamp, so a card reappearing on the
// board did not advance FetchProjectUpdatedAt and the engine's poll could miss
// it. Keeping one helper is what stops the two APIs disagreeing again about
// what re-adding a card means.
//
// Caller must hold mu.
func (s *Sim) reviveCardLocked(p *projectState, existing *itemState) {
	now := s.now()
	existing.archived = false
	existing.updatedAt = now
	p.updatedAt = now
}

// parseContentNodeID splits an "issue:owner/repo#N" or "pr:owner/repo#N" ID.
func parseContentNodeID(nodeID string) (ownerRepo string, number int, err error) {
	if ownerRepo, number, err = parseIssueNodeID(nodeID); err == nil {
		return ownerRepo, number, nil
	}
	return parsePRNodeID(nodeID)
}

// projectByIDLocked resolves a project node ID. Caller must hold mu.
func (s *Sim) projectByIDLocked(projectID string) (*projectState, error) {
	for _, key := range sortedKeys(s.projects) {
		if s.projects[key].id == projectID {
			return s.projects[key], nil
		}
	}
	return nil, fmt.Errorf("simgh: project %q not found", projectID)
}

// laterOf returns the later of two times.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
