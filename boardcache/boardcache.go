package boardcache

import (
	"strconv"
	"sync"

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

func (a *GitHubAdapter) RateLimitStats() (rest, graphql gh.RateLimitStats) {
	return a.client.RateLimitStats()
}

// itemKey returns the cache key for an issue item: "owner/repo#number".
func itemKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// prKey returns the cache key for a PR: "owner/repo#pr<prNumber>".
func prKey(repo string, prNumber int) string {
	return repo + "#pr" + strconv.Itoa(prNumber)
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

	// Fallback for cache misses
	fallback ReadClient
	logFn    func(format string, args ...any)
}

// NewCacheImpl creates an empty cache backed by fallback for misses.
func NewCacheImpl(fallback ReadClient, logFn func(format string, args ...any)) *CacheImpl {
	return &CacheImpl{
		items:       make(map[string]*gh.ProjectItem),
		deepFetched: make(map[string]bool),
		checkRuns:   make(map[string][]gh.CheckRun),
		linkedPRs:   make(map[string]*gh.PRDetails),
		shaToKey:    make(map[string]string),
		itemIDToKey: make(map[string]string),
		fallback:    fallback,
		logFn:       logFn,
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
// Falls back to GitHub when the cache has not been bootstrapped.
func (c *CacheImpl) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	c.mu.RLock()

	if len(c.items) == 0 {
		c.mu.RUnlock()
		c.logFn("[cache] miss: FetchProjectBoard not yet bootstrapped — fetching from GitHub\n")
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
		mergeDeepFields(cached, item)
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

// RateLimitStats always delegates to GitHub.
func (c *CacheImpl) RateLimitStats() (rest, graphql gh.RateLimitStats) {
	return c.fallback.RateLimitStats()
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

// mergeDeepFields copies deep fields from src into dst (for cache population after fallback).
func mergeDeepFields(dst, src *gh.ProjectItem) {
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
