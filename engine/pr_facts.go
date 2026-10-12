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
	// prFactsCheckBudgetPerPoll bounds the head evaluations that can cost a
	// GitHub read per poll (a check-run fetch that is not served from a READY
	// cache, and the live suite/required-context reads of classifyLandingCI).
	// A head whose cached READY set is unchanged is restamped for free and is
	// not charged. An item beyond the budget is left alone and ages into stale.
	prFactsCheckBudgetPerPoll = 8
	// prFactsRESTPerPoll bounds the REST PR-detail reads of the fallback for
	// items whose linked PR the board probe cannot see (a base:<branch> PR).
	prFactsRESTPerPoll = 5
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
	order int64 // the verdict's ObservedAt in unix nanos; zero (never) sorts first
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
		var order int64
		if at := state.LinkedPR.CIVerdict.ObservedAt; !at.IsZero() {
			order = at.UnixNano()
		}
		cands = append(cands, prFactsCandidate{item: item, repo: repo, order: order})
	}
	// Oldest verdict first, so a budget shortfall rotates through the items.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].order < cands[j].order })

	checkBudget, restBudget := prFactsCheckBudgetPerPoll, prFactsRESTPerPoll
	for _, c := range cands {
		e.refreshPRFacts(c, pollStart, now, &checkBudget, &restBudget)
	}
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
	if lpr.Facts.ObservedAt.Before(pollStart) && *restBudget > 0 {
		*restBudget--
		d, err := e.client.FetchPRDetails(owner, name, lpr.Number)
		if err != nil || d == nil {
			if err != nil {
				e.logf(c.item.Number, "pr-facts", "could not read PR #%d: %v\n", lpr.Number, err)
			}
		} else {
			// The head SHA is written only when the store has never learned one:
			// PRHeadSHAUpdated feeds the gates' SHA-invalidation scan, so an
			// overseer-only refresher must never move a head the engine already
			// holds (the facts record keeps its own HeadSHA either way).
			if lpr.HeadSHA == "" && d.HeadSHA != "" {
				e.store.Apply(itemstate.FromProbe{Inner: itemstate.PRHeadSHAUpdated{Repo: c.repo, Number: c.item.Number, LinkedPRNum: lpr.Number, SHA: d.HeadSHA}})
			}
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
	// tell, so nothing is computed (the API keeps saying unknown).
	if lpr.Facts.State != "open" || lpr.Facts.Merged || lpr.HeadSHA == "" {
		return
	}
	head := lpr.HeadSHA
	rec := lpr.CIVerdict

	// A READY cached set whose latest runs are unchanged since a green verdict
	// is restamped for free: it is exactly the set FetchCheckRuns would serve on
	// trust (ADR 2180), and the verdict for it was computed with the live suite
	// and required-context reads.
	if rec.Verdict == string(ciVerdictGreen) && rec.HeadSHA == head && len(lpr.CheckRuns) > 0 {
		if status, _, _ := gh.ClassifyCheckRuns(lpr.CheckRuns); status == gh.CheckRunsReady && fingerprintCheckRuns(lpr.CheckRuns) == rec.RunsFingerprint {
			rec.ObservedAt = now
			e.store.Apply(itemstate.CIVerdictObserved{Repo: c.repo, Number: c.item.Number, Verdict: rec})
			return
		}
	}

	if *checkBudget <= 0 {
		return
	}
	*checkBudget--
	runs, err := e.readClient.FetchCheckRuns(owner, name, head)
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
