package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// The trust gate (#2551, ADR 2551, R2): while the engine's intended state and
// GitHub disagree about a stage's completion label, no advance trusts the copy
// in item.Labels.
//
// The hole it closes: a comment pass removes stage:<S>:complete for the rework
// (beginStageRework) and, if that removal does not stick, the label stays visible.
// The non-Validate catch-up advance (runCatchUpPhase2) has no worker or rework
// guard and reads item.Labels, so it would advance an item whose rework was never
// checked. The gate is deliberately narrow: it does nothing unless the stage's
// complete label is marked diverged (label_repair.go) or a rework of that stage is
// open, so every other item behaves exactly as before. Where it applies it fails
// closed, like the pause guard (ADR 2341): an unreadable label set is a refusal.
//
// The other in-scope labels need no gate. The pause pair is read live by the pause
// guard at every decision, and no advance reads the lock, editing or in_progress
// labels.

// SetLabelTrustGateDisabledForTest neutralises the gate (R2) so a test can show the
// twin fails without it. Per Engine.
func (e *Engine) SetLabelTrustGateDisabledForTest(disabled bool) {
	e.labelTrustOff.Store(disabled)
}

// refuseIfLabelsUntrusted reports whether the advance of item out of stage must be
// refused because the stage's completion label cannot be trusted. site names the
// decision ("advancing"). A refusal logs once, writes no label or comment, pauses
// nothing and records the short merge-unsettled admission cooldown so the decision
// is taken again on a later poll.
func (e *Engine) refuseIfLabelsUntrusted(item gh.ProjectItem, stage *stages.Stage, site string) bool {
	if e.labelTrustOff.Load() {
		return false
	}
	repoKey := itemOwnerRepoString(item, e.defaultRepo())
	ref := issueRef(repoKey, item.Number)
	complete := "stage:" + stage.Name + ":complete"
	g := &e.labels

	reworking := g.reworkOpenFor(ref) == stage.Name
	if !reworking && !g.marked(ref, complete) {
		return false
	}

	reason := ""
	switch {
	case reworking:
		// A rework of this stage is open: whatever the label shows, the stage is not
		// complete until the pass ends and the engine restores or re-derives it.
		reason = "a rework of this stage is open"
	default:
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		labels, err := e.liveLabels(owner, repo, item.Number)
		switch {
		case err != nil:
			reason = fmt.Sprintf("the label set could not be re-read (%v)", err)
		default:
			present := hasLabel(labels, complete)
			in, hasIntent := g.intent(ref, complete)
			switch {
			case hasIntent && present != in.added:
				reason = fmt.Sprintf("GitHub %s %s but the engine's last write %s it", presentWord(present), complete, addedWord(in.added))
			case !present:
				// The cached item claims a completion GitHub does not have.
				reason = fmt.Sprintf("GitHub does not carry %s although the cached item does", complete)
			default:
				g.unmark(ref, complete) // GitHub and the engine agree: trusted again
			}
		}
	}
	if reason == "" {
		return false
	}
	e.logf(item.Number, "label-trust", "not %s: %s — %s; the decision is taken again once they agree\n", site, complete, reason)
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   repoKey,
		Number: item.Number,
		Reason: mergeUnsettledCooldownReason,
		Until:  e.now().Add(e.mergeGateRecheckInterval()),
	})
	return true
}

func addedWord(added bool) string {
	if added {
		return "added"
	}
	return "removed"
}
