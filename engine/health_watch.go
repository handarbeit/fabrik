package engine

import (
	"context"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The ingestion health watcher (#2532, ADR 2532, FR-007).
//
// Event-replaceable re-checks and the safety-net stamp are stretched while
// ingestion is strictly healthy for a repo (transportSet.ReliableFor). The next
// idle interval was computed while healthy and may already be up to an hour, and
// the periodic-re-eval entries were stamped the safety-net interval ahead. When
// reliability is lost for a repo, today's cadence must return within about one
// poll: this goroutine notices the transition (it reads memory only, no GitHub
// call), pulls that repo's periodic-re-eval entries back to githubRecheckInterval
// and wakes the poll loop with a non-blocking send (a buffered channel already
// coalesces a flapping transport into one wake).

const minHealthWatchInterval = 5 * time.Second

func (e *Engine) healthWatchInterval() time.Duration {
	d := time.Duration(e.cfg.PollSeconds) * time.Second
	if d < minHealthWatchInterval {
		d = minHealthWatchInterval
	}
	return d
}

// runHealthWatch ticks every interval until ctx is done.
func (e *Engine) runHealthWatch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prev = e.healthWatchTick(prev)
		}
	}
}

// healthWatchTick snapshots per-repo reliability, re-clamps and wakes for every
// repo that was reliable and no longer is, and returns the new snapshot.
func (e *Engine) healthWatchTick(prev map[string]bool) map[string]bool {
	repos := map[string]bool{}
	e.store.Scan(func(st *itemstate.ItemState) {
		if !st.IsClosed && st.Repo != "" {
			repos[st.Repo] = true
		}
	})
	next := make(map[string]bool, len(repos))
	var lost []string
	for repo := range repos {
		ok := e.reliableFor(repo, "")
		next[repo] = ok
		if prev[repo] && !ok {
			lost = append(lost, repo)
		}
	}
	for _, repo := range lost {
		e.reclampPeriodicReEval(repo)
	}
	if len(lost) > 0 {
		e.logf(0, "poll", "webhook ingestion no longer reliable for %v — restoring the full re-check cadence\n", lost)
		if e.wakeCh != nil {
			select {
			case e.wakeCh <- struct{}{}:
			default:
			}
		}
	}
	return next
}

// reclampPeriodicReEval moves every periodic-re-eval entry of repo that lies
// beyond today's interval back to now + periodicReEvalInterval(repo) (which is
// githubRecheckInterval while ingestion is not reliable). It only ever shortens.
func (e *Engine) reclampPeriodicReEval(repo string) {
	limit := e.periodicReEvalDue(repo)
	type target struct {
		number int
	}
	var targets []target
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.Repo != repo || st.IsClosed {
			return
		}
		if t, ok := st.CooldownAt[reasonPeriodicReEval]; ok && t.After(limit) {
			targets = append(targets, target{st.Number})
		}
	})
	for _, tg := range targets {
		e.store.Apply(itemstate.CooldownRecorded{Repo: repo, Number: tg.number, Reason: reasonPeriodicReEval, Until: limit})
	}
}
