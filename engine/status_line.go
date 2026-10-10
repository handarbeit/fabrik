package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The status-line writer (#2048, ADR 2048): one general, engine-wide writer
// that projects a one-line description of what an item is doing or waiting on
// into an optional ProjectV2 text field (default name "Fabrik").
//
// It is DISPLAY-ONLY. Nothing reads the field — or the writer's own record of
// it — back to make a decision; labels and the item store stay the only
// state. The record of the last value written exists solely to skip a write
// that would change nothing.
//
// Rules, each pinned by engine/status_line_test.go:
//   - write only on a transition, and only when the line differs from the last
//     one this process wrote for the item (never per turn, never per poll);
//   - clearing at Done is the one required exit; other stale lines persist
//     until the next transition (documented, not repaired by a read-back);
//   - lines are capped at statusLineMaxLen characters (runes, not bytes);
//   - an absent / non-text / disabled field makes every call a silent no-op,
//     with exactly one [startup] line saying so;
//   - a failed write is logged and swallowed — it never alters the transition
//     that triggered it, and because the record is only updated on success the
//     next transition retries;
//   - writes do not register webhook echoes: the echo key
//     (projects_v2_item:edited:<ItemID>) has no field discriminator, so it
//     would collide with Status-move registrations and a missing delivery would
//     inflate the ADR-042 miss counter.

// statusLineMaxLen is the cap on a line, in characters.
const statusLineMaxLen = 60

// statusLineEllipsis marks a truncated line.
const statusLineEllipsis = "…"

// statusLineLookupRetry is the minimum gap between two failed attempts to look
// the field up. Without it a persistent failure (a token without project read
// scope, an API outage) would cost a blocking GraphQL request on every
// transition, from the poll and train goroutines alike.
const statusLineLookupRetry = 5 * time.Minute

// displayFieldKind is the ProjectV2 field type a display field is written as.
type displayFieldKind int

const (
	displayText displayFieldKind = iota
	displayDate
)

func (k displayFieldKind) String() string {
	if k == displayDate {
		return "date"
	}
	return "text"
}

// displayFieldSpec is the immutable description of one display-only field the
// writer maintains: the status line (#2048), Last activity and Last run
// (#2049). The mutable per-field state lives in displayFieldState; every field
// has its own, so a missing or failing field never affects another.
type displayFieldSpec struct {
	label  string // log tag and "unavailable" wording, e.g. "status-line"
	cfgKey string // YAML key, for the startup line
	kind   displayFieldKind
	name   string // configured board field name; "" = off
}

func (e *Engine) statusLineSpec() displayFieldSpec {
	return displayFieldSpec{"status-line", "project_fields.status_line", displayText, e.cfg.StatusLineField}
}

func (e *Engine) lastActivitySpec() displayFieldSpec {
	return displayFieldSpec{"last-activity", "project_fields.last_activity", displayDate, e.cfg.LastActivityField}
}

func (e *Engine) lastRunSpec() displayFieldSpec {
	return displayFieldSpec{"last-run", "project_fields.last_run", displayText, e.cfg.LastRunField}
}

// displayFieldState is one display field's writer state, owned by the Engine.
// It is in memory only and never read to make a decision: last exists solely
// to skip a write that would change nothing.
type displayFieldState struct {
	mu        sync.Mutex
	attempted bool   // field lookup has completed (successfully) once
	logged    bool   // the single "unavailable" startup line has been emitted
	projectID string // project the field was resolved on
	fieldID   string // "" = feature off or field missing / wrong type
	last      map[string]string

	// lookupMu serialises field lookups so concurrent first writes make one
	// request, and guards the failure bookkeeping below.
	lookupMu       sync.Mutex
	lookupFailedAt time.Time // time of the last failed lookup; zero = none outstanding
	lookupWarned   bool      // the failure warning has been logged this outage

	// keyLocks serialises writes per item so two goroutines (poll, train
	// worker) cannot land their mutations out of order relative to the record.
	keyLocks sync.Map // key -> *sync.Mutex
}

func (s *displayFieldState) keyLock(key string) *sync.Mutex {
	m, _ := s.keyLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// lookupDisplayField fetches the field by name with the spec's type; "" when
// it is absent or of another type.
func (e *Engine) lookupDisplayField(spec displayFieldSpec, projectID string) (string, error) {
	if spec.kind == displayDate {
		f, err := e.client.FetchDateField(projectID, spec.name)
		if err != nil || f == nil {
			return "", err
		}
		return f.ID, nil
	}
	f, err := e.client.FetchTextField(projectID, spec.name)
	if err != nil || f == nil {
		return "", err
	}
	return f.ID, nil
}

// resolveDisplayField looks the configured field up on the project once and
// logs the single startup line when the feature is unavailable. It is safe to
// call repeatedly; only a successful lookup (found or definitively absent) is
// final, so a transient API error at startup is retried lazily by a later
// write rather than disabling the feature for the process lifetime. Failed
// lookups are rate-limited to one per statusLineLookupRetry and warned about
// once per outage.
func (e *Engine) resolveDisplayField(spec displayFieldSpec, s *displayFieldState, projectID string) {
	s.lookupMu.Lock()
	defer s.lookupMu.Unlock()

	s.mu.Lock()
	done := s.attempted
	s.mu.Unlock()
	if done {
		return
	}

	if spec.name == "" {
		e.finishDisplayFieldResolve(spec, s, projectID, "", "disabled by "+spec.cfgKey+": off")
		return
	}
	if projectID == "" {
		return // board metadata not known yet; try again later
	}
	// A recent failed lookup is not repeated on every transition.
	if !s.lookupFailedAt.IsZero() && e.now().Sub(s.lookupFailedAt) < statusLineLookupRetry {
		return
	}
	id, err := e.lookupDisplayField(spec, projectID)
	if err != nil {
		s.lookupFailedAt = e.now()
		if !s.lookupWarned { // once per outage, not once per retry
			s.lookupWarned = true
			e.logf(0, "startup", "warning: could not look up the %q %s field: %v — it is not written until this succeeds (retrying every %s)\n", spec.name, spec.label, err, statusLineLookupRetry)
		}
		return
	}
	s.lookupFailedAt = time.Time{}
	s.lookupWarned = false
	if id == "" {
		e.finishDisplayFieldResolve(spec, s, projectID, "",
			fmt.Sprintf("no %s field named %q on the project board (create one to enable it)", spec.kind, spec.name))
		return
	}
	e.finishDisplayFieldResolve(spec, s, projectID, id, "")
}

func (e *Engine) finishDisplayFieldResolve(spec displayFieldSpec, s *displayFieldState, projectID, fieldID, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempted {
		return
	}
	s.attempted = true
	s.projectID = projectID
	s.fieldID = fieldID
	if fieldID == "" && !s.logged {
		s.logged = true
		e.logf(0, "startup", "%s field unavailable — %s; it will not be written\n", spec.label, why)
	}
}

// displayFieldHandle returns the resolved field ID and its project, resolving
// lazily the first time a write is attempted if startup could not.
func (e *Engine) displayFieldHandle(spec displayFieldSpec, s *displayFieldState) (string, string) {
	s.mu.Lock()
	attempted := s.attempted
	s.mu.Unlock()
	if !attempted {
		var projectID string
		if c := e.cache(); c != nil {
			projectID = c.ProjectID()
		}
		e.resolveDisplayField(spec, s, projectID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fieldID, s.projectID
}

// resolveStatusLineField resolves the status-line field (see
// resolveDisplayField).
func (e *Engine) resolveStatusLineField(projectID string) {
	e.resolveDisplayField(e.statusLineSpec(), &e.statusLine, projectID)
}

// resolveRunFields resolves the Last activity and Last run fields (#2049),
// each with its own lookup and startup line.
func (e *Engine) resolveRunFields(projectID string) {
	e.resolveDisplayField(e.lastActivitySpec(), &e.lastActivity, projectID)
	e.resolveDisplayField(e.lastRunSpec(), &e.lastRun, projectID)
}

// setStatusLine writes line to item's status-line field. It is a no-op when
// the feature is unavailable or the line equals the last one written for the
// item. Errors are logged and swallowed.
func (e *Engine) setStatusLine(item gh.ProjectItem, line string) {
	e.writeDisplayField(e.statusLineSpec(), &e.statusLine, item, truncateStatusLine(line), false)
}

// clearStatusLine clears item's status-line field (the item reached Done). With
// no record for the item — a restart wiped it — one blind clear is made per
// call until it succeeds; afterwards repeats are skipped.
func (e *Engine) clearStatusLine(item gh.ProjectItem) {
	e.writeDisplayField(e.statusLineSpec(), &e.statusLine, item, "", true)
}

// writeDisplayField writes value to item's field described by spec. It is a
// no-op when the field is off/unavailable or value equals the last one this
// process wrote for the item. A failed write is logged and swallowed; the
// record is updated only on success so the next transition retries.
func (e *Engine) writeDisplayField(spec displayFieldSpec, s *displayFieldState, item gh.ProjectItem, value string, clear bool) {
	if spec.name == "" || item.ItemID == "" {
		return
	}
	fieldID, projectID := e.displayFieldHandle(spec, s)
	if fieldID == "" || projectID == "" {
		return
	}
	key := issueKey(item, e.defaultRepo())

	kl := s.keyLock(key)
	kl.Lock()
	defer kl.Unlock()

	s.mu.Lock()
	prev, known := s.last[key]
	s.mu.Unlock()
	if known && prev == value {
		return
	}

	var err error
	switch {
	case clear:
		err = e.client.ClearProjectItemField(projectID, item.ItemID, fieldID)
	case spec.kind == displayDate:
		err = e.client.UpdateProjectItemDateField(projectID, item.ItemID, fieldID, value)
	default:
		err = e.client.UpdateProjectItemTextField(projectID, item.ItemID, fieldID, value)
	}
	if err != nil {
		e.logf(item.Number, spec.label, "warning: could not write %s %q: %v\n", spec.label, value, err)
		return
	}

	s.mu.Lock()
	if s.last == nil {
		s.last = make(map[string]string)
	}
	s.last[key] = value
	s.mu.Unlock()

	// The write bumps the project item's updatedAt; advance the staleness
	// baseline like every other self-write (#1090) so the probe does not
	// re-fetch the item for our own display write.
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
}

// truncateStatusLine caps line at statusLineMaxLen characters, counting
// runes, ending a truncated line with an ellipsis.
func truncateStatusLine(line string) string {
	line = strings.Join(strings.Fields(line), " ") // single-line, no stray whitespace
	if utf8.RuneCountInString(line) <= statusLineMaxLen {
		return line
	}
	runes := []rune(line)
	return strings.TrimRight(string(runes[:statusLineMaxLen-1]), " ") + statusLineEllipsis
}

// clearStatusLineIfShowing clears item's line only while it is still exactly
// line. It ends an activity line (e.g. "<Stage> · running") that nothing
// replaced, so an idle card does not keep claiming to be running, without
// disturbing a newer line another transition wrote in the meantime.
func (e *Engine) clearStatusLineIfShowing(item gh.ProjectItem, line string) {
	if e.cfg.StatusLineField == "" {
		return
	}
	key := issueKey(item, e.defaultRepo())
	s := &e.statusLine
	s.mu.Lock()
	cur, known := s.last[key]
	s.mu.Unlock()
	if known && cur == truncateStatusLine(line) {
		e.clearStatusLine(item)
	}
}

// setMembersStatusLine writes the same line for every merge-train member. One
// mutation per member; each is skipped when that member's line is unchanged.
func (e *Engine) setMembersStatusLine(members []trainMember, line string) {
	for _, m := range members {
		e.setStatusLine(m.item, line)
	}
}

// clearMembersStatusLine clears the line of every member (they left the train
// for a column whose own transition will write a fresh line, or for Done).
func (e *Engine) clearMembersStatusLine(members []trainMember) {
	for _, m := range members {
		e.clearStatusLine(m.item)
	}
}

// withStatusLine shows line for the duration of an activity and returns the
// function that ends it: the previous line is put back (or the field cleared
// when there was none), but only if nothing wrote a newer line in the
// meantime. Use as `defer e.withStatusLine(item, line)()`.
func (e *Engine) withStatusLine(item gh.ProjectItem, line string) func() {
	if e.cfg.StatusLineField == "" {
		return func() {}
	}
	key := issueKey(item, e.defaultRepo())
	s := &e.statusLine
	s.mu.Lock()
	prev, hadPrev := s.last[key]
	s.mu.Unlock()
	shown := truncateStatusLine(line)
	e.setStatusLine(item, line)
	return func() {
		s.mu.Lock()
		cur, known := s.last[key]
		s.mu.Unlock()
		if !known || cur != shown {
			return
		}
		if hadPrev && prev != "" {
			e.setStatusLine(item, prev)
		} else {
			e.clearStatusLine(item)
		}
	}
}
