package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// The local read API backend (#1967, ADR-1966-a).
//
// Zero-GitHub-cost rule (R7): everything here is served from the item Store
// and the engine's own in-memory state. This file must never reach GitHub or a
// read path that can — not through the engine's GitHub clients, not through
// Store.Get (it has a GitHub fallback), and not through the engine's
// label-applied-at helper (it falls back to a live REST fetch). It reads the
// Store only through Peek and Scan, and label timestamps only from the
// snapshot's record-on-write map, where a missing entry means "unknown".
// TestLocalAPISourceDoesNotReachGitHub pins this statically.
//
// Concurrency: the API runs on socket goroutines. It reads the Store (RLock,
// copies out), already-guarded engine state (claudeSuspendMu, sem, the
// Store's repo-worker markers) and daemonHealth, a mutex-guarded mirror of the
// poll loop's own single-goroutine fields. It never holds a lock across
// socket I/O: each call snapshots, releases, then returns a plain value that
// the server serialises.

// LocalAPIBackend returns the engine's localapi.Backend.
func (e *Engine) LocalAPIBackend() localapi.Backend { return localAPIBackend{e: e} }

type localAPIBackend struct{ e *Engine }

const statusEnteredBasis = "daemon-observed: since this daemon process first saw the item in its current column (resets on every restart, ADR-1833)"

// Cycle-counter kinds reported by fabrik_status, in display order.
var counterKinds = []string{"attempts", "review-cycles", "ci-fix-cycles", "rebase-cycles", "enqueue-cycles", "tools-denied-retries", "slice-retries"}

// ---- envelope ----

func (b localAPIBackend) envelope(now time.Time) localapi.Envelope {
	h := b.e.health.snapshot()
	env := localapi.Envelope{
		AsOf:              now.UTC(),
		LastPollAttemptAt: localapi.KnownTime(h.PollAttemptAt),
		LastPollSuccessAt: localapi.KnownTime(h.PollSuccessAt),
	}
	if !h.PollSuccessAt.IsZero() {
		env.SinceLastPollSuccessSeconds = localapi.Some(secs(now.Sub(h.PollSuccessAt)))
	}
	if !h.StartedAt.IsZero() {
		if up := now.Sub(h.StartedAt); up > 0 {
			env.DaemonUptimeSeconds = secs(up)
		}
	}
	return env
}

func secs(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

func ageSince(now, t time.Time) localapi.Known[int64] {
	if t.IsZero() {
		return localapi.Unknown[int64]()
	}
	return localapi.Some(secs(now.Sub(t)))
}

func (b localAPIBackend) stallThreshold(overrideSeconds int64) time.Duration {
	if overrideSeconds > 0 {
		return time.Duration(overrideSeconds) * time.Second
	}
	if b.e.cfg.StallThreshold > 0 {
		return b.e.cfg.StallThreshold
	}
	return 30 * time.Minute
}

// ---- stage and attention helpers ----

func (e *Engine) stageByName(name string) *stages.Stage {
	for _, s := range e.cfg.Stages {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// workerBudget is the wall-clock budget of a worker running the stage at
// status: its max_wall_time, or the Claude inactivity timeout when the stage
// sets none (a silent worker is killed at that point).
func (e *Engine) workerBudget(status string) time.Duration {
	if s := e.stageByName(status); s != nil && s.MaxWallTime > 0 {
		return s.MaxWallTime
	}
	return claudeInactivityTimeout
}

func (e *Engine) attentionStageKind(status string) attention.StageKind {
	s := e.stageByName(status)
	switch {
	case s == nil:
		return attention.KindNone
	case s.Unmanaged:
		return attention.KindUnmanaged
	case s.CleanupWorktree:
		return attention.KindCleanup
	case s.HoldingStage:
		return attention.KindHolding
	default:
		return attention.KindManaged
	}
}

// limitFor resolves a counter kind's configured limit for display and for the
// attention classifier. A zero limit is "unknown" (a test engine never
// resolved one) except for attempts, where MaxRetries == 0 means unlimited.
func (e *Engine) limitFor(kind string) localapi.LimitMax {
	var n int
	switch kind {
	case "attempts":
		if e.cfg.MaxRetries <= 0 {
			return localapi.LimitMax{Kind: "unlimited"}
		}
		n = e.cfg.MaxRetries
	case "review-cycles":
		n = e.cfg.MaxReviewCycles
	case "ci-fix-cycles":
		n = e.cfg.MaxCiFixCycles
	case "rebase-cycles":
		n = e.cfg.MaxRebaseCycles
	case "enqueue-cycles":
		n = e.cfg.MaxEnqueueCycles
	case "tools-denied-retries":
		n = e.cfg.MaxToolsDeniedRetries
	case "slice-retries":
		n = e.cfg.MaxSliceRetries
	}
	if n > 0 {
		return localapi.LimitMax{Kind: "limit", Value: n}
	}
	return localapi.LimitMax{Kind: "unknown"}
}

func counterValue(ss itemstate.StageState, kind, stage string) int {
	switch kind {
	case "attempts":
		return ss.Attempts[stage]
	case "review-cycles":
		return ss.ReviewCycles[stage]
	case "ci-fix-cycles":
		return ss.CIFixCycles[stage]
	case "rebase-cycles":
		return ss.RebaseCycles[stage]
	case "enqueue-cycles":
		return ss.EnqueueCycles[stage]
	case "tools-denied-retries":
		return ss.ToolsDeniedRetries[stage]
	case "slice-retries":
		return ss.SliceRetries[stage]
	}
	return 0
}

// counterStages lists every stage name that has an entry for kind.
func counterStages(ss itemstate.StageState, kind string) []string {
	var names []string
	add := func(m map[string]int) {
		for k := range m {
			names = append(names, k)
		}
	}
	switch kind {
	case "attempts":
		add(ss.Attempts)
	case "review-cycles":
		add(ss.ReviewCycles)
	case "ci-fix-cycles":
		add(ss.CIFixCycles)
	case "rebase-cycles":
		add(ss.RebaseCycles)
	case "enqueue-cycles":
		add(ss.EnqueueCycles)
	case "tools-denied-retries":
		add(ss.ToolsDeniedRetries)
	case "slice-retries":
		add(ss.SliceRetries)
	}
	return names
}

// attentionInput extracts the classifier input from one item's state. st may
// point at live Store memory (inside Scan): everything is copied out, nothing
// retained, nothing blocking.
func (e *Engine) attentionInput(st *itemstate.ItemState, now time.Time, threshold time.Duration, suspendedUntil time.Time) attention.Input {
	in := attention.Input{
		Now: now,
		Cfg: attention.Config{
			StallThreshold:    threshold,
			CIWaitTimeout:     e.ciWaitTimeout(),
			CIBackstopTimeout: e.ciBackstopTimeout(),
			ReviewWaitTimeout: e.cfg.ReviewWaitTimeout,
		},
		Status:               st.Status,
		StageKind:            e.attentionStageKind(st.Status),
		Closed:               st.IsClosed,
		Terminal:             st.Terminal,
		Labels:               append([]string(nil), st.Labels...),
		PausedByEngine:       st.StageState.PausedByEngine[st.Status],
		StatusEnteredAt:      st.StatusEnteredAt,
		ClaudeSuspendedUntil: suspendedUntil,
	}
	for _, kind := range counterKinds {
		in.Counters = append(in.Counters, attention.Counter{
			Name: kind,
			N:    counterValue(st.StageState, kind, st.Status),
			Max:  attentionMax(e.limitFor(kind)),
		})
	}
	if w := st.Worker; w != nil {
		in.HasWorker = true
		in.WorkerStartedAt = w.StartedAt
		in.WorkerBudget = e.workerBudget(st.Status)
	}
	if len(st.CooldownAt) > 0 {
		in.Cooldowns = make(map[string]time.Time, len(st.CooldownAt))
		for k, v := range st.CooldownAt {
			in.Cooldowns[k] = v
		}
	}
	if len(st.LabelAppliedAt) > 0 {
		in.LabelAppliedAt = make(map[string]time.Time, len(st.LabelAppliedAt))
		for k, v := range st.LabelAppliedAt {
			in.LabelAppliedAt[k] = v
		}
	}
	for _, t := range st.StageState.LastAttemptAt {
		if t.After(in.LastAttemptAt) {
			in.LastAttemptAt = t
		}
	}
	if lpr := st.LinkedPR; lpr != nil {
		in.LastCIProgressAt = lpr.LastCIProgressAt
		in.HasOpenUnmergedPR = lpr.Number != 0 && !lpr.Merged && lpr.State != "closed"
	}
	return in
}

func attentionMax(l localapi.LimitMax) int {
	switch l.Kind {
	case "limit":
		return l.Value
	case "unlimited":
		return attention.MaxUnlimited
	}
	return 0
}

func attentionInfo(res attention.Result, now time.Time, threshold time.Duration, latest localapi.Known[string]) localapi.AttentionInfo {
	info := localapi.AttentionInfo{
		State:                 string(res.State),
		Code:                  res.Code,
		Summary:               res.Summary,
		Reasons:               []localapi.ReasonInfo{},
		LatestFabrikComment:   latest,
		StallThresholdSeconds: secs(threshold),
		Next: localapi.NextInfo{
			Action:    res.Next.Action,
			At:        localapi.KnownTime(res.Next.At),
			Deadlines: []localapi.DeadlineInfo{},
		},
		Progress: localapi.ProgressInfo{At: localapi.KnownTime(res.ProgressAt)},
	}
	if !res.ProgressAt.IsZero() {
		info.Progress.AgeSeconds = localapi.Some(secs(res.ProgressAge))
	}
	for _, r := range res.Reasons {
		info.Reasons = append(info.Reasons, localapi.ReasonInfo{Code: r.Code, Meaning: r.Meaning})
	}
	for _, d := range res.Next.Deadlines {
		info.Next.Deadlines = append(info.Next.Deadlines, localapi.DeadlineInfo{
			Kind: d.Kind, At: localapi.KnownTime(d.At), Basis: d.Basis,
		})
	}
	return info
}

func milestoneField(st *itemstate.ItemState) localapi.MilestoneField {
	switch {
	case !st.MilestoneKnown:
		return localapi.MilestoneField{State: localapi.MilestoneUnknown}
	case st.Milestone == nil:
		return localapi.MilestoneField{State: localapi.MilestoneNone}
	default:
		return localapi.MilestoneField{State: localapi.MilestoneSet, Title: st.Milestone.Title, Number: st.Milestone.Number}
	}
}

func hasLabelStr(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

func issueRef(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

// ---- status ----

// resolveIssue parses "owner/repo#N" or a bare "N" (valid only when the daemon
// manages exactly one repo).
func (b localAPIBackend) resolveIssue(ref string) (repo string, number int, err error) {
	ref = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if ref == "" {
		return "", 0, localapi.Errorf(localapi.CodeBadRequest, "issue is required: owner/repo#N, or N when the daemon manages exactly one repo")
	}
	if i := strings.LastIndex(ref, "#"); i > 0 {
		n, perr := strconv.Atoi(ref[i+1:])
		if perr != nil || n <= 0 || !strings.Contains(ref[:i], "/") {
			return "", 0, localapi.Errorf(localapi.CodeBadRequest, "bad issue %q: want owner/repo#N", ref)
		}
		return ref[:i], n, nil
	}
	n, perr := strconv.Atoi(ref)
	if perr != nil || n <= 0 {
		return "", 0, localapi.Errorf(localapi.CodeBadRequest, "bad issue %q: want owner/repo#N or N", ref)
	}
	repos := b.managedRepos()
	switch len(repos) {
	case 1:
		return repos[0], n, nil
	case 0:
		return "", 0, localapi.Errorf(localapi.CodeNotFound, "the daemon has no cached items yet; use owner/repo#N")
	default:
		return "", 0, localapi.Errorf(localapi.CodeAmbiguous, "the daemon manages %d repos (%s); use owner/repo#N", len(repos), strings.Join(repos, ", "))
	}
}

// managedRepos is the repo set this daemon serves: its configured repo, else
// the distinct repos of the cached items.
func (b localAPIBackend) managedRepos() []string {
	if r := b.e.defaultRepo(); r != "" {
		return []string{r}
	}
	seen := map[string]bool{}
	b.e.store.Scan(func(it *itemstate.ItemState) {
		if !it.IsPR && it.Repo != "" {
			seen[it.Repo] = true
		}
	})
	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos
}

func (b localAPIBackend) suspendedUntil(now time.Time) time.Time {
	d, _ := b.e.claudeSuspendedUntilTime(now)
	return d
}

func (b localAPIBackend) Status(p localapi.StatusParams) (*localapi.StatusResult, error) {
	repo, number, err := b.resolveIssue(p.Issue)
	if err != nil {
		return nil, err
	}
	snap, ok := b.e.store.Peek(repo, number)
	if !ok {
		return nil, localapi.Errorf(localapi.CodeNotFound, "%s is not in the daemon's cache (the daemon does not fetch from GitHub to answer)", issueRef(repo, number))
	}
	st := snap.State()
	now := b.e.now()
	threshold := b.stallThreshold(0)
	res := attention.Classify(b.e.attentionInput(&st, now, threshold, b.suspendedUntil(now)))

	out := &localapi.StatusResult{
		Envelope:  b.envelope(now),
		Issue:     issueRef(st.Repo, st.Number),
		Title:     st.Title,
		URL:       knownString(st.URL),
		Milestone: milestoneField(&st),
		Cache: localapi.CacheInfo{
			LastDeepFetchAt: localapi.KnownTime(st.LastDeepFetchAt),
			AgeSeconds:      ageSince(now, st.LastDeepFetchAt),
		},
		Place:      b.place(&st),
		Limits:     b.limits(&st),
		Worker:     b.worker(&st),
		LastInvoke: lastInvocation(&st),
		PR:         b.prInfo(&st),
		Blockers:   blockers(&st),
		Attention:  attentionInfo(res, now, threshold, latestFabrikComment(&st)),
	}
	if pr := &out.PR; pr.State == "linked" && st.Status != "" {
		if hs := holdingStage(b.e.cfg); hs != nil && st.Status == hs.Name {
			pr.MergeTrain = b.trainPosition(&st)
		}
	}
	return out, nil
}

func knownString(s string) localapi.Known[string] {
	if s == "" {
		return localapi.Unknown[string]()
	}
	return localapi.Some(s)
}

func (b localAPIBackend) place(st *itemstate.ItemState) localapi.Place {
	pl := localapi.Place{
		Status:             knownString(st.Status),
		StatusEnteredAt:    localapi.KnownTime(st.StatusEnteredAt),
		StatusEnteredBasis: statusEnteredBasis,
		StageLabels:        []string{},
		FabrikLabels:       []string{},
		Autonomy:           "none",
	}
	labels := append([]string(nil), st.Labels...)
	sort.Strings(labels)
	for _, l := range labels {
		switch {
		case strings.HasPrefix(l, "stage:"):
			pl.StageLabels = append(pl.StageLabels, l)
		case strings.HasPrefix(l, "fabrik:"):
			pl.FabrikLabels = append(pl.FabrikLabels, l)
		}
	}
	switch {
	case hasLabelStr(st.Labels, "fabrik:cruise"):
		pl.Autonomy = "cruise" // cruise wins when both are present
	case hasLabelStr(st.Labels, "fabrik:yolo"):
		pl.Autonomy = "yolo"
	}
	return pl
}

func (b localAPIBackend) limits(st *itemstate.ItemState) []localapi.Limit {
	out := []localapi.Limit{}
	for _, kind := range counterKinds {
		max := b.e.limitFor(kind)
		stageSet := map[string]bool{}
		for _, s := range counterStages(st.StageState, kind) {
			if counterValue(st.StageState, kind, s) != 0 {
				stageSet[s] = true
			}
		}
		// The current stage always reports a configured limit, even at zero.
		if st.Status != "" && max.Kind != "unknown" {
			stageSet[st.Status] = true
		}
		names := make([]string, 0, len(stageSet))
		for s := range stageSet {
			names = append(names, s)
		}
		sort.Strings(names)
		for _, s := range names {
			out = append(out, localapi.Limit{Name: s + ":" + kind, N: counterValue(st.StageState, kind, s), Max: max})
		}
	}
	return out
}

func (b localAPIBackend) worker(st *itemstate.ItemState) localapi.WorkerInfo {
	w := localapi.WorkerInfo{
		StartedAt:     localapi.Unknown[time.Time](),
		LastSignAt:    localapi.Unknown[time.Time](),
		TurnsUsed:     localapi.Unknown[int](), // never tracked live for an in-flight worker
		MaxTurns:      localapi.Unknown[int](),
		LastTurnsUsed: localapi.Unknown[int](),
	}
	stage := st.Status
	if st.Worker != nil {
		w.InFlight = true
		w.Stage = st.Worker.StageName
		w.StartedAt = localapi.KnownTime(st.Worker.StartedAt)
		w.LastSignAt = localapi.KnownTime(st.Worker.LastSignAt)
		stage = st.Worker.StageName
	}
	if s := b.e.stageByName(stage); s != nil && s.MaxTurns > 0 && w.InFlight {
		w.MaxTurns = localapi.Some(s.MaxTurns)
	}
	if n, ok := st.StageState.LastTurnsUsed[stage]; ok {
		w.LastTurnsUsed = localapi.Some(n)
	}
	return w
}

func lastInvocation(st *itemstate.ItemState) localapi.InvocationInfo {
	info := localapi.InvocationInfo{
		Outcome:         "unknown",
		IsComment:       st.LastInvocationIsComment,
		DurationSeconds: localapi.Unknown[float64](),
		Tokens:          localapi.Unknown[localapi.Tokens](),
	}
	tu := st.LastTokenUsage
	recorded := st.LastInvocationDuration > 0 || tu != (itemstate.TokenUsage{}) || len(st.StageState.LastTurnsUsed) > 0
	switch {
	case st.LastInvocationCompleted:
		info.Outcome = "completed"
	case st.LastInvocationBlocked:
		info.Outcome = "blocked"
	case st.LastInvocationErrored:
		info.Outcome = "errored"
	case st.LastInvocationTurnLimited:
		info.Outcome = "turn-limited"
	case recorded:
		info.Outcome = "incomplete"
	}
	if st.LastInvocationDuration > 0 {
		info.DurationSeconds = localapi.Some(st.LastInvocationDuration.Seconds())
	}
	if tu != (itemstate.TokenUsage{}) {
		info.Tokens = localapi.Some(localapi.Tokens{
			Input: tu.InputTokens, Output: tu.OutputTokens, CacheCreation: tu.CacheCreationTokens,
			CacheRead: tu.CacheReadTokens, CostUSD: tu.CostUSD, TurnsUsed: tu.TurnsUsed, MaxTurns: tu.MaxTurns,
		})
	}
	return info
}

// ciVerdict summarises the cached check runs only. No cached runs is unknown,
// not "none": the cache cannot tell "no CI" from "never fetched".
func ciVerdict(runs []gh.CheckRun) localapi.Known[string] {
	if len(runs) == 0 {
		return localapi.Unknown[string]()
	}
	pending := false
	for _, r := range runs {
		if r.Status != "completed" {
			pending = true
			continue
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return localapi.Some("red")
		}
	}
	if pending {
		return localapi.Some("pending")
	}
	return localapi.Some("green")
}

func (b localAPIBackend) prInfo(st *itemstate.ItemState) localapi.PRInfo {
	pr := localapi.PRInfo{
		Number:         localapi.Unknown[int](),
		PRState:        localapi.Unknown[string](),
		Draft:          localapi.Unknown[bool](),
		Merged:         localapi.Unknown[bool](),
		Mergeable:      localapi.Unknown[string](),
		MergeableState: localapi.Unknown[string](),
		CI:             localapi.Unknown[string](),
		HeadSHA:        localapi.Unknown[string](),
	}
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 {
		if st.LastDeepFetchAt.IsZero() {
			pr.State = "unknown"
		} else {
			pr.State = "none"
		}
		return pr
	}
	pr.State = "linked"
	pr.Number = localapi.Some(lpr.Number)
	if lpr.State != "" { // PR details were applied at least once
		pr.PRState = localapi.Some(lpr.State)
		pr.Draft = localapi.Some(lpr.Draft)
		pr.Merged = localapi.Some(lpr.Merged)
	}
	if lpr.Mergeable != nil {
		if *lpr.Mergeable {
			pr.Mergeable = localapi.Some("mergeable")
		} else {
			pr.Mergeable = localapi.Some("conflicting")
		}
	}
	pr.MergeableState = knownString(lpr.MergeableState)
	pr.HeadSHA = knownString(lpr.HeadSHA)
	pr.CI = ciVerdict(lpr.CheckRuns)
	return pr
}

func blockers(st *itemstate.ItemState) localapi.BlockersInfo {
	bi := localapi.BlockersInfo{State: "known", Items: []localapi.Blocker{}}
	if st.LastDeepFetchAt.IsZero() && len(st.BlockedBy) == 0 {
		bi.State = "unknown"
	}
	for _, d := range st.BlockedBy {
		repo := d.Repo
		if repo == "" {
			repo = st.Repo
		}
		bi.Items = append(bi.Items, localapi.Blocker{Issue: issueRef(repo, d.Number), State: knownString(d.State)})
	}
	return bi
}

// latestFabrikComment links the most recent 🏭 comment in the cache. The cache
// stores no comment URL, so the link is built from the issue URL and the
// comment's database ID. Unknown when no comments are cached.
func latestFabrikComment(st *itemstate.ItemState) localapi.Known[string] {
	if st.URL == "" {
		return localapi.Unknown[string]()
	}
	for i := len(st.Comments) - 1; i >= 0; i-- {
		c := st.Comments[i]
		if c.FromPR == 0 && c.DatabaseID != 0 && strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			return localapi.Some(fmt.Sprintf("%s#issuecomment-%d", st.URL, c.DatabaseID))
		}
	}
	return localapi.Unknown[string]()
}

// ---- merge train ----

// trainRow is the slice of an item the merge-train position needs.
type trainRow struct {
	repo, base string
	number     int
	entered    time.Time
}

// trainBase returns the cached base: label value, or "" for the default
// partition. A label-based approximation of groupQueuedByRepoAndBase: no
// WorktreeManager, no branch-existence fallback.
func trainBase(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "base:") && len(l) > len("base:") {
			return strings.TrimPrefix(l, "base:")
		}
	}
	return defaultPartitionBase
}

// trainRows collects the members of the holding stage as the batcher would see
// them: not closed, not paused, deep-fetched.
func (b localAPIBackend) trainRows() []trainRow {
	hs := holdingStage(b.e.cfg)
	if hs == nil {
		return nil
	}
	var rows []trainRow
	b.e.store.Scan(func(it *itemstate.ItemState) {
		if it.IsPR || it.Status != hs.Name || it.IsClosed || hasLabelStr(it.Labels, "fabrik:paused") || it.LastDeepFetchAt.IsZero() {
			return
		}
		rows = append(rows, trainRow{repo: it.Repo, base: trainBase(it.Labels), number: it.Number, entered: it.StatusEnteredAt})
	})
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].entered.Equal(rows[j].entered) {
			return rows[i].entered.Before(rows[j].entered)
		}
		return rows[i].number < rows[j].number
	})
	return rows
}

func (b localAPIBackend) trainWorkerActive(repo, base string) bool {
	key := mergeTrainKey(repo, base)
	for _, k := range b.e.store.RepoWorkerKeys() {
		if k == key {
			return true
		}
	}
	return false
}

func (b localAPIBackend) trainPosition(st *itemstate.ItemState) *localapi.TrainPosition {
	if st.IsClosed || hasLabelStr(st.Labels, "fabrik:paused") || st.LastDeepFetchAt.IsZero() {
		return nil // not a batchable member as far as the cache can tell
	}
	base := trainBase(st.Labels)
	pos, total := 0, 0
	for _, r := range b.trainRows() {
		if r.repo != st.Repo || r.base != base {
			continue
		}
		total++
		if r.number == st.Number {
			pos = total
		}
	}
	if pos == 0 {
		return nil
	}
	return &localapi.TrainPosition{
		Partition:      mergeTrainKey(st.Repo, base),
		Position:       pos,
		Of:             total,
		WorkerInFlight: b.trainWorkerActive(st.Repo, base),
		Basis:          "cached base: label; ordered by daemon-observed time in the holding stage, then issue number",
	}
}

// ---- board ----

type boardRow struct {
	repo      string
	number    int
	title     string
	status    string
	labels    []string
	milestone localapi.MilestoneField
	hydrated  bool
	openBlock bool
	lastDeep  time.Time
	entered   time.Time
	hasWorker bool
	input     attention.Input
}

func (r boardRow) ref() string { return issueRef(r.repo, r.number) }

func (b localAPIBackend) Board(p localapi.BoardParams) (*localapi.BoardResult, error) {
	now := b.e.now()
	threshold := b.stallThreshold(p.StallThresholdSeconds)
	suspended := b.suspendedUntil(now)
	var filter localapi.BoardFilter
	if p.Filter != nil {
		filter = *p.Filter
	}

	var rows []boardRow
	b.e.store.Scan(func(it *itemstate.ItemState) {
		if it.IsPR {
			return
		}
		r := boardRow{
			repo: it.Repo, number: it.Number, title: it.Title, status: it.Status,
			labels:    append([]string(nil), it.Labels...),
			milestone: milestoneField(it),
			hydrated:  !it.LastDeepFetchAt.IsZero(),
			lastDeep:  it.LastDeepFetchAt,
			entered:   it.StatusEnteredAt,
			hasWorker: it.Worker != nil,
			input:     b.e.attentionInput(it, now, threshold, suspended),
		}
		for _, d := range it.BlockedBy {
			if strings.EqualFold(d.State, "OPEN") {
				r.openBlock = true
			}
		}
		rows = append(rows, r)
	})

	res := &localapi.BoardResult{
		Envelope:              b.envelope(now),
		View:                  p.View,
		Filter:                filter,
		StallThresholdSeconds: secs(threshold),
		StatusEnteredBasis:    statusEnteredBasis,
		ItemsScanned:          len(rows),
	}

	kept := rows[:0:0]
	for _, r := range rows {
		if !b.matches(r, filter, &res.Excluded) {
			continue
		}
		kept = append(kept, r)
	}

	switch p.View {
	case localapi.ViewFlow:
		res.Flow = b.flow(kept)
	default:
		res.Attention = b.attentionView(kept, now, threshold)
	}
	return res, nil
}

// matches applies the board filter. A filter the cache cannot evaluate for an
// item (unknown milestone, un-deep-fetched blockers) excludes the item and
// counts it, instead of guessing.
func (b localAPIBackend) matches(r boardRow, f localapi.BoardFilter, ex *localapi.ExcludedCounts) bool {
	if f.Repo != "" && r.repo != f.Repo {
		return false
	}
	if f.Column != "" && r.status != f.Column {
		return false
	}
	if f.Label != "" && !hasLabelStr(r.labels, f.Label) {
		return false
	}
	if f.Milestone != "" {
		switch r.milestone.State {
		case localapi.MilestoneUnknown:
			ex.UnknownMilestone++
			return false
		case localapi.MilestoneNone:
			if f.Milestone != localapi.MilestoneNone {
				return false
			}
		default:
			if r.milestone.Title != f.Milestone {
				return false
			}
		}
	}
	if f.HasOpenBlockers {
		if !r.hydrated {
			ex.NotDeepFetched++
			return false
		}
		if !r.openBlock {
			return false
		}
	}
	return true
}

func (b localAPIBackend) attentionView(rows []boardRow, now time.Time, threshold time.Duration) []localapi.AttentionEntry {
	type ranked struct {
		row boardRow
		res attention.Result
	}
	var picked []ranked
	for _, r := range rows {
		res := attention.Classify(r.input)
		if res.Rank == attention.NotRanked {
			continue
		}
		picked = append(picked, ranked{r, res})
	}
	sort.SliceStable(picked, func(i, j int) bool {
		a, c := picked[i], picked[j]
		if a.res.Rank != c.res.Rank {
			return a.res.Rank < c.res.Rank
		}
		if a.row.repo != c.row.repo {
			return a.row.repo < c.row.repo
		}
		return a.row.number < c.row.number
	})
	out := make([]localapi.AttentionEntry, 0, len(picked))
	for _, p := range picked {
		out = append(out, localapi.AttentionEntry{
			Issue:      p.row.ref(),
			Title:      p.row.title,
			Status:     knownString(p.row.status),
			Milestone:  p.row.milestone,
			State:      string(p.res.State),
			Code:       p.res.Code,
			Reason:     p.res.Summary,
			Rank:       p.res.Rank,
			NextAction: p.res.Next.Action,
			NextAt:     localapi.KnownTime(p.res.Next.At),
			Cache: localapi.CacheInfo{
				LastDeepFetchAt: localapi.KnownTime(p.row.lastDeep),
				AgeSeconds:      ageSince(now, p.row.lastDeep),
			},
		})
	}
	return out
}

func (b localAPIBackend) flow(rows []boardRow) []localapi.ColumnFlow {
	type col struct {
		count   int
		oldest  boardRow
		hasOld  bool
		workers []string
	}
	cols := map[string]*col{}
	for _, r := range rows {
		if r.input.Closed {
			continue
		}
		name := r.status
		if name == "" {
			name = localapi.UnknownText
		}
		c := cols[name]
		if c == nil {
			c = &col{}
			cols[name] = c
		}
		c.count++
		if !r.entered.IsZero() && (!c.hasOld || r.entered.Before(c.oldest.entered) ||
			(r.entered.Equal(c.oldest.entered) && r.number < c.oldest.number)) {
			c.oldest, c.hasOld = r, true
		}
		if r.hasWorker {
			c.workers = append(c.workers, r.ref())
		}
	}

	order := map[string]int{}
	for i, s := range b.e.cfg.Stages {
		order[s.Name] = i
	}
	names := make([]string, 0, len(cols))
	for n := range cols {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		_, iok := order[names[i]]
		_, jok := order[names[j]]
		switch {
		case iok && jok:
			return stageOrder(b.e, names[i]) < stageOrder(b.e, names[j])
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})

	now := b.e.now()
	out := make([]localapi.ColumnFlow, 0, len(names))
	for _, n := range names {
		c := cols[n]
		sort.Strings(c.workers)
		cf := localapi.ColumnFlow{Column: n, Count: c.count, InFlightWorkers: append([]string{}, c.workers...)}
		if c.hasOld {
			cf.OldestInColumn = localapi.Some(localapi.OldestItem{
				Issue: c.oldest.ref(), Since: localapi.KnownTime(c.oldest.entered), AgeSeconds: ageSince(now, c.oldest.entered),
			})
		}
		out = append(out, cf)
	}
	return out
}

func stageOrder(e *Engine, name string) int {
	if s := e.stageByName(name); s != nil {
		return s.Order
	}
	return 1 << 30
}

// ---- health ----

func (b localAPIBackend) Health(localapi.HealthParams) (*localapi.HealthResult, error) {
	e := b.e
	now := e.now()
	h := e.health.snapshot()

	res := &localapi.HealthResult{
		Envelope: b.envelope(now),
		Version:  e.cfg.Version,
		Poll: localapi.PollHealth{
			LastAttemptAt:       localapi.KnownTime(h.PollAttemptAt),
			LastSuccessAt:       localapi.KnownTime(h.PollSuccessAt),
			SinceAttemptSeconds: ageSince(now, h.PollAttemptAt),
			SinceSuccessSeconds: ageSince(now, h.PollSuccessAt),
		},
		Webhook: localapi.WebhookHealth{
			Mode:              b.webhookMode(),
			LastEventAt:       localapi.KnownTime(h.WebhookEventAt),
			SinceEventSeconds: ageSince(now, h.WebhookEventAt),
		},
		Reconcile: localapi.ReconcileHealth{
			LastSuccessAt:       localapi.KnownTime(h.ReconcileOKAt),
			SinceSuccessSeconds: ageSince(now, h.ReconcileOKAt),
		},
		MergeTrain: []localapi.TrainPartition{},
	}

	if until, suspended := e.claudeSuspendedUntilTime(now); suspended {
		res.Claude = localapi.ClaudeHealth{Suspended: true, SuspendedUntil: localapi.Some(until.UTC())}
	} else {
		res.Claude = localapi.ClaudeHealth{SuspendedUntil: localapi.Unknown[time.Time]()}
	}

	bo := h.Backoff
	res.Backoff = localapi.BackoffHealth{
		Observed:   bo.Observed,
		Remaining:  localapi.Unknown[int](),
		Low:        localapi.Unknown[bool](),
		Ratio:      localapi.Unknown[float64](),
		RestPaused: localapi.Some(bo.RestPaused),
	}
	if bo.Observed {
		res.Backoff.Remaining = localapi.Some(bo.Remaining)
		res.Backoff.Low = localapi.Some(bo.Low)
		res.Backoff.Ratio = localapi.Some(bo.Ratio)
	}

	inUse := 0
	e.store.Scan(func(it *itemstate.ItemState) {
		if it.Worker != nil {
			inUse++
		}
	})
	res.Workers = localapi.WorkerSlots{InUse: inUse, MaxConcurrent: localapi.Unknown[int]()}
	if e.cfg.MaxConcurrent > 0 {
		res.Workers.MaxConcurrent = localapi.Some(e.cfg.MaxConcurrent)
	}

	res.MergeTrain = b.partitions()
	return res, nil
}

func (b localAPIBackend) webhookMode() string {
	m := b.e.webhookMgr
	switch {
	case m == nil:
		return "off"
	case m.IsDisabled():
		return "disabled"
	case m.IsHealthyOrStartingUp():
		return "healthy"
	default:
		return "unhealthy"
	}
}

// partitions lists every (repo, base) partition that has members or a worker.
func (b localAPIBackend) partitions() []localapi.TrainPartition {
	type key struct{ repo, base string }
	parts := map[key]*localapi.TrainPartition{}
	get := func(repo, base string) *localapi.TrainPartition {
		k := key{repo, base}
		p := parts[k]
		if p == nil {
			p = &localapi.TrainPartition{Repo: repo, Base: base, Members: []string{}}
			parts[k] = p
		}
		return p
	}
	for _, r := range b.trainRows() {
		p := get(r.repo, r.base)
		p.Members = append(p.Members, issueRef(r.repo, r.number))
	}
	for _, k := range b.e.store.RepoWorkerKeys() {
		repo, base, _ := strings.Cut(k, ":")
		get(repo, base).WorkerInFlight = true
	}
	out := make([]localapi.TrainPartition, 0, len(parts))
	for _, p := range parts {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Base < out[j].Base
	})
	return out
}
