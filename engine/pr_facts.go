package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Overseer PR facts and CI verdict (#2122, ADR 2122).
//
// The local API answers from the Store alone, so for an item outside a CI gate
// it saw only the head SHA: nothing else ever asked GitHub for the PR's state
// or its check runs. settlePRFacts is the poll-goroutine half that fills the
// gap. It runs the gates' own classifier (ciStatusVerdictFor ->
// classifyLandingCI, unmodified) over the head's check runs and stores the
// result, with the head it was computed for and when, in
// LinkedPRState.CIVerdict. The read side only renders that record.
//
// The writes are quiet by construction: PRFactsObserved/CIVerdictObserved
// return the non-wake PRFactsChanged flag, never call SelfWriteObserved, never
// move a freshness baseline and never touch LastCIProgressAt. Nothing a gate
// reads is written here.
const (
	// prFactsCheckBudgetPerPoll bounds the head evaluations per poll. An
	// evaluation is a live check-runs read plus, for a ready set, the uncached
	// check-suite read and (for a required context with no check run) a
	// combined-status read of classifyLandingCI: 1 to 3 REST calls, nothing
	// memoised. Only a green verdict read live within prFactsGreenRecheck is
	// restamped for free and not charged; a pending, red or check-less head, or
	// a ready set demoted to pending by allCheckRunsPassed, is evaluated every
	// poll it is reached. An item beyond the budget is left alone and ages into
	// stale.
	prFactsCheckBudgetPerPoll = 8
	// prFactsRESTPerPoll bounds the REST PR-detail reads of the fallback for
	// items whose linked PR the board probe cannot see (a base:<branch> PR).
	prFactsRESTPerPoll = 5
	// prFactsGreenRecheck is how long a green verdict may be restamped without
	// reading the head's check runs from GitHub. A cached READY set is trusted
	// forever by FetchCheckRuns (ADR 2180) and nothing on a poll-only deployment
	// refreshes it, so a check rerun that fails on the same head would stay
	// invisible; past this age the verdict is recomputed from a live read.
	prFactsGreenRecheck = 5 * time.Minute
)

// SetPRFactsRefreshDisabledForTest switches the overseer PR-fact writers off
// (the probe's scalar apply and settlePRFacts), so a test can show the sim
// scenario fails without them. Test seam only; production never calls this.
func (e *Engine) SetPRFactsRefreshDisabledForTest(disabled bool) {
	e.prFactsRefreshDisabledForTest = disabled
}

// prFactsCandidate is one item the refresher may act on this poll.
type prFactsCandidate struct {
	item  gh.ProjectItem
	repo  string
	key   string
	order int64 // unix nanos of the last poll this item spent budget; zero (never) sorts first
}

// settlePRFacts refreshes, for every managed open item with a linked PR, the PR
// facts the probe cannot supply and the CI verdict for the PR's head. Poll
// goroutine only; bounded by prFactsCheckBudgetPerPoll and prFactsRESTPerPoll.
func (e *Engine) settlePRFacts(board *gh.ProjectBoard) {
	if e.prFactsRefreshDisabledForTest || board == nil {
		return
	}
	now := e.now()
	pollStart := e.health.snapshot().PollStartedAt
	if pollStart.IsZero() {
		pollStart = now
	}

	var cands []prFactsCandidate
	spenders := 0
	if e.prFactsAttemptAt == nil {
		e.prFactsAttemptAt = map[string]time.Time{}
	}
	for _, item := range board.Items {
		if item.IsClosed || item.IsPR || item.Status == "" {
			continue
		}
		if st := stages.FindStage(e.cfg.Stages, item.Status); st == nil || st.Unmanaged {
			continue
		}
		repo := itemOwnerRepoString(item, e.defaultRepo())
		snap, ok := e.store.Peek(repo, item.Number)
		if !ok {
			continue
		}
		state := snap.State()
		if state.Terminal || state.LinkedPR == nil || state.LinkedPR.Number == 0 {
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, item.Number)
		var order int64
		if at := e.prFactsAttemptAt[key]; !at.IsZero() {
			order = at.UnixNano()
		}
		cands = append(cands, prFactsCandidate{item: item, repo: repo, key: key, order: order})
		if canSpendPRFactsBudget(state.LinkedPR, now) {
			spenders++
		}
	}
	// Least recently attempted first, so a budget shortfall rotates through the
	// items whatever the outcome of an attempt (a failed read or a merged PR
	// stamps nothing on the PR, but still moves to the back).
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].order < cands[j].order })
	e.prFactsRotation.Store(int64(max(1, (spenders+prFactsCheckBudgetPerPoll-1)/prFactsCheckBudgetPerPoll)))

	checkBudget, restBudget := prFactsCheckBudgetPerPoll, prFactsRESTPerPoll
	for _, c := range cands {
		e.refreshPRFacts(c, pollStart, now, &checkBudget, &restBudget)
	}
	// Forget attempt stamps of items that are no longer candidates (closed,
	// terminal, off the board, PR unlinked) so the map cannot outgrow the board.
	live := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		live[c.key] = struct{}{}
	}
	for key := range e.prFactsAttemptAt {
		if _, ok := live[key]; !ok {
			delete(e.prFactsAttemptAt, key)
		}
	}
}

// canSpendPRFactsBudget reports whether refreshing this PR can cost a GitHub
// read this poll. A PR whose facts say it is merged or closed gets no verdict,
// and a green verdict read live within prFactsGreenRecheck is restamped for
// free; neither occupies a place in the rotation that attention tolerates.
func canSpendPRFactsBudget(lpr *itemstate.LinkedPRState, now time.Time) bool {
	if lpr.Facts.State != "" && (lpr.Facts.State != "open" || lpr.Facts.Merged) {
		return false
	}
	rec := lpr.CIVerdict
	return !(rec.Verdict == string(ciVerdictGreen) && rec.HeadSHA == lpr.AgreedHead() &&
		!rec.LiveReadAt.IsZero() && now.Sub(rec.LiveReadAt) < prFactsGreenRecheck)
}

// refreshPRFacts is settlePRFacts for one item: the REST facts fallback when
// the probe did not stamp the facts this poll, then the CI verdict for the
// head.
func (e *Engine) refreshPRFacts(c prFactsCandidate, pollStart, now time.Time, checkBudget, restBudget *int) {
	owner, name := itemOwnerRepo(c.item, e.defaultRepo())
	snap, ok := e.store.Peek(c.repo, c.item.Number)
	if !ok {
		return
	}
	st := snap.State()
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 {
		return
	}

	// Facts the probe did not stamp this poll (a base:<branch> PR, which
	// closedByPullRequestsReferences omits, or a probe without the scalars)
	// come from one bounded REST read of the PR.
	knownClosed := lpr.Facts.State != "" && (lpr.Facts.State != "open" || lpr.Facts.Merged) && now.Sub(lpr.Facts.ObservedAt) < prFactsGreenRecheck
	if lpr.Facts.ObservedAt.Before(pollStart) && *restBudget > 0 && !knownClosed {
		*restBudget--
		e.prFactsAttemptAt[c.key] = now
		d, err := e.client.FetchPRDetails(owner, name, lpr.Number)
		if err != nil || d == nil {
			if err != nil {
				e.logf(c.item.Number, "pr-facts", "could not read PR #%d: %v\n", lpr.Number, err)
			}
		} else {
			// The refresher never writes the store's HeadSHA: PRHeadSHAUpdated
			// feeds the gates' SHA-invalidation scan (a head recorded here ahead
			// of the engine's own paths reads as a push after Validate
			// completed), so the facts record keeps its own HeadSHA and the
			// verdict below waits for the engine to learn the head.
			ms := strings.ToLower(d.MergeableState)
			if ms == "unknown" {
				ms = ""
			}
			e.store.Apply(itemstate.PRFactsObserved{Repo: c.repo, Number: c.item.Number, PRNumber: lpr.Number, Facts: itemstate.PRFacts{
				State:          strings.ToLower(d.State),
				Draft:          d.Draft,
				Merged:         d.Merged,
				MergeableState: ms,
				HeadSHA:        d.HeadSHA,
				ObservedAt:     now,
			}})
			if snap, ok = e.store.Peek(c.repo, c.item.Number); !ok {
				return
			}
			st = snap.State()
			lpr = st.LinkedPR
			if lpr == nil {
				return
			}
		}
	}

	// A verdict applies to an open PR only; facts never observed mean we cannot
	// tell, so nothing is computed (the API keeps saying unknown). The same holds
	// when the engine-recorded head and the facts' head disagree: neither is known
	// to be the newer, so there is no agreed head and no verdict is read or kept
	// (a green for a head the PR has moved past is the worst thing to show).
	if lpr.Facts.State != "open" || lpr.Facts.Merged || lpr.AgreedHead() == "" {
		return
	}
	head := lpr.AgreedHead()
	rec := lpr.CIVerdict

	// A green verdict whose runs were read live within prFactsGreenRecheck is
	// restamped for free, so a green PR costs one live read per window rather
	// than one per poll. The cache is never refreshed on a poll-only deployment
	// (it stays empty there), so the window, not the cache, bounds how long an
	// unverified green is carried; when the cache does hold runs (a webhook or a
	// gate filled it) they must still be a READY set matching the verdict's
	// fingerprint, so a failing check_run delivered since surfaces at once.
	if rec.Verdict == string(ciVerdictGreen) && rec.HeadSHA == head &&
		!rec.LiveReadAt.IsZero() && now.Sub(rec.LiveReadAt) < prFactsGreenRecheck {
		unchanged := len(lpr.CheckRuns) == 0
		if !unchanged {
			status, _, _ := gh.ClassifyCheckRuns(lpr.CheckRuns)
			unchanged = status == gh.CheckRunsReady && fingerprintCheckRuns(lpr.CheckRuns) == rec.RunsFingerprint
		}
		if unchanged {
			rec.ObservedAt = now
			e.store.Apply(itemstate.CIVerdictObserved{Repo: c.repo, Number: c.item.Number, Verdict: rec})
			return
		}
	}

	if *checkBudget <= 0 {
		return
	}
	*checkBudget--
	e.prFactsAttemptAt[c.key] = now
	// Read live, not through the cache: a cached READY set is served on trust
	// and would hide a rerun that failed on this head. Nothing is written to the
	// Store from this read, so what the gates see is unchanged.
	runs, err := e.client.FetchCheckRuns(owner, name, head)
	if err != nil {
		// Keep the previous record, unstamped: it ages into stale rather than
		// being presented as freshly confirmed.
		e.logf(c.item.Number, "pr-facts", "could not read check runs for %s: %v\n", head, err)
		return
	}
	verdict := e.ciStatusVerdictFor(owner, name, lpr.Facts.MergeableState, head, runs)
	next := itemstate.CIVerdictRecord{
		Verdict:         string(verdict),
		HeadSHA:         head,
		RunsFingerprint: fingerprintCheckRuns(runs),
		ObservedAt:      now,
		LiveReadAt:      now,
	}
	if verdict == ciVerdictRed {
		next.Failing = failingCheckNames(runs)
	}
	e.store.Apply(itemstate.CIVerdictObserved{Repo: c.repo, Number: c.item.Number, Verdict: next})
}

func fingerprintCheckRuns(runs []gh.CheckRun) string {
	latest := gh.LatestCheckRuns(runs)
	lines := make([]string, 0, len(latest))
	for _, r := range latest {
		lines = append(lines, fmt.Sprintf("%s|%d|%s|%s", r.Name, r.ID, r.Status, r.Conclusion))
	}
	sort.Strings(lines)
	sum := sha1.Sum([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// failingCheckNames lists the names of the latest failing runs, de-duplicated
// and capped at ciStatusMaxFailing.
func failingCheckNames(runs []gh.CheckRun) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range failingCheckRuns(runs) {
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		out = append(out, r.Name)
		if len(out) == ciStatusMaxFailing {
			break
		}
	}
	return out
}
