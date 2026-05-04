package boardcache

import (
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
)

// ReadClient is the subset of engine.GitHubClient covering read-only board/item/PR/check-run state.
// engine.GitHubClient is a strict superset; the concrete gh.Client satisfies this interface.
type ReadClient interface {
	FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error)
	FetchItemDetails(item *gh.ProjectItem) error
	FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error)
	FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error)
	FetchPRMergeable(owner, repo string, prNumber int) (*bool, error)
	FetchPRMergeableState(owner, repo string, prNumber int) (string, error)
	FetchLabels(owner, repo string, issueNumber int) ([]string, error)
	FetchStatusField(projectID string) (*gh.StatusField, error)
	FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error)
	FetchPRsForSHA(owner, repo, sha string) ([]int, error)
	RateLimitStats() (rest, graphql gh.RateLimitStats)
}

// GitHubAdapter wraps a ReadClient with pass-through implementations.
// When --board-cache=none, the engine sets e.readClient = NewGitHubAdapter(e.client).
type GitHubAdapter struct {
	client ReadClient
}

// NewGitHubAdapter wraps any ReadClient as a pass-through GitHubAdapter.
func NewGitHubAdapter(client ReadClient) *GitHubAdapter {
	return &GitHubAdapter{client: client}
}

func (a *GitHubAdapter) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return a.client.FetchProjectBoard(owner, repo, projectNum, ownerType)
}

func (a *GitHubAdapter) FetchItemDetails(item *gh.ProjectItem) error {
	return a.client.FetchItemDetails(item)
}

func (a *GitHubAdapter) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	return a.client.FetchCheckRuns(owner, repo, sha)
}

func (a *GitHubAdapter) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	return a.client.FetchLinkedPR(owner, repo, issueNumber)
}

func (a *GitHubAdapter) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return a.client.FetchPRMergeable(owner, repo, prNumber)
}

func (a *GitHubAdapter) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	return a.client.FetchPRMergeableState(owner, repo, prNumber)
}

func (a *GitHubAdapter) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	return a.client.FetchLabels(owner, repo, issueNumber)
}

func (a *GitHubAdapter) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return a.client.FetchStatusField(projectID)
}

func (a *GitHubAdapter) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	return a.client.FetchPRClosingIssues(owner, repo, prNumber)
}

func (a *GitHubAdapter) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	return a.client.FetchPRsForSHA(owner, repo, sha)
}

func (a *GitHubAdapter) RateLimitStats() (rest, graphql gh.RateLimitStats) {
	return a.client.RateLimitStats()
}

// ItemKey returns the cache key for an issue item: "owner/repo#number".
// Exported so callers in other packages (e.g. engine) can construct keys
// without duplicating the format string.
func ItemKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// itemKey is the package-internal alias for ItemKey.
func itemKey(repo string, number int) string {
	return ItemKey(repo, number)
}

// prKey returns the cache key for a PR: "owner/repo#pr<prNumber>".
func prKey(repo string, prNumber int) string {
	return repo + "#pr" + strconv.Itoa(prNumber)
}

// missKey returns the negative-cache key for a PR number: "miss:owner/repo#prN".
func missKey(repo string, prNumber int) string {
	return "miss:" + repo + "#" + strconv.Itoa(prNumber)
}

// missKeyForSHA returns the negative-cache key for a commit SHA: "miss:sha:SHA".
func missKeyForSHA(sha string) string {
	return "miss:sha:" + sha
}

const recentMissTTL = 10 * time.Minute

// parseItemKey parses "owner/repo#N" into (repo, number, true).
// Returns ("", 0, false) on invalid input.
func parseItemKey(key string) (repo string, number int, ok bool) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return "", 0, false
	}
	repo = key[:idx]
	numStr := key[idx+1:]
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return "", 0, false
	}
	return repo, n, true
}

// snapshotToProjectItem converts a Store Snapshot to a gh.ProjectItem.
func snapshotToProjectItem(snap itemstate.Snapshot) gh.ProjectItem {
	s := snap.State()
	pi := gh.ProjectItem{
		ID:        s.ID,
		ItemID:    s.ItemID,
		Number:    s.Number,
		Title:     s.Title,
		Body:      s.Body,
		Status:    s.Status,
		URL:       s.URL,
		Repo:      s.Repo,
		IsPR:      s.IsPR,
		IsClosed:  s.IsClosed,
		UpdatedAt: s.UpdatedAt,
		Labels:    s.Labels,
		Assignees: s.Assignees,
		Comments:  s.Comments,
		Author:    s.Author,
		BlockedBy: s.BlockedBy,
	}
	if s.LinkedPR != nil {
		pi.LinkedPRNumber = s.LinkedPR.Number
		pi.LinkedPRReviews = s.LinkedPR.Reviews
		pi.LinkedPRReviewRequests = s.LinkedPR.ReviewRequests
		pi.LinkedPRReviewThreadComments = s.LinkedPR.ThreadComments
		pi.LinkedPRResolvedThreadCount = s.LinkedPR.ResolvedThreadCount
	}
	return pi
}

// CacheImpl is a goroutine-safe in-memory board cache. Webhook delta functions write
// to it; the poll loop reads from it via the ReadClient interface. Falls back to a
// fallback ReadClient on cache miss.
//
// Internal ownership split:
//   - store owns: items, shaToKey, itemIDToKey
//   - CacheImpl owns: paused, recentMissCache, projectID/Title/OwnerType,
//     linkedPRs, checkRuns, prNumToKey, localDeltaAt
//
// Locking invariant: mu guards CacheImpl-local fields only. Store has its own
// internal mutex. NEVER hold mu while calling any Store method — this prevents
// deadlock if Store observers call back into CacheImpl.
type CacheImpl struct {
	// mu guards CacheImpl-local fields only. Never held during Store calls.
	mu sync.RWMutex

	// Board metadata (from last Bootstrap/Reconcile).
	projectID        string
	projectTitle     string
	projectOwnerType string

	// paused is a stream-health control flag. When true, ApplyDelta is a no-op.
	paused bool

	// recentMissCache is a negative cache preventing repeated REST lookups.
	// Keys are "miss:owner/repo#prN" or "miss:sha:SHA".
	recentMissCache map[string]time.Time

	// checkRuns stores check runs keyed by commit SHA. Stays on CacheImpl because
	// the Store silently drops CheckRunCompleted for SHAs not yet linked to any
	// item. Pre-linkage runs are kept here and also forwarded to Store once linkage
	// is resolved.
	checkRuns map[string][]gh.CheckRun

	// linkedPRs stores full PR details keyed by prKey(repo, prNum).
	// TODO(phase3-x): migrate to Store when LinkedPRState gains Title/State/Merged/Draft.
	linkedPRs map[string]*gh.PRDetails

	// prNumToKey maps prKey(repo, prNum) → issue itemKey.
	// Replaces the linear scan of Store items for PR-number → issue lookups in delta handlers.
	prNumToKey map[string]string

	// localDeltaAt records the last time a webhook bumped an item.
	// FetchProjectBoard uses max(ItemState.UpdatedAt, localDeltaAt[key]) so that
	// webhook-driven changes are visible to itemMayNeedWork before the next Reconcile.
	localDeltaAt map[string]time.Time

	// store owns per-item state (items, shaToKey, itemIDToKey).
	store *itemstate.Store

	fallback ReadClient
	logFn    func(format string, args ...any)
}

// NewCacheImpl creates an empty cache backed by fallback for misses.
func NewCacheImpl(fallback ReadClient, logFn func(format string, args ...any)) *CacheImpl {
	return &CacheImpl{
		checkRuns:       make(map[string][]gh.CheckRun),
		linkedPRs:       make(map[string]*gh.PRDetails),
		prNumToKey:      make(map[string]string),
		localDeltaAt:    make(map[string]time.Time),
		recentMissCache: make(map[string]time.Time),
		store:           itemstate.NewStore(nil),
		fallback:        fallback,
		logFn:           logFn,
	}
}

// Bootstrap populates the cache wholesale from a freshly-fetched board.
// Called once at engine startup before the first dispatch cycle.
func (c *CacheImpl) Bootstrap(board *gh.ProjectBoard) {
	// Reset Store atomically — clears all prior item state and indexes.
	c.store.Reset(board.Items)

	c.mu.Lock()
	c.projectID = board.ProjectID
	c.projectTitle = board.Title
	c.projectOwnerType = board.OwnerType
	// Reset CacheImpl-local maps so they're consistent with the fresh Store state.
	c.prNumToKey = make(map[string]string)
	c.localDeltaAt = make(map[string]time.Time)
	c.linkedPRs = make(map[string]*gh.PRDetails)
	c.checkRuns = make(map[string][]gh.CheckRun)
	c.mu.Unlock()

	c.logFn("[cache] bootstrap complete: %d items\n", len(board.Items))
}

// Reconcile replaces shallow board state from a fresh board fetch.
// Preserves deep fields (Comments, LinkedPRReviews, etc.) for items that have
// already been deep-fetched. Logs the drift count when items differ.
// Shallow drift in linkage (LinkedPRNumber) invalidates deep cache for the
// affected key, forcing a fresh FetchItemDetails on next access.
func (c *CacheImpl) Reconcile(board *gh.ProjectBoard) {
	newKeys := make(map[string]bool, len(board.Items))
	drifted := 0

	for i := range board.Items {
		pi := &board.Items[i]
		key := itemKey(pi.Repo, pi.Number)
		newKeys[key] = true

		snap, err := c.store.Get(pi.Repo, pi.Number)
		if err != nil {
			// New item not in Store — add it fully.
			drifted++
			c.store.Apply(itemstate.IssueOpened{Item: *pi})
			continue
		}

		s := snap.State()

		// Detect shallow drift to count items that differ.
		if s.Status != pi.Status ||
			len(s.Labels) != len(pi.Labels) ||
			!s.UpdatedAt.Equal(pi.UpdatedAt) {
			drifted++
		}

		// Apply shallow update to Store (preserves deep fields).
		c.store.Apply(itemstate.ShallowBoardItemUpdated{
			Repo:   pi.Repo,
			Number: pi.Number,
			Item:   *pi,
		})

		// Detect linkage drift: deep cache's LinkedPRNumber ≠ fresh shallow board value.
		if !s.LastDeepFetchAt.IsZero() {
			var deepPRNum int
			if s.LinkedPR != nil {
				deepPRNum = s.LinkedPR.Number
			}
			shallowPRNum := pi.LinkedPRNumberShallow
			if deepPRNum != shallowPRNum {
				c.store.Apply(itemstate.DeepFetchInvalidated{Repo: pi.Repo, Number: pi.Number})
				c.logFn("[cache] linkage drift detected for issue #%d: stale linked PR=%d, fresh=%d — invalidating deep cache\n",
					pi.Number, deepPRNum, shallowPRNum)
			}
		}
	}

	// Remove items no longer on the board.
	for _, snap := range c.store.All() {
		key := itemKey(snap.Repo(), snap.Number())
		if !newKeys[key] {
			drifted++
			c.store.Remove(snap.Repo(), snap.Number())
			// Clean up CacheImpl-side prNumToKey so stale PR numbers don't resurrect ghost items.
			if lpr := snap.LinkedPR(); lpr != nil && lpr.Number != 0 {
				pk := prKey(snap.Repo(), lpr.Number)
				c.mu.Lock()
				delete(c.prNumToKey, pk)
				c.mu.Unlock()
			}
		}
	}

	c.mu.Lock()
	c.projectID = board.ProjectID
	c.projectTitle = board.Title
	c.projectOwnerType = board.OwnerType
	c.mu.Unlock()

	if drifted > 0 {
		c.logFn("[reconciliation] %d items differed\n", drifted)
	}
}

// resolvePRLinkage looks up which cached issue is closed by the given PR by
// fetching the PR body via REST and parsing closing keywords. Must be called
// without c.mu held (the REST call is a network operation). Returns the cache
// key and issue number of the first closing issue found in the Store. On a
// transient REST error the error is returned and callers should NOT record a
// negative-miss entry (a retry on the next webhook may succeed). Returns
// ("", 0, false, nil) when the PR body has no recognized closing reference or
// none of the referenced issues are in this cache.
func (c *CacheImpl) resolvePRLinkage(owner, repo string, prNumber int) (key string, issueNumber int, found bool, err error) {
	fullRepo := owner + "/" + repo
	issues, err := c.fallback.FetchPRClosingIssues(owner, repo, prNumber)
	if err != nil {
		c.logFn("[cache] resolvePRLinkage: fetch closing issues for PR #%d: %v\n", prNumber, err)
		return "", 0, false, err
	}
	if len(issues) == 0 {
		return "", 0, false, nil
	}

	// Store has nil fallback — Get returns ErrNotFound on miss without calling GitHub.
	for _, issNum := range issues {
		k := itemKey(fullRepo, issNum)
		if _, storeErr := c.store.Get(fullRepo, issNum); storeErr == nil {
			return k, issNum, true, nil
		}
	}
	return "", 0, false, nil
}

// Pause stops delta application (called on WebhookStreamUnhealthy transition).
func (c *CacheImpl) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
}

// Resume re-enables delta application (called after reconciliation on stream recovery).
func (c *CacheImpl) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
}

// IsPaused returns true when delta application is paused.
func (c *CacheImpl) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// FetchProjectBoard returns a *gh.ProjectBoard reconstructed from the Store.
// Falls back to GitHub when the cache has not been bootstrapped or is paused.
func (c *CacheImpl) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	// Check paused first; Store.All is called outside any c.mu hold.
	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	snaps := c.store.All()

	if len(snaps) == 0 || paused {
		if len(snaps) == 0 {
			c.logFn("[cache] miss: FetchProjectBoard not yet bootstrapped — fetching from GitHub\n")
		}
		return c.fallback.FetchProjectBoard(owner, repo, projectNum, ownerType)
	}

	c.mu.RLock()
	projectID := c.projectID
	projectTitle := c.projectTitle
	projectOwnerType := c.projectOwnerType
	// Snapshot localDeltaAt for consistent max(UpdatedAt, deltaAt) computation.
	localDeltaAtCopy := make(map[string]time.Time, len(c.localDeltaAt))
	for k, v := range c.localDeltaAt {
		localDeltaAtCopy[k] = v
	}
	c.mu.RUnlock()

	items := make([]gh.ProjectItem, 0, len(snaps))
	for _, snap := range snaps {
		pi := snapshotToProjectItem(snap)
		// Override UpdatedAt if a webhook has bumped it more recently than GitHub's timestamp.
		key := itemKey(snap.Repo(), snap.Number())
		if t, ok := localDeltaAtCopy[key]; ok && t.After(pi.UpdatedAt) {
			pi.UpdatedAt = t
		}
		items = append(items, pi)
	}

	return &gh.ProjectBoard{
		ProjectID: projectID,
		Title:     projectTitle,
		OwnerType: projectOwnerType,
		Items:     items,
	}, nil
}

// FetchItemDetails copies cached deep fields into the passed item pointer.
// Deep fields: Body, URL, Author, Assignees, BlockedBy, Comments, LinkedPRNumber,
// LinkedPRReviewRequests, LinkedPRReviews, LinkedPRReviewThreadComments, LinkedPRResolvedThreadCount.
// Falls back to GitHub on cache miss and populates the cache with the result.
func (c *CacheImpl) FetchItemDetails(item *gh.ProjectItem) error {
	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	if paused {
		return c.fallback.FetchItemDetails(item)
	}

	snap, err := c.store.Get(item.Repo, item.Number)
	if err == nil {
		s := snap.State()
		if !s.LastDeepFetchAt.IsZero() {
			// Cache hit — copy deep fields into item.
			copyDeepFieldsFromState(item, s)
			return nil
		}
	}

	c.logFn("[cache] miss for #%d — fetching from GitHub\n", item.Number)
	if err := c.fallback.FetchItemDetails(item); err != nil {
		return err
	}

	// Write back to Store via ItemDeepFetched.
	c.store.Apply(itemstate.ItemDeepFetched{
		Repo:       item.Repo,
		Number:     item.Number,
		FreshState: *item,
	})
	// If FetchItemDetails learned a LinkedPRNumber, backfill prNumToKey so that
	// future PR review/comment deltas can route via the normal (non-auto-heal) path.
	if item.LinkedPRNumber != 0 {
		pk := prKey(item.Repo, item.LinkedPRNumber)
		issKey := itemKey(item.Repo, item.Number)
		c.mu.Lock()
		if _, exists := c.prNumToKey[pk]; !exists {
			c.prNumToKey[pk] = issKey
		}
		c.mu.Unlock()
	}
	return nil
}

// FetchCheckRuns returns cached check runs for a SHA; falls back to GitHub on miss.
func (c *CacheImpl) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	c.mu.RLock()
	if c.paused {
		c.mu.RUnlock()
		return c.fallback.FetchCheckRuns(owner, repo, sha)
	}
	if runs, ok := c.checkRuns[sha]; ok {
		result := make([]gh.CheckRun, len(runs))
		copy(result, runs)
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	c.logFn("[cache] miss: FetchCheckRuns sha=%s — fetching from GitHub\n", sha)
	runs, err := c.fallback.FetchCheckRuns(owner, repo, sha)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.checkRuns[sha] = runs
	c.mu.Unlock()
	return runs, nil
}

// FetchLinkedPR returns cached PR details for an issue; falls back to GitHub on miss.
func (c *CacheImpl) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	fullRepo := owner + "/" + repo
	issKey := itemKey(fullRepo, issueNumber)

	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	if paused {
		return c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	}

	// Get LinkedPRNumber from Store (Store has its own mutex; no c.mu held).
	var linkedPRNum int
	snap, snapErr := c.store.Get(fullRepo, issueNumber)
	if snapErr == nil {
		if lpr := snap.LinkedPR(); lpr != nil {
			linkedPRNum = lpr.Number
		}
	}

	if linkedPRNum != 0 {
		pk := prKey(fullRepo, linkedPRNum)
		c.mu.RLock()
		if pr, ok := c.linkedPRs[pk]; ok {
			prCopy := *pr
			c.mu.RUnlock()
			return &prCopy, nil
		}
		c.mu.RUnlock()
	}

	c.logFn("[cache] miss: FetchLinkedPR #%d — fetching from GitHub\n", issueNumber)
	pr, err := c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	if pr != nil {
		pk := prKey(fullRepo, pr.Number)
		c.mu.Lock()
		c.linkedPRs[pk] = pr
		c.mu.Unlock()
		// If the item had no LinkedPRNumber in Store, update it without touching deep-fetch state.
		// Original behavior: only set LinkedPRNumber; do not change LastDeepFetchAt.
		if linkedPRNum == 0 && snapErr == nil {
			// Preserve existing HeadSHA if any (LinkedPR may already have a SHA from a webhook).
			existingSHA := ""
			if s := snap.State(); s.LinkedPR != nil {
				existingSHA = s.LinkedPR.HeadSHA
			}
			c.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:        fullRepo,
				Number:      issueNumber,
				LinkedPRNum: pr.Number,
				SHA:         existingSHA,
			})
			// Also update prNumToKey.
			pk2 := prKey(fullRepo, pr.Number)
			c.mu.Lock()
			c.prNumToKey[pk2] = issKey
			c.mu.Unlock()
		}
	}
	return pr, nil
}

// FetchPRMergeable always delegates to GitHub — mergeability changes without webhooks.
func (c *CacheImpl) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return c.fallback.FetchPRMergeable(owner, repo, prNumber)
}

// FetchPRMergeableState always delegates to GitHub — mergeability changes without webhooks.
func (c *CacheImpl) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	return c.fallback.FetchPRMergeableState(owner, repo, prNumber)
}

// FetchLabels returns the cached label list for an issue; falls back to GitHub on miss.
func (c *CacheImpl) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	fullRepo := owner + "/" + repo

	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	if paused {
		return c.fallback.FetchLabels(owner, repo, issueNumber)
	}

	snap, err := c.store.Get(fullRepo, issueNumber)
	if err == nil {
		return cloneStrings(snap.Labels()), nil
	}

	c.logFn("[cache] miss: FetchLabels #%d — fetching from GitHub\n", issueNumber)
	return c.fallback.FetchLabels(owner, repo, issueNumber)
}

// FetchStatusField always delegates to GitHub — project metadata, not board-item state.
func (c *CacheImpl) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return c.fallback.FetchStatusField(projectID)
}

// FetchPRClosingIssues always delegates to GitHub — used by the auto-heal path in delta handlers.
func (c *CacheImpl) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	return c.fallback.FetchPRClosingIssues(owner, repo, prNumber)
}

// FetchPRsForSHA always delegates to GitHub — used by the auto-heal path in applyCheckRunCompleted.
func (c *CacheImpl) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	return c.fallback.FetchPRsForSHA(owner, repo, sha)
}

// RateLimitStats always delegates to GitHub.
func (c *CacheImpl) RateLimitStats() (rest, graphql gh.RateLimitStats) {
	return c.fallback.RateLimitStats()
}

// UpdateItemStatus updates the Status field for the item identified by key.
// No-op when the key is not in the cache. Safe for concurrent use.
func (c *CacheImpl) UpdateItemStatus(key, newStatus string) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] UpdateItemStatus: key %q not found — no-op\n", key)
		return
	}
	_, changes, _ := c.store.Apply(itemstate.LocalStatusUpdated{
		Repo:      repo,
		Number:    number,
		NewStatus: newStatus,
	})
	if len(changes) == 0 {
		// Item not found in Store or status unchanged.
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.localDeltaAt[key] = now
	c.mu.Unlock()
}

// ApplyStatusBatch updates Status for items identified by project-item node IDs.
// Entries whose itemID is not in the Store's itemIDToKey index are silently skipped.
// Safe for concurrent use.
func (c *CacheImpl) ApplyStatusBatch(updates map[string]string) {
	for itemID, status := range updates {
		snap, changes, _ := c.store.Apply(itemstate.ProjectV2ItemEdited{
			ItemID:    itemID,
			NewStatus: status,
		})
		if len(changes) == 0 {
			continue
		}
		key := itemKey(snap.Repo(), snap.Number())
		now := time.Now()
		c.mu.Lock()
		c.localDeltaAt[key] = now
		c.mu.Unlock()
	}
}

// GetItemID returns the project-item node ID (PVTI_...) for the given cache key.
// Returns ("", false) when the key is not present or has no ItemID.
func (c *CacheImpl) GetItemID(key string) (string, bool) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		return "", false
	}
	snap, err := c.store.Get(repo, number)
	if err != nil {
		return "", false
	}
	s := snap.State()
	if s.ItemID == "" {
		return "", false
	}
	return s.ItemID, true
}

// ProjectID returns the project node ID stored from the last Bootstrap/Reconcile call.
// Returns "" when the cache has not yet been bootstrapped.
func (c *CacheImpl) ProjectID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.projectID
}

// copyDeepFieldsFromState overlays deep fields from an ItemState onto a *gh.ProjectItem.
// Shallow fields (Labels, Status, Title, UpdatedAt) are left unchanged in dst.
func copyDeepFieldsFromState(dst *gh.ProjectItem, s itemstate.ItemState) {
	dst.Body = s.Body
	dst.URL = s.URL
	dst.Author = s.Author
	dst.Assignees = cloneStrings(s.Assignees)
	dst.BlockedBy = cloneDependencies(s.BlockedBy)
	dst.Comments = cloneComments(s.Comments)
	if s.LinkedPR != nil {
		dst.LinkedPRNumber = s.LinkedPR.Number
		dst.LinkedPRReviews = clonePRReviews(s.LinkedPR.Reviews)
		dst.LinkedPRReviewRequests = cloneReviewRequests(s.LinkedPR.ReviewRequests)
		dst.LinkedPRReviewThreadComments = cloneComments(s.LinkedPR.ThreadComments)
		dst.LinkedPRResolvedThreadCount = s.LinkedPR.ResolvedThreadCount
	}
}

// copyDeepFields overlays the deep fields from src onto dst.
// Shallow fields (Labels, Status, Title, UpdatedAt) are left unchanged in dst.
func copyDeepFields(dst, src *gh.ProjectItem) {
	dst.Body = src.Body
	dst.URL = src.URL
	dst.Author = src.Author
	dst.Assignees = cloneStrings(src.Assignees)
	dst.BlockedBy = cloneDependencies(src.BlockedBy)
	dst.Comments = cloneComments(src.Comments)
	dst.LinkedPRNumber = src.LinkedPRNumber
	dst.LinkedPRReviewRequests = cloneReviewRequests(src.LinkedPRReviewRequests)
	dst.LinkedPRReviews = clonePRReviews(src.LinkedPRReviews)
	dst.LinkedPRReviewThreadComments = cloneComments(src.LinkedPRReviewThreadComments)
	dst.LinkedPRResolvedThreadCount = src.LinkedPRResolvedThreadCount
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneComments(s []gh.Comment) []gh.Comment {
	if s == nil {
		return nil
	}
	out := make([]gh.Comment, len(s))
	copy(out, s)
	return out
}

func cloneDependencies(s []gh.Dependency) []gh.Dependency {
	if s == nil {
		return nil
	}
	out := make([]gh.Dependency, len(s))
	copy(out, s)
	return out
}

func cloneReviewRequests(s []gh.ReviewRequest) []gh.ReviewRequest {
	if s == nil {
		return nil
	}
	out := make([]gh.ReviewRequest, len(s))
	copy(out, s)
	return out
}

func clonePRReviews(s []gh.PRReview) []gh.PRReview {
	if s == nil {
		return nil
	}
	out := make([]gh.PRReview, len(s))
	copy(out, s)
	return out
}
