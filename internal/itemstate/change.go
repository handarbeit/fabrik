package itemstate

import (
	"strings"
	"time"
)

// ChangeFlags is a bitmask describing which logical field groups of an ItemState
// were altered by a mutation. Observers use this to cheaply filter whether a
// Change is relevant to them without inspecting the full Snapshot.
type ChangeFlags uint32

const (
	// StatusChanged indicates the project-board Status column changed.
	StatusChanged ChangeFlags = 1 << iota
	// LabelsChanged indicates the Labels slice changed.
	LabelsChanged
	// LockChanged indicates Lock state was acquired, released, or modified.
	LockChanged
	// StageStateChanged indicates StageState (attempts, cycles, pauses) changed.
	StageStateChanged
	// WorkerChanged indicates any Worker-handle field changed (set, cleared, heartbeat, PID).
	WorkerChanged
	// WorkerLifecycleChanged is a sub-flag emitted only by WorkerEntered and WorkerExited.
	// It is the flag that drives wakeChFlags / mayNeedWork so heartbeats and PID-sets
	// do not cause spurious deep-fetch cycles for in-flight items.
	WorkerLifecycleChanged
	// CooldownChanged indicates CooldownAt map entries were added or removed.
	CooldownChanged
	// LinkedPRChanged indicates LinkedPR state (including check runs) changed.
	LinkedPRChanged
	// CommentsChanged indicates Comments or PR thread comments changed.
	CommentsChanged
	// AssigneesChanged indicates the Assignees slice changed.
	AssigneesChanged
	// TitleBodyChanged indicates Title, Body, URL, or Author changed.
	TitleBodyChanged
	// StateChanged indicates the open/closed State or IsClosed changed.
	StateChanged
	// BlockedByChanged indicates the BlockedBy dependency list changed.
	BlockedByChanged
	// DeepFetchChanged indicates LastDeepFetchAt or LastDeepFetchFailureAt changed.
	DeepFetchChanged
	// InvocationChanged indicates LastInvocationCompleted, LastInvocationBlocked,
	// or LastTokenUsage changed.
	InvocationChanged
	// BaseBranchChanged indicates BaseBranchWarned map changed.
	BaseBranchChanged
	// ItemRemoved indicates the item was removed from the board during a Reset.
	// This flag is distinct from StateChanged (issue open/closed) and is emitted
	// only by Store.Reset for items present in the old map but absent from the
	// new items slice.
	ItemRemoved
	// CheckRunChanged indicates a check run was written to the pre-linkage
	// pendingCheckRuns buffer (SHA not yet linked to any item). Not in wakeChFlags:
	// the CI gate is catch-up-driven, not wake-driven. The flag exists for
	// observability (e.g. future TUI consumers) without forcing a poll wake.
	CheckRunChanged
	// TerminalChanged indicates the Terminal flag was explicitly set or cleared via
	// TerminalFlagSet. Store-internal clears (when status changes) piggyback on
	// StatusChanged and do not emit TerminalChanged.
	TerminalChanged
	// CommentBreakerChanged indicates ItemState.CommentBreaker (invocation
	// timestamps or last author) changed. Informational only — not in
	// wakeChFlags/cycleSetFlags, same treatment as InvocationChanged.
	CommentBreakerChanged
	// PRStateChanged is a narrower sub-flag of LinkedPRChanged, set only by
	// PRDetailsUpdated (LinkedPR.Title/State/Merged/Draft — a genuine PR-level
	// state transition such as merged, closed, or draft<->ready). Distinct from
	// LinkedPRChanged so consumers that only care about the PR's own state (not
	// review activity, check runs, or thread comments, which also set
	// LinkedPRChanged) can filter precisely.
	PRStateChanged
	// SelfWriteBaselineChanged indicates LastSeenSourceUpdatedAt was advanced by
	// a SelfWriteObserved mutation carrying a GitHub timestamp (#1090, #1945). Pure bookkeeping for the probe
	// staleness check — intentionally excluded from wakeChFlags/cycleSetFlags
	// (engine/observers.go) so it never wakes the poll loop or bypasses the
	// dispatch cooldown.
	SelfWriteBaselineChanged
	// LabelAppliedAtChanged indicates ItemState.LabelAppliedAt was written by a
	// LabelAppliedAtRecorded mutation (#1314). Informational only — like
	// SelfWriteBaselineChanged, intentionally excluded from wakeChFlags/
	// cycleSetFlags so it never wakes the poll loop or bypasses the dispatch
	// cooldown.
	LabelAppliedAtChanged
	// MilestoneChanged indicates ItemState.Milestone or MilestoneKnown changed
	// (#1967 R10) — a board fetch or a milestoned/demilestoned webhook delta.
	// Informational only, intentionally excluded from wakeChFlags/cycleSetFlags
	// so a milestone change never wakes the poll loop.
	MilestoneChanged
	// EngineWriteRecordedChanged indicates ItemState.EngineWriteAt gained or
	// lost an entry (#2159). Informational bookkeeping for the reconcile
	// protection, intentionally excluded from wakeChFlags/cycleSetFlags so it
	// never wakes the poll loop.
	EngineWriteRecordedChanged
	// StatusFactsChanged indicates PendingRetry, GateHold, ConflictResolution or
	// RebaseCause changed (#2121). Facts the attention classifier reads; like
	// EngineWriteRecordedChanged they are intentionally excluded from
	// wakeChFlags/cycleSetFlags so recording one never wakes the poll loop.
	StatusFactsChanged
	// PRFactsChanged indicates LinkedPR.Facts or LinkedPR.CIVerdict changed
	// (#2122). Display facts for the overseer API; intentionally excluded from
	// wakeChFlags/cycleSetFlags so a poll-side write never wakes the poll.
	PRFactsChanged
	// DependenciesInvalidatedChanged indicates a DependenciesInvalidated mutation
	// changed the item (#1995): its deep cache was invalidated and/or its
	// "dep-blocked" admission cooldown was made due so a dependency change is
	// re-read promptly. Unlike DeepFetchChanged it is in wakeChFlags/cycleSetFlags.
	DependenciesInvalidatedChanged
	// SubIssueLinksChanged indicates ItemState.SubIssues or ParentIssue changed
	// (#2352). An informational fact no engine code dispatches on; intentionally
	// excluded from wakeChFlags/cycleSetFlags so a link change never wakes the poll.
	SubIssueLinksChanged
)

// Change describes what fields a mutation altered. Delivered to every Observer
// after a successful Store.Apply.
type Change struct {
	// Repo is "owner/repo" identifying the item.
	Repo string
	// Number is the issue number.
	Number int
	// Fields is a bitmask of ChangeFlags indicating which field groups changed.
	Fields ChangeFlags
	// LabelDeltas lists each label the mutation actually added or removed, in a
	// deterministic order (additions in item order, then removals in prior order).
	// It is computed from a before/after diff, so an idempotent write or a
	// reconcile that re-observes an unchanged label contributes nothing, and a
	// webhook echo that follows the engine's own write-through is a no-op that
	// never reaches observers (#1968 R3). Empty for an item's first population
	// (Reset, a BoardReconciled/mutation that creates the item) — that is a
	// baseline, not a change.
	LabelDeltas []LabelDelta
	// Origin records which kind of mutation produced the change. Meaningful for
	// LabelDeltas attribution.
	Origin ChangeOrigin
	// Sender is the GitHub login that triggered a webhook label mutation; empty
	// when unknown or not a webhook.
	Sender string
	// EchoOfEngine is true when a webhook label mutation matched the engine's
	// own echo registry, i.e. it is the echo of a write Fabrik made.
	EchoOfEngine bool
	// Requester is the sanitised overseer subscriber that requested the label
	// write behind this change (#2125); empty for an engine-autonomous write, an
	// unechoed webhook or a non-label change.
	Requester string
	// Created is true when the mutation created the record (the store had not
	// held the item). Never set by Reset or for a mid-run reset of known items
	// (#2123): only an item the running store learns about is an arrival.
	Created bool
	// PrevStatus is the item's board Status before a StatusChanged mutation. It
	// is empty when the previous Status was unknown, which makes the move a
	// baseline rather than a column change (#2123).
	PrevStatus string
	// StatusAt is the GitHub time of a webhook-delivered Status move; zero when
	// unknown — never "now" (#2123).
	StatusAt time.Time
	// Closed is true when the mutation moved an already-held item from open to
	// closed (#2123).
	Closed bool
	// Path names the cache-refresh path that produced the change when it came
	// from a reconcile, a deep fetch, the probe loop or the Layer 1 status refresh (#2170, #2347); PathNone for every
	// other mutation. Origin cannot tell these apart (all are OriginOther, or
	// OriginProbe for the probe), and the cache-divergence detector needs the name.
	Path ChangePath
	// StatusFrom/StatusTo are the board column before and after, set whenever
	// StatusChanged is in Fields and the item existed before (#2170).
	StatusFrom, StatusTo string
	// EngineStatusWrite is true for a LocalStatusUpdated write-through: Fabrik
	// moved the item itself.
	EngineStatusWrite bool
}

// ChangePath names the refresh path behind a Change (#2170).
type ChangePath uint8

const (
	// PathNone is every mutation that is not a refresh from GitHub.
	PathNone ChangePath = iota
	// PathReconcile is a board reconcile: ShallowBoardItemUpdated or a
	// BoardReconciled item.
	PathReconcile
	// PathDeepFetch is ItemDeepFetched.
	PathDeepFetch
	// PathProbe is a mutation applied by the probe loop (FromProbe).
	PathProbe
	// PathRefresh is the Layer 1 status refresh (StatusRefreshed, #2347).
	PathRefresh
)

// String is the wire name used in cache-divergence events.
func (p ChangePath) String() string {
	switch p {
	case PathReconcile:
		return "reconcile"
	case PathDeepFetch:
		return "deep-fetch"
	case PathProbe:
		return "probe"
	case PathRefresh:
		return "status-refresh"
	}
	return ""
}

// LabelDelta is one label added to or removed from an item.
type LabelDelta struct {
	Label string
	Added bool
}

// ChangeOrigin classifies the mutation behind a Change.
type ChangeOrigin uint8

const (
	// OriginOther is any mutation that is not a label write: a board reconcile,
	// deep fetch, probe, or other state change. A label delta with this origin
	// was first observed by polling, so its actor is unknown.
	OriginOther ChangeOrigin = iota
	// OriginEngine is a LocalLabelAdded/LocalLabelRemoved write-through — Fabrik
	// wrote this label.
	OriginEngine
	// OriginWebhook is an IssueLabeled/IssueUnlabeled delta from a webhook.
	OriginWebhook
	// OriginProbe is a mutation applied by the per-poll board probe loop
	// (runProbeAndDeepFetch) wrapped in FromProbe (#2080). The probe runs at the
	// start of the same poll that dispatches work, so waking the poll loop for its
	// own writes only buys a redundant early poll; the wake observer ignores it.
	OriginProbe
	// OriginLiveRead is a mutation a gate applied after a live GitHub read inside
	// the poll (FromLiveRead, #2350). The wake observer ignores it for the same
	// reason as OriginProbe.
	OriginLiveRead
)

// diffLabels returns the labels added to and removed from before to reach after.
func diffLabels(before, after []string) []LabelDelta {
	var out []LabelDelta
	for _, l := range after {
		if !containsString(before, l) {
			out = append(out, LabelDelta{Label: l, Added: true})
		}
	}
	for _, l := range before {
		if !containsString(after, l) {
			out = append(out, LabelDelta{Label: l, Added: false})
		}
	}
	return out
}

// eligibilityLabel reports whether adding or removing label changes whether the
// engine will move the item on by itself or is blocked on a human (#2120 R3).
func eligibilityLabel(label string) bool {
	switch label {
	case "fabrik:cruise", "fabrik:yolo", "fabrik:blocked", "fabrik:paused":
		return true
	}
	return strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":complete")
}

// stampEligibility records the store clock's "now" in item.EligibilityChangedAt
// when the label change from before to item.Labels touched an eligibility label,
// and in item.StatusMovedAt when the item moved between two known columns (#2120
// R3). Called only for an item the store already held, with s.mu held. The time
// comes from the store clock (SetClock) so a test or sim clock that drives the
// classifier drives the stamps too: with time.Now() a sim clock set before real
// time would put the stamp in the classifier's future.
func (s *Store) stampEligibility(item *ItemState, before ItemState) {
	now := s.nowLocked()
	for _, d := range diffLabels(before.Labels, item.Labels) {
		if eligibilityLabel(d.Label) {
			item.EligibilityChangedAt = now
			break
		}
	}
	if before.Status != "" && item.Status != "" && before.Status != item.Status {
		item.StatusMovedAt = now
	}
}

// SetClock makes the store stamp EligibilityChangedAt and StatusMovedAt from now
// instead of time.Now(). The engine passes its own clock so the stamps and the
// classifier that reads them agree. nil restores time.Now().
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.clock = now
	s.mu.Unlock()
}

// nowLocked returns the store clock's time; s.mu must be held.
func (s *Store) nowLocked() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// clearStatusFacts drops the in-memory attention facts a mutation has made stale
// (#2121): a pending retry and a gate hold end when the item is paused, closed,
// moves column or completes a stage; a conflict resolution ends when the item
// closes; the rebase cause ends with its fabrik:rebase-needed label. Returns
// StatusFactsChanged when anything was cleared. Called with the store lock held,
// for an item the store already held.
func clearStatusFacts(item *ItemState, before ItemState) ChangeFlags {
	var ended bool
	for _, d := range diffLabels(before.Labels, item.Labels) {
		if d.Added && (d.Label == "fabrik:paused" || (strings.HasPrefix(d.Label, "stage:") && strings.HasSuffix(d.Label, ":complete"))) {
			ended = true
		}
	}
	if item.IsClosed && !before.IsClosed {
		ended = true
		if item.ConflictResolution != nil {
			item.ConflictResolution = nil
		}
	}
	if before.Status != "" && item.Status != before.Status {
		ended = true
	}
	var changed bool
	if ended {
		if item.PendingRetry != nil {
			item.PendingRetry = nil
			changed = true
		}
		if item.GateHold != nil {
			item.GateHold = nil
			changed = true
		}
	}
	if item.RebaseCause != "" && !containsString(item.Labels, "fabrik:rebase-needed") {
		item.RebaseCause = ""
		changed = true
	}
	if changed || (item.IsClosed && !before.IsClosed && before.ConflictResolution != nil) {
		return StatusFactsChanged
	}
	return 0
}
