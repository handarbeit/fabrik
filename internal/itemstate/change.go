package itemstate

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
	// a SelfWriteObserved mutation (#1090). Pure bookkeeping for the probe
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
