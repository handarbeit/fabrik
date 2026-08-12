package pruefer

import (
	"testing"
)

// TestAcquirePRGateDistinctPRsGetDistinctGates is the regression test for the
// 64-stripe collision defect: ReviewFromEvent TryLocks the gate and drops the
// event when the claim fails, so two unrelated PRs sharing a lock meant one
// PR's in-flight review silently suppressed the other's — logged, misleadingly,
// as "a review is already in flight for this PR".
//
// With the old fixed 64-stripe scheme this fails by construction: 200 distinct
// PR numbers cannot map to 64 stripes without collisions (pigeonhole).
func TestAcquirePRGateDistinctPRsGetDistinctGates(t *testing.T) {
	d := &Daemon{}

	const numPRs = 200
	seen := make(map[*prGate]int, numPRs)
	releases := make([]func(), 0, numPRs)
	t.Cleanup(func() {
		for _, r := range releases {
			r()
		}
	})

	for i := 1; i <= numPRs; i++ {
		g, release := d.acquirePRGate("owner", "repo", i)
		releases = append(releases, release)
		if prev, dup := seen[g]; dup {
			t.Fatalf("PR #%d shares a gate with PR #%d — a TryLock on one would falsely drop the other", i, prev)
		}
		seen[g] = i
	}
}

// TestAcquirePRGateSamePRSharesGate is the other half of the contract: the
// gate must still serialize two callers naming the same PR, which is the
// single-flight property runReview depends on.
func TestAcquirePRGateSamePRSharesGate(t *testing.T) {
	d := &Daemon{}

	g1, r1 := d.acquirePRGate("owner", "repo", 7)
	defer r1()
	g2, r2 := d.acquirePRGate("owner", "repo", 7)
	defer r2()

	if g1 != g2 {
		t.Fatal("two callers for the same PR got different gates — concurrent duplicate reviews would be possible")
	}
	if !g1.mu.TryLock() {
		t.Fatal("gate unexpectedly already locked")
	}
	defer g1.mu.Unlock()
	if g2.mu.TryLock() {
		g2.mu.Unlock()
		t.Fatal("second caller claimed the same PR's gate while it was held")
	}
}

// TestAcquirePRGateCaseInsensitive: the same PR referred to with different
// owner/repo casing must resolve to one gate, or the casing divergence that
// isWatchedRepo now tolerates would reintroduce concurrent duplicate reviews.
func TestAcquirePRGateCaseInsensitive(t *testing.T) {
	d := &Daemon{}

	g1, r1 := d.acquirePRGate("Handarbeit", "Fabrik", 42)
	defer r1()
	g2, r2 := d.acquirePRGate("handarbeit", "fabrik", 42)
	defer r2()

	if g1 != g2 {
		t.Fatal("same PR with different casing got different gates")
	}
}

// TestAcquirePRGateReleasedGatesAreEvicted pins the bound that justified
// replacing the fixed-size stripe array with a map: entries must not
// accumulate for the daemon's lifetime.
func TestAcquirePRGateReleasedGatesAreEvicted(t *testing.T) {
	d := &Daemon{}

	for i := 1; i <= 500; i++ {
		_, release := d.acquirePRGate("owner", "repo", i)
		release()
	}

	d.prGatesMu.Lock()
	n := len(d.prGates)
	d.prGatesMu.Unlock()

	if n != 0 {
		t.Fatalf("prGates retained %d entries after every holder released; expected 0", n)
	}
}

// TestAcquirePRGateRefcountedAcrossHolders: a gate must survive one holder
// releasing while another still holds it, or the surviving holder's lock stops
// excluding anyone.
func TestAcquirePRGateRefcountedAcrossHolders(t *testing.T) {
	d := &Daemon{}

	g1, r1 := d.acquirePRGate("owner", "repo", 3)
	_, r2 := d.acquirePRGate("owner", "repo", 3)
	r2()

	g3, r3 := d.acquirePRGate("owner", "repo", 3)
	defer r3()
	if g1 != g3 {
		t.Fatal("gate was evicted while a holder still had a reference")
	}
	r1()
}

// TestIsWatchedRepoCaseInsensitive: owner/repo arrive from the webhook payload
// in GitHub's canonical casing, while WatchedRepos is hand-typed. A divergence
// silently dropped every event for that repo.
func TestIsWatchedRepoCaseInsensitive(t *testing.T) {
	d := &Daemon{Config: Config{WatchedRepos: []string{"handarbeit/fabrik"}}}

	for _, tc := range []struct {
		owner, repo string
		want        bool
	}{
		{"handarbeit", "fabrik", true},
		{"Handarbeit", "Fabrik", true},
		{"HANDARBEIT", "FABRIK", true},
		{"handarbeit", "fabrik-test", false},
		{"someoneelse", "fabrik", false},
	} {
		if got := d.isWatchedRepo(tc.owner, tc.repo); got != tc.want {
			t.Errorf("isWatchedRepo(%q, %q) = %v, want %v", tc.owner, tc.repo, got, tc.want)
		}
	}
}

// TestClientForOwnerCaseInsensitive: Clients is keyed by the operator-typed
// owner, looked up with the webhook's canonical casing. Without the fallback a
// casing mismatch dropped every event for that owner — while isWatchedRepo
// admitted it, so the two checks disagreed.
func TestClientForOwnerCaseInsensitive(t *testing.T) {
	client := newFakeLister()
	d := &Daemon{Clients: map[string]GitHubLister{"handarbeit": client}}

	if got, ok := d.clientForOwner("handarbeit"); !ok || got != GitHubLister(client) {
		t.Error("exact-case lookup failed")
	}
	if got, ok := d.clientForOwner("Handarbeit"); !ok || got != GitHubLister(client) {
		t.Errorf("case-insensitive fallback failed for %q", "Handarbeit")
	}
	if _, ok := d.clientForOwner("someoneelse"); ok {
		t.Error("unknown owner resolved to a client")
	}
}

// TestClientForOwnerNoClients guards the nil-map path.
func TestClientForOwnerNoClients(t *testing.T) {
	d := &Daemon{}
	if _, ok := d.clientForOwner("handarbeit"); ok {
		t.Error("resolved a client from a nil Clients map")
	}
}
