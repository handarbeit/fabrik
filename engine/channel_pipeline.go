package engine

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Pipeline stall events (#2124, ADR 2124). The poll loop publishes its
// lifecycle into the daemonHealth mirror and this file only reads the mirror
// (plus the Store and the durable train run phases, through partitionStates),
// on the channel consumer's own goroutine and clock tick — so a blocked poll is
// still detected (FR-012), and nothing here can reach GitHub (R9) or change
// engine state (R10).
//
// One pipeline-stalled / pipeline-resumed pair per episode, for the poll loop
// ("scope=poll") and for each merge-train partition ("scope=merge-train"). The
// dedup key carries the episode's anchor time, so the hub's persisted dedup
// state never swallows a later episode.

// stallEpisode is the consumer-owned memory of one open stall.
type stallEpisode struct {
	active bool
	// anchor identifies the episode: the poll's start (or the missed due time)
	// for the poll, the oldest member's queued time for a partition.
	anchor time.Time
	// epoch is the health mirror's ended-episode counter when it opened.
	epoch uint64
	// repo and partition identify a merge-train partition episode.
	repo, partition string
	// seq makes the dedup key unique per episode within a process even when
	// two episodes share an anchor (a partition whose run died and whose
	// members were never dequeued). A restart resets it, but the anchors
	// (daemon-observed times) differ across restarts.
	seq uint64
}

func (ep *stallEpisode) key(scope string) string {
	return scope + ":" + ep.anchor.UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatUint(ep.seq, 10)
}

// checkPipeline evaluates the poll-level and partition-level stall episodes.
// Consumer goroutine only.
func (ce *channelEvents) checkPipeline() {
	e := ce.e
	now := e.now()
	h := e.health.snapshot()
	var out []channelevents.Event
	out = append(out, ce.checkPollStall(now, h)...)
	out = append(out, ce.checkForgeStall(now, h)...)
	out = append(out, ce.checkPartitionStalls(now, h)...)
	ce.enqueueDerived(out)
}

func (ce *channelEvents) checkPollStall(now time.Time, h healthSnapshot) []channelevents.Event {
	e := ce.e
	v := e.pollVerdictFor(h, now)
	ep := &ce.pollEp
	var out []channelevents.Event
	if ep.active && (h.StallEpoch != ep.epoch || (v.Known && !v.Overdue)) {
		out = append(out, pollResumedEvent(ep, now))
		ep.active = false
	}
	if !ep.active && v.Overdue {
		anchor := h.NextPollDueAt
		switch {
		case h.PollInProgress:
			anchor = h.PollStartedAt
		case anchor.IsZero():
			anchor = h.StartedAt
		}
		ce.episodeSeq++
		*ep = stallEpisode{active: true, anchor: anchor, epoch: h.StallEpoch, seq: ce.episodeSeq}
		out = append(out, ce.pollStalledEvent(ep, v, h, now))
	}
	return out
}

// checkForgeStall opens a pipeline-stalled (scope=forge) episode when the
// forge-unreachable hold engages and closes it with pipeline-resumed when the
// hold lifts (#2393, ADR 2393). It reads only the health mirror.
func (ce *channelEvents) checkForgeStall(now time.Time, h healthSnapshot) []channelevents.Event {
	ep := &ce.forgeEp
	var out []channelevents.Event
	if ep.active && (!h.ForgeHeld || !h.ForgeSince.Equal(ep.anchor)) {
		out = append(out, forgeResumedEvent(ep, now))
		ep.active = false
	}
	if !ep.active && h.ForgeHeld {
		ce.episodeSeq++
		*ep = stallEpisode{active: true, anchor: h.ForgeSince, seq: ce.episodeSeq}
		out = append(out, forgeStalledEvent(ep, h, now))
	}
	return out
}

func forgeStalledEvent(ep *stallEpisode, h healthSnapshot, now time.Time) channelevents.Event {
	return finishEvent(channelevents.Event{
		Type:     channelevents.PipelineStalled,
		At:       now,
		DedupKey: "pipeline-stalled:" + ep.key("forge"),
		Content: fmt.Sprintf("GitHub has been unreachable since %s (%d consecutive transport failures, last: %s); no new model work starts until a GitHub call succeeds",
			h.ForgeSince.UTC().Format(time.RFC3339), h.ForgeFailures, h.ForgeLastError),
		Meta: map[string]string{
			"scope":    "forge",
			"cause":    "forge-unreachable",
			"failures": strconv.Itoa(h.ForgeFailures),
		},
	})
}

func forgeResumedEvent(ep *stallEpisode, now time.Time) channelevents.Event {
	return finishEvent(channelevents.Event{
		Type:     channelevents.PipelineResumed,
		At:       now,
		DedupKey: "pipeline-resumed:" + ep.key("forge"),
		Content:  fmt.Sprintf("GitHub answered again, %d s after dispatch was held; new model work resumes", int64(now.Sub(ep.anchor).Seconds())),
		Meta: map[string]string{
			"scope":           "forge",
			"blocked_seconds": strconv.FormatInt(int64(now.Sub(ep.anchor).Seconds()), 10),
		},
	})
}

func (ce *channelEvents) pollStalledEvent(ep *stallEpisode, v pollVerdict, h healthSnapshot, now time.Time) channelevents.Event {
	ev := channelevents.Event{
		Type:     channelevents.PipelineStalled,
		At:       now,
		DedupKey: "pipeline-stalled:" + ep.key("poll"),
		Content:  pollStallDescription(v, h.PollStep) + "; items that only need the poll are reported as waiting for it, not stalled",
		Meta: map[string]string{
			"scope":                 "poll",
			"cause":                 v.Cause,
			"interval_seconds":      strconv.FormatInt(int64(ce.e.pollInterval().Seconds()), 10),
			"blocked_seconds":       strconv.FormatInt(int64(v.Blocked.Seconds()), 10),
			"overdue_after_seconds": strconv.FormatInt(int64(v.OverdueAfter.Seconds()), 10),
		},
	}
	if h.PollInProgress && h.PollStep != "" {
		ev.Meta["step"] = h.PollStep
	}
	return finishEvent(ev)
}

func pollResumedEvent(ep *stallEpisode, now time.Time) channelevents.Event {
	return finishEvent(channelevents.Event{
		Type:     channelevents.PipelineResumed,
		At:       now,
		DedupKey: "pipeline-resumed:" + ep.key("poll"),
		Content:  fmt.Sprintf("the poll loop recovered, %d s after it became overdue", int64(now.Sub(ep.anchor).Seconds())),
		Meta: map[string]string{
			"scope":           "poll",
			"blocked_seconds": strconv.FormatInt(int64(now.Sub(ep.anchor).Seconds()), 10),
		},
	})
}

func (ce *channelEvents) checkPartitionStalls(now time.Time, h healthSnapshot) []channelevents.Event {
	e := ce.e
	if ce.partEp == nil {
		ce.partEp = map[string]*stallEpisode{}
	}
	states := e.partitionStates(now, h)
	seen := map[string]bool{}
	var out []channelevents.Event
	for i := range states {
		st := &states[i]
		seen[st.Key] = true
		ep := ce.partEp[st.Key]
		if ep != nil && ep.active && (st.RunActive || len(st.Members) == 0) {
			out = append(out, partitionResumedEvent(ep, now))
			ep.active = false
		}
		if st.Live.Stalled && (ep == nil || !ep.active) {
			ce.episodeSeq++
			ep = &stallEpisode{active: true, anchor: st.OldestQueued, epoch: h.StallEpoch, repo: st.Repo, partition: st.Key, seq: ce.episodeSeq}
			ce.partEp[st.Key] = ep
			out = append(out, ce.partitionStalledEvent(st, ep, now))
		}
	}
	// A partition that vanished entirely (its members left and no run remains)
	// ends its episode too.
	var gone []string
	for k := range ce.partEp {
		if !seen[k] {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		if ep := ce.partEp[k]; ep.active {
			out = append(out, partitionResumedEvent(ep, now))
		}
		delete(ce.partEp, k)
	}
	return out
}

func (ce *channelEvents) partitionStalledEvent(st *partitionState, ep *stallEpisode, now time.Time) channelevents.Event {
	ev := channelevents.Event{
		Type:     channelevents.PipelineStalled,
		Repo:     st.Repo,
		At:       now,
		DedupKey: "pipeline-stalled:" + ep.key(st.Key),
		Content: fmt.Sprintf("merge train %s has %d member(s) queued for %d s and no run started (%s)",
			st.Key, len(st.Members), int64(now.Sub(st.OldestQueued).Seconds()), st.Live.Cause),
		Meta: map[string]string{
			"scope":                 "merge-train",
			"cause":                 st.Live.Cause,
			"partition":             st.Key,
			"members":               strconv.Itoa(len(st.Members)),
			"oldest_queued_seconds": strconv.FormatInt(int64(now.Sub(st.OldestQueued).Seconds()), 10),
			"interval_seconds":      strconv.FormatInt(int64(ce.e.pollInterval().Seconds()), 10),
		},
	}
	if st.Live.IdleReason != "" {
		ev.Meta["idle_reason"] = st.Live.IdleReason
	}
	return finishEvent(ev)
}

func partitionResumedEvent(ep *stallEpisode, now time.Time) channelevents.Event {
	return finishEvent(channelevents.Event{
		Type:     channelevents.PipelineResumed,
		Repo:     ep.repo,
		At:       now,
		DedupKey: "pipeline-resumed:" + ep.key(ep.partition),
		Content:  fmt.Sprintf("merge train %s is moving again (a run started or its members left Queued)", ep.partition),
		Meta: map[string]string{
			"scope":     "merge-train",
			"partition": ep.partition,
		},
	})
}

// pipelineCatchUpEvents lists the stalls open right now (the poll loop and any
// stalled partition) as catch-up snapshots for a new subscriber. Unlike an
// item's time-driven stalled, a pipeline stall is current account state. No
// dedup key and no episode memory are touched. Safe from any goroutine: it
// reads only the mirror, the Store and the run phases.
func (e *Engine) pipelineCatchUpEvents(now time.Time) []channelevents.Event {
	h := e.health.snapshot()
	v := e.pollVerdictFor(h, now)
	var out []channelevents.Event
	if v.Overdue {
		ce := &channelEvents{e: e}
		ev := ce.pollStalledEvent(&stallEpisode{anchor: now}, v, h, now)
		ev.DedupKey = ""
		ev.Content = "(catch-up) " + ev.Content
		ev.Meta["catch_up"] = "true"
		out = append(out, ev)
	}
	if h.ForgeHeld {
		ev := forgeStalledEvent(&stallEpisode{anchor: h.ForgeSince}, h, now)
		ev.DedupKey = ""
		ev.Content = "(catch-up) " + ev.Content
		ev.Meta["catch_up"] = "true"
		out = append(out, ev)
	}
	for _, st := range e.partitionStates(now, h) {
		if !st.Live.Stalled {
			continue
		}
		st := st
		ce := &channelEvents{e: e}
		ev := ce.partitionStalledEvent(&st, &stallEpisode{anchor: st.OldestQueued}, now)
		ev.DedupKey = ""
		ev.Content = "(catch-up) " + ev.Content
		ev.Meta["catch_up"] = "true"
		out = append(out, ev)
	}
	return out
}
