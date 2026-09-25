//go:build e2e

package e2e

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMergeTrainQueuedDeeperThanBatchCap is the live-e2e proof of ADR-1833's
// deterministic Queued ordering: with MORE members Queued than max_batch_size,
// the train must pick the same first-N by entry order on every poll and land the
// rest in a second batch. It queues seven clean members against the default
// max_batch_size of 5 and asserts three things:
//
//   - A1: the first trial holds exactly the first five members.
//   - A2: batch membership stays stable from the first snapshot to the batch's
//     landing — exactly one "batch snapshot" line, no unmerged-closed trial PR.
//   - A3: all seven land, in two batches (5, then 2).
//
// Before #1833 the in-memory board cache returned items in Go map-iteration
// order (Store.All ranges over a map), so the capped batch was an arbitrary
// subset that changed poll to poll. No other scenario queues more members than
// the cap (the happy path queues three against 5), so nothing in the release
// gate would catch a regression of that ordering. The sim bed cannot host this
// defect at all: it uses the pass-through adapter, never the map-backed
// itemstate.Store (ADR-1833, "Non-vacuity proof").
//
// # Gating the batch with fabrik:paused
//
// The engine has no batching dwell: the first poll that sees a non-paused
// Queued member dispatches a worker. Queuing seven members sequentially takes
// minutes against a 60s poll, so an ungated run would dispatch on a partial
// batch of one to four and fail A1/A2 for the wrong reason. The scenario
// therefore files every member already carrying fabrik:paused, places all seven
// in Queued (paused Queued members are excluded from the partition and cause no
// engine side effect — itemNeedsWork returns early for holding stages), then
// removes the label from all seven concurrently. The unpause window is about one
// API round-trip, not zero: if a poll lands inside it the scenario detects the
// partial view (first cap line not "7 Queued", or first snapshot under five
// members) and fails with an explicit "re-run" message rather than reporting an
// engine regression.
//
// # What "first five by entry order" means here
//
// Entry order is (StatusEnteredAt, issue number), where StatusEnteredAt is
// stamped by the engine's store when it OBSERVES the Status change — not a
// GitHub timestamp. QueueMemberPaused files and places members sequentially, so
// placement order equals issue-number order under either observation timing
// (separate polls order by observation; members observed in one poll are
// stamped in board-fetch order, which is add order). "First five by entry
// order" is therefore the five lowest issue numbers. That cannot distinguish
// (StatusEnteredAt, Number) from Number alone, but it is exactly what the
// pre-#1833 map-order bug would get wrong (about a 1-in-21 chance of matching).
// Permuted placement to test true time ordering would cost 5+ minutes of extra
// wall-clock and is deliberately not done.
//
// # Isolation (R4)
//
// NOT t.Parallel(), on RepoAlpha/main — the default partition production uses.
// Any other Queued member on the same (repo, base) would join the partition and
// change the batch composition. Go runs non-parallel tests to completion before
// resuming any t.Parallel() test, so this runs with no sibling train scenario
// active (like restart, redsingleton and runaway); the run.sh "on" leg needs no
// change. A pre-flight fails loudly if a stale open, non-paused Queued item is
// already on the board.
//
// # Log scoping (R5)
//
// Every log read starts at a LogOffset taken immediately before the unpause and
// is scoped to this repo's trainKey and to this scenario's own members. The
// strings are copied from the engine and pinned by
// mergetrain_batchcap_parse_test.go.
//
// Wall-clock: ~30–60 min (two green trial CI cycles: a 5-member trial then a
// 2-member trial). Cost: low — no Claude conflict invocations (every member
// writes a distinct path).
func TestMergeTrainQueuedDeeperThanBatchCap(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	const (
		base        = "main"
		memberCount = 7
		batchCap    = 5
	)
	repo := env.RepoAlpha
	// The default-base partition's trainKey is the bare owner/repo (mergeTrainKey).
	trainKey := repo

	// R2 pre-flight: the scenario is only meaningful at the default cap of 5. This
	// is best-effort (the harness cannot read the running engine's config); the
	// authoritative check is the engine's own "max_batch_size=N" in the cap line.
	// There is deliberately no cache-mode check: board_cache_mode was never a real
	// config key (#1544) and a live bed always runs the in-memory cache.
	if n := configuredMaxBatchSize(env); n != 0 && n != batchCap {
		t.Skipf("bed max_batch_size is configured as %d, scenario needs the default %d (unset FABRIK_MAX_BATCH_SIZE / config.yaml max_batch_size)", n, batchCap)
	}

	// R4 pre-flight: nothing else may already be Queued on this partition.
	stale, err := staleQueuedMembers(env, repo)
	if err != nil {
		t.Fatalf("pre-flight: could not check %s for stale Queued items: %v", repo, err)
	}
	if len(stale) > 0 {
		t.Fatalf("pre-flight: %s already has open, non-paused Queued item(s) %v — they would join this batch and change its "+
			"composition; move them out of Queued (or close them) and re-run", repo, stale)
	}

	ensurePausedLabelExists(t, env, repo)
	scenarioStart := time.Now().Add(-time.Minute)

	// --- Queue seven clean members, all paused, in a known order. ---
	var nums, prs []int
	for i := 1; i <= memberCount; i++ {
		marker := fmt.Sprintf("bc%d", i)
		n, pr := QueueMemberPaused(t, env, repo, base, marker,
			fmt.Sprintf("e2e/train/batchcap/m%d.txt", i), fmt.Sprintf("batch-cap member %d\n", i))
		repauseOnFailure(t, env, repo, n)
		nums = append(nums, n)
		prs = append(prs, pr)
	}
	if !sort.IntsAreSorted(nums) {
		t.Fatalf("member issue numbers %v are not ascending — placement order no longer equals issue-number order, "+
			"so \"first five by entry order\" cannot be pinned to the five lowest numbers", nums)
	}
	firstFive, lastTwo := nums[:batchCap], nums[batchCap:]
	t.Logf("queued %d paused members %v (PRs %v); expected batches: %v then %v", memberCount, nums, prs, firstFive, lastTwo)

	// --- Release all seven at once. ---
	offset := LogOffset(t, env)
	if errs := removePausedConcurrently(env, repo, nums); len(errs) > 0 {
		t.Fatalf("could not release the batch (members stay paused; nothing has formed): %v", errs)
	}
	t.Logf("removed %s from all %d members; scanning bed log from offset %d", pausedLabel, memberCount, offset)

	isSnapshot := func(l string) bool { _, ok := parseBatchSnapshot(l, trainKey); return ok }
	isMerged := func(l string) bool { _, ok := parseMergedIntegration(l, repo); return ok }

	// --- A1: the first batch the engine forms. ---
	waitForLogMatch(t, env, offset, 10*time.Minute, "first \"batch snapshot for "+trainKey+"\" after the unpause", isSnapshot)
	lines := logLinesSince(t, env, offset)
	snapIdx := indexOfLine(lines, 0, isSnapshot)
	snapMembers, _ := parseBatchSnapshot(lines[snapIdx], trainKey)

	var capQueued, capMax int
	capFound := false
	for _, l := range lines[:snapIdx] {
		if q, m, ok := parseBatchCapped(l, trainKey); ok {
			capQueued, capMax, capFound = q, m, true
			break
		}
	}
	switch {
	case capFound && capMax != batchCap:
		t.Fatalf("engine reports max_batch_size=%d, scenario needs %d — bed prerequisite not met (see tests/e2e/README.md)", capMax, batchCap)
	case !capFound && len(snapMembers) >= memberCount:
		t.Fatalf("first snapshot lists %d members with no \"batch capped\" line — the bed's max_batch_size is >= %d, "+
			"so batch-cap selection was never exercised (scenario needs %d)", len(snapMembers), memberCount, batchCap)
	case !capFound || capQueued != memberCount || len(snapMembers) != batchCap:
		t.Fatalf("poll boundary straddled the unpause: the engine's first view of the partition had fewer than %d Queued members "+
			"(cap line found=%v, queued=%d; snapshot lists %d: %v). This is a harness race, not an engine regression — re-run.",
			memberCount, capFound, capQueued, len(snapMembers), snapMembers)
	}
	if !sameMemberSet(snapMembers, firstFive) {
		t.Fatalf("A1: first batch snapshot lists %v, want the first five by entry order %v (max_batch_size=%d, %d Queued) — "+
			"the capped selection is not the earliest-entered members (pre-#1833 map-order behaviour)\n%s",
			snapMembers, firstFive, capMax, capQueued, dumpLines(lines[:snapIdx+1]))
	}
	t.Logf("A1: cap line %d Queued / max_batch_size=%d; first snapshot %v (listed order) == first five %v", capQueued, capMax, snapMembers, firstFive)

	// --- A2: stability, from first snapshot to the batch-1 landing. ---
	// The window ends at "merged integration PR", not at "landing complete":
	// once members start advancing to Done the Queued set legitimately shrinks
	// and the engine logs a genuinely new snapshot for the remainder.
	waitForLogMatch(t, env, offset, 60*time.Minute, "\"merged integration PR\" for "+repo+" (batch 1)", isMerged)
	lines = logLinesSince(t, env, offset)
	mergeIdx := indexOfLine(lines, 0, isMerged)
	merged1, _ := parseMergedIntegration(lines[mergeIdx], repo)

	var window []string
	for _, l := range lines[:mergeIdx] {
		if isSnapshot(l) {
			window = append(window, l)
		}
	}
	if len(window) != 1 {
		t.Fatalf("A2: %d \"batch snapshot\" lines between the first snapshot and the batch-1 merge, want exactly 1 "+
			"(the composition changed, i.e. selection churned):\n%s", len(window), strings.Join(window, "\n"))
	}
	if got, _ := parseBatchSnapshot(window[0], trainKey); !sameMemberSet(got, firstFive) {
		t.Fatalf("A2: the only snapshot in the window lists %v, want %v", got, firstFive)
	}

	// The first trial must hold exactly five survivors, and the PR that merged
	// must be a five-survivor trial.
	trialSurvivors := map[int]int{}
	firstTrial := 0
	for _, l := range lines[:mergeIdx] {
		if pr, n, ok := parseOpenedDraftCI(l, repo); ok {
			trialSurvivors[pr] = n
			if firstTrial == 0 {
				firstTrial = pr
			}
		}
	}
	if firstTrial == 0 || trialSurvivors[firstTrial] != batchCap {
		t.Fatalf("A1: first trial before the batch-1 merge reports %d survivor(s) (PR #%d), want %d\n%s",
			trialSurvivors[firstTrial], firstTrial, batchCap, dumpLines(lines[:mergeIdx+1]))
	}
	if trialSurvivors[merged1] != batchCap {
		t.Fatalf("A1/A2: merged integration PR #%d was opened with %d survivor(s) (%v), want %d",
			merged1, trialSurvivors[merged1], trialSurvivors, batchCap)
	}
	t.Logf("A1/A2: exactly one snapshot before merge; first trial PR #%d had %d survivors; batch-1 integration PR #%d merged",
		firstTrial, batchCap, merged1)

	// --- A3: all seven land, in two batches. ---
	for i, n := range firstFive {
		WaitForMemberLanded(t, env, repo, n, 15*time.Minute)
		waitForPRClosed(t, env, repo, prs[i], 5*time.Minute)
	}
	// Batch 2 needs a second trial CI cycle; the first wait carries that budget.
	WaitForMemberLanded(t, env, repo, lastTwo[0], 60*time.Minute)
	WaitForMemberLanded(t, env, repo, lastTwo[1], 10*time.Minute)
	for _, pr := range prs[batchCap:] {
		waitForPRClosed(t, env, repo, pr, 5*time.Minute)
	}
	waitForLogMatch(t, env, offset, 5*time.Minute, "second \"landing complete\" (2 members) for "+trainKey, func(l string) bool {
		_, n, ok := parseLandingComplete(l, trainKey)
		return ok && n == len(lastTwo)
	})
	lines = logLinesSince(t, env, offset)

	type landing struct{ pr, members int }
	var landings []landing
	var mergedPRs []int
	for _, l := range lines {
		if pr, n, ok := parseLandingComplete(l, trainKey); ok {
			landings = append(landings, landing{pr, n})
		}
		if pr, ok := parseMergedIntegration(l, repo); ok {
			mergedPRs = append(mergedPRs, pr)
		}
	}
	if len(landings) != 2 || landings[0].members != batchCap || landings[1].members != len(lastTwo) {
		t.Fatalf("A3: \"landing complete\" lines = %+v, want exactly two: %d members then %d", landings, batchCap, len(lastTwo))
	}
	if len(mergedPRs) != 2 || mergedPRs[0] != landings[0].pr || mergedPRs[1] != landings[1].pr {
		t.Fatalf("A3: \"merged integration PR\" numbers %v do not match the landing-complete PRs %+v", mergedPRs, landings)
	}

	// Batch 2: a two-survivor trial, preceded by a snapshot of exactly {m6, m7}.
	merged2 := mergedPRs[1]
	trial2Idx := indexOfLine(lines, mergeIdx, func(l string) bool {
		pr, _, ok := parseOpenedDraftCI(l, repo)
		return ok && pr == merged2
	})
	if trial2Idx < 0 {
		t.Fatalf("A3: no \"opened draft CI PR #%d\" line for the batch-2 integration PR\n%s", merged2, dumpLines(lines))
	}
	if _, n, _ := parseOpenedDraftCI(lines[trial2Idx], repo); n != len(lastTwo) {
		t.Fatalf("A3: batch-2 trial PR #%d opened with %d survivor(s), want %d", merged2, n, len(lastTwo))
	}
	var lastSnapBeforeTrial2 []int
	for _, l := range lines[:trial2Idx] {
		if got, ok := parseBatchSnapshot(l, trainKey); ok {
			lastSnapBeforeTrial2 = got
		}
	}
	if !sameMemberSet(lastSnapBeforeTrial2, lastTwo) {
		t.Fatalf("A3: last snapshot before the batch-2 trial lists %v, want exactly the remaining two %v", lastSnapBeforeTrial2, lastTwo)
	}
	t.Logf("A3 (log): landings %+v; batch-2 trial PR #%d had %d survivors after snapshot %v", landings, merged2, len(lastTwo), lastSnapBeforeTrial2)

	// --- GitHub state: two merged integration PRs, none closed-unmerged. ---
	trainPRs, err := listTrainPRsSince(env, repo, scenarioStart, nums)
	if err != nil {
		t.Fatalf("list this scenario's train PRs: %v", err)
	}
	var desc []string
	for _, p := range trainPRs {
		desc = append(desc, fmt.Sprintf("#%d(state=%s merged=%v members=%v)", p.Number, p.State, p.Merged, p.Members))
	}
	for _, p := range trainPRs {
		if !p.Merged {
			t.Fatalf("A2: train PR #%d is %s and not merged — a trial was abandoned or left open. PRs: %s", p.Number, p.State, strings.Join(desc, " "))
		}
	}
	if len(trainPRs) != 2 {
		t.Fatalf("A3: %d train PR(s) cover these members, want exactly 2 (5 then 2): %s", len(trainPRs), strings.Join(desc, " "))
	}
	if !sameMemberSet(trainPRs[0].Members, firstFive) || !sameMemberSet(trainPRs[1].Members, lastTwo) {
		t.Fatalf("A3: train PR memberships %s, want %v then %v", strings.Join(desc, " "), firstFive, lastTwo)
	}
	if trainPRs[0].Number != landings[0].pr || trainPRs[1].Number != landings[1].pr {
		t.Fatalf("A3: GitHub train PRs %s do not match the logged landing PRs %+v", strings.Join(desc, " "), landings)
	}
	WaitForNoStaleTrainArtifacts(t, env, repo, 2*time.Minute)
	t.Logf("batch-cap verified: %d members landed as %v (PR #%d) then %v (PR #%d); one stable snapshot, no abandoned trial",
		memberCount, firstFive, trainPRs[0].Number, lastTwo, trainPRs[1].Number)
}

// indexOfLine returns the index of the first line at or after from satisfying
// match, or -1.
func indexOfLine(lines []string, from int, match func(string) bool) int {
	for i := from; i < len(lines); i++ {
		if match(lines[i]) {
			return i
		}
	}
	return -1
}

// dumpLines renders only the merge-train lines of a log window, for failure output.
func dumpLines(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		if strings.Contains(l, "merge-train") {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
