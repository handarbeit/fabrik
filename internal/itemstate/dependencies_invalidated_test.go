package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func deepFetched(t *testing.T, s *Store, n int) {
	t.Helper()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	applyExpect(t, s, ItemDeepFetched{Repo: testRepo, Number: n, FreshState: gh.ProjectItem{
		Repo: testRepo, Number: n, UpdatedAt: at, LinkedPRUpdatedAt: at,
	}}, DeepFetchChanged)
}

func applyFlags(t *testing.T, s *Store, m Mutation) ChangeFlags {
	t.Helper()
	_, changes, err := s.Apply(m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var f ChangeFlags
	for _, c := range changes {
		f |= c.Fields
	}
	return f
}

func TestDependenciesInvalidated_ClearsDeepCacheAndFlags(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	deepFetched(t, s, 1)
	f := applyFlags(t, s, DependenciesInvalidated{Repo: testRepo, Number: 1})
	if f&DependenciesInvalidatedChanged == 0 || f&DeepFetchChanged == 0 {
		t.Fatalf("flags = %b; want DependenciesInvalidatedChanged|DeepFetchChanged", f)
	}
	it := getItem(t, s, testRepo, 1)
	if !it.LastDeepFetchAt.IsZero() || !it.LastSeenSourceUpdatedAt.IsZero() || !it.LastSeenPRUpdatedAt.IsZero() {
		t.Errorf("deep-fetch baselines not zeroed: %v %v %v", it.LastDeepFetchAt, it.LastSeenSourceUpdatedAt, it.LastSeenPRUpdatedAt)
	}
}

func TestDependenciesInvalidated_RepeatIsNoOp(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	deepFetched(t, s, 1)
	if f := applyFlags(t, s, DependenciesInvalidated{Repo: testRepo, Number: 1}); f == 0 {
		t.Fatal("first apply must change the item")
	}
	if f := applyFlags(t, s, DependenciesInvalidated{Repo: testRepo, Number: 1}); f != 0 {
		t.Errorf("second apply flags = %b; want 0 (the second delivery of one change must be free)", f)
	}
}

func TestDependenciesInvalidated_DropsCooldownOnlyForBlockedItem(t *testing.T) {
	until := time.Now().Add(time.Hour)

	blocked := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, blocked, LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:blocked"}, LabelsChanged)
	applyExpect(t, blocked, CooldownRecorded{Repo: testRepo, Number: 1, Reason: DepBlockedCooldown, Until: until}, CooldownChanged)
	f := applyFlags(t, blocked, DependenciesInvalidated{Repo: testRepo, Number: 1})
	if f&DependenciesInvalidatedChanged == 0 || f&CooldownChanged == 0 {
		t.Fatalf("blocked item: flags = %b; want the wake flag and CooldownChanged even with a cold deep cache", f)
	}
	if _, ok := getItem(t, blocked, testRepo, 1).CooldownAt[DepBlockedCooldown]; ok {
		t.Error("dep-blocked cooldown must be dropped for a fabrik:blocked item")
	}

	plain := newStoreWithItem(t, testRepo, 2)
	applyExpect(t, plain, CooldownRecorded{Repo: testRepo, Number: 2, Reason: DepBlockedCooldown, Until: until}, CooldownChanged)
	if f := applyFlags(t, plain, DependenciesInvalidated{Repo: testRepo, Number: 2}); f != 0 {
		t.Errorf("unlabelled item: flags = %b; want 0", f)
	}
	if got := getItem(t, plain, testRepo, 2).CooldownAt[DepBlockedCooldown]; !got.Equal(until) {
		t.Errorf("a non-blocked item's cooldown was touched: %v", got)
	}
}
