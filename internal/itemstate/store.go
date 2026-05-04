package itemstate

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	gh "github.com/verveguy/fabrik/github"
)

// ErrNotFound is returned by Store.Get when an item does not exist in the cache
// and the FallbackFetcher also cannot locate it.
var ErrNotFound = errors.New("itemstate: item not found")

// FallbackFetcher is called by Store.Get on cache miss to populate the item from GitHub.
// The actual implementation (wrapping gh.Client) is wired in Phase 3-B.
type FallbackFetcher interface {
	FetchItem(repo string, number int) (gh.ProjectItem, error)
}

// Store is the single owner of all per-item state. All mutations flow through
// Apply; all reads flow through Get or Observer subscriptions.
//
// Concurrency model:
//   - mu guards items, shaToKey, and itemIDToKey.
//   - observerMu guards the observers slice independently to avoid holding mu
//     while calling observer callbacks (which may themselves call Apply or Get).
//   - Apply holds mu for the write, captures the observer slice under observerMu,
//     then calls observers after releasing both locks.
type Store struct {
	mu    sync.RWMutex
	items map[string]*ItemState // key: "owner/repo#N"

	// Reverse-lookup indexes maintained on every Apply that touches the relevant fields.
	shaToKey    map[string]string // LinkedPR.HeadSHA → itemKey
	itemIDToKey map[string]string // ItemState.ItemID → itemKey

	observerMu sync.RWMutex
	observers  []observerEntry

	fallback FallbackFetcher
	logger   Logger
}

type observerEntry struct {
	o    Observer
	id   uint64
}

// storeOptions holds optional Store configuration.
type storeOptions struct {
	logger Logger
}

// StoreOption is a functional option for NewStore.
type StoreOption func(*storeOptions)

// WithLogger sets a diagnostic logger on the Store. The default is no-op.
func WithLogger(l Logger) StoreOption {
	return func(o *storeOptions) { o.logger = l }
}

var nextObserverID uint64
var observerIDMu sync.Mutex

func newObserverID() uint64 {
	observerIDMu.Lock()
	defer observerIDMu.Unlock()
	nextObserverID++
	return nextObserverID
}

// NewStore creates a new Store. fallback may be nil, in which case cache misses
// return ErrNotFound instead of triggering a live fetch.
func NewStore(fallback FallbackFetcher, opts ...StoreOption) *Store {
	o := &storeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return &Store{
		items:       make(map[string]*ItemState),
		shaToKey:    make(map[string]string),
		itemIDToKey: make(map[string]string),
		fallback:    fallback,
		logger:      o.logger,
	}
}

// Apply mutates state. Every state change flows through here.
//
// Returns the updated Snapshot for the affected item, a list of Changes (zero or
// one for single-item mutations; multiple for BoardReconciled), and any error.
//
// If the mutation results in no field change (no-op), no Changes are returned
// and no Observers are notified (invariant I6).
//
// For BoardReconciled, the returned Snapshot is zero-valued; each affected item
// produces one Change in the returned slice.
func (s *Store) Apply(m Mutation) (Snapshot, []Change, error) {
	switch v := m.(type) {
	case BoardReconciled:
		return s.applyBoardReconciled(v)
	case ProjectV2ItemEdited:
		return s.applyProjectV2ItemEdited(v)
	case CheckRunCompleted:
		return s.applyCheckRunCompleted(v)
	default:
		return s.applySingleItem(m)
	}
}

// Get returns an immutable snapshot of the current ItemState for the given item.
//
// On cache miss, Get calls FallbackFetcher.FetchItem (if set), applies the result
// as an IssueOpened mutation, and returns the new snapshot (invariant I9).
//
// Returns ErrNotFound if:
//   - the item is not in the cache AND
//   - the fallback is nil OR the fallback also returns an error.
func (s *Store) Get(repo string, number int) (Snapshot, error) {
	key := itemKeyFor(repo, number)

	// Fast path: item in cache.
	s.mu.RLock()
	item, ok := s.items[key]
	if ok {
		snap := newSnapshot(*item)
		s.mu.RUnlock()
		return snap, nil
	}
	s.mu.RUnlock()

	// Cache miss — fetch from GitHub without holding any lock (avoids reentrancy
	// deadlock if FetchItem calls Apply internally).
	if s.fallback == nil {
		return Snapshot{}, ErrNotFound
	}
	fetched, err := s.fallback.FetchItem(repo, number)
	if err != nil {
		return Snapshot{}, ErrNotFound
	}
	fetched.Repo = repo
	fetched.Number = number

	// Populate cache via the normal Apply path.
	snap, _, applyErr := s.Apply(IssueOpened{Item: fetched})
	if applyErr != nil {
		return Snapshot{}, applyErr
	}
	return snap, nil
}

// Subscribe registers an observer that will be called after every successful
// non-no-op Apply. Returns an unsubscribe function; calling it removes the
// observer. The unsubscribe function is safe to call concurrently.
func (s *Store) Subscribe(o Observer) func() {
	id := newObserverID()
	s.observerMu.Lock()
	s.observers = append(s.observers, observerEntry{o: o, id: id})
	s.observerMu.Unlock()
	return func() {
		s.observerMu.Lock()
		defer s.observerMu.Unlock()
		for i, e := range s.observers {
			if e.id == id {
				s.observers = append(s.observers[:i], s.observers[i+1:]...)
				return
			}
		}
	}
}

// logf emits a diagnostic message if a logger is configured.
func (s *Store) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger(format, args...)
	}
}

// notify calls each observer with the given Change and Snapshot, outside any lock.
func (s *Store) notify(observers []observerEntry, change Change, snap Snapshot) {
	for _, e := range observers {
		e.o.OnChange(change, snap)
	}
}

// captureObservers returns a snapshot of the current observer list under read lock.
func (s *Store) captureObservers() []observerEntry {
	s.observerMu.RLock()
	defer s.observerMu.RUnlock()
	if len(s.observers) == 0 {
		return nil
	}
	obs := make([]observerEntry, len(s.observers))
	copy(obs, s.observers)
	return obs
}

// ---- single-item mutation dispatch ----

func (s *Store) applySingleItem(m Mutation) (Snapshot, []Change, error) {
	key := m.itemKey()
	if key == "" {
		return Snapshot{}, nil, errors.New("itemstate: mutation returned empty key")
	}

	s.mu.Lock()

	item := s.getOrCreate(key, m)
	// Deep-copy before state so no-op detection is not fooled by shared maps/pointers.
	before := newSnapshot(*item).state

	flags := s.applyToItem(item, m)

	// No-op detection: if nothing changed, skip observers.
	if reflect.DeepEqual(before, *item) {
		snap := newSnapshot(*item) // capture while lock held
		s.mu.Unlock()
		return snap, nil, nil
	}

	s.updateIndexes(before, *item, key)

	snap := newSnapshot(*item)
	repo, number := item.Repo, item.Number
	s.mu.Unlock()

	change := Change{Repo: repo, Number: number, Fields: flags}
	obs := s.captureObservers()
	s.notify(obs, change, snap)

	return snap, []Change{change}, nil
}

// getOrCreate returns the existing *ItemState for key, or creates a new zero-value one.
// Must be called with s.mu held for writing.
func (s *Store) getOrCreate(key string, m Mutation) *ItemState {
	if item, ok := s.items[key]; ok {
		return item
	}
	// Parse repo and number from the key "owner/repo#N".
	repo, number := parseKey(key)
	item := &ItemState{Repo: repo, Number: number}
	s.items[key] = item
	return item
}

// applyToItem applies a single-item mutation to item in-place and returns ChangeFlags.
// Must be called with s.mu held for writing.
func (s *Store) applyToItem(item *ItemState, m Mutation) ChangeFlags {
	switch v := m.(type) {

	case IssueOpened:
		// Merge labels: opened may arrive out-of-order, after labeled/unlabeled
		// events have already been applied. Unioning preserves any labels added
		// by subsequent delta events rather than overwriting them with the
		// (stale) creation-time label list from the webhook payload.
		merged := v.Item
		merged.Labels = unionStrings(item.Labels, v.Item.Labels)
		return applyProjectItem(item, merged)

	case IssueLabeled:
		if !containsString(item.Labels, v.Label) {
			item.Labels = append(item.Labels, v.Label)
		}
		return LabelsChanged

	case IssueUnlabeled:
		item.Labels = removeString(item.Labels, v.Label)
		return LabelsChanged

	case IssueClosed:
		item.State = "closed"
		item.IsClosed = true
		return StateChanged

	case IssueReopened:
		item.State = "open"
		item.IsClosed = false
		return StateChanged

	case IssueEdited:
		item.Title = v.Title
		item.Body = v.Body
		return TitleBodyChanged

	case IssueAssigneesUpdated:
		item.Assignees = copyStrings(v.Assignees)
		return AssigneesChanged

	case PRReviewRequested:
		ensureLinkedPR(item, 0)
		item.LinkedPR.ReviewRequests = copyReviewRequests(v.Reviewers)
		return LinkedPRChanged

	case PRReviewRequestRemoved:
		ensureLinkedPR(item, 0)
		item.LinkedPR.ReviewRequests = removeReviewRequest(item.LinkedPR.ReviewRequests, v.Login)
		return LinkedPRChanged

	case IssueCommentCreated:
		// Idempotent: skip if comment with this DatabaseID already exists.
		for _, existing := range item.Comments {
			if existing.DatabaseID == v.Comment.DatabaseID && v.Comment.DatabaseID != 0 {
				return 0
			}
		}
		item.Comments = append(item.Comments, v.Comment)
		return CommentsChanged

	case LocalStatusUpdated:
		item.Status = v.NewStatus
		return StatusChanged

	case LocalLabelAdded:
		if !containsString(item.Labels, v.Label) {
			item.Labels = append(item.Labels, v.Label)
		}
		return LabelsChanged

	case LocalLabelRemoved:
		item.Labels = removeString(item.Labels, v.Label)
		return LabelsChanged

	case LocalCommentAdded:
		item.Comments = append(item.Comments, v.Comment)
		return CommentsChanged

	case LocalLockAcquired:
		item.Lock = &LockState{
			HolderUser: v.User,
			HeldByThis: true,
			AcquiredAt: v.AcquiredAt,
		}
		var flags ChangeFlags = LockChanged
		if v.Worker != nil {
			w := *v.Worker
			item.Worker = &w
			flags |= WorkerChanged
		}
		return flags

	case LocalLockReleased:
		item.Lock = nil
		return LockChanged

	case ItemDeepFetched:
		flags := applyProjectItem(item, v.FreshState)
		item.LastDeepFetchAt = time.Now()
		item.LastDeepFetchFailureAt = time.Time{} // clear failure on success
		return flags | DeepFetchChanged

	case StageAttempted:
		ensureStageStateMaps(item)
		item.StageState.LastAttemptAt[v.StageName] = v.At
		return StageStateChanged

	case StageRetryIncremented:
		ensureStageStateMaps(item)
		item.StageState.Attempts[v.StageName]++
		return StageStateChanged

	case StageRetryCleared:
		ensureStageStateMaps(item)
		item.StageState.Attempts[v.StageName] = 0
		return StageStateChanged

	case ReviewCycleIncremented:
		ensureStageStateMaps(item)
		item.StageState.ReviewCycles[v.StageName]++
		return StageStateChanged

	case CIFixCycleIncremented:
		ensureStageStateMaps(item)
		item.StageState.CIFixCycles[v.StageName]++
		return StageStateChanged

	case RebaseCycleIncremented:
		ensureStageStateMaps(item)
		item.StageState.RebaseCycles[v.StageName]++
		return StageStateChanged

	case EnginePaused:
		ensureStageStateMaps(item)
		item.StageState.PausedByEngine[v.StageName] = true
		return StageStateChanged

	case EngineUnpaused:
		ensureStageStateMaps(item)
		delete(item.StageState.PausedByEngine, v.StageName)
		return StageStateChanged

	case EngineCyclesCleared:
		ensureStageStateMaps(item)
		delete(item.StageState.ReviewCycles, v.StageName)
		delete(item.StageState.CIFixCycles, v.StageName)
		delete(item.StageState.RebaseCycles, v.StageName)
		return StageStateChanged

	case StageLastAttemptCleared:
		ensureStageStateMaps(item)
		delete(item.StageState.LastAttemptAt, v.StageName)
		return StageStateChanged

	case CommentProcessed:
		ensureStageStateMaps(item)
		item.StageState.ProcessedComments[v.CommentID] = v.At
		return StageStateChanged

	case CooldownRecorded:
		if item.CooldownAt == nil {
			item.CooldownAt = make(map[string]time.Time)
		}
		item.CooldownAt[v.Reason] = v.Until
		return CooldownChanged

	case WorkerHeartbeat:
		if item.Worker == nil {
			return 0 // no-op: heartbeat for a worker that has already exited
		}
		item.Worker.LastSignAt = v.At
		return WorkerChanged

	case WorkerPIDSet:
		if item.Worker == nil {
			return 0 // no-op: worker already exited
		}
		item.Worker.PID = v.PID
		return WorkerChanged

	case WorkerExited:
		item.Worker = nil
		return WorkerChanged

	case InvocationRecorded:
		item.LastInvocationCompleted = v.Completed
		item.LastInvocationBlocked = v.Blocked
		item.LastTokenUsage = v.Usage
		return InvocationChanged

	case DeepFetchFailed:
		item.LastDeepFetchFailureAt = v.At
		return DeepFetchChanged

	case CIMergePendingStarted:
		ensureLinkedPR(item, 0)
		item.LinkedPR.CIMergePendingSince = v.At
		return LinkedPRChanged

	case CIMergePendingCleared:
		if item.LinkedPR != nil {
			item.LinkedPR.CIMergePendingSince = time.Time{}
		}
		return LinkedPRChanged

	case PRChecksObserved:
		ensureLinkedPR(item, 0)
		if item.LinkedPR.HasHadChecks {
			return 0 // already set; no-op
		}
		item.LinkedPR.HasHadChecks = true
		return LinkedPRChanged

	case BaseBranchWarnRecorded:
		if item.BaseBranchWarned == nil {
			item.BaseBranchWarned = make(map[string]bool)
		}
		item.BaseBranchWarned[v.Branch] = true
		return BaseBranchChanged

	case PRReviewSubmitted:
		ensureLinkedPR(item, v.Number)
		// Upsert by DatabaseID so a re-review replaces the prior entry.
		for i, r := range item.LinkedPR.Reviews {
			if r.DatabaseID == v.Review.DatabaseID && v.Review.DatabaseID != 0 {
				item.LinkedPR.Reviews[i] = v.Review
				return LinkedPRChanged
			}
		}
		item.LinkedPR.Reviews = append(item.LinkedPR.Reviews, v.Review)
		return LinkedPRChanged

	case PRReviewCommentCreated:
		ensureLinkedPR(item, v.PRNumber)
		// Idempotent: skip if comment with this NodeID already exists.
		for _, existing := range item.LinkedPR.ThreadComments {
			if existing.ID == v.Comment.ID {
				return 0
			}
		}
		item.LinkedPR.ThreadComments = append(item.LinkedPR.ThreadComments, v.Comment)
		return LinkedPRChanged | CommentsChanged

	case DeepFetchInvalidated:
		item.LastDeepFetchAt = time.Time{}
		return DeepFetchChanged

	case PRHeadSHAUpdated:
		ensureLinkedPR(item, v.LinkedPRNum)
		if v.LinkedPRNum != 0 {
			item.LinkedPR.Number = v.LinkedPRNum
		}
		item.LinkedPR.HeadSHA = v.SHA
		return LinkedPRChanged

	case ReviewThreadCommentAdded:
		ensureLinkedPR(item, 0)
		// Idempotent by NodeID.
		for _, existing := range item.LinkedPR.ThreadComments {
			if existing.ID == v.Comment.ID {
				return 0
			}
		}
		item.LinkedPR.ThreadComments = append(item.LinkedPR.ThreadComments, v.Comment)
		return LinkedPRChanged | CommentsChanged

	case ShallowBoardItemUpdated:
		return applyShallowItem(item, v.Item)
	}

	return 0
}

// applyBoardReconciled processes a bulk board snapshot, updating or creating each item.
func (s *Store) applyBoardReconciled(v BoardReconciled) (Snapshot, []Change, error) {
	var changes []Change

	for _, pi := range v.Items {
		key := itemKeyFor(pi.Repo, pi.Number)
		if key == "" {
			continue
		}

		s.mu.Lock()
		item := s.getOrCreate(key, IssueOpened{Item: pi})
		before := newSnapshot(*item).state
		flags := applyProjectItem(item, pi)

		if reflect.DeepEqual(before, *item) {
			s.mu.Unlock()
			continue
		}

		s.updateIndexes(before, *item, key)
		snap := newSnapshot(*item)
		repo, number := item.Repo, item.Number
		s.mu.Unlock()

		change := Change{Repo: repo, Number: number, Fields: flags}
		obs := s.captureObservers()
		s.notify(obs, change, snap)
		changes = append(changes, change)
	}

	return Snapshot{}, changes, nil
}

// applyProjectV2ItemEdited handles a board-status update keyed by project item ID.
func (s *Store) applyProjectV2ItemEdited(v ProjectV2ItemEdited) (Snapshot, []Change, error) {
	s.mu.Lock()
	key, ok := s.itemIDToKey[v.ItemID]
	if !ok {
		s.mu.Unlock()
		s.logf("ProjectV2ItemEdited: unknown itemID %s", v.ItemID)
		return Snapshot{}, nil, nil
	}
	item := s.items[key]
	before := newSnapshot(*item).state
	item.Status = v.NewStatus
	if reflect.DeepEqual(before, *item) {
		snap := newSnapshot(*item)
		s.mu.Unlock()
		return snap, nil, nil
	}
	snap := newSnapshot(*item)
	repo, number := item.Repo, item.Number
	s.mu.Unlock()

	change := Change{Repo: repo, Number: number, Fields: StatusChanged}
	obs := s.captureObservers()
	s.notify(obs, change, snap)
	return snap, []Change{change}, nil
}

// applyCheckRunCompleted routes a check run to the item whose LinkedPR.HeadSHA matches.
func (s *Store) applyCheckRunCompleted(v CheckRunCompleted) (Snapshot, []Change, error) {
	s.mu.Lock()
	key, ok := s.shaToKey[v.SHA]
	if !ok {
		s.mu.Unlock()
		s.logf("CheckRunCompleted: unknown SHA %s", v.SHA)
		return Snapshot{}, nil, nil
	}
	item := s.items[key]
	before := newSnapshot(*item).state
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{}
	}
	// Update or append the check run by ID.
	found := false
	for i, cr := range item.LinkedPR.CheckRuns {
		if cr.ID == v.Run.ID {
			item.LinkedPR.CheckRuns[i] = v.Run
			found = true
			break
		}
	}
	if !found {
		item.LinkedPR.CheckRuns = append(item.LinkedPR.CheckRuns, v.Run)
	}
	item.LinkedPR.HasHadChecks = true

	if reflect.DeepEqual(before, *item) {
		snap := newSnapshot(*item)
		s.mu.Unlock()
		return snap, nil, nil
	}
	snap := newSnapshot(*item)
	repo, number := item.Repo, item.Number
	s.mu.Unlock()

	change := Change{Repo: repo, Number: number, Fields: LinkedPRChanged}
	obs := s.captureObservers()
	s.notify(obs, change, snap)
	return snap, []Change{change}, nil
}

// updateIndexes keeps shaToKey and itemIDToKey consistent after a mutation.
// Must be called with s.mu held for writing.
func (s *Store) updateIndexes(before, after ItemState, key string) {
	// ItemID index.
	if before.ItemID != after.ItemID {
		if before.ItemID != "" {
			delete(s.itemIDToKey, before.ItemID)
		}
		if after.ItemID != "" {
			s.itemIDToKey[after.ItemID] = key
		}
	} else if after.ItemID != "" {
		s.itemIDToKey[after.ItemID] = key
	}

	// HeadSHA index.
	beforeSHA := ""
	afterSHA := ""
	if before.LinkedPR != nil {
		beforeSHA = before.LinkedPR.HeadSHA
	}
	if after.LinkedPR != nil {
		afterSHA = after.LinkedPR.HeadSHA
	}
	if beforeSHA != afterSHA {
		if beforeSHA != "" {
			delete(s.shaToKey, beforeSHA)
		}
		if afterSHA != "" {
			s.shaToKey[afterSHA] = key
		}
	} else if afterSHA != "" {
		s.shaToKey[afterSHA] = key
	}
}

// ---- helpers ----

// applyProjectItem copies all fields from a gh.ProjectItem into item and returns ChangeFlags.
func applyProjectItem(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	var flags ChangeFlags

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	if item.ItemID != pi.ItemID {
		item.ItemID = pi.ItemID
	}

	if item.Title != pi.Title || item.Body != pi.Body || item.URL != pi.URL || item.Author != pi.Author {
		item.Title = pi.Title
		item.Body = pi.Body
		item.URL = pi.URL
		item.Author = pi.Author
		flags |= TitleBodyChanged
	}

	if !reflect.DeepEqual(item.Assignees, pi.Assignees) {
		item.Assignees = copyStrings(pi.Assignees)
		flags |= AssigneesChanged
	}

	if item.State != stateFrom(pi) || item.IsClosed != pi.IsClosed || item.IsPR != pi.IsPR {
		item.State = stateFrom(pi)
		item.IsClosed = pi.IsClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if !reflect.DeepEqual(item.Labels, pi.Labels) {
		item.Labels = copyStrings(pi.Labels)
		flags |= LabelsChanged
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}

	if !reflect.DeepEqual(item.BlockedBy, pi.BlockedBy) {
		item.BlockedBy = copyDeps(pi.BlockedBy)
		flags |= BlockedByChanged
	}

	if !reflect.DeepEqual(item.Comments, pi.Comments) {
		item.Comments = copyComments(pi.Comments)
		flags |= CommentsChanged
	}

	// Sync LinkedPR fields from ProjectItem.
	if pi.LinkedPRNumber != 0 {
		if item.LinkedPR == nil {
			item.LinkedPR = &LinkedPRState{}
			flags |= LinkedPRChanged
		}
		lpr := item.LinkedPR
		if lpr.Number != pi.LinkedPRNumber ||
			lpr.HeadSHA != "" || // already set by richer fetch — don't overwrite
			!reflect.DeepEqual(lpr.Reviews, pi.LinkedPRReviews) ||
			!reflect.DeepEqual(lpr.ReviewRequests, pi.LinkedPRReviewRequests) ||
			!reflect.DeepEqual(lpr.ThreadComments, pi.LinkedPRReviewThreadComments) ||
			lpr.ResolvedThreadCount != pi.LinkedPRResolvedThreadCount {
			if lpr.Number != pi.LinkedPRNumber {
				lpr.Number = pi.LinkedPRNumber
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.Reviews, pi.LinkedPRReviews) {
				lpr.Reviews = copyPRReviews(pi.LinkedPRReviews)
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.ReviewRequests, pi.LinkedPRReviewRequests) {
				lpr.ReviewRequests = copyReviewRequests(pi.LinkedPRReviewRequests)
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.ThreadComments, pi.LinkedPRReviewThreadComments) {
				lpr.ThreadComments = copyComments(pi.LinkedPRReviewThreadComments)
				flags |= LinkedPRChanged | CommentsChanged
			}
			if lpr.ResolvedThreadCount != pi.LinkedPRResolvedThreadCount {
				lpr.ResolvedThreadCount = pi.LinkedPRResolvedThreadCount
				flags |= LinkedPRChanged
			}
		}
	} else if item.LinkedPR != nil && item.LinkedPR.Number != 0 {
		// PR was delinked — only clear the number, preserve any richer state.
		// (Full removal would need an explicit IssuePRDelinked mutation.)
	}

	if flags == 0 && item.Repo == "" {
		// First-time population of identity fields.
		item.Repo = pi.Repo
		item.Number = pi.Number
		return TitleBodyChanged | StatusChanged | LabelsChanged
	}

	item.Repo = pi.Repo
	item.Number = pi.Number

	return flags
}

// All returns an immutable snapshot of every item currently in the Store.
// The returned slice is a point-in-time snapshot; subsequent mutations do not
// affect it. Safe to call concurrently with Apply.
func (s *Store) All() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snaps := make([]Snapshot, 0, len(s.items))
	for _, item := range s.items {
		snaps = append(snaps, newSnapshot(*item))
	}
	return snaps
}

// Remove deletes the item identified by (repo, number) from the Store and
// updates the shaToKey and itemIDToKey indexes accordingly.
// No-op when the item is not present.
func (s *Store) Remove(repo string, number int) {
	key := itemKeyFor(repo, number)
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[key]
	if !ok {
		return
	}
	if item.ItemID != "" {
		delete(s.itemIDToKey, item.ItemID)
	}
	if item.LinkedPR != nil && item.LinkedPR.HeadSHA != "" {
		delete(s.shaToKey, item.LinkedPR.HeadSHA)
	}
	delete(s.items, key)
}

// RemoveByItemID removes the item identified by its project ItemID (board-side ID).
// Returns the repo and number of the removed item, and ok=true, so callers can
// clean up secondary state (e.g. prNumToKey entries). No-op and ok=false if the
// ItemID is not in the index.
func (s *Store) RemoveByItemID(itemID string) (repo string, number int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, found := s.itemIDToKey[itemID]
	if !found {
		return "", 0, false
	}
	item, exists := s.items[key]
	if !exists {
		delete(s.itemIDToKey, itemID)
		return "", 0, false
	}
	repo = item.Repo
	number = item.Number
	if item.LinkedPR != nil && item.LinkedPR.HeadSHA != "" {
		delete(s.shaToKey, item.LinkedPR.HeadSHA)
	}
	delete(s.itemIDToKey, itemID)
	delete(s.items, key)
	return repo, number, true
}

// Reset atomically replaces all Store state with the items in the given slice.
// Existing items, indexes, and deep-fetch state are cleared. This is used by
// Bootstrap to ensure a clean slate (unlike Reconcile, which preserves deep state).
func (s *Store) Reset(items []gh.ProjectItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]*ItemState, len(items))
	s.shaToKey = make(map[string]string, len(items))
	s.itemIDToKey = make(map[string]string, len(items))
	for i := range items {
		pi := items[i]
		key := itemKeyFor(pi.Repo, pi.Number)
		if key == "" {
			continue
		}
		item := &ItemState{Repo: pi.Repo, Number: pi.Number}
		applyProjectItem(item, pi)
		s.items[key] = item
		if item.ItemID != "" {
			s.itemIDToKey[item.ItemID] = key
		}
		if item.LinkedPR != nil && item.LinkedPR.HeadSHA != "" {
			s.shaToKey[item.LinkedPR.HeadSHA] = key
		}
	}
}

// applyShallowItem updates only the shallow board fields of item from pi.
// Deep fields (Body, Comments, Assignees, BlockedBy, LinkedPRReviews, etc.)
// are left unchanged. Used by CacheImpl.Reconcile to apply shallow board
// updates without wiping deep-fetched data.
func applyShallowItem(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	var flags ChangeFlags

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	if item.ItemID != pi.ItemID && pi.ItemID != "" {
		item.ItemID = pi.ItemID
	}

	if item.Title != pi.Title {
		item.Title = pi.Title
		flags |= TitleBodyChanged
	}

	if item.URL != pi.URL && pi.URL != "" {
		item.URL = pi.URL
		flags |= TitleBodyChanged
	}

	if item.State != stateFrom(pi) || item.IsClosed != pi.IsClosed || item.IsPR != pi.IsPR {
		item.State = stateFrom(pi)
		item.IsClosed = pi.IsClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if !reflect.DeepEqual(item.Labels, pi.Labels) {
		item.Labels = copyStrings(pi.Labels)
		flags |= LabelsChanged
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}

	return flags
}

func stateFrom(pi gh.ProjectItem) string {
	if pi.IsClosed {
		return "closed"
	}
	return "open"
}

func ensureLinkedPR(item *ItemState, prNumber int) {
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{Number: prNumber}
	}
}

func ensureStageStateMaps(item *ItemState) {
	ss := &item.StageState
	if ss.Attempts == nil {
		ss.Attempts = make(map[string]int)
	}
	if ss.LastAttemptAt == nil {
		ss.LastAttemptAt = make(map[string]time.Time)
	}
	if ss.PausedByEngine == nil {
		ss.PausedByEngine = make(map[string]bool)
	}
	if ss.ReviewCycles == nil {
		ss.ReviewCycles = make(map[string]int)
	}
	if ss.CIFixCycles == nil {
		ss.CIFixCycles = make(map[string]int)
	}
	if ss.RebaseCycles == nil {
		ss.RebaseCycles = make(map[string]int)
	}
	if ss.ProcessedComments == nil {
		ss.ProcessedComments = make(map[string]time.Time)
	}
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func removeReviewRequest(rrs []gh.ReviewRequest, login string) []gh.ReviewRequest {
	out := rrs[:0:0]
	for _, rr := range rrs {
		if rr.Login != login {
			out = append(out, rr)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unionStrings returns a slice containing all elements of existing plus any
// elements from incoming that are not already present. Order is preserved.
func unionStrings(existing, incoming []string) []string {
	result := copyStrings(existing)
	for _, s := range incoming {
		if !containsString(result, s) {
			result = append(result, s)
		}
	}
	return result
}

func removeString(ss []string, s string) []string {
	out := ss[:0:0]
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseKey splits "owner/repo#N" into repo and number.
func parseKey(key string) (string, int) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return key, 0
	}
	repo := key[:idx]
	numStr := key[idx+1:]
	n := 0
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return repo, 0
		}
		n = n*10 + int(c-'0')
	}
	return repo, n
}
