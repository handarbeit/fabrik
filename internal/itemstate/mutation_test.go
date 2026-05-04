package itemstate

import (
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
)

const testRepo = "owner/repo"

func newStoreWithItem(t *testing.T, repo string, number int) *Store {
	t.Helper()
	s := NewStore(nil)
	if _, _, err := s.Apply(IssueOpened{Item: testProjectItem(repo, number)}); err != nil {
		t.Fatalf("seed IssueOpened: %v", err)
	}
	return s
}

func getItem(t *testing.T, s *Store, repo string, number int) ItemState {
	t.Helper()
	snap, err := s.Get(repo, number)
	if err != nil {
		t.Fatalf("Get(%q, %d): %v", repo, number, err)
	}
	return snap.State()
}

func applyExpect(t *testing.T, s *Store, m Mutation, wantFlags ChangeFlags) Snapshot {
	t.Helper()
	snap, changes, err := s.Apply(m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if wantFlags != 0 {
		if len(changes) == 0 {
			t.Fatalf("Apply(%T): expected Change with flags %b, got no changes", m, wantFlags)
		}
		if changes[0].Fields&wantFlags == 0 {
			t.Errorf("Apply(%T): flags %b does not include expected %b", m, changes[0].Fields, wantFlags)
		}
	}
	return snap
}

// ---- IssueOpened ----

func TestApplyIssueOpened(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_abc"
	snap := applyExpect(t, s, IssueOpened{Item: pi}, TitleBodyChanged)
	st := snap.State()
	if st.Title != "Test issue" {
		t.Errorf("Title = %q; want %q", st.Title, "Test issue")
	}
	if st.Status != "Implement" {
		t.Errorf("Status = %q; want %q", st.Status, "Implement")
	}
	if st.Repo != testRepo || st.Number != 1 {
		t.Errorf("Repo/Number = %q/%d; want %q/1", st.Repo, st.Number, testRepo)
	}
}

// ---- IssueLabeled / IssueUnlabeled ----

func TestApplyIssueLabeled(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, IssueLabeled{Repo: testRepo, Number: 1, Label: "enhancement"}, LabelsChanged)
	st := getItem(t, s, testRepo, 1)
	if !containsString(st.Labels, "enhancement") {
		t.Error("label 'enhancement' not added")
	}
}

func TestApplyIssueUnlabeled(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1) // seeded with Labels: ["bug"]
	applyExpect(t, s, IssueUnlabeled{Repo: testRepo, Number: 1, Label: "bug"}, LabelsChanged)
	st := getItem(t, s, testRepo, 1)
	if containsString(st.Labels, "bug") {
		t.Error("label 'bug' not removed")
	}
}

// ---- IssueClosed / IssueReopened ----

func TestApplyIssueClosed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, IssueClosed{Repo: testRepo, Number: 1}, StateChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.IsClosed || st.State != "closed" {
		t.Error("IssueClosed did not set IsClosed/State")
	}
}

func TestApplyIssueReopened(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(IssueClosed{Repo: testRepo, Number: 1})
	applyExpect(t, s, IssueReopened{Repo: testRepo, Number: 1}, StateChanged)
	st := getItem(t, s, testRepo, 1)
	if st.IsClosed || st.State != "open" {
		t.Error("IssueReopened did not restore state")
	}
}

// ---- IssueCommentCreated ----

func TestApplyIssueCommentCreated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "c1", Body: "hello"}
	applyExpect(t, s, IssueCommentCreated{Repo: testRepo, Number: 1, Comment: c}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	found := false
	for _, got := range st.Comments {
		if got.ID == "c1" {
			found = true
		}
	}
	if !found {
		t.Error("comment not appended")
	}
}

// ---- PRReviewSubmitted ----

func TestApplyPRReviewSubmitted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	rev := gh.PRReview{Author: "bob", State: "APPROVED"}
	applyExpect(t, s, PRReviewSubmitted{Repo: testRepo, Number: 1, Review: rev}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.Reviews) == 0 {
		t.Fatal("LinkedPR.Reviews not set")
	}
	if st.LinkedPR.Reviews[0].Author != "bob" {
		t.Errorf("Reviews[0].Author = %q; want %q", st.LinkedPR.Reviews[0].Author, "bob")
	}
}

// ---- PRReviewCommentCreated ----

func TestApplyPRReviewCommentCreated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "rc1", Body: "nit"}
	applyExpect(t, s, PRReviewCommentCreated{Repo: testRepo, PRNumber: 1, Comment: c}, LinkedPRChanged|CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.ThreadComments) == 0 {
		t.Fatal("LinkedPR.ThreadComments not set")
	}
}

// ---- LocalStatusUpdated ----

func TestApplyLocalStatusUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"}, StatusChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Status != "Review" {
		t.Errorf("Status = %q; want Review", st.Status)
	}
}

// ---- LocalLabelAdded / LocalLabelRemoved ----

func TestApplyLocalLabelAdded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:yolo"}, LabelsChanged)
	if !containsString(getItem(t, s, testRepo, 1).Labels, "fabrik:yolo") {
		t.Error("label not added")
	}
}

func TestApplyLocalLabelRemoved(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:yolo"})
	applyExpect(t, s, LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "fabrik:yolo"}, LabelsChanged)
	if containsString(getItem(t, s, testRepo, 1).Labels, "fabrik:yolo") {
		t.Error("label not removed")
	}
}

// ---- LocalCommentAdded ----

func TestApplyLocalCommentAdded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "local1", Body: "Fabrik posted"}
	applyExpect(t, s, LocalCommentAdded{Repo: testRepo, Number: 1, Comment: c}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	found := false
	for _, got := range st.Comments {
		if got.ID == "local1" {
			found = true
		}
	}
	if !found {
		t.Error("local comment not appended")
	}
}

// ---- LocalLockAcquired / LocalLockReleased ----

func TestApplyLocalLockAcquired(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	acquiredAt := time.Now()
	w := &WorkerHandle{PID: 999, StageName: "Implement", StartedAt: acquiredAt}
	applyExpect(t, s, LocalLockAcquired{Repo: testRepo, Number: 1, User: "alice", Worker: w, AcquiredAt: acquiredAt}, LockChanged|WorkerChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Lock == nil || st.Lock.HolderUser != "alice" || !st.Lock.HeldByThis {
		t.Error("Lock not set correctly")
	}
	if !st.Lock.AcquiredAt.Equal(acquiredAt) {
		t.Error("Lock.AcquiredAt not set from mutation")
	}
	if st.Worker == nil || st.Worker.PID != 999 {
		t.Error("Worker not set correctly")
	}
}

func TestApplyLocalLockReleased(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "alice", Worker: nil, AcquiredAt: time.Now()})
	applyExpect(t, s, LocalLockReleased{Repo: testRepo, Number: 1}, LockChanged)
	if getItem(t, s, testRepo, 1).Lock != nil {
		t.Error("Lock not cleared")
	}
}

// ---- ItemDeepFetched ----

func TestApplyItemDeepFetched(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	fresh := testProjectItem(testRepo, 1)
	fresh.Title = "Updated Title"
	applyExpect(t, s, ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: fresh}, DeepFetchChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Title != "Updated Title" {
		t.Errorf("Title = %q; want Updated Title", st.Title)
	}
	if st.LastDeepFetchAt.IsZero() {
		t.Error("LastDeepFetchAt not set")
	}
}

// ---- StageAttempted ----

func TestApplyStageAttempted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	applyExpect(t, s, StageAttempted{Repo: testRepo, Number: 1, StageName: "Implement", At: at}, StageStateChanged)
	st := getItem(t, s, testRepo, 1)
	// StageAttempted only sets LastAttemptAt; it does NOT increment Attempts.
	// Attempts is incremented exclusively by StageRetryIncremented (on stage failure).
	if st.StageState.Attempts["Implement"] != 0 {
		t.Errorf("Attempts[Implement] = %d; want 0 (StageAttempted must not touch Attempts)", st.StageState.Attempts["Implement"])
	}
	if !st.StageState.LastAttemptAt["Implement"].Equal(at) {
		t.Error("LastAttemptAt not set")
	}
}

// ---- StageRetryIncremented ----

func TestApplyStageRetryIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	// StageAttempted does not increment Attempts; StageRetryIncremented does.
	s.Apply(StageAttempted{Repo: testRepo, Number: 1, StageName: "Review", At: time.Now()})
	applyExpect(t, s, StageRetryIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.Attempts["Review"] != 1 {
		t.Error("Attempts not incremented to 1")
	}
}

// ---- StageRetryCleared ----

func TestApplyStageRetryCleared(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(StageRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	applyExpect(t, s, StageRetryCleared{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.Attempts["Implement"] != 0 {
		t.Error("Attempts not cleared")
	}
}

// ---- ReviewCycleIncremented ----

func TestApplyReviewCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"] != 1 {
		t.Error("ReviewCycles not incremented")
	}
}

// ---- CIFixCycleIncremented ----

func TestApplyCIFixCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, CIFixCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.CIFixCycles["Validate"] != 1 {
		t.Error("CIFixCycles not incremented")
	}
}

// ---- RebaseCycleIncremented ----

func TestApplyRebaseCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, RebaseCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.RebaseCycles["Review"] != 1 {
		t.Error("RebaseCycles not incremented")
	}
}

// ---- EnginePaused ----

func TestApplyEnginePaused(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, EnginePaused{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if !getItem(t, s, testRepo, 1).StageState.PausedByEngine["Implement"] {
		t.Error("PausedByEngine not set")
	}
}

// ---- CooldownRecorded ----

func TestApplyCooldownRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	until := time.Now().Add(10 * time.Minute)
	applyExpect(t, s, CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until}, CooldownChanged)
	got := getItem(t, s, testRepo, 1).CooldownAt["retry"]
	if !got.Equal(until) {
		t.Errorf("CooldownAt[retry] = %v; want %v", got, until)
	}
}

// ---- WorkerHeartbeat / WorkerExited ----

func TestApplyWorkerHeartbeat(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
	at := time.Now()
	applyExpect(t, s, WorkerHeartbeat{Repo: testRepo, Number: 1, At: at}, WorkerChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Worker == nil || !st.Worker.LastSignAt.Equal(at) {
		t.Error("Worker.LastSignAt not set")
	}
}

func TestApplyWorkerExited(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
	applyExpect(t, s, WorkerExited{Repo: testRepo, Number: 1}, WorkerChanged)
	if getItem(t, s, testRepo, 1).Worker != nil {
		t.Error("Worker not cleared")
	}
}

// ---- InvocationRecorded ----

func TestApplyInvocationRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	usage := TokenUsage{InputTokens: 100, OutputTokens: 200, TurnsUsed: 5, MaxTurns: 50}
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		Completed: true,
		Blocked:   false,
		Usage:     usage,
	}, InvocationChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.LastInvocationCompleted {
		t.Error("LastInvocationCompleted not set")
	}
	if st.LastTokenUsage.InputTokens != 100 {
		t.Errorf("LastTokenUsage.InputTokens = %d; want 100", st.LastTokenUsage.InputTokens)
	}
}

func TestApplyInvocationRecordedIsComment(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		IsComment: true,
	}, InvocationChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.LastInvocationIsComment {
		t.Error("LastInvocationIsComment not set when IsComment=true")
	}

	// A subsequent stage-run invocation (IsComment=false) clears the flag.
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		IsComment: false,
	}, InvocationChanged)
	st = getItem(t, s, testRepo, 1)
	if st.LastInvocationIsComment {
		t.Error("LastInvocationIsComment should be false after IsComment=false invocation")
	}
}

// ---- DeepFetchFailed ----

func TestApplyDeepFetchFailed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	applyExpect(t, s, DeepFetchFailed{Repo: testRepo, Number: 1, At: at}, DeepFetchChanged)
	if !getItem(t, s, testRepo, 1).LastDeepFetchFailureAt.Equal(at) {
		t.Error("LastDeepFetchFailureAt not set")
	}
}

// ---- BaseBranchWarnRecorded ----

func TestApplyBaseBranchWarnRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, BaseBranchWarnRecorded{Repo: testRepo, Number: 1, Branch: "feat/x"}, BaseBranchChanged)
	if !getItem(t, s, testRepo, 1).BaseBranchWarned["feat/x"] {
		t.Error("BaseBranchWarned not set")
	}
}

// ---- BoardReconciled ----

func TestApplyBoardReconciled(t *testing.T) {
	s := NewStore(nil)
	items := []gh.ProjectItem{
		testProjectItem(testRepo, 10),
		testProjectItem(testRepo, 11),
	}
	_, changes, err := s.Apply(BoardReconciled{Items: items})
	if err != nil {
		t.Fatalf("Apply(BoardReconciled): %v", err)
	}
	if len(changes) != 2 {
		t.Errorf("changes = %d; want 2", len(changes))
	}
	// Items should now be in the store.
	for _, pi := range items {
		if _, err := s.Get(pi.Repo, pi.Number); err != nil {
			t.Errorf("Get(%d) after BoardReconciled: %v", pi.Number, err)
		}
	}
}

// ---- ProjectV2ItemEdited (itemID-based routing) ----

func TestApplyProjectV2ItemEdited(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_xyz"
	s.Apply(IssueOpened{Item: pi})

	applyExpect(t, s, ProjectV2ItemEdited{ItemID: "PVTI_xyz", NewStatus: "Review"}, StatusChanged)
	if getItem(t, s, testRepo, 1).Status != "Review" {
		t.Error("Status not updated via ProjectV2ItemEdited")
	}
}

// ---- CheckRunCompleted (SHA-based routing) ----

func TestApplyCheckRunCompleted(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 10
	s.Apply(IssueOpened{Item: pi})
	// Directly set HeadSHA via a mutation that exercises the field.
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{Number: 10}
	}
	item.LinkedPR.HeadSHA = "abc123"
	s.shaToKey["abc123"] = key
	s.mu.Unlock()

	run := gh.CheckRun{ID: 1, Name: "CI", Status: "completed", Conclusion: "success"}
	applyExpect(t, s, CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: run}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.CheckRuns) == 0 {
		t.Fatal("CheckRuns not set")
	}
	if !st.LinkedPR.HasHadChecks {
		t.Error("HasHadChecks not set")
	}
}

// ---- ChangeFlags coverage: every flag must be exercised ----

func TestChangeFlagsCoverage(t *testing.T) {
	// Maps every ChangeFlag to a mutation that should produce it.
	type flagCase struct {
		flag ChangeFlags
		name string
		mut  func(s *Store, number int) Mutation
	}
	cases := []flagCase{
		{StatusChanged, "StatusChanged", func(s *Store, n int) Mutation {
			return LocalStatusUpdated{Repo: testRepo, Number: n, NewStatus: "Review"}
		}},
		{LabelsChanged, "LabelsChanged", func(s *Store, n int) Mutation {
			return IssueLabeled{Repo: testRepo, Number: n, Label: "new-label"}
		}},
		{LockChanged, "LockChanged", func(s *Store, n int) Mutation {
			return LocalLockAcquired{Repo: testRepo, Number: n, User: "u"}
		}},
		{StageStateChanged, "StageStateChanged", func(s *Store, n int) Mutation {
			return StageAttempted{Repo: testRepo, Number: n, StageName: "S", At: time.Now()}
		}},
		{WorkerChanged, "WorkerChanged/Heartbeat", func(s *Store, n int) Mutation {
			now := time.Now()
			s.Apply(LocalLockAcquired{Repo: testRepo, Number: n, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
			return WorkerHeartbeat{Repo: testRepo, Number: n, At: now}
		}},
		{WorkerChanged, "WorkerChanged/PIDSet", func(s *Store, n int) Mutation {
			now := time.Now()
			s.Apply(LocalLockAcquired{Repo: testRepo, Number: n, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
			return WorkerPIDSet{Repo: testRepo, Number: n, PID: 12345}
		}},
		{CooldownChanged, "CooldownChanged", func(s *Store, n int) Mutation {
			return CooldownRecorded{Repo: testRepo, Number: n, Reason: "r", Until: time.Now().Add(time.Minute)}
		}},
		{LinkedPRChanged, "LinkedPRChanged", func(s *Store, n int) Mutation {
			return PRReviewSubmitted{Repo: testRepo, Number: n, Review: gh.PRReview{Author: "r"}}
		}},
		{CommentsChanged, "CommentsChanged", func(s *Store, n int) Mutation {
			return IssueCommentCreated{Repo: testRepo, Number: n, Comment: gh.Comment{ID: "x"}}
		}},
		{AssigneesChanged, "AssigneesChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.Assignees = []string{"bob", "carol"}
			return IssueOpened{Item: pi}
		}},
		{TitleBodyChanged, "TitleBodyChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.Title = "Changed Title"
			return IssueOpened{Item: pi}
		}},
		{StateChanged, "StateChanged", func(s *Store, n int) Mutation {
			return IssueClosed{Repo: testRepo, Number: n}
		}},
		{BlockedByChanged, "BlockedByChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.BlockedBy = []gh.Dependency{{Number: 99}}
			return IssueOpened{Item: pi}
		}},
		{DeepFetchChanged, "DeepFetchChanged", func(s *Store, n int) Mutation {
			return DeepFetchFailed{Repo: testRepo, Number: n, At: time.Now()}
		}},
		{InvocationChanged, "InvocationChanged", func(s *Store, n int) Mutation {
			return InvocationRecorded{Repo: testRepo, Number: n, Completed: true}
		}},
		{BaseBranchChanged, "BaseBranchChanged", func(s *Store, n int) Mutation {
			return BaseBranchWarnRecorded{Repo: testRepo, Number: n, Branch: "b"}
		}},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(nil)
			number := i + 100 // unique number per sub-test
			s.Apply(IssueOpened{Item: testProjectItem(testRepo, number)})

			_, changes, err := s.Apply(c.mut(s, number))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(changes) == 0 {
				t.Fatalf("expected Change with flag %b, got no changes", c.flag)
			}
			if changes[0].Fields&c.flag == 0 {
				t.Errorf("Change.Fields %b does not include %b", changes[0].Fields, c.flag)
			}
		})
	}
}

// ---- I6: No-op mutation idempotency ----

// TestNoOpMutationProducesNoChange verifies that applying an identical mutation
// twice produces no observer event the second time (invariant I6).
func TestNoOpMutationProducesNoChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	// First application: changes status.
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))

	// Second application: same status — should be a no-op.
	_, changes, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	if err != nil {
		t.Fatalf("Apply no-op: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("no-op Apply returned %d changes; want 0", len(changes))
	}
	if fired != 0 {
		t.Errorf("observer fired %d times for no-op mutation; want 0", fired)
	}
}

// TestNoOpLabelAddProducesNoChange verifies label idempotency.
func TestNoOpLabelAddProducesNoChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	// "bug" is already in the seed item labels.
	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))

	_, changes, _ := s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "bug"})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("label no-op produced change (changes=%d, fired=%d)", len(changes), fired)
	}
}

// TestNoOpCooldownSameValue verifies CooldownRecorded is a no-op when value unchanged.
func TestNoOpCooldownSameValue(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	until := time.Unix(9999, 0)
	s.Apply(CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))
	_, changes, _ := s.Apply(CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("cooldown no-op produced change (changes=%d, fired=%d)", len(changes), fired)
	}
}

// ---- CIMergePendingStarted / CIMergePendingCleared ----

func TestApplyCIMergePendingStarted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Unix(12345, 0)
	applyExpect(t, s, CIMergePendingStarted{Repo: testRepo, Number: 1, At: at}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil {
		t.Fatal("LinkedPR is nil after CIMergePendingStarted")
	}
	if !st.LinkedPR.CIMergePendingSince.Equal(at) {
		t.Errorf("CIMergePendingSince = %v; want %v", st.LinkedPR.CIMergePendingSince, at)
	}
}

func TestApplyCIMergePendingCleared(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Unix(12345, 0)
	s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 1, At: at})
	applyExpect(t, s, CIMergePendingCleared{Repo: testRepo, Number: 1}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR != nil && !st.LinkedPR.CIMergePendingSince.IsZero() {
		t.Errorf("CIMergePendingSince not cleared: %v", st.LinkedPR.CIMergePendingSince)
	}
}

func TestApplyCIMergePendingClearedWithNoLinkedPR(t *testing.T) {
	// CIMergePendingCleared on an item with no LinkedPR should be a no-op (not panic).
	s := newStoreWithItem(t, testRepo, 1)
	_, changes, err := s.Apply(CIMergePendingCleared{Repo: testRepo, Number: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// No-op: LinkedPR is nil so clearing is trivially done; we allow no changes.
	_ = changes
}

// ---- PRChecksObserved ----

func TestApplyPRChecksObserved(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, PRChecksObserved{Repo: testRepo, Number: 1}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || !st.LinkedPR.HasHadChecks {
		t.Error("HasHadChecks not set after PRChecksObserved")
	}
}

func TestApplyPRChecksObservedIsMonotonic(t *testing.T) {
	// Applying PRChecksObserved twice: second should be no-op (stays true).
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(PRChecksObserved{Repo: testRepo, Number: 1})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))
	_, changes, _ := s.Apply(PRChecksObserved{Repo: testRepo, Number: 1})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("second PRChecksObserved was not a no-op (changes=%d, fired=%d)", len(changes), fired)
	}
}

// ---- ItemDeepFetched clears LastDeepFetchFailureAt ----

func TestItemDeepFetchedClearsFailureAt(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	s.Apply(DeepFetchFailed{Repo: testRepo, Number: 1, At: at})
	if st := getItem(t, s, testRepo, 1); st.LastDeepFetchFailureAt.IsZero() {
		t.Fatal("precondition: LastDeepFetchFailureAt not set")
	}

	fresh := testProjectItem(testRepo, 1)
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: fresh})
	if st := getItem(t, s, testRepo, 1); !st.LastDeepFetchFailureAt.IsZero() {
		t.Errorf("LastDeepFetchFailureAt not cleared by ItemDeepFetched: %v", st.LastDeepFetchFailureAt)
	}
}

// ---- Snapshot immutability for LinkedPR fields ----

func TestHeldSnapshotLinkedPRFieldsUnchanged(t *testing.T) {
	// Verify that mutations to CIMergePendingSince and HasHadChecks do not affect
	// a held Snapshot taken before the mutation.
	s := newStoreWithItem(t, testRepo, 99)
	held, _ := s.Get(testRepo, 99)
	// held.LinkedPR() is nil at this point.

	s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 99, At: time.Unix(999, 0)})
	s.Apply(PRChecksObserved{Repo: testRepo, Number: 99})

	// Held snapshot should still have nil LinkedPR.
	if lpr := held.LinkedPR(); lpr != nil {
		t.Errorf("held snapshot LinkedPR became non-nil after mutations: %+v", lpr)
	}
}
