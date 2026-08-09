package simgh

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// This file is the mutation log: an ordered, queryable record of what the
// engine did to the model.
//
// # It records attempts, not mutations
//
// The name is R4's, but the contract is wider than the name suggests: **every
// intercepted call gets an entry, whether it succeeded, failed inside the
// model, or was failed by the fault injector**, and reads are recorded
// alongside mutations. That is not incidental — a log of mutations that
// happened could not show "N failed attempts followed by the success", which
// is the whole point of asserting against an injected fault. Entry.Mutation
// separates the two populations, and Mutations() is the view R4's ordering
// questions are asked of.
//
// # Two consumers
//
// Scenarios asserting ordering guarantees — several of the engine's contracts
// are ordering claims rather than state claims (ADR-060 specifies that
// fabrik:awaiting-done is the *very first* mutation after the markers are
// parsed, so the decision survives a failure of everything that follows), and
// no amount of reading terminal labels can assert that. And the harness,
// printing recent activity when an AdvanceUntil exhausts: a deterministic bed
// that fails without saying what happened costs more to debug than the live
// bed it replaces.
//
// # Ordering contract
//
// Entries are appended in two phases: a slot is reserved *before* the
// underlying call and completed *after* it. Appending only on return would let
// two concurrent calls be logged in the opposite order from the one in which
// they reached the model. Reserving first makes ordering:
//
//   - **exact within a goroutine** — which is what every real ordering
//     assertion is, since ADR-060's guarantee is one worker's sequential code;
//   - **reserve-order, best effort, across goroutines** — two workers whose
//     calls interleave inside the model are ordered by which reserved its slot
//     first, which is as close to a happens-before as an out-of-band log can
//     get.
//
// Recorded in FIDELITY.md rather than left implicit, because a cross-goroutine
// ordering assertion that looks exact and is not is a flaky test waiting to
// happen.
//
// # Locking
//
// MutationLog owns its own mutex and never holds it across a call into Sim.
// It is therefore a leaf in the package's lock ordering: it may be acquired
// while no Sim lock is held (which is the only way Instrumented uses it), and
// Sim's own code never touches it. See sim.go's package doc.

// Args carries the identifying arguments of an intercepted call. Its fields
// are deliberately generic rather than per-method, so all sixty wrappers stay
// mechanically uniform and a diff scan is enough to review them.
//
// Zero values mean "not applicable to this method", not "empty" — a query
// filtering on Number == 0 matches every non-issue-scoped call.
type Args struct {
	Owner string
	Repo  string
	// Number is the issue or PR number the call targets, 0 when the method is
	// not issue- or PR-scoped.
	Number int
	// Label is the label name for label mutations.
	Label string
	// SHA is a commit SHA or a ref (FetchCombinedStatus accepts either).
	SHA string
	// ID is whichever opaque identifier the method takes: project ID, item ID,
	// status field/option ID, review thread ID, or node ID. Methods taking two
	// (UpdateProjectItemStatus) put the primary one here and the rest in Values.
	ID string
	// Body is a comment or issue body.
	Body string
	// Values holds any remaining call-specific strings — reviewer logins, a
	// status option ID, a branch name, a merge strategy.
	Values []string
}

// String renders the arguments compactly for harness debugging output. Only
// the fields the call actually set appear.
func (a Args) String() string {
	var parts []string
	if a.Owner != "" || a.Repo != "" {
		parts = append(parts, a.Owner+"/"+a.Repo)
	}
	if a.Number != 0 {
		parts = append(parts, fmt.Sprintf("#%d", a.Number))
	}
	if a.Label != "" {
		parts = append(parts, "label="+a.Label)
	}
	if a.SHA != "" {
		parts = append(parts, "sha="+a.SHA)
	}
	if a.ID != "" {
		parts = append(parts, "id="+a.ID)
	}
	if len(a.Values) > 0 {
		parts = append(parts, strings.Join(a.Values, ","))
	}
	if a.Body != "" {
		parts = append(parts, fmt.Sprintf("body=%dB", len(a.Body)))
	}
	return strings.Join(parts, " ")
}

// Entry is one intercepted call.
type Entry struct {
	// Seq is the entry's position in reserve order, starting at 0. It is also
	// its index in the slice Entries returns.
	Seq int
	// Method is the engine.GitHubClient method name, e.g. "AddLabelToIssue".
	Method string
	Args   Args
	// Mutation distinguishes state-changing calls from reads.
	Mutation bool
	// At is the injected clock's reading when the slot was reserved.
	At time.Time
	// Err is the call's error, nil on success.
	Err error
	// Injected reports that Err came from the fault table rather than from the
	// model — the difference between "the scenario made this fail" and "the
	// model refused it".
	Injected bool
	// Pending reports that the slot was reserved but never completed: the
	// goroutine is still inside the call, or it panicked. A completed sweep
	// with pending entries left is a bug in the wrapper, and the concurrency
	// test asserts against it.
	Pending bool
}

// Failed reports whether the call returned an error, from either source.
func (e Entry) Failed() bool { return e.Err != nil }

// String renders one entry as a single line for harness debugging output.
func (e Entry) String() string {
	kind := "read"
	if e.Mutation {
		kind = "mutate"
	}
	outcome := "ok"
	switch {
	case e.Pending:
		outcome = "pending"
	case e.Injected:
		outcome = "INJECTED: " + e.Err.Error()
	case e.Err != nil:
		outcome = "err: " + e.Err.Error()
	}
	return fmt.Sprintf("%4d %-6s %-30s %-40s %s", e.Seq, kind, e.Method, e.Args.String(), outcome)
}

// MutationLog is the ordered record. The zero value is ready to use.
type MutationLog struct {
	mu      sync.Mutex
	entries []Entry
}

// NewMutationLog returns an empty log.
func NewMutationLog() *MutationLog { return &MutationLog{} }

// reserve appends a pending entry and returns its sequence number. It is the
// first half of the two-phase append described in this file's doc comment.
func (l *MutationLog) reserve(method string, mutation bool, args Args, at time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := len(l.entries)
	l.entries = append(l.entries, Entry{
		Seq:      seq,
		Method:   method,
		Args:     args,
		Mutation: mutation,
		At:       at,
		Pending:  true,
	})
	return seq
}

// complete fills in a reserved slot's outcome.
func (l *MutationLog) complete(seq int, err error, injected bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < 0 || seq >= len(l.entries) {
		// Unreachable through Instrumented, which always passes back the
		// sequence reserve just returned. Panicking rather than ignoring it
		// keeps a future caller from silently losing an outcome.
		panic(fmt.Sprintf("simgh: MutationLog.complete: sequence %d out of range (len %d)", seq, len(l.entries)))
	}
	l.entries[seq].Err = err
	l.entries[seq].Injected = injected
	l.entries[seq].Pending = false
}

// Entries returns every entry in reserve order.
func (l *MutationLog) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Len reports how many calls have been intercepted.
func (l *MutationLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Reset empties the log. Sequence numbers restart at 0, so an index captured
// before a reset is meaningless after one.
func (l *MutationLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

// Find returns every entry matching pred, in order.
func (l *MutationLog) Find(pred func(Entry) bool) []Entry {
	var out []Entry
	for _, e := range l.Entries() {
		if pred(e) {
			out = append(out, e)
		}
	}
	return out
}

// Mutations returns only the state-changing calls — R4's view of the log,
// with reads filtered out.
func (l *MutationLog) Mutations() []Entry {
	return l.Find(func(e Entry) bool { return e.Mutation })
}

// ByMethod returns every call to the named method.
func (l *MutationLog) ByMethod(method string) []Entry {
	return l.Find(MethodIs(method))
}

// ByIssue returns every call carrying the given issue or PR number.
func (l *MutationLog) ByIssue(number int) []Entry {
	return l.Find(func(e Entry) bool { return e.Args.Number == number })
}

// Tail returns the last n entries, or all of them when there are fewer. This
// is what the harness prints when an AdvanceUntil exhausts.
func (l *MutationLog) Tail(n int) []Entry {
	all := l.Entries()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// Dump renders the last n entries as newline-separated lines, for a failure
// message.
func (l *MutationLog) Dump(n int) string {
	entries := l.Tail(n)
	if len(entries) == 0 {
		return "(mutation log empty)"
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.String())
	}
	return strings.Join(lines, "\n")
}

// IndexOf returns the sequence number of the first entry matching pred.
//
// It errors rather than returning a sentinel when nothing matches, for the
// same reason Precedes does: a predicate that matches nothing is almost always
// a mis-specified query, and answering it with a number would let the caller
// carry on as though it had found something.
func (l *MutationLog) IndexOf(pred func(Entry) bool) (int, error) {
	for _, e := range l.Entries() {
		if pred(e) {
			return e.Seq, nil
		}
	}
	return -1, fmt.Errorf("simgh: mutation log has no entry matching the predicate (%d entries recorded)", l.Len())
}

// Precedes reports whether the first entry matching a comes before the first
// entry matching b.
//
// It returns an error when *either* predicate matches nothing. Answering false
// in that case would be the canonical false pass for an ordering assertion: a
// test asserting "the fabrik:awaiting-done add precedes the status update"
// would pass unchanged after the awaiting-done add stopped happening at all,
// which is precisely the regression the assertion exists to catch.
func (l *MutationLog) Precedes(a, b func(Entry) bool) (bool, error) {
	ai, err := l.IndexOf(a)
	if err != nil {
		return false, fmt.Errorf("simgh: Precedes: first predicate: %w", err)
	}
	bi, err := l.IndexOf(b)
	if err != nil {
		return false, fmt.Errorf("simgh: Precedes: second predicate: %w", err)
	}
	return ai < bi, nil
}

// --- predicate helpers -----------------------------------------------------
//
// These exist so ordering assertions read as claims about the engine rather
// than as struct-field comparisons. They compose with Find, IndexOf and
// Precedes.

// MethodIs matches every call to the named method.
func MethodIs(method string) func(Entry) bool {
	return func(e Entry) bool { return e.Method == method }
}

// OnIssue matches every call carrying the given issue or PR number.
func OnIssue(number int) func(Entry) bool {
	return func(e Entry) bool { return e.Args.Number == number }
}

// LabelAdded matches the AddLabelToIssue call that applied label to number.
func LabelAdded(number int, label string) func(Entry) bool {
	return And(MethodIs("AddLabelToIssue"), OnIssue(number), func(e Entry) bool { return e.Args.Label == label })
}

// LabelRemoved matches the RemoveLabelFromIssue call that removed label from
// number.
func LabelRemoved(number int, label string) func(Entry) bool {
	return And(MethodIs("RemoveLabelFromIssue"), OnIssue(number), func(e Entry) bool { return e.Args.Label == label })
}

// Succeeded matches entries whose call returned no error.
func Succeeded() func(Entry) bool {
	return func(e Entry) bool { return !e.Pending && e.Err == nil }
}

// Errored matches entries whose call returned an error, from either source.
func Errored() func(Entry) bool {
	return func(e Entry) bool { return e.Err != nil }
}

// WasInjected matches entries the fault table failed.
func WasInjected() func(Entry) bool {
	return func(e Entry) bool { return e.Injected }
}

// And composes predicates conjunctively.
func And(preds ...func(Entry) bool) func(Entry) bool {
	return func(e Entry) bool {
		for _, p := range preds {
			if !p(e) {
				return false
			}
		}
		return true
	}
}
