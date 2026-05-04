package boardcache

import (
	"strconv"
	"sync"
	"time"

	gh "github.com/verveguy/fabrik/github"
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

// resolvePRLinkage looks up which cached issue is closed by the given PR by
// fetching the PR body via REST and parsing closing keywords. Must be called
// without c.mu held (the REST call is a network operation). Returns the cache
// key and issue number of the first closing issue found in c.items, or
// ("", 0, false) when no match is found.
func (c *CacheImpl) resolvePRLinkage(owner, repo string, prNumber int) (key string, issueNumber int, found bool) {
	fullRepo := owner + "/" + repo
	issues, err := c.fallback.FetchPRClosingIssues(owner, repo, prNumber)
	if err != nil {
		c.logFn("[cache] resolvePRLinkage: fetch closing issues for PR #%d: %v\n", prNumber, err)
		return "", 0, false
	}
	if len(issues) == 0 {
		return "", 0, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, issNum := range issues {
		k := itemKey(fullRepo, issNum)
		if _, ok := c.items[k]; ok {
			return k, issNum, true
		}
	}
	return "", 0, false
}

// CacheImpl is a goroutine-safe in-memory board cache. Webhook delta functions write
// to it; the poll loop reads from it via the ReadClient interface. Falls back to a
// fallback ReadClient on cache miss.
type CacheImpl struct {
	mu sync.RWMutex

	// Board items: key = owner/repo#issueNumber
	items      map[string]*gh.ProjectItem
	deepFetched map[string]bool // set of keys that have had FetchItemDetails called

	// Check runs: key = commit SHA
	checkRuns map[string][]gh.CheckRun

	// PR details: key = owner/repo#pr<prNumber>
	linkedPRs map[string]*gh.PRDetails

	// Reverse lookups
	shaToKey    map[string]string // SHA → issueKey (owner/repo#number)
	itemIDToKey map[string]string // ItemID → issueKey

	// Board metadata (from last Bootstrap/Reconcile)
	projectID       string
	projectTitle    string
	projectOwnerType string

	// Stream health: when paused, ApplyDelta is a no-op
	paused bool

	// Negative cache: keys are "miss:owner/repo#prN" or "miss:sha:SHA".
	// Entries with TTL > 10 minutes are pruned lazily on access.
	recentMissCache map[string]time.Time

	// Fallback for cache misses
	fallback ReadClient
	logFn    func(format string, args ...any)
}

// NewCacheImpl creates an empty cache backed by fallback for misses.
func NewCacheImpl(fallback ReadClient, logFn func(format string, args ...any)) *CacheImpl {
	return &CacheImpl{
		items:           make(map[string]*gh.ProjectItem),
		deepFetched:     make(map[string]bool),
		checkRuns:       make(map[string][]gh.CheckRun),
		linkedPRs:       make(map[string]*gh.PRDetails),
		shaToKey:        make(map[string]string),
		itemIDToKey:     make(map[string]string),
		recentMissCache: make(map[string]time.Time),
		fallback:        fallback,
		logFn:           logFn,
	}
}

// Bootstrap populates the cache wholesale from a freshly-fetched board.
// Called once at engine startup before the first dispatch cycle.
func (c *CacheImpl) Bootstrap(board *gh.ProjectBoard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.populateFromBoard(board)
	c.logFn("[cache] bootstrap complete: %d items\n", len(board.Items))
}

// Reconcile replaces shallow board state from a fresh board fetch.
// Preserves deep fields (Comments, LinkedPRReviews, etc.) for items that have
// already been deep-fetched. Logs the drift count when items differ.
// Shallow drift in linkage (LinkedPRNumber) invalidates deep cache for the
// affected key, forcing a fresh FetchItemDetails on next access.
func (c *CacheImpl) Reconcile(board *gh.ProjectBoard) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newKeys := make(map[string]bool, len(board.Items))
	drifted := 0

	for i := range board.Items {
		item := &board.Items[i]
		key := itemKey(item.Repo, item.Number)
		newKeys[key] = true

		if existing, ok := c.items[key]; ok {
			if existing.Status != item.Status ||
				len(existing.Labels) != len(item.Labels) ||
				existing.UpdatedAt != item.UpdatedAt {
				drifted++
			}
			// Update shallow fields; preserve deep fields from cache.
			existing.Status = item.Status
			existing.Labels = cloneStrings(item.Labels)
			existing.Title = item.Title
			existing.UpdatedAt = item.UpdatedAt
			existing.IsClosed = item.IsClosed
			existing.IsPR = item.IsPR
			existing.ItemID = item.ItemID
			existing.URL = item.URL
			// Update the ItemID reverse lookup.
			if item.ItemID != "" {
				c.itemIDToKey[item.ItemID] = key
			}
			// Detect linkage drift: if deep cache says LinkedPRNumber=X but the
			// fresh shallow board shows a different value, the cached deep state was
			// captured before the issue↔PR linkage appeared (or changed) on GitHub.
			// Invalidate deepFetched so the next FetchItemDetails re-fetches from GitHub.
			if c.deepFetched[key] && existing.LinkedPRNumber != item.LinkedPRNumberShallow {
				delete(c.deepFetched, key)
				c.logFn("[cache] linkage drift detected for issue #%d: stale linked PR=%d, fresh=%d — invalidating deep cache\n",
					item.Number, existing.LinkedPRNumber, item.LinkedPRNumberShallow)
			}
		} else {
			drifted++
			cp := *item
			c.items[key] = &cp
			if item.ItemID != "" {
				c.itemIDToKey[item.ItemID] = key
			}
		}
	}

	// Remove items that are no longer on the board.
	for key := range c.items {
		if !newKeys[key] {
			drifted++
			delete(c.items, key)
			delete(c.deepFetched, key)
		}
	}

	c.projectID = board.ProjectID
	c.projectTitle = board.Title
	c.projectOwnerType = board.OwnerType

	if drifted > 0 {
		c.logFn("[reconciliation] %d items differed\n", drifted)
	}
}

// populateFromBoard replaces items map entirely from a board fetch.
// Must be called with c.mu held (write).
func (c *CacheImpl) populateFromBoard(board *gh.ProjectBoard) {
	c.projectID = board.ProjectID
	c.projectTitle = board.Title
	c.projectOwnerType = board.OwnerType

	c.items = make(map[string]*gh.ProjectItem, len(board.Items))
	c.itemIDToKey = make(map[string]string, len(board.Items))
	c.deepFetched = make(map[string]bool)

	for i := range board.Items {
		item := board.Items[i]
		key := itemKey(item.Repo, item.Number)
		cp := item
		c.items[key] = &cp
		if item.ItemID != "" {
			c.itemIDToKey[item.ItemID] = key
		}
	}
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

// FetchProjectBoard returns a *gh.ProjectBoard reconstructed from the items map.
// Falls back to GitHub when the cache has not been bootstrapped or is paused.
func (c *CacheImpl) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	c.mu.RLock()

	if len(c.items) == 0 || c.paused {
		c.mu.RUnlock()
		if len(c.items) == 0 {
			c.logFn("[cache] miss: FetchProjectBoard not yet bootstrapped — fetching from GitHub\n")
		}
		return c.fallback.FetchProjectBoard(owner, repo, projectNum, ownerType)
	}

	items := make([]gh.ProjectItem, 0, len(c.items))
	for _, item := range c.items {
		items = append(items, *item)
	}
	board := &gh.ProjectBoard{
		ProjectID: c.projectID,
		Title:     c.projectTitle,
		OwnerType: c.projectOwnerType,
		Items:     items,
	}
	c.mu.RUnlock()
	return board, nil
}

// FetchItemDetails copies cached deep fields into the passed item pointer.
// Deep fields: Body, URL, Author, Assignees, BlockedBy, Comments, LinkedPRNumber,
// LinkedPRReviewRequests, LinkedPRReviews, LinkedPRReviewThreadComments, LinkedPRResolvedThreadCount.
// Falls back to GitHub on cache miss and populates the cache with the result.
func (c *CacheImpl) FetchItemDetails(item *gh.ProjectItem) error {
	key := itemKey(item.Repo, item.Number)

	c.mu.RLock()
	if c.paused {
		c.mu.RUnlock()
		return c.fallback.FetchItemDetails(item)
	}
	cached, ok := c.items[key]
	deepFetched := c.deepFetched[key]
	if ok && deepFetched {
		copyDeepFields(item, cached)
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	c.logFn("[cache] miss for #%d — fetching from GitHub\n", item.Number)
	if err := c.fallback.FetchItemDetails(item); err != nil {
		return err
	}

	// Store the deep fields in the cache.
	c.mu.Lock()
	if cached, ok := c.items[key]; ok {
		copyDeepFields(cached, item)
	} else {
		cp := *item
		c.items[key] = &cp
	}
	c.deepFetched[key] = true
	c.mu.Unlock()
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
	issKey := itemKey(owner+"/"+repo, issueNumber)

	c.mu.RLock()
	if c.paused {
		c.mu.RUnlock()
		return c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	}
	item, itemOK := c.items[issKey]
	if itemOK && item.LinkedPRNumber != 0 {
		pk := prKey(owner+"/"+repo, item.LinkedPRNumber)
		if pr, ok := c.linkedPRs[pk]; ok {
			prCopy := *pr
			c.mu.RUnlock()
			return &prCopy, nil
		}
	}
	c.mu.RUnlock()

	c.logFn("[cache] miss: FetchLinkedPR #%d — fetching from GitHub\n", issueNumber)
	pr, err := c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	if pr != nil {
		pk := prKey(owner+"/"+repo, pr.Number)
		c.mu.Lock()
		c.linkedPRs[pk] = pr
		// Update item's LinkedPRNumber if not set.
		if item2, ok := c.items[issKey]; ok && item2.LinkedPRNumber == 0 {
			item2.LinkedPRNumber = pr.Number
		}
		c.mu.Unlock()
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
	key := itemKey(owner+"/"+repo, issueNumber)

	c.mu.RLock()
	if c.paused {
		c.mu.RUnlock()
		return c.fallback.FetchLabels(owner, repo, issueNumber)
	}
	if item, ok := c.items[key]; ok {
		labels := cloneStrings(item.Labels)
		c.mu.RUnlock()
		return labels, nil
	}
	c.mu.RUnlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		c.logFn("[cache] UpdateItemStatus: key %q not found — no-op\n", key)
		return
	}
	item.Status = newStatus
	item.UpdatedAt = time.Now()
}

// ApplyStatusBatch updates Status for items identified by project-item node IDs.
// Entries whose itemID is not in itemIDToKey are silently skipped (not yet bootstrapped).
// Safe for concurrent use.
func (c *CacheImpl) ApplyStatusBatch(updates map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for itemID, status := range updates {
		key, ok := c.itemIDToKey[itemID]
		if !ok {
			continue
		}
		item, ok := c.items[key]
		if !ok {
			continue
		}
		if item.Status != status {
			item.Status = status
			item.UpdatedAt = time.Now()
		}
	}
}

// GetItemID returns the project-item node ID (PVTI_...) for the given cache key.
// Returns ("", false) when the key is not present or has no ItemID.
func (c *CacheImpl) GetItemID(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[key]
	if !ok || item.ItemID == "" {
		return "", false
	}
	return item.ItemID, true
}

// ProjectID returns the project node ID stored from the last Bootstrap/Reconcile call.
// Returns "" when the cache has not yet been bootstrapped.
func (c *CacheImpl) ProjectID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.projectID
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
