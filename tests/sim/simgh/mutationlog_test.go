package simgh

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// statusMove resolves the identifiers UpdateProjectItemStatus needs and moves
// the board's single card to the named column through the instrumented client.
func statusMove(t *testing.T, in *Instrumented, column string) {
	t.Helper()
	item := firstItem(t, in.Sim())
	field, err := in.Sim().FetchStatusField(item.projectID)
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}
	if err := in.UpdateProjectItemStatus(item.projectID, item.itemID, field.FieldID, field.Options[column]); err != nil {
		t.Fatalf("UpdateProjectItemStatus(%s): %v", column, err)
	}
}

// TestMutationLogAnswersAnOrderingQuery is AC6.
//
// The guarantee it stands in for is ADR-060's: fabrik:awaiting-done is applied
// as the *very first* mutation after the markers are parsed, so the decision
// survives a failure of everything that follows. That is an ordering claim, and
// no amount of reading terminal labels can assert it — both orders leave the
// same final state.
//
// The second half is what makes the first non-vacuous: the same query run
// against the opposite order must return the opposite answer.
func TestMutationLogAnswersAnOrderingQuery(t *testing.T) {
	labelFirst := LabelAdded(7, "fabrik:awaiting-done")
	statusUpdate := MethodIs("UpdateProjectItemStatus")

	t.Run("label before status", func(t *testing.T) {
		in, _ := newInstrumented(t)
		if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err != nil {
			t.Fatalf("AddLabelToIssue: %v", err)
		}
		statusMove(t, in, "Done")

		got, err := in.Log().Precedes(labelFirst, statusUpdate)
		if err != nil {
			t.Fatalf("Precedes: %v", err)
		}
		if !got {
			t.Errorf("Precedes(awaiting-done add, status update) = false, want true\nlog:\n%s", in.Log().Dump(20))
		}
	})

	t.Run("status before label", func(t *testing.T) {
		in, _ := newInstrumented(t)
		statusMove(t, in, "Done")
		if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err != nil {
			t.Fatalf("AddLabelToIssue: %v", err)
		}

		got, err := in.Log().Precedes(labelFirst, statusUpdate)
		if err != nil {
			t.Fatalf("Precedes: %v", err)
		}
		if got {
			t.Errorf("Precedes(awaiting-done add, status update) = true after reordering, want false\nlog:\n%s", in.Log().Dump(20))
		}
	})
}

// A predicate matching nothing must be an error, not a false. Answering false
// would make "the awaiting-done add precedes the status update" keep passing
// after the awaiting-done add stopped happening at all — precisely the
// regression the assertion exists to catch.
func TestPrecedesErrorsWhenAPredicateMatchesNothing(t *testing.T) {
	in, _ := newInstrumented(t)
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	t.Run("first predicate unmatched", func(t *testing.T) {
		_, err := in.Log().Precedes(MethodIs("ArchiveProjectItem"), LabelAdded(7, "fabrik:awaiting-done"))
		if err == nil {
			t.Fatal("Precedes returned nil error for a predicate matching nothing")
		}
		if !strings.Contains(err.Error(), "first predicate") {
			t.Errorf("error %q does not say which predicate failed", err)
		}
	})

	t.Run("second predicate unmatched", func(t *testing.T) {
		_, err := in.Log().Precedes(LabelAdded(7, "fabrik:awaiting-done"), MethodIs("ArchiveProjectItem"))
		if err == nil {
			t.Fatal("Precedes returned nil error for a predicate matching nothing")
		}
		if !strings.Contains(err.Error(), "second predicate") {
			t.Errorf("error %q does not say which predicate failed", err)
		}
	})

	t.Run("IndexOf too", func(t *testing.T) {
		if _, err := in.Log().IndexOf(MethodIs("ArchiveProjectItem")); err == nil {
			t.Fatal("IndexOf returned nil error for a predicate matching nothing")
		}
	})
}

// The log records reads as well as mutations — its contract is "every
// intercepted call", not "every mutation". Mutations() is the narrower view
// R4's ordering questions are asked of.
func TestLogRecordsReadsAndMutationsSeparately(t *testing.T) {
	in, _ := newInstrumented(t)

	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:paused"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}

	if got := in.Log().Len(); got != 3 {
		t.Fatalf("log has %d entries, want 3\n%s", got, in.Log().Dump(20))
	}
	muts := in.Log().Mutations()
	if len(muts) != 1 || muts[0].Method != "AddLabelToIssue" {
		t.Errorf("Mutations() = %v, want exactly the AddLabelToIssue entry", muts)
	}
	reads := in.Log().ByMethod("FetchLabels")
	if len(reads) != 2 {
		t.Errorf("ByMethod(FetchLabels) returned %d entries, want 2", len(reads))
	}
	for _, e := range reads {
		if e.Mutation {
			t.Errorf("FetchLabels entry marked as a mutation: %s", e)
		}
	}
}

func TestLogQueriesByIssueAndTail(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Sim().SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "other", Status: "Implement"})
	if err := in.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, n := range []int{7, 8, 7} {
		if err := in.AddLabelToIssue("acme", "widgets", n, "fabrik:paused"); err != nil {
			t.Fatalf("AddLabelToIssue(#%d): %v", n, err)
		}
	}

	if got := len(in.Log().ByIssue(7)); got != 2 {
		t.Errorf("ByIssue(7) returned %d entries, want 2", got)
	}
	if got := len(in.Log().ByIssue(8)); got != 1 {
		t.Errorf("ByIssue(8) returned %d entries, want 1", got)
	}

	tail := in.Log().Tail(2)
	if len(tail) != 2 {
		t.Fatalf("Tail(2) returned %d entries, want 2", len(tail))
	}
	if tail[0].Seq != 1 || tail[1].Seq != 2 {
		t.Errorf("Tail(2) returned sequences %d,%d, want 1,2", tail[0].Seq, tail[1].Seq)
	}
	if got := len(in.Log().Tail(99)); got != 3 {
		t.Errorf("Tail(99) returned %d entries, want all 3", got)
	}
}

// Args.Repo must carry the bare repository name for *every* method, so one
// FailWhen predicate or log query works across all of them.
//
// FetchItemDetails is the only wrapper that has to derive owner and repo rather
// than being handed them, because gh.ProjectItem.Repo is the composite
// "owner/repo" — and it is also the call the settle scans retry, so it is the
// likeliest target of a narrowed fault. The reflection-driven completeness
// tests invoke it with a zero-valued item and so cannot see this at all.
func TestFetchItemDetailsSplitsTheCompositeRepo(t *testing.T) {
	in, _ := newInstrumented(t)

	item, err := in.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if item.Repo != "acme/widgets" {
		t.Fatalf("ProjectItem.Repo = %q, want the composite form this test exists to split", item.Repo)
	}
	in.Log().Reset()

	if err := in.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	entries := in.Log().ByMethod("FetchItemDetails")
	if len(entries) != 1 {
		t.Fatalf("logged %d FetchItemDetails entries, want 1", len(entries))
	}
	if got := entries[0].Args; got.Owner != "acme" || got.Repo != "widgets" {
		t.Errorf("Args = {Owner:%q Repo:%q}, want {acme widgets} — a composite here would make a "+
			"FailWhen narrowed on Repo silently never match", got.Owner, got.Repo)
	}

	// And the narrowing a settle-scan scenario would actually write must fire.
	in.Faults().FailWhen("FetchItemDetails",
		func(a Args) bool { return a.Repo == "widgets" && a.Number == 7 },
		Always, ErrRateLimit())
	if err := in.FetchItemDetails(item); err == nil {
		t.Error("a fault narrowed on the bare repo name did not fire on FetchItemDetails")
	}
}

// Sequence numbers are the log's ordering, so they must be dense and
// monotonic from zero — Precedes compares them directly.
func TestLogSequencesAreDenseAndMonotonic(t *testing.T) {
	in, _ := newInstrumented(t)
	for i := 0; i < 5; i++ {
		if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
			t.Fatalf("FetchLabels: %v", err)
		}
	}
	for i, e := range in.Log().Entries() {
		if e.Seq != i {
			t.Fatalf("entry at index %d has Seq %d", i, e.Seq)
		}
		if e.Pending {
			t.Fatalf("entry %d is still pending after the call returned: %s", i, e)
		}
	}
}

func TestLogReset(t *testing.T) {
	in, _ := newInstrumented(t)
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	in.Log().Reset()
	if got := in.Log().Len(); got != 0 {
		t.Fatalf("Len() = %d after Reset, want 0", got)
	}
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if got := in.Log().Entries()[0].Seq; got != 0 {
		t.Errorf("first entry after Reset has Seq %d, want 0", got)
	}
}

// An entry must carry the error the model returned, not just the fact that
// something went wrong — the harness prints these when an AdvanceUntil
// exhausts, and "something failed" is not a debugging aid.
func TestLogRecordsModelErrors(t *testing.T) {
	in, _ := newInstrumented(t)
	if _, err := in.FetchLabels("acme", "nosuch", 7); err == nil {
		t.Fatal("FetchLabels on an unseeded repo returned no error")
	}
	entries := in.Log().ByMethod("FetchLabels")
	if len(entries) != 1 {
		t.Fatalf("got %d FetchLabels entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Err == nil {
		t.Fatal("entry recorded no error for a failed call")
	}
	if e.Injected {
		t.Error("a model-side failure was recorded as injected")
	}
	if !strings.Contains(e.String(), "err:") {
		t.Errorf("Entry.String() = %q, want it to show the error", e.String())
	}
}

// The log's timestamps come from the injected clock, like every other
// time-bearing value in the package. A wall-clock read here would make an
// entry's At incomparable with the label applied-at values the engine's gates
// anchor on.
func TestLogTimestampsComeFromTheInjectedClock(t *testing.T) {
	in, clk := newInstrumented(t)
	before := clk.Now()
	if err := in.AddLabelToIssue("acme", "widgets", 7, "a"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	clk.Advance(90 * time.Minute)
	if err := in.AddLabelToIssue("acme", "widgets", 7, "b"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	entries := in.Log().Entries()
	if !entries[0].At.Equal(before) {
		t.Errorf("first entry At = %v, want the clock's reading %v", entries[0].At, before)
	}
	if got := entries[1].At.Sub(entries[0].At); got != 90*time.Minute {
		t.Errorf("entries are %v apart, want 90m — the log is not reading the injected clock", got)
	}
}

// TestEveryInterfaceMethodIsInstrumented is the drift detector for the
// decorator, and the reason *Sim is held as a named field rather than embedded.
//
// The compile-time assertion in instrumented.go proves *Instrumented satisfies
// engine.GitHubClient; it cannot prove each method actually goes through the
// wrapper. This does: it invokes all sixty by reflection and asserts each
// produced exactly one log entry named for itself. A wrapper that delegated
// without logging, or logged under a copy-pasted neighbour's name, fails here.
func TestEveryInterfaceMethodIsInstrumented(t *testing.T) {
	names := InterfaceMethods()
	if len(names) < 60 {
		t.Fatalf("reflection found only %d engine.GitHubClient methods; the interface should have ~60", len(names))
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			in, _ := newInstrumented(t)
			callInterfaceMethod(t, in, name)

			entries := in.Log().Entries()
			if len(entries) != 1 {
				t.Fatalf("calling %s produced %d log entries, want exactly 1\n%s", name, len(entries), in.Log().Dump(10))
			}
			if entries[0].Method != name {
				t.Errorf("calling %s logged method %q — a copy-pasted method name in the wrapper", name, entries[0].Method)
			}
			if entries[0].Pending {
				t.Errorf("calling %s left its log entry pending", name)
			}
		})
	}
}

// Mutation classification is not derivable by reflection, so it is asserted
// against an explicit list. A read misclassified as a mutation would pollute
// Mutations(), which is the view every R4 ordering assertion is built on.
func TestMutationFlagMatchesTheInterfaceSemantics(t *testing.T) {
	wantMutations := map[string]bool{
		"AddBlockedByIssue": true, "AddComment": true, "AddCommentReaction": true,
		"AddLabelToIssue": true, "AddPRReviewCommentReaction": true, "AddProjectV2ItemById": true,
		"AddReviewRequest": true, "ArchiveProjectItem": true, "CloseIssue": true,
		"CreateDraftPR": true, "CreateIssue": true, "CreatePR": true,
		"DeleteForwardingHooks": true, "DeleteReviewRequest": true,
		"DequeuePullRequest": true, "DisablePullRequestAutoMerge": true,
		"EnablePullRequestAutoMerge": true, "EnqueuePullRequest": true,
		"MarkPRReady": true, "MergePR": true, "RemoveLabelFromIssue": true,
		"ReopenIssue": true, "ResolveReviewThread": true, "SeedLabels": true, "UpdateComment": true,
		"UpdateIssueBody": true, "UpdatePRBase": true, "UpdateProjectItemStatus": true,
	}

	for _, name := range InterfaceMethods() {
		t.Run(name, func(t *testing.T) {
			in, _ := newInstrumented(t)
			callInterfaceMethod(t, in, name)
			entries := in.Log().Entries()
			if len(entries) != 1 {
				t.Fatalf("calling %s produced %d log entries, want 1", name, len(entries))
			}
			if got := entries[0].Mutation; got != wantMutations[name] {
				t.Errorf("%s logged Mutation=%v, want %v", name, got, wantMutations[name])
			}
		})
	}
}

// complete's out-of-range guard is unreachable through Instrumented, which
// always hands back the sequence reserve returned. Asserting it panics keeps a
// future direct caller from silently discarding an outcome.
func TestCompleteRejectsAnUnknownSequence(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("complete with an out-of-range sequence did not panic")
		}
	}()
	NewMutationLog().complete(42, errors.New("boom"), false)
}

func TestDumpOfAnEmptyLogIsReadable(t *testing.T) {
	if got := NewMutationLog().Dump(10); !strings.Contains(got, "empty") {
		t.Errorf("Dump on an empty log = %q, want it to say so", got)
	}
}
