package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/warnings"
)

// The operator warnings-panel entries for the display-only board fields
// (#2218, ADR 2218; docs/state-machine.md §7.11, §7.17).
//
// The three fields (status line, Last activity, Last run) are resolved
// independently, each with its own displayFieldState. A board that lacks them
// still runs; but the operator has configured fields that will never be
// written, and nothing else says so, so the aggregate is surfaced as panel
// entries:
//
//   - display_field:missing — one entry naming every enabled field the board
//     does not have. Fix: `fabrik repair-board --apply` (ADR 2110), except on a
//     user-owned board, which repair-board refuses (R7): there the entry says
//     to create the fields by hand and carries no FixAction.
//   - display_field:mismatch:<label> — one entry per field whose name is taken
//     by a field of another type. repair-board reports that as a type mismatch
//     and never writes it, so these carry no FixAction.
//   - display_field_lookup:<label> — a lookup that failed (recorded by
//     resolveDisplayField, not here).
//
// An absent field is final for the process (resolveDisplayField's `attempted`),
// so a missing entry clears at the next start after the fields are created, or
// at once when the missing ones are set to `off`.
//
// Display-only: nothing here changes how, or whether, a field is written.

const (
	displayFieldType       = "display_field"
	displayFieldLookupType = "display_field_lookup"
)

// displayFieldWarnState remembers the last outcome set the aggregate was
// computed for, so a poll that changes nothing does no panel I/O.
type displayFieldWarnState struct {
	mu   sync.Mutex
	sig  string
	done bool
}

// resolveDisplayFields resolves the three display fields on projectID and then
// brings the warnings panel in line with the result. Safe to call every poll.
func (e *Engine) resolveDisplayFields(projectID string) {
	e.resolveStatusLineField(projectID)
	e.resolveRunFields(projectID)
	e.reportDisplayFieldWarnings()
}

type displayFieldSnapshot struct {
	spec    displayFieldSpec
	outcome displayFieldOutcome
}

func (e *Engine) displayFieldSnapshots() []displayFieldSnapshot {
	pairs := []struct {
		spec displayFieldSpec
		s    *displayFieldState
	}{
		{e.statusLineSpec(), &e.statusLine},
		{e.lastActivitySpec(), &e.lastActivity},
		{e.lastRunSpec(), &e.lastRun},
	}
	out := make([]displayFieldSnapshot, 0, len(pairs))
	for _, p := range pairs {
		p.s.mu.Lock()
		o := p.s.outcome
		if !p.s.attempted {
			o = displayUnresolved
		}
		p.s.mu.Unlock()
		out = append(out, displayFieldSnapshot{p.spec, o})
	}
	return out
}

func (e *Engine) reportDisplayFieldWarnings() {
	w := &e.displayFieldWarn
	w.mu.Lock()
	defer w.mu.Unlock()

	snaps := e.displayFieldSnapshots()
	userOwned := strings.EqualFold(e.cfg.OwnerType, "user")

	var sig strings.Builder
	var missing []displayFieldSnapshot
	var mismatched []displayFieldSnapshot
	conclusive := true
	for _, sn := range snaps {
		fmt.Fprintf(&sig, "%s=%d;", sn.spec.label, sn.outcome)
		switch sn.outcome {
		case displayAbsent:
			missing = append(missing, sn)
		case displayMismatch:
			mismatched = append(mismatched, sn)
		case displayUnresolved:
			conclusive = false
		}
	}
	fmt.Fprintf(&sig, "user=%t", userOwned)
	if w.done && w.sig == sig.String() {
		return
	}

	var conds []warnings.Condition
	if len(missing) > 0 {
		conds = append(conds, missingDisplayFieldsCondition(missing, userOwned))
	}
	for _, sn := range mismatched {
		conds = append(conds, mismatchedDisplayFieldCondition(sn))
	}

	var err error
	if conclusive {
		// Every enabled field has an answer, so the check ran to completion:
		// clear whatever a previous process (or an earlier state of this one)
		// left that no longer holds.
		err = warnings.Reconcile(nil, displayFieldType, conds)
	} else {
		// Some field is still unresolved (a failed lookup, or the project not
		// known yet): record what is known, resolve nothing.
		for _, c := range conds {
			c.Type = displayFieldType
			if rerr := warnings.Report(nil, c); rerr != nil && err == nil {
				err = rerr
			}
		}
	}
	if err != nil {
		e.logf(0, "startup", "recording display-field warnings: %v\n", err)
		return // retried on the next call
	}
	w.sig = sig.String()
	w.done = true
}

func displayFieldDescription(sn displayFieldSnapshot) string {
	return fmt.Sprintf("%q (%s field, %s)", sn.spec.name, sn.spec.kind, sn.spec.cfgKey)
}

func missingDisplayFieldsCondition(missing []displayFieldSnapshot, userOwned bool) warnings.Condition {
	descs := make([]string, 0, len(missing))
	names := make([]string, 0, len(missing))
	for _, sn := range missing {
		descs = append(descs, displayFieldDescription(sn))
		names = append(names, sn.spec.name)
	}
	sort.Strings(names)

	detail := "The project board has no " + strings.Join(descs, ", ") + ". " +
		"Fabrik runs without them, but they are never written, so the board does not show what each item is doing."
	c := warnings.Condition{
		Subject:     "missing",
		Title:       "Display fields missing from the project board: " + strings.Join(names, ", "),
		Fingerprint: strings.Join(names, ","),
	}
	if userOwned {
		// fabrik repair-board refuses a user-owned board (ADR 1714 R7).
		c.Detail = detail + "\n\nCreate them by hand in the board's settings with the types above, or set the keys to `off`. " +
			"(`fabrik repair-board` cannot create fields on a user-owned board.)"
		return c
	}
	c.Detail = detail + "\n\nFix: fabrik repair-board --apply (creates the missing fields), or set the keys to `off`."
	c.FixAction = "fabrik_command"
	c.FixParams = map[string]string{"args": "repair-board --apply"}
	return c
}

func mismatchedDisplayFieldCondition(sn displayFieldSnapshot) warnings.Condition {
	return warnings.Condition{
		Subject: "mismatch:" + sn.spec.label,
		Title:   fmt.Sprintf("Display field %q is not a %s field", sn.spec.name, sn.spec.kind),
		Detail: fmt.Sprintf("The project board has a field named %q, but it is not a %s field, so Fabrik does not write it. "+
			"`fabrik repair-board` reports this as a type mismatch and never changes an existing field.\n\n"+
			"Rename or delete the board field, or point %s at a different name.", sn.spec.name, sn.spec.kind, sn.spec.cfgKey),
		Fingerprint: sn.spec.label + ":" + sn.spec.name + ":type-mismatch",
	}
}
