package boardcache

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// ReadClient is the subset of engine.GitHubClient covering read-only board/item/PR/check-run state.
// engine.GitHubClient is a strict superset; the concrete gh.Client satisfies this interface.
type ReadClient interface {
	FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error)
	FetchItemDetails(item *gh.ProjectItem) error
	FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error)
	FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error)
	FetchPRMergeableFields(owner, repo string, prNumber int) (mergeable *bool, mergeableState string, err error)
	FetchPRMergeable(owner, repo string, prNumber int) (*bool, error)
	FetchPRMerged(owner, repo string, prNumber int) (bool, error)
	FetchPRMergeableState(owner, repo string, prNumber int) (string, error)
	FetchLabels(owner, repo string, issueNumber int) ([]string, error)
	FetchStatusField(projectID string) (*gh.StatusField, error)
	FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error)
	FetchPRsForSHA(owner, repo, sha string) ([]int, error)
	FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error)
	RateLimitStats() (rest, graphql gh.RateLimitStats)
}

// GitHubAdapter wraps a ReadClient with pass-through implementations.
// Used as the fallback inside CacheImpl (cache miss → forward to GitHub) and
// directly in NewWithDeps test wiring (engine tests bypass CacheImpl).
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

func (a *GitHubAdapter) FetchPRMergeableFields(owner, repo string, prNumber int) (*bool, string, error) {
	return a.client.FetchPRMergeableFields(owner, repo, prNumber)
}

func (a *GitHubAdapter) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return a.client.FetchPRMergeable(owner, repo, prNumber)
}

func (a *GitHubAdapter) FetchPRMerged(owner, repo string, prNumber int) (bool, error) {
	return a.client.FetchPRMerged(owner, repo, prNumber)
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

func (a *GitHubAdapter) FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error) {
	return a.client.FetchProjectItem(owner, repo, issueNumber)
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
//   - store owns: items, shaToKey, itemIDToKey, prToKey, pendingCheckRuns
//   - CacheImpl owns: paused, recentMissCache, projectID/Title/OwnerType, localDeltaAt
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

	// pauseObsMu guards pauseObservers. Separate from mu to avoid deadlock
	// when observers call back into CacheImpl (which acquires mu).
	pauseObsMu sync.Mutex
	// pauseObservers are called outside mu after every Pause/Resume transition.
	// Each func receives true on Pause and false on Resume.
	pauseObservers []func(bool)

	// recentMissCache is a negative cache preventing repeated REST lookups.
	// Keys are "miss:owner/repo#prN" or "miss:sha:SHA".
	recentMissCache map[string]time.Time

	// localDeltaAt records the last time a webhook bumped an item.
	// FetchProjectBoard uses max(ItemState.UpdatedAt, localDeltaAt[key]) so that
	// webhook-driven changes are visible to itemMayNeedWork before the next Reconcile.
	localDeltaAt map[string]time.Time

	// store owns per-item state (items, shaToKey, itemIDToKey, prToKey, pendingCheckRuns).
	store *itemstate.Store

	fallback    ReadClient
	logFn       func(format string, args ...any)
	matchEchoFn func(eventType, action, key string) // injected by engine; nil when cache disabled
}

// SetMatchEchoFn injects the MatchEcho function from the webhook manager.
func (c *CacheImpl) SetMatchEchoFn(fn func(eventType, action, key string)) {
	c.matchEchoFn = fn
}

// NewCacheImpl creates an empty cache backed by fallback for misses.
// store must be the shared *itemstate.Store owned by the engine — passing nil panics.
func NewCacheImpl(fallback ReadClient, store *itemstate.Store, logFn func(format string, args ...any)) *CacheImpl {
	if store == nil {
		panic("boardcache.NewCacheImpl: store must not be nil")
	}
	return &CacheImpl{
		localDeltaAt:    make(map[string]time.Time),
		recentMissCache: make(map[string]time.Time),
		store:           store,
		fallback:        fallback,
		logFn:           logFn,
	}
}

// BootstrapFromProbe populates the cache from a ProbeProjectBoard result instead
// of a full FetchProjectBoard. This is the preferred cold-start path: probe costs
// ~250 nodes vs ~2350 for a full shallow fetch on a 47-item board.
//
// Labels are absent from probe results; startup scans that rely on label data
// (runStartupTransientLabelScan, runStartupTerminalScan) will see empty label sets
// and silently become no-ops after this bootstrap path. This is an accepted
// trade-off: stale transient labels on closed terminal items will not be detected
// at startup (very low probability; requires a crash mid-Done-stage). Active items
// are deep-fetched on the first probe cycle, populating their labels normally.
//
// Sets LinkedPR.Number from each probe item's LinkedPRNumber so the subsequent
// probe cycle does not see spurious linkage-drift on items that already have a PR.
//
// Must be called before any engine mutations flow through the shared store.
func (c *CacheImpl) BootstrapFromProbe(items []gh.BoardProbeItem, projectID string) {
	// Construct synthetic ProjectItems from probe data.
	// applyProjectItem (called by Reset) populates LinkedPR.Number from LinkedPRNumber,
	// which prevents the subsequent probe from seeing spurious "linkage drift".
	syntheticItems := make([]gh.ProjectItem, 0, len(items))
	for _, pi := range items {
		syntheticItems = append(syntheticItems, gh.ProjectItem{
			ID:             pi.ContentID,
			ItemID:         pi.ItemID,
			Number:         pi.Number,
			IsPR:           pi.IsPR,
			IsClosed:       pi.IsClosed,
			Status:         pi.Status,
			Repo:           pi.Repo,
			UpdatedAt:      pi.EffectiveUpdatedAt,
			LinkedPRNumber: pi.LinkedPRNumber,
		})
	}

	// Reset Store atomically — clears all prior item state and indexes.
	c.store.Reset(syntheticItems)

	c.mu.Lock()
	c.projectID = projectID
	// projectTitle and projectOwnerType are not available from probe results;
	// they remain empty until the next Reconcile. The engine reads OwnerType
	// from e.cfg.OwnerType directly, not from cache, so this is safe.
	c.localDeltaAt = make(map[string]time.Time)
	c.mu.Unlock()

	c.logFn("[cache] probe bootstrap complete: %d items\n", len(items))
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

		// Propagate HeadSHA from the shallow board query when present. The reconcile
		// path runs every 60 min and acts as a backstop repair path for stale-SHA state.
		if pi.LinkedPRHeadSHA != "" && pi.LinkedPRNumberShallow != 0 {
			c.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:        pi.Repo,
				Number:      pi.Number,
				LinkedPRNum: pi.LinkedPRNumberShallow,
				SHA:         pi.LinkedPRHeadSHA,
			})
		}

		// Note: linkage drift detection (LinkedPRNumber changes) was previously
		// performed here using LinkedPRNumberShallow. It has moved to the probe
		// loop in engine/poll.go (runProbeAndDeepFetch), which compares the probe's
		// closedByPullRequestsReferences[0].number against the cache's LinkedPR.Number.
	}

	// Remove items no longer on the board.
	// Store.Remove handles cleanup of the prToKey index automatically.
	for _, snap := range c.store.All() {
		key := itemKey(snap.Repo(), snap.Number())
		if !newKeys[key] {
			drifted++
			c.store.Remove(snap.Repo(), snap.Number())
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

// RecordPRLinkage records an authoritative PR→issue mapping in the Store index.
// fullRepo must be in "owner/repo" format. Called by the engine immediately after
// CreateDraftPR succeeds, so all subsequent webhooks for this PR resolve to the
// correct issue without consulting the regex.
// No-op when the mapping is already present (avoids clobbering real webhook data)
// or when the issue is not yet in the Store (cold-cache bootstrap not yet complete).
func (c *CacheImpl) RecordPRLinkage(fullRepo string, prNumber, issueNumber int) {
	// Skip if already indexed — avoids clobbering a real PRDetailsUpdated from a webhook.
	if _, found := c.store.GetByPRKey(fullRepo, prNumber); found {
		return
	}
	// Guard: do not create a phantom Store entry for an issue that was never bootstrapped.
	if _, err := c.store.Get(fullRepo, issueNumber); err != nil {
		return
	}
	c.store.Apply(itemstate.PRDetailsUpdated{
		Repo:     fullRepo,
		Number:   issueNumber,
		PRNumber: prNumber,
	})
}

// resolvePRLinkage looks up which cached issue is closed by the given PR.
// It first checks the authoritative Store index (prToKey), which is populated by
// RecordPRLinkage at CreateDraftPR time. Only falls back to REST + regex when no
// authoritative mapping is present. Must be called without c.mu held (the REST call
// is a network operation). Returns the cache key and issue number of the first
// closing issue found in the Store. On a transient REST error the error is returned
// and callers should NOT record a negative-miss entry (a retry on the next webhook
// may succeed). Returns ("", 0, false, false, nil) when the PR body has no recognized
// closing reference or none of the referenced issues are in this cache.
//
// healed is true only when the REST+regex fallback path was taken (a real auto-heal
// occurred). It is false on an authoritative index hit; callers must NOT apply
// DeepFetchInvalidated or log "auto-heal" when healed is false.
func (c *CacheImpl) resolvePRLinkage(owner, repo string, prNumber int) (key string, issueNumber int, found bool, healed bool, err error) {
	fullRepo := owner + "/" + repo

	// Authoritative path: engine pre-recorded the mapping at CreateDraftPR time.
	// No heal occurred — the index already had the correct answer.
	if k, ok := c.store.GetByPRKey(fullRepo, prNumber); ok {
		_, issNum, parseOk := parseItemKey(k)
		if parseOk {
			return k, issNum, true, false, nil
		}
	}

	// Fallback: fetch PR body via REST and scan for closing keywords.
	issues, err := c.fallback.FetchPRClosingIssues(owner, repo, prNumber)
	if err != nil {
		c.logFn("[cache] resolvePRLinkage: fetch closing issues for PR #%d: %v\n", prNumber, err)
		return "", 0, false, false, err
	}
	if len(issues) == 0 {
		return "", 0, false, false, nil
	}

	// Store has nil fallback — Get returns ErrNotFound on miss without calling GitHub.
	for _, issNum := range issues {
		k := itemKey(fullRepo, issNum)
		if _, storeErr := c.store.Get(fullRepo, issNum); storeErr == nil {
			return k, issNum, true, true, nil
		}
	}
	return "", 0, false, false, nil
}

// Pause stops delta application (called on WebhookStreamUnhealthy transition).
// Observers are called after the lock is released, and only on an actual
// false→true transition to avoid spamming observers on repeated Pause calls.
func (c *CacheImpl) Pause() {
	c.mu.Lock()
	prior := c.paused
	c.paused = true
	c.mu.Unlock()
	if !prior {
		c.callPauseObservers(true)
	}
}

// Resume re-enables delta application (called after reconciliation on stream recovery).
// Observers are called after the lock is released, and only on an actual
// true→false transition to avoid spamming observers on repeated Resume calls.
func (c *CacheImpl) Resume() {
	c.mu.Lock()
	prior := c.paused
	c.paused = false
	c.mu.Unlock()
	if prior {
		c.callPauseObservers(false)
	}
}

// callPauseObservers snapshots and calls all registered pause observers.
// Must be called outside c.mu to avoid deadlock.
func (c *CacheImpl) callPauseObservers(paused bool) {
	c.pauseObsMu.Lock()
	obs := make([]func(bool), len(c.pauseObservers))
	copy(obs, c.pauseObservers)
	c.pauseObsMu.Unlock()
	for _, fn := range obs {
		if fn != nil {
			fn(paused)
		}
	}
}

// SubscribePause registers a function that is called after every Pause/Resume
// transition. The function receives true when the cache is paused and false when
// resumed. Observers run outside c.mu, so calling other CacheImpl methods from an
// observer is safe. Calling Pause or Resume re-entrantly from within an observer
// is semantically wrong (double-fire, inconsistent state) and must be avoided.
// The returned func unsubscribes the observer.
func (c *CacheImpl) SubscribePause(fn func(bool)) func() {
	c.pauseObsMu.Lock()
	c.pauseObservers = append(c.pauseObservers, fn)
	idx := len(c.pauseObservers) - 1
	c.pauseObsMu.Unlock()
	return func() {
		c.pauseObsMu.Lock()
		// Nil the slot; callPauseObservers skips nils on next call.
		if idx < len(c.pauseObservers) {
			c.pauseObservers[idx] = nil
		}
		c.pauseObsMu.Unlock()
	}
}

// IsPaused returns true when delta application is paused.
func (c *CacheImpl) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// IsBootstrapped returns true when the Store contains at least one cached item.
// Uses HasItems (O(1), non-allocating) rather than All to avoid a full snapshot
// allocation that would otherwise duplicate the one inside FetchProjectBoard.
// Called outside c.mu per the "NEVER hold mu while calling Store" invariant.
func (c *CacheImpl) IsBootstrapped() bool {
	return c.store.HasItems()
}

// IsItemDeepFetched returns true when the cache holds deep-fetched details for
// the given item (LastDeepFetchAt is non-zero). Called outside c.mu.
//
// Note: this only reports whether a deep fetch ever happened; it does not check
// freshness. Callers that need to know whether the cache is currently
// authoritative should use IsItemCacheFresh.
func (c *CacheImpl) IsItemDeepFetched(repo string, number int) bool {
	snap, err := c.store.Get(repo, number)
	if err != nil {
		return false
	}
	return !snap.State().LastDeepFetchAt.IsZero()
}

// IsItemCacheFresh returns true when the cache has deep-fetched details for the
// given item AND those details are not stale relative to the supplied
// sourceUpdatedAt (typically pi.UpdatedAt from a fresh board read). When the
// cache is stale, FetchItemDetails will fall through to a GraphQL deep-fetch.
// Called outside c.mu.
func (c *CacheImpl) IsItemCacheFresh(repo string, number int, sourceUpdatedAt time.Time) bool {
	snap, err := c.store.Get(repo, number)
	if err != nil {
		return false
	}
	s := snap.State()
	if s.LastDeepFetchAt.IsZero() {
		return false
	}
	return !cacheIsStale(sourceUpdatedAt, s.LastSeenSourceUpdatedAt)
}

// cacheIsStale reports whether the cached deep state is stale relative to the
// fresh board's source updatedAt. Returns true when sourceUpdatedAt is strictly
// after lastSeen — i.e., something on GitHub has bumped updatedAt since we last
// captured a deep fetch.
//
// Edge cases:
//   - lastSeen is zero (legacy cache entries written before this field existed):
//     treat as stale so the next FetchItemDetails refreshes once and populates
//     LastSeenSourceUpdatedAt going forward.
//   - sourceUpdatedAt is zero (board read did not populate it): treat as fresh
//     to avoid a hot-loop refetch when no signal is available.
func cacheIsStale(sourceUpdatedAt, lastSeen time.Time) bool {
	if sourceUpdatedAt.IsZero() {
		return false
	}
	if lastSeen.IsZero() {
		return true
	}
	return sourceUpdatedAt.After(lastSeen)
}

// Subscribe registers an observer on the underlying Store. The returned func
// unsubscribes the observer. Safe to call from any goroutine.
func (c *CacheImpl) Subscribe(o itemstate.Observer) func() {
	return c.store.Subscribe(o)
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
//
// Cache freshness contract: the cache is treated as authoritative only while the
// fresh board's pi.UpdatedAt has not advanced past LastSeenSourceUpdatedAt (the
// pi.UpdatedAt observed at the moment of the last successful deep fetch). When
// the board's updatedAt is newer, the cache is stale and we fall through to a
// real GraphQL fetch. pi.UpdatedAt is computed by FetchProjectBoard as
// max(issue.updatedAt, projectItem.updatedAt, linkedPR.updatedAt), so PR-side
// changes (new reviews, comments, draft toggles) bump it. Webhooks remain a
// pure optimization that mutate the cache between polls; in their absence (or
// when they're unhealthy), the board's updatedAt correctly forces a re-fetch.
//
// Falls back to GitHub on cache miss or when the cache is stale, and populates
// the cache with the result.
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
			if !cacheIsStale(item.UpdatedAt, s.LastSeenSourceUpdatedAt) {
				// Cache hit — copy deep fields into item.
				copyDeepFieldsFromState(item, s)
				return nil
			}
			c.logFn("[cache] stale for #%d (board updatedAt %s > last-seen %s) — refetching from GitHub\n",
				item.Number, item.UpdatedAt.Format(time.RFC3339), s.LastSeenSourceUpdatedAt.Format(time.RFC3339))
		} else {
			c.logFn("[cache] miss for #%d — fetching from GitHub\n", item.Number)
		}
	} else {
		c.logFn("[cache] miss for #%d — fetching from GitHub\n", item.Number)
	}
	if err := c.fallback.FetchItemDetails(item); err != nil {
		return err
	}

	// Write back to Store via ItemDeepFetched.
	// ItemDeepFetched → applyProjectItem → ensureLinkedPR sets lpr.Number = pi.LinkedPRNumber,
	// which then triggers updateIndexes to populate the prToKey index automatically.
	// No explicit prNumToKey backfill needed.
	c.store.Apply(itemstate.ItemDeepFetched{
		Repo:       item.Repo,
		Number:     item.Number,
		FreshState: *item,
	})
	// Apply the head SHA separately: applyProjectItem deliberately skips HeadSHA to avoid
	// clobbering a webhook-populated SHA with an empty value from the shallow path.
	// The deep fetch always carries an authoritative headRefOid from the GraphQL query.
	if item.LinkedPRHeadSHA != "" && item.LinkedPRNumber != 0 {
		c.store.Apply(itemstate.PRHeadSHAUpdated{
			Repo:        item.Repo,
			Number:      item.Number,
			LinkedPRNum: item.LinkedPRNumber,
			SHA:         item.LinkedPRHeadSHA,
		})
	}
	return nil
}

// FetchCheckRuns returns cached check runs for a SHA; falls back to GitHub on miss.
// Reads from Store.CheckRunsBySHA, which covers both pre-linkage (pendingCheckRuns)
// and post-linkage (LinkedPR.CheckRuns) runs. On total miss, fetches from GitHub and
// populates the Store via CheckRunCompleted mutations so subsequent calls are served
// from cache.
func (c *CacheImpl) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	if paused {
		return c.fallback.FetchCheckRuns(owner, repo, sha)
	}

	if runs := c.store.CheckRunsBySHA(sha); len(runs) > 0 {
		// Never trust a cached read that would classify as FAILED without
		// confirming it live first: on a webhook-less deployment there is no
		// check_run event to refresh a stale cached failure, and blindly
		// trusting it would let a superseded/rerun check keep shadowing its
		// own fresh rerun forever (#958 leg 3). A cached WAIT or READY
		// classification is still served from cache.
		if status, _, _ := gh.ClassifyCheckRuns(runs); status != gh.CheckRunsFailed {
			return runs, nil
		}
		c.logFn("[cache] cached check runs for sha=%s classify as FAILED — refetching from GitHub before trusting it\n", sha)
	}

	c.logFn("[cache] miss: FetchCheckRuns sha=%s — fetching from GitHub\n", sha)
	fullRepo := owner + "/" + repo
	runs, err := c.fallback.FetchCheckRuns(owner, repo, sha)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		c.store.Apply(itemstate.CheckRunCompleted{Repo: fullRepo, SHA: sha, Run: run})
	}
	return runs, nil
}

// FetchLinkedPR returns cached PR details for an issue; falls back to GitHub on miss.
func (c *CacheImpl) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	fullRepo := owner + "/" + repo

	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	if paused {
		return c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	}

	// Get LinkedPR state from Store (Store has its own mutex; no c.mu held).
	var linkedPRNum int
	snap, snapErr := c.store.Get(fullRepo, issueNumber)
	if snapErr == nil {
		if lpr := snap.LinkedPR(); lpr != nil {
			linkedPRNum = lpr.Number
		}
	}

	// Cache-hit: PR details are in the Store (Title is non-empty once PRDetailsUpdated fires,
	// HeadSHA is non-empty once PRHeadSHAUpdated fires). Require both to prevent serving an
	// incomplete record — a non-empty Title with empty HeadSHA would force checkCIGate to
	// block indefinitely (empty HeadSHA → blocked, not cleared) and trigger a REST fallback
	// on every poll until the SHA is populated. Requiring HeadSHA here ensures the cache only
	// serves fully-populated records, keeping the REST fallback path rare.
	if linkedPRNum != 0 && snapErr == nil {
		if lpr := snap.LinkedPR(); lpr != nil && lpr.Title != "" && lpr.HeadSHA != "" {
			return &gh.PRDetails{
				Number:              lpr.Number,
				Title:               lpr.Title,
				State:               lpr.State,
				Merged:              lpr.Merged,
				Draft:               lpr.Draft,
				HeadSHA:             lpr.HeadSHA,
				MergeableState:      lpr.MergeableState,
				IsMergeQueueEnabled: lpr.IsMergeQueueEnabled,
				IsInMergeQueue:      lpr.IsInMergeQueue,
				MergeQueueEntry:     lpr.MergeQueueEntry,
			}, nil
		}
	}

	c.logFn("[cache] miss: FetchLinkedPR #%d — fetching from GitHub\n", issueNumber)
	pr, err := c.fallback.FetchLinkedPR(owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	if pr != nil {
		// Write PR details into the Store via PRDetailsUpdated. This fires LinkedPRChanged
		// so observers see the change, and updates the prToKey reverse index automatically.
		c.store.Apply(itemstate.PRDetailsUpdated{
			Repo:     fullRepo,
			Number:   issueNumber,
			PRNumber: pr.Number,
			Title:    pr.Title,
			State:    pr.State,
			Merged:   pr.Merged,
			Draft:    pr.Draft,
		})
		// Always write the fresh HeadSHA from the REST response. This fires on every
		// fallback (not just at link-establishment) so any prior empty-SHA state is
		// repaired immediately. PRHeadSHAUpdated is idempotent; the REST-fresh SHA is
		// always authoritative. prToKey is maintained by PRDetailsUpdated above.
		c.store.Apply(itemstate.PRHeadSHAUpdated{
			Repo:        fullRepo,
			Number:      issueNumber,
			LinkedPRNum: pr.Number,
			SHA:         pr.HeadSHA,
		})
	}
	return pr, nil
}

// FetchPRMergeableFields always delegates to GitHub — mergeability changes without webhooks.
func (c *CacheImpl) FetchPRMergeableFields(owner, repo string, prNumber int) (*bool, string, error) {
	return c.fallback.FetchPRMergeableFields(owner, repo, prNumber)
}

// FetchPRMergeable always delegates to GitHub — mergeability changes without webhooks.
func (c *CacheImpl) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return c.fallback.FetchPRMergeable(owner, repo, prNumber)
}

// FetchPRMerged always delegates to GitHub — the authoritative merged flag must be
// fresh (the cache/list-endpoint copy lags right after a merge).
func (c *CacheImpl) FetchPRMerged(owner, repo string, prNumber int) (bool, error) {
	return c.fallback.FetchPRMerged(owner, repo, prNumber)
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

// FetchProjectItem always delegates to GitHub — used by ensureIssueInStore for the fallback fetch path.
func (c *CacheImpl) FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error) {
	return c.fallback.FetchProjectItem(owner, repo, issueNumber)
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
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.LocalStatusUpdated{
		Repo:      repo,
		Number:    number,
		NewStatus: newStatus,
	})
	if len(changes) == 0 {
		// Status unchanged.
		return
	}
	c.bumpLocalDeltaAt(key)
}

// ApplyLabelAdded updates the cached label list for the item identified by key,
// appending label if not already present. No-op when the key is not in the cache.
// Safe for concurrent use.
func (c *CacheImpl) ApplyLabelAdded(key, label string) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] ApplyLabelAdded: key %q invalid — no-op\n", key)
		return
	}
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.LocalLabelAdded{
		Repo:   repo,
		Number: number,
		Label:  label,
	})
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
}

// ApplyLabelRemoved updates the cached label list for the item identified by key,
// removing label if present. No-op when the key is not in the cache or label is absent.
// Safe for concurrent use.
func (c *CacheImpl) ApplyLabelRemoved(key, label string) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] ApplyLabelRemoved: key %q invalid — no-op\n", key)
		return
	}
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.LocalLabelRemoved{
		Repo:   repo,
		Number: number,
		Label:  label,
	})
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
}

// ApplyIssueClosed marks the item identified by key as closed in the store.
// No-op when the key is not in the cache. Safe for concurrent use.
func (c *CacheImpl) ApplyIssueClosed(key string) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] ApplyIssueClosed: key %q invalid — no-op\n", key)
		return
	}
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.IssueClosed{
		Repo:   repo,
		Number: number,
	})
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
}

// ApplyCommentAdded appends comment to the cached comment list for the item
// identified by key. No-op when the key is not in the cache.
// Safe for concurrent use.
//
// Note: LocalCommentAdded dedups by DatabaseID, mirroring IssueCommentCreated.
// If a webhook echo for the same comment arrives before the next Reconcile,
// the repeated LocalCommentAdded is a no-op rather than a duplicate append.
func (c *CacheImpl) ApplyCommentAdded(key string, comment gh.Comment) {
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] ApplyCommentAdded: key %q invalid — no-op\n", key)
		return
	}
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.LocalCommentAdded{
		Repo:    repo,
		Number:  number,
		Comment: comment,
	})
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
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
		c.bumpLocalDeltaAt(key)
	}
}

// RegisterItemID sets the project-item node ID for an existing cache entry that
// was added without one (e.g., via issues.opened before projects_v2_item.created).
// No-op when key is not in the Store or itemID is empty. Safe for concurrent use.
func (c *CacheImpl) RegisterItemID(key, itemID string) {
	if itemID == "" {
		return
	}
	repo, number, ok := parseItemKey(key)
	if !ok {
		c.logFn("[cache] RegisterItemID: key %q invalid — no-op\n", key)
		return
	}
	// Guard: do not create a phantom Store entry for a key that was never bootstrapped.
	if _, err := c.store.Get(repo, number); err != nil {
		return
	}
	_, changes, _ := c.store.Apply(itemstate.ItemIDRegistered{
		Repo:   repo,
		Number: number,
		ItemID: itemID,
	})
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
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

// LightReconcile fetches a fresh shallow board snapshot from GitHub and compares
// it against the current cache state on two fields: status and updatedAt.
// Label count is intentionally excluded: the board query returns at most 30 labels
// (shallow), while the cache may hold the full deep-fetched set; comparing counts
// would produce persistent false-positive drift for issues with >30 labels. Label
// mutations are captured by updatedAt, so the two-field check is sufficient.
//
// Note: LightReconcile still calls FetchProjectBoard (full shallow with labels).
// The per-poll probe path (engine/poll.go runProbeAndDeepFetch) is the primary
// cost-reduction mechanism; LightReconcile fires at most 20×/hour in webhook
// mode only, making it a lower-priority optimization target.
//
// Returns the number of drifted items, their keys, and the fresh board
// (to avoid a double-fetch when the caller passes it to Reconcile on drift).
//
// On network error the method returns a non-nil err, nil freshBoard, and 0 drift.
// The caller should log a warning and make no health state change.
//
// LightReconcile must not hold c.mu during the FetchProjectBoard call, following
// the "NEVER hold mu while calling Store" invariant and avoiding slow I/O under lock.
func (c *CacheImpl) LightReconcile(owner, repo string, projectNum int, ownerType string) (driftCount int, driftedKeys []string, freshBoard *gh.ProjectBoard, err error) {
	type entry struct {
		status    string
		updatedAt time.Time
		gateLabel string
	}

	// Snapshot current cache items. Store.All() handles its own locking.
	snaps := c.store.All()
	cached := make(map[string]entry, len(snaps))
	for _, snap := range snaps {
		s := snap.State()
		cached[itemKey(snap.Repo(), snap.Number())] = entry{
			status:    s.Status,
			updatedAt: s.UpdatedAt,
			gateLabel: fabrikManagedLabelKey(s.Labels),
		}
	}

	// Fetch fresh board from GitHub (no lock held).
	freshBoard, err = c.fallback.FetchProjectBoard(owner, repo, projectNum, ownerType)
	if err != nil {
		return 0, nil, nil, err
	}

	freshKeys := make(map[string]bool, len(freshBoard.Items))
	for i := range freshBoard.Items {
		pi := &freshBoard.Items[i]
		key := itemKey(pi.Repo, pi.Number)
		freshKeys[key] = true
		e, inCache := cached[key]
		if !inCache {
			driftCount++
			driftedKeys = append(driftedKeys, key)
			continue
		}
		// Compare status, updatedAt, AND the fabrik-managed label set. Label
		// comparison is scoped to fabrik: / stage: labels (the ones that gate
		// dispatch) rather than the full set: the board query truncates labels
		// (first: 30), so comparing all labels would false-positive for issues
		// with many labels (#641). The gate-label subset is small and never
		// truncated. Including it here closes the hole where a store label set
		// diverges from GitHub with a matching updatedAt (e.g. updatedAt laundered
		// by a deep-fetch that syncs updatedAt but not labels), which otherwise
		// strands an item at a gate forever on webhook-less deployments (#955).
		if e.status != pi.Status ||
			!e.updatedAt.Equal(pi.UpdatedAt) ||
			e.gateLabel != fabrikManagedLabelKey(pi.Labels) {
			driftCount++
			driftedKeys = append(driftedKeys, key)
		}
	}

	// Items in the cache that are no longer on the fresh board are also drift.
	for key := range cached {
		if !freshKeys[key] {
			driftCount++
			driftedKeys = append(driftedKeys, key)
		}
	}

	return driftCount, driftedKeys, freshBoard, nil
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

// fabrikManagedLabelKey returns an order-independent canonical key of the
// fabrik-managed labels (fabrik: / stage: prefixes) in the slice — the labels
// that gate dispatch. Used by LightReconcile to detect gate-label drift between
// the cache and GitHub without false-positiving on the board query's label
// truncation (only these few labels are compared, and they never exceed the cap).
func fabrikManagedLabelKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	managed := make([]string, 0, len(labels))
	for _, l := range labels {
		if strings.HasPrefix(l, "fabrik:") || strings.HasPrefix(l, "stage:") {
			managed = append(managed, l)
		}
	}
	sort.Strings(managed)
	return strings.Join(managed, "\n")
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
