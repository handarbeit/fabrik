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
	// WorkerChanged indicates the Worker handle was set or cleared.
	WorkerChanged
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
}
