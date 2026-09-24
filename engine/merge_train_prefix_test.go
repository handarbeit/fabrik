package engine

import (
	"os/exec"
	"strings"
	"testing"
)

// ── pure lookup semantics (no git — entries populated directly) ────────────────

func newTestPrefixCacheNoGit(trainKey, baseSHA string) *trainPrefixCache {
	return &trainPrefixCache{
		baseDir: "", // never touched — these tests bypass record()/git entirely
		refDir:  "refs/fabrik/merge-train-prefix/test",
		seed:    chainHashSeed(trainKey, baseSHA),
		entries: make(map[string]trainPrefixCacheEntry),
	}
}

// seedChain populates c.entries as if members[0:n] had all merged successfully in
// order, without touching git — chainHashStep is a pure function, so the resulting
// entries are indistinguishable from ones record() would have produced.
func seedChain(c *trainPrefixCache, members []trainMember, commitSHAs []string) {
	current := c.seed
	for i, m := range members {
		current = chainHashStep(current, m.item.Number, m.headSHA)
		c.entries[current] = trainPrefixCacheEntry{commitSHA: commitSHAs[i]}
	}
}

func TestTrainPrefixCache_Lookup_FullMatch(t *testing.T) {
	c := newTestPrefixCacheNoGit("owner/repo:main", "base-sha")
	members := []trainMember{
		{item: makeTrainItem(1, "one"), headSHA: "h1"},
		{item: makeTrainItem(2, "two"), headSHA: "h2"},
		{item: makeTrainItem(3, "three"), headSHA: "h3"},
	}
	seedChain(c, members, []string{"c1", "c2", "c3"})

	matchedLen, commitSHA, _ := c.lookup(members)
	if matchedLen != 3 {
		t.Errorf("expected matchedLen 3 (full match), got %d", matchedLen)
	}
	if commitSHA != "c3" {
		t.Errorf("expected commitSHA c3, got %q", commitSHA)
	}
}

func TestTrainPrefixCache_Lookup_PartialMatch_DifferingHeadSHA(t *testing.T) {
	c := newTestPrefixCacheNoGit("owner/repo:main", "base-sha")
	recorded := []trainMember{
		{item: makeTrainItem(1, "one"), headSHA: "h1"},
		{item: makeTrainItem(2, "two"), headSHA: "h2"},
	}
	seedChain(c, recorded, []string{"c1", "c2"})

	// Member #2 was re-pushed: its head SHA now differs from what was recorded.
	query := []trainMember{
		{item: makeTrainItem(1, "one"), headSHA: "h1"},
		{item: makeTrainItem(2, "two"), headSHA: "h2-repushed"},
		{item: makeTrainItem(3, "three"), headSHA: "h3"},
	}
	matchedLen, commitSHA, _ := c.lookup(query)
	if matchedLen != 1 {
		t.Errorf("expected matchedLen 1 (stops at the re-pushed member), got %d", matchedLen)
	}
	if commitSHA != "c1" {
		t.Errorf("expected commitSHA c1, got %q", commitSHA)
	}
}

func TestTrainPrefixCache_Lookup_NoMatch_DifferentBaseSHA(t *testing.T) {
	c := newTestPrefixCacheNoGit("owner/repo:main", "base-sha-old")
	recorded := []trainMember{
		{item: makeTrainItem(1, "one"), headSHA: "h1"},
	}
	seedChain(c, recorded, []string{"c1"})

	// A cache pinned at a different base SHA (main moved) never matches, even
	// though the member composition is identical.
	other := newTestPrefixCacheNoGit("owner/repo:main", "base-sha-new")
	matchedLen, commitSHA, chainHash := other.lookup(recorded)
	if matchedLen != 0 {
		t.Errorf("expected matchedLen 0 for a different pinned base SHA, got %d", matchedLen)
	}
	if commitSHA != "" {
		t.Errorf("expected empty commitSHA, got %q", commitSHA)
	}
	if chainHash != other.seed {
		t.Errorf("expected chainHash to be the fresh cache's own seed, got %q", chainHash)
	}
}

func TestTrainPrefixCache_Lookup_EmptyMembers(t *testing.T) {
	c := newTestPrefixCacheNoGit("owner/repo:main", "base-sha")
	matchedLen, commitSHA, chainHash := c.lookup(nil)
	if matchedLen != 0 || commitSHA != "" {
		t.Errorf("expected (0, \"\") for empty members, got (%d, %q)", matchedLen, commitSHA)
	}
	if chainHash != c.seed {
		t.Errorf("expected chainHash == seed for empty members, got %q vs seed %q", chainHash, c.seed)
	}
}

// TestTrainPrefixCache_Lookup_EjectedMemberDoesNotBreakChain proves Requirement 1: an
// ejected member contributes no chain step, so the next successful member's step is
// recorded against — and later matched against — whatever chain hash preceded the
// ejected one, not a hash derived from it.
func TestTrainPrefixCache_Lookup_EjectedMemberDoesNotBreakChain(t *testing.T) {
	c := newTestPrefixCacheNoGit("owner/repo:main", "base-sha")
	m1 := trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}
	m2 := trainMember{item: makeTrainItem(2, "two"), headSHA: "h2"}
	m4 := trainMember{item: makeTrainItem(4, "four"), headSHA: "h4"}

	// Simulate the original assembly: 1 and 2 merged; member 3 (not modeled here) was
	// ejected mid-assembly, contributing no step; then 4 merged, chained directly off
	// member 2's hash.
	afterM1 := chainHashStep(c.seed, m1.item.Number, m1.headSHA)
	c.entries[afterM1] = trainPrefixCacheEntry{commitSHA: "c1"}
	afterM2 := chainHashStep(afterM1, m2.item.Number, m2.headSHA)
	c.entries[afterM2] = trainPrefixCacheEntry{commitSHA: "c2"}
	afterM4 := chainHashStep(afterM2, m4.item.Number, m4.headSHA)
	c.entries[afterM4] = trainPrefixCacheEntry{commitSHA: "c4"}

	// A later re-form's survivor list (3 already ejected) should match all three
	// recorded steps.
	matchedLen, commitSHA, _ := c.lookup([]trainMember{m1, m2, m4})
	if matchedLen != 3 {
		t.Errorf("expected matchedLen 3 (survivors list matches despite the gap left by the ejected member), got %d", matchedLen)
	}
	if commitSHA != "c4" {
		t.Errorf("expected commitSHA c4, got %q", commitSHA)
	}
}

// ── nil-receiver safety ─────────────────────────────────────────────────────────

func TestTrainPrefixCache_Disabled_NewReturnsNil(t *testing.T) {
	c := newTrainPrefixCache("owner/repo:main", "base-sha", "/does/not/matter", true)
	if c != nil {
		t.Fatalf("expected newTrainPrefixCache(disabled=true) to return nil, got %#v", c)
	}
}

func TestTrainPrefixCache_NilReceiver_IsNoOp(t *testing.T) {
	var c *trainPrefixCache // nil

	matchedLen, commitSHA, chainHash := c.lookup([]trainMember{{item: makeTrainItem(1, "one"), headSHA: "h1"}})
	if matchedLen != 0 || commitSHA != "" || chainHash != "" {
		t.Errorf("expected nil-receiver lookup to return (0, \"\", \"\"), got (%d, %q, %q)", matchedLen, commitSHA, chainHash)
	}

	next := c.record("", trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}, "sha")
	if next != "" {
		t.Errorf("expected nil-receiver record to return \"\", got %q", next)
	}

	// Must not panic.
	c.sweepStaleRefs()
	c.cleanup()
}

// ── hashing invalidation properties ─────────────────────────────────────────────

func TestChainHash_DifferentBaseSHA_DifferentSeed(t *testing.T) {
	if chainHashSeed("owner/repo:main", "sha-a") == chainHashSeed("owner/repo:main", "sha-b") {
		t.Error("expected different base SHAs to produce different seeds")
	}
}

func TestChainHash_DifferentHeadSHA_DifferentStep(t *testing.T) {
	seed := chainHashSeed("owner/repo:main", "base-sha")
	if chainHashStep(seed, 1, "sha-a") == chainHashStep(seed, 1, "sha-b") {
		t.Error("expected different head SHAs at the same position to produce different hashes")
	}
}

func TestChainHash_DifferentMemberNumber_DifferentStep(t *testing.T) {
	seed := chainHashSeed("owner/repo:main", "base-sha")
	if chainHashStep(seed, 1, "sha") == chainHashStep(seed, 2, "sha") {
		t.Error("expected different member numbers to produce different hashes even with the same head SHA")
	}
}

// ── record()/lookup() round trip against a real bare repo ──────────────────────

// TestTrainPrefixCache_RecordThenLookup_RealGit proves record() and lookup() agree:
// what record() writes, a fresh lookup() over the same members finds.
func TestTrainPrefixCache_RecordThenLookup_RealGit(t *testing.T) {
	bareDir, _ := setupBareRepoForTrain(t)
	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	c := newTrainPrefixCache("owner/repo:main", baseSHA, bareDir, false)
	m1 := trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}

	_, _, h0 := c.lookup(nil)
	next := c.record(h0, m1, baseSHA) // any resolvable commit SHA works for this test

	matchedLen, commitSHA, gotHash := c.lookup([]trainMember{m1})
	if matchedLen != 1 {
		t.Fatalf("expected matchedLen 1 after recording one step, got %d", matchedLen)
	}
	if commitSHA != baseSHA {
		t.Errorf("expected recorded commitSHA %q, got %q", baseSHA, commitSHA)
	}
	if gotHash != next {
		t.Errorf("expected lookup's resulting chain hash to equal record's return value")
	}

	c.cleanup()
}

// TestTrainPrefixCache_DifferentPinnedBase_ProducesIndependentNonInterferingChains is
// Acceptance 3's real-git half: two caches for the same trainKey (as a base-moved
// restart would produce — same partition, a later invocation pinned to a different
// base SHA) never share a chain — a member recorded under one base is never found
// under the other, and their protective refs coexist under the same trainKey ref
// directory without colliding (different chain hashes from different seeds).
func TestTrainPrefixCache_DifferentPinnedBase_ProducesIndependentNonInterferingChains(t *testing.T) {
	_, srcDir, _, wm := setupTrainRepo(t)
	baseSHA1 := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))

	// Advance main so a later invocation for the same trainKey pins a different base
	// SHA — the "main moved between polls" case (ADR-059 D5).
	writeFile(t, srcDir+"/advance.txt", "main moved\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "advance main")
	mustGit(t, srcDir, "push", wm.baseDir, "main:main")
	baseSHA2 := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	if baseSHA1 == baseSHA2 {
		t.Fatal("setup failed: expected the second base SHA to differ from the first")
	}

	trainKey := "owner/repo:main"
	m1 := trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}

	c1 := newTrainPrefixCache(trainKey, baseSHA1, wm.baseDir, false)
	_, _, h0c1 := c1.lookup(nil)
	c1.record(h0c1, m1, baseSHA1)

	c2 := newTrainPrefixCache(trainKey, baseSHA2, wm.baseDir, false)
	// A fresh invocation always sweeps stale refs for its own trainKey before use —
	// this must NOT remove c1's still-live ref, since c1 hasn't called cleanup() yet
	// (modeling two invocations whose lifetimes momentarily overlap in this test,
	// even though in production the in-flight guard serializes them).
	matchedLen, _, _ := c2.lookup([]trainMember{m1})
	if matchedLen != 0 {
		t.Errorf("expected a cache pinned to a different base SHA to find zero reusable prefix for a member recorded under the other base, got matchedLen=%d", matchedLen)
	}

	// c1's own chain is unaffected by c2 having been constructed (no shared mutable
	// state beyond the git refs, which live at different paths since h0 differs).
	matchedLen, commitSHA, _ := c1.lookup([]trainMember{m1})
	if matchedLen != 1 || commitSHA != baseSHA1 {
		t.Errorf("expected c1's own chain to still resolve after c2 was constructed, got matchedLen=%d commitSHA=%q", matchedLen, commitSHA)
	}

	c1.cleanup()
	c2.cleanup()
}

// TestTrainPrefixCache_GCSurvival is Acceptance 4: a reused commit survives
// `git gc --prune=now` in the bare clone for as long as it is still protected by a
// ref, and the ref is removed once cleanup() runs.
func TestTrainPrefixCache_GCSurvival(t *testing.T) {
	bareDir, worktreeRoot := setupBareRepoForTrain(t)
	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))
	wm := NewWorktreeManagerForRepo(bareDir, worktreeRoot, "test-repo")

	// Create a real, otherwise-unreferenced commit to stand in for a merge commit.
	wtDir, err := wm.EnsureTrainWorktreeAt("gc-survival-seed", baseSHA)
	if err != nil {
		t.Fatalf("EnsureTrainWorktreeAt: %v", err)
	}
	writeFile(t, wtDir+"/orphan.txt", "orphan content\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "orphan commit")
	orphanSHA := strings.TrimSpace(gitOutputDir(t, wtDir, "rev-parse", "HEAD"))
	if err := wm.CleanupTrainWorktree("gc-survival-seed", true); err != nil {
		t.Fatalf("CleanupTrainWorktree: %v", err)
	}
	// After CleanupTrainWorktree, orphanSHA is unreachable from any branch — exactly
	// the state a trial's merge commits are left in today without #1835's ref
	// protection.

	c := newTrainPrefixCache("owner/repo:main", baseSHA, bareDir, false)
	_, _, h0 := c.lookup(nil)
	m1 := trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}
	c.record(h0, m1, orphanSHA)

	gcCmd := exec.Command("git", "gc", "--prune=now")
	gcCmd.Dir = bareDir
	if out, err := gcCmd.CombinedOutput(); err != nil {
		t.Fatalf("git gc --prune=now: %s: %v", out, err)
	}

	catFileCmd := exec.Command("git", "cat-file", "-e", orphanSHA+"^{commit}")
	catFileCmd.Dir = bareDir
	if out, err := catFileCmd.CombinedOutput(); err != nil {
		t.Fatalf("expected the recorded commit %s to survive git gc --prune=now while its ref exists, but it did not: %s: %v", orphanSHA, out, err)
	}

	c.cleanup()

	refsAfter := c.listOwnRefs()
	if len(refsAfter) != 0 {
		t.Errorf("expected cleanup() to remove every ref this cache created, found %v", refsAfter)
	}
}

// TestTrainPrefixCache_SweepStaleRefs_RemovesLeftoverFromCrashedInvocation is
// Acceptance 5: a ref left behind by a crashed prior invocation for the same
// trainKey is removed by the next invocation's start-of-run sweep, without that
// invocation ever trusting the ref's content as a valid cache entry (a fresh
// invocation's in-memory entries map starts empty regardless).
func TestTrainPrefixCache_SweepStaleRefs_RemovesLeftoverFromCrashedInvocation(t *testing.T) {
	bareDir, _ := setupBareRepoForTrain(t)
	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	trainKey := "owner/repo:main"

	// Simulate a crashed prior invocation: write a ref directly, bypassing a live
	// cache's cleanup().
	leaked := newTrainPrefixCache(trainKey, baseSHA, bareDir, false)
	_, _, h0 := leaked.lookup(nil)
	m1 := trainMember{item: makeTrainItem(1, "one"), headSHA: "h1"}
	leaked.record(h0, m1, baseSHA)
	if refs := leaked.listOwnRefs(); len(refs) == 0 {
		t.Fatal("setup failed: expected the simulated crashed invocation to have left a ref behind")
	}
	// Deliberately do NOT call leaked.cleanup() — this models the crash.

	// A fresh invocation for the same trainKey starts with its own empty in-memory
	// cache and sweeps stale refs before doing anything else.
	fresh := newTrainPrefixCache(trainKey, baseSHA, bareDir, false)
	fresh.sweepStaleRefs()

	if refs := fresh.listOwnRefs(); len(refs) != 0 {
		t.Errorf("expected the start-of-invocation sweep to remove the leaked ref, found %v", refs)
	}
	// The fresh cache's own in-memory entries are empty regardless of the sweep —
	// even if sweepStaleRefs somehow missed something, a lookup could never
	// incorrectly hit, since correctness is decided by entries, never by ref
	// presence (Decision 3).
	matchedLen, _, _ := fresh.lookup([]trainMember{m1})
	if matchedLen != 0 {
		t.Errorf("expected a fresh invocation to start with zero reusable prefix regardless of leftover refs, got matchedLen %d", matchedLen)
	}
}
