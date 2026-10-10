package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
)

// Post-landing invalidation of conflicting Queued members (#2047, ADR-2047,
// docs/state-machine.md §6.32).
//
// Once a train lands, the partition's base has moved. Every still-Queued member that now
// genuinely conflicts with the new base is stale, and without this scan the train only
// finds out by spending a trial (and often a pause) on each one. The scan is cheap and
// local: one `git fetch`, then `git merge-tree --write-tree` per member in the repo's own
// bare clone — no CI, no GitHub API reads. Only a real merge conflict reroutes a member;
// file overlap alone never does (FR-008), and any inability to run the check leaves the
// member alone (FR-011 — fail-SAFE, the opposite polarity to the overlap filter, because
// here the costly mistake is moving a member that was fine).
//
// It runs on the poll goroutine in routeQueuedGroup, and only while no merge-train worker
// is in flight for the partition. With no worker, nobody owns the Queued members, so the
// poll goroutine may reroute them directly (the ADR-1208 ownership rule), and it sees the
// full uncapped partition including members the worker deferred for overlap.

// landedMember is one member a landing path recorded.
type landedMember struct {
	number int
	prNum  int
}

type invalidateState struct {
	mu sync.Mutex
	// landed holds, per trainKey, the members landed since the partition was last
	// scanned. Written by landing paths (worker goroutines), consumed by the poll
	// goroutine. In memory only: a restart drops a pending scan, which costs one trial.
	landed map[string]map[int]landedMember
	// notified is the head SHA each member was last announced as invalidated at
	// ("owner/repo#N"), the ping-pong backstop that keeps a stale-cache bounce from
	// producing a comment every poll (mirrors mergeTrainCIDeferred).
	notified map[string]string
	disabled bool // test seam: SetMergeTrainInvalidationDisabledForTest
}

// SetMergeTrainInvalidationDisabledForTest neutralises the post-landing scan (the FR-015
// seam): landings are not recorded and no scan runs.
func (e *Engine) SetMergeTrainInvalidationDisabledForTest(disabled bool) {
	e.invalidate.mu.Lock()
	defer e.invalidate.mu.Unlock()
	e.invalidate.disabled = disabled
}

// noteTrainLanded records that member m landed on trainKey's partition, so the next scan
// of that partition knows the base moved and which members to exclude and attribute to.
// Called beside resetEjectionCount at every landing site.
func (e *Engine) noteTrainLanded(trainKey string, m trainMember) {
	e.invalidate.mu.Lock()
	defer e.invalidate.mu.Unlock()
	if e.invalidate.disabled {
		return
	}
	if e.invalidate.landed == nil {
		e.invalidate.landed = make(map[string]map[int]landedMember)
	}
	if e.invalidate.landed[trainKey] == nil {
		e.invalidate.landed[trainKey] = make(map[int]landedMember)
	}
	e.invalidate.landed[trainKey][m.item.Number] = landedMember{number: m.item.Number, prNum: m.prNum}
}

// hasTrainLanded reports whether a landing awaits a scan for trainKey.
func (e *Engine) hasTrainLanded(trainKey string) bool {
	e.invalidate.mu.Lock()
	defer e.invalidate.mu.Unlock()
	return len(e.invalidate.landed[trainKey]) > 0
}

// takeTrainLanded consumes the landed set for trainKey. Consumed exactly once, so there
// is no retry loop: a scan that could not run costs at most what the train costs today.
func (e *Engine) takeTrainLanded(trainKey string) []landedMember {
	e.invalidate.mu.Lock()
	defer e.invalidate.mu.Unlock()
	set := e.invalidate.landed[trainKey]
	delete(e.invalidate.landed, trainKey)
	out := make([]landedMember, 0, len(set))
	for _, lm := range set {
		out = append(out, lm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out
}

// recordInvalidated reports whether this is a NEW invalidation of issue n at headSHA.
func (e *Engine) recordInvalidated(owner, repo string, n int, headSHA string) bool {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, n)
	e.invalidate.mu.Lock()
	defer e.invalidate.mu.Unlock()
	if e.invalidate.notified == nil {
		e.invalidate.notified = make(map[string]string)
	}
	if e.invalidate.notified[key] == headSHA {
		return false
	}
	e.invalidate.notified[key] = headSHA
	return true
}

// mergeTreeConflicts runs `git merge-tree --write-tree` for baseRef and headRef in dir
// (the repo's bare clone). A clean merge exits 0 (conflict=false). A real merge conflict
// exits 1 and prints the merged tree's OID on the first stdout line, followed by the
// conflicted paths (conflict=true). Every other outcome — a git too old for --write-tree
// (< 2.38), an unknown ref or a missing object — is an error, which callers map to "leave
// the member alone".
//
// Exit 1 alone is NOT proof of a conflict: git merge-tree also exits 1 for an unresolvable
// ref ("not something we can merge", empty stdout). The conflict is therefore only
// believed when stdout begins with an object ID, and both refs are resolved up front.
//
// It writes tree objects into the shared bare clone. They are unreferenced and are
// collected by `git gc`; they are not refs, so ADR-1835's ref sweeping never sees them.
func mergeTreeConflicts(dir, baseRef, headRef string) (paths []string, conflict bool, err error) {
	for _, ref := range []string{baseRef, headRef} {
		if _, rerr := gitRevParse(dir, ref+"^{commit}"); rerr != nil {
			return nil, false, fmt.Errorf("cannot resolve %s: %w", ref, rerr)
		}
	}
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--name-only", "--no-messages", baseRef, headRef)
	cmd.Dir = dir
	cmd.Env = nonInteractiveGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return nil, false, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
		return nil, false, fmt.Errorf("git merge-tree %s %s: %w: %s", baseRef, headRef, runErr, strings.TrimSpace(stderr.String()))
	}
	// Output: <tree OID>\n<conflicted path>\n...  (--no-messages drops the trailing
	// informational block).
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if !objectIDRE.MatchString(strings.TrimSpace(lines[0])) {
		return nil, false, fmt.Errorf("git merge-tree %s %s: exit 1 without a result tree (not a conflict): %s", baseRef, headRef, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, true, nil
}

// objectIDRE matches a full SHA-1 or SHA-256 object ID.
var objectIDRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// landedAttribution names the landed members a conflict is attributable to: those whose
// cached changed files intersect the conflicted paths. merge-tree cannot say which landed
// member caused a conflict, so when nothing intersects (or no list is cached) every
// member landed this pass is named instead.
func (e *Engine) landedAttribution(owner, repo string, landed []landedMember, conflicted []string) []int {
	set := make(map[string]bool, len(conflicted))
	for _, p := range conflicted {
		set[p] = true
	}
	var hit []int
	for _, lm := range landed {
		files, ok := e.cachedPRFiles(owner, repo, lm.prNum)
		if !ok {
			continue
		}
		for _, f := range files {
			if set[f] {
				hit = append(hit, lm.number)
				break
			}
		}
	}
	if len(hit) == 0 {
		for _, lm := range landed {
			hit = append(hit, lm.number)
		}
	}
	return hit
}

func joinIssueRefs(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}

// invalidateConflictingQueued is the post-landing scan for one partition. candidates is
// the partition's train candidates (queue-enabled items already excluded); it returns
// those that remain Queued — a rerouted member is removed so it cannot be batched in the
// same poll. Every failure path returns the candidates unchanged.
func (e *Engine) invalidateConflictingQueued(g queuedRepoGroup, projectID string, landed []landedMember, candidates []gh.ProjectItem) []gh.ProjectItem {
	if len(landed) == 0 || len(candidates) == 0 {
		return candidates
	}
	repoKey := g.repoKey
	owner, repo := parseOwnerRepo(repoKey)

	// ADR-1821 R9: a rerouted member is claimed by the reroute target's wait_for_ci gate;
	// without it nothing would re-detect the conflict and the member would strand.
	target := stageBeforeHolding(e.cfg, holdingStage(e.cfg))
	if target == nil || target.WaitForCI == nil || !*target.WaitForCI {
		e.logfRepo(repoKey, "merge-train", "post-landing invalidation skipped for %s: reroute target stage does not have wait_for_ci — nothing would re-detect a rerouted member's conflict\n", g.trainKey)
		return candidates
	}

	e.mu.Lock()
	wm, ok := e.worktreeManagers[repoKey]
	e.mu.Unlock()
	if !ok {
		e.logfRepo(repoKey, "merge-train", "post-landing invalidation skipped for %s: no WorktreeManager registered\n", g.trainKey)
		return candidates
	}

	base := g.base
	if base == defaultPartitionBase {
		var err error
		if base, err = wm.DefaultBaseBranch(); err != nil {
			e.logfRepo(repoKey, "merge-train", "post-landing invalidation skipped for %s: cannot determine base branch: %v\n", g.trainKey, err)
			return candidates
		}
	}

	// A git network call, not an API call: the bare clone must see the landing before a
	// merge-tree against the new base means anything. If the fetch fails the old base may
	// be used, which finds no spurious conflict — a missed invalidation, never a false one.
	if out, err := wm.FetchOrigin(); err != nil {
		e.logfRepo(repoKey, "merge-train", "post-landing invalidation for %s: fetch origin failed: %s — leaving Queued members alone\n", g.trainKey, strings.TrimSpace(out))
		return candidates
	}
	baseRef := "refs/remotes/origin/" + base
	if _, err := gitRevParse(wm.BaseDir(), baseRef+"^{commit}"); err != nil {
		e.logfRepo(repoKey, "merge-train", "post-landing invalidation for %s: cannot resolve %s: %v — leaving Queued members alone\n", g.trainKey, baseRef, err)
		return candidates
	}

	landedSet := make(map[int]bool, len(landed))
	for _, lm := range landed {
		landedSet[lm.number] = true
	}

	remaining := make([]gh.ProjectItem, 0, len(candidates))
	for _, item := range candidates {
		if landedSet[item.Number] || hasLabel(item.Labels, "fabrik:editing") || hasLabel(item.Labels, "fabrik:paused") {
			remaining = append(remaining, item)
			continue
		}
		headRef := fmt.Sprintf("refs/remotes/origin/fabrik/issue-%d", item.Number)
		headSHA, err := gitRevParse(wm.BaseDir(), headRef+"^{commit}")
		if err != nil {
			e.logf(item.Number, "merge-train", "post-landing check of #%d skipped: cannot resolve %s: %v — leaving Queued\n", item.Number, headRef, err)
			remaining = append(remaining, item)
			continue
		}
		conflicted, conflict, err := mergeTreeConflicts(wm.BaseDir(), baseRef, headSHA)
		if err != nil {
			e.logf(item.Number, "merge-train", "post-landing check of #%d failed: %v — leaving Queued\n", item.Number, err)
			remaining = append(remaining, item)
			continue
		}
		if !conflict {
			remaining = append(remaining, item)
			continue
		}
		if !e.rerouteInvalidatedMember(projectID, owner, repo, item, headSHA, target.Name, e.landedAttribution(owner, repo, landed, conflicted)) {
			remaining = append(remaining, item)
		}
	}
	return remaining
}

// rerouteInvalidatedMember moves a member whose merge with the new base conflicts off
// Queued (FR-007, ADR-1208 reroute-before-side-effects): the reroute first, and only on
// success the label, the comment and the log line. It never calls ejectMember, never
// touches mergeTrainEjectionCounts and never pauses (FR-009). Reports whether the member
// left Queued.
//
// fabrik:rebase-needed is applied directly (best-effort) rather than left to Validate's
// mergeability gate: GitHub's mergeable state can lag the push by several polls, and a
// member that kept stage:Validate:complete would otherwise bounce straight back to Queued
// under yolo/cruise. The gate re-derives the label from live mergeability on every pass,
// so a wrongly applied one self-heals.
func (e *Engine) rerouteInvalidatedMember(projectID, owner, repo string, item gh.ProjectItem, headSHA, targetName string, causes []int) bool {
	n := item.Number
	if !e.rerouteQueuedMemberOffHolding(projectID, item) {
		e.logf(n, "merge-train", "#%d conflicts with the new base but could not be rerouted off Queued — leaving it for a trial\n", n)
		return false
	}

	repoKey := owner + "/" + repo
	if count, ok := e.takePendingReviewEject(repoKey, n); ok {
		e.logf(n, "merge-train", "dropping pending review-finding eject signal (%d finding(s)) for #%d — superseded by the post-landing reroute\n", count, n)
	}
	if e.takePendingCommentEject(repoKey, n) {
		e.logf(n, "merge-train", "dropping pending unprocessed-comment eject signal for #%d — superseded by the post-landing reroute\n", n)
	}

	if !hasLabel(item.Labels, "fabrik:rebase-needed") {
		if err := e.addLabelChecked(item, "fabrik:rebase-needed"); err != nil {
			e.logf(n, "merge-train", "warn: could not apply fabrik:rebase-needed to #%d: %v — %s's mergeability gate will derive it\n", n, err, targetName)
		}
	}

	e.logf(n, "merge-train", "invalidated #%d: conflicts with landed %s (merge-tree)\n", n, joinIssueRefs(causes))

	if !e.recordInvalidated(owner, repo, n, headSHA) {
		return true
	}
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — rerouted (conflicts with new base)**\n\n"+
		"After %s landed, #%d no longer merges cleanly into the base branch (checked locally with `git merge-tree` against head `%s`), so it was taken out of the Queued column before a trial could be spent on it. "+
		"This issue has moved back to %s with `fabrik:rebase-needed` and has **not** been paused; the ordinary rebase path will resolve the conflict, and once %s completes again it will re-queue.",
		joinIssueRefs(causes), n, shortSHA(headSHA), targetName, targetName)
	if _, err := e.client.AddComment(owner, repo, n, msg); err != nil {
		e.logf(n, "merge-train", "warn: could not post post-landing reroute comment: %v\n", err)
	}
	return true
}
