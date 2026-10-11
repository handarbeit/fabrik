//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMergeTrainColdCacheBaseMember is the e2e regression for the #1688 incident
// shape: a base:<branch> member sitting in Queued when the engine restarts, so the
// post-restart board cache is COLD for that item. The merge train pinned the
// repository default branch for such a member and assembled/landed its batch on
// the wrong base. Two independent fixes shipped in 0.0.83:
//
//   - #1772 (ADR-1772): groupQueuedByRepoAndBase excludes any member the cache has
//     not deep-fetched yet (IsItemDeepFetched) — logging "cache entry for #N not yet
//     hydrated (no deep-fetch recorded) — excluding from batching this poll, will
//     retry" — so an unhydrated item can never be bucketed under the default base.
//   - #1773 (ADR-1773): refuseIfBaseContradictsMembers refuses to open/reuse an
//     integration/landing PR whose pinned base is the default while a member's LIVE
//     base: label says otherwise ("REFUSING to open/reuse integration PR for …").
//
// mergetrain_twobase_test.go covers partitioning with a WARM cache and
// mergetrain_restart_test.go covers restart safety on the default base; nothing else
// covers a base:<branch> member across a cold-cache restart.
//
// # Flow — stop, queue, start (not "queue, then restart")
//
// A literal "queue members, then RestartFabrikTestBed" races the bed's 60s poll: the
// batch can form before the restart lands, and the scenario would silently prove
// nothing. The deterministic order is instead:
//
//  1. Create a per-run throwaway base branch (its own (repo, base) partition —
//     ADR-1648 — so no sibling train scenario can share the batch).
//  2. Stop the bed. With the engine down nothing can form a batch.
//  3. While down: queue two base:<branch> members, and file the WorktreeManager
//     primer (below).
//  4. Start the bed. The first poll of the fresh process is the first
//     batch-formation opportunity, and its cache is provably virgin: New() builds a
//     fresh store, poll 1 seeds it with BootstrapFromProbe (empty labels, no
//     deep-fetch recorded), and no path deep-fetches a Queued item before
//     handleMergeTrainBatch runs (itemMayNeedWork is false for holding stages).
//
// The cold window is ASSERTED, not assumed: every member must have a "not yet
// hydrated" log line after the restart. A missing line fails the scenario as
// vacuous — it never passes silently. On a webhook-enabled bed poll 1 hydrates
// members before batching, so no cold window exists; the scenario skips there.
//
// # Two members, not one
//
// A one-member batch takes trySingletonFastPath, which lands the member's own PR
// (already targeting the declared base) and — per ADR-1773 — cannot reproduce the
// contradiction. It would pass on pre-fix code. Two members on one base force a
// trial branch and a CreateDraftPR(…, p.baseBranch, …), the PR the wrong base would
// corrupt. Both hydrate in the same poll-2 probe, so they batch together.
//
// # WorktreeManager primer
//
// After a restart Engine.worktreeManagers is empty. groupQueuedByRepoAndBase
// excludes a base:-labelled member ("WorktreeManager not yet registered for …")
// until something calls ensureRepoReady for the repo, and prepareTrainWorker — the
// only train-side registration — is unreachable because grouping runs first. A
// restarted bed holding only Queued base: members would sit in Queued forever.
// The primer is an issue in the same repo at Specify with an open blockedBy edge:
// processItem calls ensureRepoReady BEFORE checkDependencies, which then labels it
// fabrik:blocked before any lock or Claude call. So it registers the manager with
// no Claude spend and touches neither main nor the train. (A default-base Queued
// primer would land on main and conflict with A3.) The blocker is deliberately not
// added to the board. That latent engine gap is out of scope here (test-only).
//
// # Assertions
//
//   - Cold evidence (R2): each member has a "not yet hydrated" line after restart.
//     Pre-#1772 code has no such line.
//   - A1: both members land; the landing PR is merged, is NOT the singleton fast
//     path, targets the declared branch, and the engine logs "landing complete for
//     <owner/repo>:<branch>". Pre-#1772 the poll-1 batch pins the default: the CI PR
//     targets main, and either it lands there or #1773 refuses.
//   - A2: EVERY fabrik/merge-train/* PR (state=all, so closed PRs count — the whole
//     scenario window, not the final state) that closes a member targets the
//     declared branch, and no "batch snapshot for <owner/repo>: …" line under the
//     BARE default key lists a member. Pre-#1772 the poll-1 batch opens a CI PR
//     against main carrying the members' commits — visible even if #1773 later
//     refuses the landing, which makes A2 the sharp #1772 detector.
//   - A3: the members' marker files are absent on main and present on the declared
//     branch. main's head SHA is compared before/after but only LOGGED (other repo
//     activity may legitimately move it); path absence is the check. With neither
//     defence in place the files land on main.
//   - A4 (#1773): the healthy run must NOT show "REFUSING to open/reuse integration
//     PR" for these members, because #1772 prevents the contradiction — asserted
//     absent. Pre-#1772 with #1773 intact the line appears and the members stay
//     Queued until a later warm poll re-forms them.
//
// # Auth legs
//
// The scenario posts NO comments. Members and their PRs are filed as FABRIK_TOKEN's
// account (arbeithand) in both PAT and GitHub App modes (#1861), so no classification
// difference arises. Were a comment posted it would read as human in BOTH modes
// (filterHuman only excludes [bot] logins; the engine's own comments are excluded by
// their "🏭 **Fabrik" prefix, which a harness comment lacks) and settleQueuedCommentCause
// would eject the member from Queued — and the reviewer token's account is human
// too, so switching to it would not help. Under App auth the engine pushes with
// installation git credentials (#1846) and needs contents:write, the same
// requirement as every sibling train scenario, so no leg-specific skip is needed.
//
// NOT t.Parallel(): it stops the shared bed, so it must not run alongside the other
// train scenarios. Go runs non-parallel tests to completion before resuming
// parallel ones.
//
// Wall-clock: ~15–30 min (stop/start ~1–2, queueing ~1–3, polls 1–2 ~1–2, trial
// assembly + CI + landing ~10–25). Cost: low — no Claude invocation (no conflicts, no
// stage dispatch), a small GitHub API cost.
func TestMergeTrainColdCacheBaseMember(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	if bedWebhooksEnabled(env) {
		t.Skip("bed has webhooks enabled — poll 1 hydrates Queued members before batching, so there is no cold window to exercise")
	}

	repo := env.RepoAlpha
	stale, err := staleQueuedMembers(env, repo, anyBase)
	if err != nil {
		t.Fatalf("pre-flight: could not list stale Queued members on %s: %v", repo, err)
	}
	if len(stale) > 0 {
		t.Fatalf("pre-flight: non-paused Queued member(s) %v already on %s — they would join this scenario's batch; clear them first", stale, repo)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	branch := fmt.Sprintf("e2e-cold-base-%s", stamp)
	trainKey := repo + ":" + branch
	mainBefore := defaultBranchSHA(t, env, repo, "main")
	CreateThrowawayBaseBranch(t, env, repo, branch)
	windowStart := time.Now().UTC().Add(-time.Minute)

	// Register the restart BEFORE stopping so a failure between stop and start can
	// never leave the bed down for the rest of the suite (StartFabrikTestBed is a
	// no-op when the bed is already up).
	t.Cleanup(func() { StartFabrikTestBed(t, env) })
	StopFabrikTestBed(t, env)

	// --- bed is DOWN: nothing can form a batch until it comes back ---
	var members []int
	memberPaths := map[int]string{}
	memberPRs := map[int]int{}
	for _, m := range []struct{ marker, path, content string }{
		{"coldbase-a", "e2e/train/coldbase/a.txt", "cold-cache base member a\n"},
		{"coldbase-b", "e2e/train/coldbase/b.txt", "cold-cache base member b\n"},
	} {
		iss, pr := QueueMemberOnBase(t, env, repo, branch, m.marker, m.path, m.content)
		members = append(members, iss)
		memberPaths[iss] = uniqueMemberPath(m.path, iss)
		memberPRs[iss] = pr
	}

	// WorktreeManager primer: a blocked issue at Specify (see the doc comment).
	blocker := FileIssue(t, env, repo,
		fmt.Sprintf("e2e cold-cache primer blocker (%s)", stamp),
		"e2e cold-cache scenario: open blocker that is never worked and never on the board.")
	primer := FileIssue(t, env, repo,
		fmt.Sprintf("e2e cold-cache WorktreeManager primer (%s)", stamp),
		"e2e cold-cache scenario: blocked on an open issue so it registers the repo's WorktreeManager (ensureRepoReady) without invoking Claude.")
	primerItem := AddIssueToProject(t, env, repo, primer)
	AddBlockedBy(t, env, repo, primer, repo, blocker)
	AssertBlockedBy(t, env, repo, primer, repo, blocker)
	SetIssueStatus(t, env, primerItem, "Specify")
	t.Logf("seeded while bed down: members %v on base %q, primer #%d blocked by #%d", members, branch, primer, blocker)

	// The restart's bootstrap board fetch lists the ProjectV2 items. Until that
	// listing shows every seeded member in Queued (and the primer), the fetch can
	// miss them and the engine then DISCOVERS them as new items and deep-fetches
	// them at once — the cold-cache state this scenario targets never exists (0.0.83
	// gate, 18:45:59Z: the listing lagged the adds). QueueMemberOnBase already
	// awaited each member; re-confirm every one together, right before the start,
	// plus the primer, so the scenario starts cold only when the listing agrees.
	// Only a timeout here is Inconclusive (#1973, #1974); the assertions below are untouched.
	for _, n := range members {
		AwaitStatusVisible(t, env, repo, n, "Queued", awaitSeedTimeout)
	}
	AwaitStatusVisible(t, env, repo, primer, "Specify", awaitSeedTimeout)

	// --- restart: first poll of the fresh process is the first batch opportunity ---
	StartFabrikTestBed(t, env)
	// Run() rotates the old fabrik.log to fabrik.log.1 and opens a fresh file on
	// every start (#2094), so a pre-restart LogOffset would point past the new EOF. Offset 0 is the whole new run; every
	// matcher below is additionally scoped to this scenario's own issue numbers and
	// train key, so a line from any other activity is harmless.
	const logStart = int64(0)

	// Cold evidence (R2) for every member. A missing line means the cache was not
	// cold for that item and the scenario proved nothing.
	for _, n := range members {
		n := n
		// A precondition guard (#1973): without the cold-cache line the scenario never
		// reached the state it tests. Only the TIMEOUT is inconclusive; every
		// assertion below stays a Fatalf/Errorf.
		waitForLogMatchInconclusive(t, env, logStart, 10*time.Minute,
			fmt.Sprintf("cold-cache exclusion for member #%d (\"not yet hydrated\") — its absence means the run was vacuous", n),
			func(l string) bool { return matchNotHydrated(l, n) })
	}
	t.Logf("cold evidence: every member was excluded as unhydrated after the restart")

	// A1: the batch forms on the declared base's partition and lands.
	waitForLogMatch(t, env, logStart, 35*time.Minute,
		fmt.Sprintf("\"landing complete for %s\"", trainKey),
		func(l string) bool { _, _, ok := parseLandingComplete(l, trainKey); return ok })
	for _, n := range members {
		WaitForMemberLanded(t, env, repo, n, 15*time.Minute)
		waitForPRClosed(t, env, repo, memberPRs[n], 10*time.Minute)
		WaitForIssueClosed(t, env, repo, n, 10*time.Minute)
	}
	landingPR, viaFastPath := waitForLandingPRDetail(t, env, repo, memberPRs[members[0]], 2*time.Minute)
	if viaFastPath {
		// A precondition guard (#1973): evidence the members did not batch together,
		// found before the A1/A2 assertions it protects.
		Inconclusive(t, "member #%d landed via the singleton fast path (PR #%d) — that path cannot reproduce the #1688 contradiction, so A1/A2 would be vacuous; the members did not batch together",
			members[0], landingPR)
	}
	assertPRMerged(t, env, repo, landingPR)
	if got := PRBaseRef(t, env, repo, landingPR); got != branch {
		t.Errorf("A1: landing PR #%d targets %q, want the declared base %q", landingPR, got, branch)
	}

	// A2: every train PR carrying a member, over the whole window (state=all).
	prs, err := listTrainPRsSince(env, repo, windowStart, members)
	if err != nil {
		t.Fatalf("A2: %v", err)
	}
	if len(prs) == 0 {
		t.Fatalf("A2: no fabrik/merge-train/* PR carrying members %v found since %s — cannot vouch for the base of PRs never seen", members, windowStart.Format(time.RFC3339))
	}
	for _, p := range trainPRsNotOnBase(prs, branch) {
		t.Errorf("A2: train PR #%d (merged=%v, state=%s, closes %v) targets %q, want %q — a PR carrying this member's commits must never target another base",
			p.Number, p.Merged, p.State, p.Members, p.Base, branch)
	}
	lines := readLogLinesFrom(t, env, logStart)
	for _, l := range lines {
		if got, ok := parseBatchSnapshot(l, repo); ok && intersects(got, members) {
			t.Errorf("A2: a batch formed under the bare default key %q listing member(s) %v — the unhydrated member was bucketed under the default base (#1688):\n%s",
				repo, got, strings.TrimSpace(l))
		}
	}

	// A3: marker files never reach main; they are on the declared branch.
	for _, n := range members {
		path := memberPaths[n]
		onMain, err := fileExistsOnRef(env, repo, path, "main")
		if err != nil {
			t.Fatalf("A3: could not check %s on main: %v", path, err)
		}
		if onMain {
			t.Errorf("A3: member #%d's file %s is on main — the member landed on the default branch", n, path)
		}
		onBase, err := fileExistsOnRef(env, repo, path, branch)
		if err != nil {
			t.Fatalf("A3: could not check %s on %s: %v", path, branch, err)
		}
		if !onBase {
			t.Errorf("A3: member #%d's file %s is missing from the declared base %s after landing", n, path, branch)
		}
	}
	if mainAfter := defaultBranchSHA(t, env, repo, "main"); mainAfter != mainBefore {
		t.Logf("A3 (informational): main moved %s → %s during the scenario — other repo activity is legitimate; member files were checked by path above", mainBefore, mainAfter)
	}

	// A4: #1772 prevents the contradiction, so #1773's guard must not have fired.
	for _, l := range lines {
		if matchRefusing(l, repo, members) {
			t.Errorf("A4: healthy run logged #1773's refusal — #1772 should have prevented the default-pinned batch:\n%s", strings.TrimSpace(l))
		}
	}

	WaitForNoStaleTrainArtifacts(t, env, repo, 2*time.Minute)
	t.Logf("cold-cache base:%s verified: members %v excluded as unhydrated, landed via PR #%d on %q, no train PR ever targeted another base, main untouched",
		branch, members, landingPR, branch)
}

// notHydratedTail is the engine's own message from engine/poll.go
// (groupQueuedByRepoAndBase, #1772), copied verbatim after the "%d" is filled in:
//
//	"cache entry for #%d not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry\n"
//
// logged via e.logf(item.Number, "merge-train", …), so a real line carries the
// "[#N merge-train]" prefix.
const notHydratedTail = "not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry"

// matchNotHydrated reports whether line is the #1772 hydration-guard exclusion for
// issue n.
func matchNotHydrated(line string, n int) bool {
	return strings.Contains(line, fmt.Sprintf("[#%d merge-train] cache entry for #%d %s", n, n, notHydratedTail))
}

// refusingHead is the engine's own message from engine/merge_train.go
// (refuseIfBaseContradictsMembers, #1773):
//
//	"REFUSING to open/reuse integration PR for %s: pinned base %q is the repository default but %d member(s) declare a contradicting base — leaving members in Queued: %s\n"
//
// where the trailing list is "#N (declares base:X)" / "#N (label read failed: …)"
// entries joined with ", ".
const refusingHead = "REFUSING to open/reuse integration PR for "

var refusingMemberRE = regexp.MustCompile(`(?:: |, )#(\d+) \(`)

// matchRefusing reports whether line is #1773's refusal for repo naming at least
// one of members among the contradicting entries.
func matchRefusing(line, repo string, members []int) bool {
	head := refusingHead + repo + ": pinned base "
	i := strings.Index(line, head)
	if i < 0 {
		return false
	}
	rest := line[i+len(head):]
	k := strings.Index(rest, "leaving members in Queued: ")
	if k < 0 {
		return false
	}
	named := []int{}
	for _, m := range refusingMemberRE.FindAllStringSubmatch(": "+rest[k+len("leaving members in Queued: "):], -1) {
		n, _ := strconv.Atoi(m[1])
		named = append(named, n)
	}
	return intersects(named, members)
}

// trainPRsNotOnBase returns the PRs whose base ref is not want.
func trainPRsNotOnBase(prs []trainPR, want string) []trainPR {
	var bad []trainPR
	for _, p := range prs {
		if p.Base != want {
			bad = append(bad, p)
		}
	}
	return bad
}

// intersects reports whether a and b share any number.
func intersects(a, b []int) bool {
	set := map[int]bool{}
	for _, n := range b {
		set[n] = true
	}
	for _, n := range a {
		if set[n] {
			return true
		}
	}
	return false
}

// fileExistsOnRef reports whether path exists on repo at ref via the contents API:
// a 404 is "absent"; any other failure is an error, so an API hiccup is never
// mistaken for "the file is not there".
func fileExistsOnRef(env *Env, repo, path, ref string) (bool, error) {
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, path, ref), "--jq", ".path")
	if err == nil {
		return true, nil
	}
	if isNotFoundOutput(out) {
		return false, nil
	}
	return false, fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
}

// isNotFoundOutput reports whether gh api output is GitHub's 404.
func isNotFoundOutput(out string) bool {
	return strings.Contains(out, "HTTP 404") || strings.Contains(out, "Not Found")
}

// bedWebhooksEnabled reports whether the bed engine would run with webhooks on,
// resolving the way cmd/root.go does: FABRIK_WEBHOOKS (the harness's own
// environment is inherited by the launched bed, and .env loading never overrides an
// already-set variable) wins over the bed's .env, which wins over config.yaml's
// top-level webhooks key.
func bedWebhooksEnabled(env *Env) bool {
	envVal := os.Getenv("FABRIK_WEBHOOKS")
	if envVal == "" {
		if v, err := readEnvFileValue(filepath.Join(env.FabrikTestDir, ".env"), "FABRIK_WEBHOOKS"); err == nil {
			envVal = v
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(env.FabrikTestDir, ".fabrik", "config.yaml"))
	return webhooksEnabled(envVal, cfg)
}

var configWebhooksRE = regexp.MustCompile(`(?m)^webhooks:\s*(\S+)`)

// webhooksEnabled is bedWebhooksEnabled's pure core: a non-empty envVal decides
// (true/1/yes, case-insensitive — resolveBool's set); otherwise the config.yaml
// top-level webhooks value does.
func webhooksEnabled(envVal string, configYAML []byte) bool {
	truthy := func(v string) bool {
		switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)) {
		case "true", "1", "yes":
			return true
		}
		return false
	}
	if strings.TrimSpace(envVal) != "" {
		return truthy(envVal)
	}
	if m := configWebhooksRE.FindSubmatch(configYAML); m != nil {
		return truthy(string(m[1]))
	}
	return false
}
