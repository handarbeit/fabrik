package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The situation digest (#2171, ADR 2171): the channel side of the rolled-up
// view. renderSituation turns a view into the event a session reads;
// situationFingerprint reduces it to what *changed in substance* so an unchanged
// view is not re-sent merely because time passed. Like the rest of channel_*.go
// it reads engine state only through the builder and never GitHub (R9).

// The per-engine situationDedupOff seam (channel_settings.go) neutralises the
// dedup (every digest is sent), so a test can show that "two quiet ticks publish
// one digest" is not vacuous.

// ---- delivery ----

// situationScopeFor is the scope of name's digest: the union of the scopes of
// the subscriptions of name that take situation. ok is false when none does.
func (ce *channelEvents) situationScopeFor(name string) (scope situationScope, cadence time.Duration, ok bool) {
	var subs []channelevents.Subscription
	for _, sub := range ce.hub.Subscriptions(name) {
		if !sub.TakesSituation() {
			continue
		}
		subs = append(subs, sub)
		if d := sub.Situation(); d > 0 && (cadence == 0 || d < cadence) {
			cadence = d
		}
	}
	if len(subs) == 0 {
		return nil, 0, false
	}
	return func(ev channelevents.Event) bool {
		for _, sub := range subs {
			if sub.ScopeMatches(ev) {
				return true
			}
		}
		return false
	}, cadence, true
}

// pushSituation builds name's view and offers it to the hub, which sends it
// unless force is false and the view is unchanged since the last digest it sent
// to name. It reports whether a digest was queued. Any goroutine.
func (e *Engine) pushSituation(name, trigger string, force bool) bool {
	ce := e.channelEvents()
	if ce == nil {
		return false
	}
	scope, _, ok := ce.situationScopeFor(name)
	if !ok {
		return false
	}
	v := e.buildSituation(scope)
	ev := finishEvent(renderSituation(v, e.now(), trigger))
	if e.chanTest.situationDedupOff.Load() {
		force = true
	}
	return ce.hub.PublishSituation(name, ev, situationFingerprint(v), force)
}

// requestSituationPush serves fabrik_situation's push argument: queue the view
// to the caller's subscriber, subject to the same dedup as the scheduled digest
// unless force. The note says why nothing was queued.
func (e *Engine) requestSituationPush(subscriber string, force bool) (bool, string) {
	ce := e.channelEvents()
	switch {
	case subscriber == "":
		return false, "no subscriber name was given, so nothing was pushed (start the shim with --subscriber)"
	case ce == nil:
		return false, "channel events are not running on this daemon, so nothing was pushed"
	}
	if _, _, ok := ce.situationScopeFor(subscriber); !ok {
		return false, "subscriber " + strconv.Quote(subscriber) + " has no subscription that takes situation, so nothing was pushed (subscribe with an empty events list or name situation)"
	}
	if e.pushSituation(subscriber, channelevents.TriggerRequest, force) {
		return true, ""
	}
	return false, "the view is unchanged since the last digest sent to " + strconv.Quote(subscriber) + "; pass force to send it again"
}

// publishSituations is the cadence: for each attached subscriber with a
// situation_seconds, once the interval has elapsed on the engine clock, build its
// view and send it if it changed. A detached subscriber is caught up on attach
// instead. Consumer goroutine only (it owns sitEval).
func (ce *channelEvents) publishSituations() {
	if !ce.ready.Load() {
		return
	}
	e := ce.e
	now := e.now()
	seen := map[string]bool{}
	var names []string
	for _, sub := range ce.hub.Subscriptions("") {
		if !seen[sub.Subscriber] {
			seen[sub.Subscriber] = true
			names = append(names, sub.Subscriber)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		_, cadence, ok := ce.situationScopeFor(name)
		if !ok || cadence <= 0 {
			delete(ce.sitEval, name)
			continue
		}
		if !ce.hub.Attached(name) {
			continue
		}
		if last, evald := ce.sitEval[name]; evald && now.Sub(last) < cadence {
			continue
		}
		if ce.sitEval == nil {
			ce.sitEval = map[string]time.Time{}
		}
		ce.sitEval[name] = now
		e.pushSituation(name, channelevents.TriggerCadence, false)
	}
}

// ---- rendering ----

// renderSituation renders v into a situation event: readable Content and string
// Meta counts. It is the one renderer, used by the scheduled digest, the
// requested digest and the catch-up. now fixes the ages shown in the text.
func renderSituation(v localapi.SituationView, now time.Time, trigger string) channelevents.Event {
	var b strings.Builder
	if v.NothingNeedsYou {
		b.WriteString("Situation: nothing needs you.\n")
	} else {
		b.WriteString("Situation:\n")
	}
	if v.UnclassifiedItems > 0 {
		fmt.Fprintf(&b, "- %d open item(s) are not classified yet (the daemon has not read their labels); this view may be incomplete.\n", v.UnclassifiedItems)
	}
	section := func(title string, sec localapi.SituationSection[localapi.SituationEntry], line func(localapi.SituationEntry) string) {
		if len(sec.Entries) == 0 && sec.Omitted == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d):\n", title, len(sec.Entries)+sec.Omitted)
		for _, en := range sec.Entries {
			b.WriteString("- " + line(en) + "\n")
		}
		if sec.Omitted > 0 {
			fmt.Fprintf(&b, "- … and %d more not shown\n", sec.Omitted)
		}
	}
	section("Awaiting a merge decision", v.MergeDecisions, func(en localapi.SituationEntry) string {
		s := itemHead(en)
		if m := en.Merge; m != nil {
			s += fmt.Sprintf(" — PR %s, reviews: %s, CI: %s", knownText(m.PR), knownText(m.Review), knownText(m.CI))
		}
		return s + optionsText(en.Options)
	})
	section("Held at a gate", v.Gates, func(en localapi.SituationEntry) string {
		s := itemHead(en)
		if g := en.Gate; g != nil {
			s += fmt.Sprintf(" — %s gate, held %s", g.Gate, ageText(g.HeldSeconds))
			if g.Reason != "" {
				s += ": " + g.Reason
			}
		}
		return s
	})
	section("Waiting for human input", v.HumanInput, func(en localapi.SituationEntry) string {
		s := itemHead(en) + " — " + en.Reason
		if h := en.Human; h != nil && h.ResumeVia != "" {
			s += " (resume: " + h.ResumeVia + ")"
		}
		return s + optionsText(en.Options)
	})
	section("Pending decisions", v.Decisions, func(en localapi.SituationEntry) string {
		s := itemHead(en) + " — " + en.Reason
		if p := en.Pending; p != nil {
			if p.AgeSeconds.Valid {
				s += ", waiting " + ageText(p.AgeSeconds)
			} else if p.AtLeastAgeSeconds.Valid {
				s += ", waiting at least " + ageText(p.AtLeastAgeSeconds)
			}
		}
		return s + optionsText(en.Options)
	})
	section("Stalled", v.Stalled, func(en localapi.SituationEntry) string {
		return itemHead(en) + " — " + en.Reason
	})

	if len(v.MergeTrain.Entries) > 0 || v.MergeTrain.Omitted > 0 {
		fmt.Fprintf(&b, "\nMerge train (%d):\n", len(v.MergeTrain.Entries)+v.MergeTrain.Omitted)
		for _, p := range v.MergeTrain.Entries {
			s := fmt.Sprintf("- %s: %d queued, phase %s", mergeTrainKey(p.Repo, p.Base), len(p.Members), p.Phase)
			if p.IdleReason != "" {
				s += ", idle: " + p.IdleReason
			}
			if p.Stalled {
				s += " (STALLED)"
			}
			b.WriteString(s + "\n")
		}
		if v.MergeTrain.Omitted > 0 {
			fmt.Fprintf(&b, "- … and %d more not shown\n", v.MergeTrain.Omitted)
		}
	}

	h := v.Health
	fmt.Fprintf(&b, "\nHealth: poll %s; workers %d/%s in use", pollText(h.Poll), h.Workers.InUse, knownText(h.Workers.MaxConcurrent))
	if h.Workers.TrainMax.Valid && h.Workers.TrainMax.V > 0 {
		fmt.Fprintf(&b, " (+train %s/%s)", knownText(h.Workers.TrainInUse), knownText(h.Workers.TrainMax))
	}
	if h.Workers.FixMax.Valid && h.Workers.FixMax.V > 0 {
		fmt.Fprintf(&b, " (+fix %s/%s)", knownText(h.Workers.FixInUse), knownText(h.Workers.FixMax))
	}
	fmt.Fprintf(&b, "; %d train member(s) queued", h.TrainQueued)
	if h.Claude.UsageLimited {
		b.WriteString("; Claude usage limit hit")
	}
	b.WriteString("\n")

	if len(v.Anomalies.Entries) > 0 || v.Anomalies.Omitted > 0 {
		fmt.Fprintf(&b, "\nAnomalies (%d):\n", len(v.Anomalies.Entries)+v.Anomalies.Omitted)
		for _, a := range v.Anomalies.Entries {
			fmt.Fprintf(&b, "- %s %s: %s\n", a.Kind, a.Subject, a.Summary)
		}
		if v.Anomalies.Omitted > 0 {
			fmt.Fprintf(&b, "- … and %d more not shown\n", v.Anomalies.Omitted)
		}
	}

	ev := channelevents.Event{
		Type:    channelevents.Situation,
		At:      now,
		Content: strings.TrimRight(b.String(), "\n"),
		Meta: map[string]string{
			channelevents.MetaTrigger: trigger,
			"nothing_needs_you":       strconv.FormatBool(v.NothingNeedsYou),
			"merge_decisions":         countText(v.MergeDecisions),
			"gates":                   countText(v.Gates),
			"human_input":             countText(v.HumanInput),
			"decisions":               countText(v.Decisions),
			"stalled":                 countText(v.Stalled),
			"anomalies":               strconv.Itoa(len(v.Anomalies.Entries) + v.Anomalies.Omitted),
			"merge_train_partitions":  strconv.Itoa(len(v.MergeTrain.Entries) + v.MergeTrain.Omitted),
			"unclassified":            strconv.Itoa(v.UnclassifiedItems),
		},
	}
	if trunc := omittedTotal(v); trunc > 0 {
		ev.Meta["omitted"] = strconv.Itoa(trunc)
	}
	switch trigger {
	case channelevents.TriggerCatchUp:
		ev.Meta["catch_up"] = "true"
		ev.Meta[channelevents.MetaSource] = channelevents.SourceCatchUp
	default:
		ev.Meta[channelevents.MetaSource] = channelevents.SourceLive
	}
	return ev
}

func itemHead(en localapi.SituationEntry) string {
	s := en.Issue
	if en.Title != "" {
		s += " " + strconv.Quote(en.Title)
	}
	if en.URL.Valid {
		s += " <" + en.URL.V + ">"
	}
	return s
}

func optionsText(opts []localapi.OptionInfo) string {
	if len(opts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		if o.Tool != "" {
			parts = append(parts, o.Action+" ["+o.Tool+"]")
		} else {
			parts = append(parts, o.Action)
		}
	}
	return " — act: " + strings.Join(parts, "; ")
}

func countText(sec localapi.SituationSection[localapi.SituationEntry]) string {
	return strconv.Itoa(len(sec.Entries) + sec.Omitted)
}

func omittedTotal(v localapi.SituationView) int {
	return v.MergeDecisions.Omitted + v.Gates.Omitted + v.HumanInput.Omitted + v.Decisions.Omitted +
		v.Stalled.Omitted + v.MergeTrain.Omitted + v.Anomalies.Omitted
}

func knownText[T any](k localapi.Known[T]) string {
	if !k.Valid {
		return localapi.UnknownText
	}
	return fmt.Sprint(k.V)
}

func ageText(k localapi.Known[int64]) string {
	if !k.Valid {
		return localapi.UnknownText
	}
	return (time.Duration(k.V) * time.Second).String()
}

func pollText(p localapi.PollHealth) string {
	switch {
	case p.Overdue.Valid && p.Overdue.V:
		return "OVERDUE (" + p.OverdueCause + ")"
	case p.InProgress:
		return "in progress"
	default:
		return "idle"
	}
}

// ---- fingerprint ----

// situationFingerprint hashes the substance of v: identity, state, code, rank,
// gate name, verdicts, partition phase and idle reason, slots, queued counts,
// truncation counts and the stable timestamps. It is an explicit allowlist, so
// a field added to the view later cannot leak into dedup by accident. It leaves
// out everything that moves with the clock or the poll cycle alone — the
// freshness envelope, every age or *_seconds figure, whether a poll happens to be
// running right now and its step — so an
// unchanged view does not re-send merely because time passed.
func situationFingerprint(v localapi.SituationView) string {
	var b strings.Builder
	w := func(parts ...string) { b.WriteString(strings.Join(parts, "\x1f") + "\x1e") }
	t := func(k localapi.Known[time.Time]) string {
		if !k.Valid {
			return localapi.UnknownText
		}
		return k.V.UTC().Format(time.RFC3339)
	}
	w("nothing", strconv.FormatBool(v.NothingNeedsYou), "unclassified", strconv.Itoa(v.UnclassifiedItems))

	entries := func(name string, sec localapi.SituationSection[localapi.SituationEntry]) {
		w("section", name, strconv.Itoa(sec.Omitted))
		es := append([]localapi.SituationEntry(nil), sec.Entries...)
		sort.SliceStable(es, func(i, j int) bool { return es[i].Issue < es[j].Issue })
		for _, en := range es {
			w("item", en.Issue, en.Title, knownText(en.URL), en.State, en.Code, strconv.Itoa(en.Rank), optionsText(en.Options))
			if m := en.Merge; m != nil {
				w("merge", knownText(m.PR), knownText(m.Review), knownText(m.CI))
			}
			if g := en.Gate; g != nil {
				w("gate", g.Gate, g.Reason, t(g.HeldSince))
			}
			if h := en.Human; h != nil {
				w("human", h.ResumeVia)
			}
			if p := en.Pending; p != nil {
				w("pending", p.Kind, p.CompletedStage, p.NextStage, t(p.Since), p.SinceBasis)
				bl := make([]string, 0, len(p.Blockers))
				for _, x := range p.Blockers {
					bl = append(bl, x.Issue+"@"+x.Column)
				}
				sort.Strings(bl)
				w("blockers", strings.Join(bl, ","))
			}
		}
	}
	entries("merge_decisions", v.MergeDecisions)
	entries("gates", v.Gates)
	entries("human_input", v.HumanInput)
	entries("decisions", v.Decisions)
	entries("stalled", v.Stalled)

	w("section", "merge_train", strconv.Itoa(v.MergeTrain.Omitted))
	ps := append([]localapi.TrainPartition(nil), v.MergeTrain.Entries...)
	sort.SliceStable(ps, func(i, j int) bool {
		return mergeTrainKey(ps[i].Repo, ps[i].Base) < mergeTrainKey(ps[j].Repo, ps[j].Base)
	})
	for _, p := range ps {
		ms := append([]string(nil), p.Members...)
		sort.Strings(ms)
		w("partition", mergeTrainKey(p.Repo, p.Base), strings.Join(ms, ","), strconv.FormatBool(p.WorkerInFlight),
			strconv.FormatBool(p.RunActive), p.Phase, p.IdleReason, strconv.FormatBool(p.Stalled))
	}

	h := v.Health
	healthFields := []string{knownText(h.Poll.Overdue), h.Poll.OverdueCause, h.Poll.BackoffReason,
		strconv.Itoa(h.Workers.InUse), knownText(h.Workers.MaxConcurrent), knownText(h.Workers.TrainInUse), knownText(h.Workers.TrainMax),
		strconv.FormatBool(h.Claude.UsageLimited), strconv.Itoa(h.TrainQueued)}
	if h.Workers.FixMax.Valid && h.Workers.FixMax.V > 0 {
		// Only when the reservation exists (#2490), so a fingerprint with it off is unchanged.
		healthFields = append(healthFields, knownText(h.Workers.FixInUse), knownText(h.Workers.FixMax))
	}
	w(append([]string{"health"}, healthFields...)...)

	w("section", "anomalies", strconv.Itoa(v.Anomalies.Omitted))
	as := append([]localapi.SituationAnomaly(nil), v.Anomalies.Entries...)
	sort.SliceStable(as, func(i, j int) bool {
		if as[i].Kind != as[j].Kind {
			return as[i].Kind < as[j].Kind
		}
		return as[i].Subject < as[j].Subject
	})
	for _, a := range as {
		w("anomaly", a.Kind, a.Scope, a.Subject, t(a.Since))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
