package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strconv"
	"strings"
)

// trainPrefixCacheEntry records the resulting merge commit SHA for one successful
// merge step in a trial assembly chain, plus the git ref protecting that commit from
// `git gc` for as long as it might still be reused.
type trainPrefixCacheEntry struct {
	commitSHA string
	ref       string
}

// trainPrefixCache lets assembleTrialBranch resume a trial assembly from an
// already-built merge commit instead of always forking fresh off the pinned base SHA
// (#1835). It is a content-addressed cache of the ordered chain of merges a single
// runMergeTrainWorker invocation has produced: h0 = chainHashSeed(trainKey, baseSHA),
// and h_i = chainHashStep(h_{i-1}, member[i].Number, member[i].headSHA) for each
// member that merged successfully — an ejected member contributes no step, so the
// next successful member's hash is computed against whatever hash preceded it. A new
// assembly's member list is matched by walking the same recurrence forward from h0;
// the longest run of hits is the longest reusable prefix. This one mechanism, with no
// separate invalidation logic, is what makes bisect's first half (an exact prefix of
// the trial it was drawn from, by construction), a re-form after ejecting a member
// (whose chain simply skips that member's step), and a changed base SHA or a
// re-pushed member's head SHA (a different h0 or a different h_i from that point on)
// all correctly handled: a lookup miss at some position IS invalidation from that
// point on. See ADR-1835.
//
// Scoped to a single worker-goroutine invocation (Decision 1, ADR-1835) — never
// shared across polls or restarts. A restart, or even the next poll's fresh worker
// dispatch, starts from a nil/empty cache, which can only ever produce today's
// behavior: full re-assembly from the pinned base (Requirement 6).
//
// entries is the sole source of truth for what counts as reusable; the git refs
// under refDir exist only to keep the corresponding commits reachable across a
// `git gc` (Decision 3) and are never consulted for correctness.
//
// Every method is nil-receiver-safe and behaves as a pure no-op on a nil
// *trainPrefixCache (newTrainPrefixCache returns nil when disabled, mirroring
// mergeTrainQueueSortDisabledForTest's shape) — callers never need to nil-check
// before use, so "no cache" degrades to exactly today's behavior with no special
// casing at call sites.
type trainPrefixCache struct {
	baseDir string // the bare clone's directory; every git command here runs with Dir=baseDir
	refDir  string // refs/fabrik/merge-train-prefix/<sha256(trainKey)> — this trainKey's own ref namespace
	seed    string // h0 = chainHashSeed(trainKey, baseSHA)

	entries map[string]trainPrefixCacheEntry // key: chain hash h_i -> the entry it produced
}

// chainHash hashes an arbitrary sequence of parts with a stdlib SHA-256, separating
// each part with a NUL byte so e.g. ("ab", "c") and ("a", "bc") never collide.
func chainHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// chainHashSeed computes h0 for a trial chain pinned at (trainKey, baseSHA) — a
// different base SHA (main moved, ADR-059 D5) always produces a different seed, so a
// chain recorded against a stale base can never be matched against a new one.
func chainHashSeed(trainKey, baseSHA string) string {
	return chainHash("seed", trainKey, baseSHA)
}

// chainHashStep advances a chain hash by one successfully-merged member — a
// different member number or a different head SHA (a re-pushed PR) always produces a
// different h_i, so nothing after that point can match a stale recording.
func chainHashStep(prev string, memberNumber int, headSHA string) string {
	return chainHash("step", prev, strconv.Itoa(memberNumber), headSHA)
}

// newTrainPrefixCache constructs a trainPrefixCache scoped to one worker invocation
// pinned at (trainKey, baseSHA), or returns nil when disabled (the
// mergeTrainPrefixReuseDisabledForTest test seam) — a nil cache is a pure no-op
// everywhere it's used, reproducing pre-#1835 behavior exactly (Acceptance 6).
//
// refDir is namespaced by trainKey alone, not by baseSHA — this is what makes
// sweepStaleRefs's "delete everything under my own directory" provably safe against a
// concurrently-running sibling (repo, base) partition sharing the same bare clone
// (ADR-1648): the in-flight guard already guarantees no other goroutine is using this
// exact trainKey right now, and a different trainKey hashes to a different directory.
func newTrainPrefixCache(trainKey, baseSHA, baseDir string, disabled bool) *trainPrefixCache {
	if disabled {
		return nil
	}
	return &trainPrefixCache{
		baseDir: baseDir,
		refDir:  "refs/fabrik/merge-train-prefix/" + chainHash("refdir", trainKey),
		seed:    chainHashSeed(trainKey, baseSHA),
		entries: make(map[string]trainPrefixCacheEntry),
	}
}

// lookup walks members forward from the cache's seed hash, returning the length of
// the longest matching prefix, the commit SHA the prefix ends at (empty when
// matchedLen is 0), and the chain hash to resume recording from (the seed itself when
// nothing matched). A nil receiver always returns (0, "", "").
func (c *trainPrefixCache) lookup(members []trainMember) (matchedLen int, commitSHA, chainHash string) {
	if c == nil {
		return 0, "", ""
	}
	current := c.seed
	for _, m := range members {
		next := chainHashStep(current, m.item.Number, m.headSHA)
		entry, ok := c.entries[next]
		if !ok {
			break
		}
		current = next
		commitSHA = entry.commitSHA
		matchedLen++
	}
	return matchedLen, commitSHA, current
}

// record advances chainHash by member's successful merge, recording its resulting
// commitSHA under a protective ref so `git gc` cannot prune it while it might still
// be reused, and returns the new chain hash for the caller to continue recording
// from. A nil receiver is a no-op returning "". If the protective ref fails to write
// (best-effort — logged nowhere here since trainPrefixCache has no access to a
// per-issue logf; the caller assembling the trial already logs its own merge
// success), the entry is deliberately NOT recorded: an unprotected commit could be
// pruned before a later trial forks off it, and a lookup miss is always safe
// (degrades to a full re-merge), while a hit against an unprotected commit is not.
func (c *trainPrefixCache) record(chainHash string, member trainMember, commitSHA string) string {
	if c == nil {
		return ""
	}
	next := chainHashStep(chainHash, member.item.Number, member.headSHA)
	ref := c.refDir + "/" + next
	if err := c.updateRef(ref, commitSHA); err != nil {
		return next
	}
	c.entries[next] = trainPrefixCacheEntry{commitSHA: commitSHA, ref: ref}
	return next
}

// updateRef creates or moves ref to point at commitSHA in the bare clone.
func (c *trainPrefixCache) updateRef(ref, commitSHA string) error {
	cmd := exec.Command("git", "update-ref", ref, commitSHA)
	cmd.Dir = c.baseDir
	_, err := cmd.CombinedOutput()
	return err
}

// listOwnRefs returns every ref currently written under this cache's own refDir.
func (c *trainPrefixCache) listOwnRefs() []string {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)", c.refDir)
	cmd.Dir = c.baseDir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	return refs
}

// sweepStaleRefs deletes every ref currently written under this cache's own refDir,
// best-effort. Called once at the start of a worker invocation (before any lookup or
// record) to mop up anything a crashed prior invocation for the same trainKey left
// behind — bounded to at most one leaked episode, since a clean invocation always
// reaches its own deferred cleanup. Also used as cleanup's own implementation: by the
// end of a worker invocation, nothing will ever look up this trainKey's chain again
// (Decision 1 — the cache is never shared across invocations), so every ref this
// invocation created can unconditionally be removed (Requirement 5) — there is no
// "still matchable" case left to preserve.
func (c *trainPrefixCache) sweepStaleRefs() {
	if c == nil {
		return
	}
	for _, ref := range c.listOwnRefs() {
		cmd := exec.Command("git", "update-ref", "-d", ref)
		cmd.Dir = c.baseDir
		cmd.CombinedOutput() // best-effort
	}
}

// cleanup removes every ref this invocation's cache created. See sweepStaleRefs's
// doc comment for why deleting unconditionally at invocation end is correct.
func (c *trainPrefixCache) cleanup() {
	c.sweepStaleRefs()
}
