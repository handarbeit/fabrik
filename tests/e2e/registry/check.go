package registry

import (
	"fmt"
	"sort"
	"strings"
)

// Check validates reg against the discovered live and sim test names and
// returns every problem found (nil when consistent).
func Check(reg *Registry, live, sim []string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if reg.Version != Version {
		add("registry version is %d, want %d", reg.Version, Version)
	}

	liveSet := toSet(live)
	simSet := toSet(sim)

	seen := map[string]bool{}
	prev := ""
	for i, e := range reg.Tests {
		if e.Name == "" {
			add("entry %d has no name", i)
			continue
		}
		if seen[e.Name] {
			add("%s: duplicate registry entry", e.Name)
		}
		seen[e.Name] = true
		if i > 0 && e.Name < prev {
			add("%s: entries are not sorted by name (follows %s)", e.Name, prev)
		}
		prev = e.Name

		if !liveSet[e.Name] {
			add("%s: stale entry — no live scenario test of that name exists in tests/e2e", e.Name)
		}
		problems = append(problems, validateEntry(e, simSet)...)
	}

	for _, n := range live {
		if !seen[n] {
			add("%s: live scenario test has no registry entry (add one to tests/e2e/registry/registry.json)", n)
		}
	}
	problems = append(problems, checkFullTraversalSet(reg)...)
	return problems
}

func validateEntry(e Entry, simSet map[string]bool) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, e.Name+": "+fmt.Sprintf(format, args...))
	}
	switch e.Parity {
	case ParitySim:
		if len(e.Sim) == 0 {
			add("parity %q requires a non-empty sim list", ParitySim)
		}
		if e.LiveOnlyReason != "" {
			add("live_only_reason %q is only valid with parity %q", e.LiveOnlyReason, ParityLiveOnly)
		}
		dup := map[string]bool{}
		for _, s := range e.Sim {
			if dup[s] {
				add("sim reference %s listed twice", s)
			}
			dup[s] = true
			if !simSet[s] {
				add("dangling sim reference %s — no top-level Test function of that name in tests/sim", s)
			}
		}
	case ParityLiveOnly:
		if len(e.Sim) != 0 {
			add("parity %q must not carry a sim list", ParityLiveOnly)
		}
		if !validReason(e.LiveOnlyReason) {
			add("unknown live_only_reason %q (want one of %v)", e.LiveOnlyReason, Reasons)
		}
		if e.LiveOnlyReason == ReasonOther && e.Note == "" {
			add("live_only_reason %q requires a note", ReasonOther)
		}
	case ParityGap:
		if len(e.Sim) != 0 {
			add("parity %q must not carry a sim list", ParityGap)
		}
		if e.LiveOnlyReason != "" {
			add("live_only_reason %q is only valid with parity %q", e.LiveOnlyReason, ParityLiveOnly)
		}
	default:
		add("unknown parity %q (want %q, %q or %q)", e.Parity, ParitySim, ParityLiveOnly, ParityGap)
	}
	out = append(out, validateSensitivity(e)...)
	out = append(out, validateIsolation(e)...)
	out = append(out, validateTraversal(e)...)
	for _, p := range e.SkipOKLegs {
		if !validLegPattern(p) {
			add("malformed skip_ok_legs pattern %q (want \"<auth>/<train>\" with auth pat|app|*, train off|on|*)", p)
		}
	}
	return out
}

// selfRecognitionMarker is the name fragment that makes a test a self-recognition
// test: such a test is about who "we" are, so it is auth-sensitive by definition.
const selfRecognitionMarker = "SelfRecognition"

func validateSensitivity(e Entry) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, e.Name+": "+fmt.Sprintf(format, args...))
	}
	axis := func(field string, v Sensitivity, reason string) {
		switch v {
		case Sensitive:
			if strings.TrimSpace(reason) == "" {
				add("%s is %q but %s_reason is empty (every sensitive mark needs a one-line reason)", field, Sensitive, field)
			}
			if strings.ContainsAny(reason, "\r\n") {
				add("%s_reason must be a single line", field)
			}
		case Neutral:
			if reason != "" {
				add("%s_reason is only valid with %s %q", field, field, Sensitive)
			}
		default:
			add("%s is %q (want %q or %q; there is no default)", field, v, Sensitive, Neutral)
		}
	}
	axis("auth", e.Auth, e.AuthReason)
	axis("train", e.Train, e.TrainReason)
	if strings.Contains(e.Name, selfRecognitionMarker) && e.Auth != Sensitive {
		add("a %s test must be auth: %s", selfRecognitionMarker, Sensitive)
	}
	return out
}

// validateIsolation checks the two #1977 markers: a reason is required and must
// be one non-empty line iff its flag is set, and a test is at most one class.
func validateIsolation(e Entry) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, e.Name+": "+fmt.Sprintf(format, args...))
	}
	marker := func(field string, on bool, reason string) {
		if on {
			if strings.TrimSpace(reason) == "" {
				add("%s is true but %s_reason is empty (every isolation mark needs a one-line reason)", field, field)
			}
			if strings.ContainsAny(reason, "\r\n") {
				add("%s_reason must be a single line", field)
			}
		} else if reason != "" {
			add("%s_reason is only valid with %s: true", field, field)
		}
	}
	marker("exclusive", e.Exclusive, e.ExclusiveReason)
	marker("default_base_train", e.DefaultBaseTrain, e.DefaultBaseTrainReason)
	if e.Exclusive && e.DefaultBaseTrain {
		add("exclusive and default_base_train are mutually exclusive (a test has one isolation class)")
	}
	return out
}

// validateTraversal enforces the #1992 entry-stage / traversal fields.
func validateTraversal(e Entry) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, e.Name+": "+fmt.Sprintf(format, args...))
	}
	if !validStage(e.EntryStage) {
		add("entry is %q (want one of %v; there is no default)", e.EntryStage, Stages)
	}
	switch e.Traversal {
	case TraversalSubject, TraversalNone:
	default:
		add("traversal is %q (want %q or %q; there is no default — %q is deliberately not a value: a test whose traversal is only set-up must seed the state instead)",
			e.Traversal, TraversalSubject, TraversalNone, "setup")
	}
	needsReason := e.Traversal == TraversalSubject || e.EntryStage == StageSpecify
	if needsReason && strings.TrimSpace(e.TraversalReason) == "" {
		add("traversal_reason is empty (required for traversal %q and for any test entering at %s: say why it is not seeded)", TraversalSubject, StageSpecify)
	}
	if !needsReason && e.TraversalReason != "" {
		add("traversal_reason is only valid with traversal %q or entry %q", TraversalSubject, StageSpecify)
	}
	if strings.ContainsAny(e.TraversalReason, "\r\n") {
		add("traversal_reason must be a single line")
	}
	if e.FullTraversal && (e.Traversal != TraversalSubject || e.EntryStage != StageSpecify) {
		add("full_traversal requires traversal %q and entry %q", TraversalSubject, StageSpecify)
	}
	return out
}

// CheckBedLifecycleCallers reports every live test in callers (the set that
// reaches a bed stop/start/restart or a bed .env rewrite, from
// ScanBedLifecycleCallers) whose registry entry is not exclusive. Entries that do
// not exist are Check's concern, not this one's.
func CheckBedLifecycleCallers(reg *Registry, callers []string) []string {
	byName := map[string]Entry{}
	for _, e := range reg.Tests {
		byName[e.Name] = e
	}
	var problems []string
	for _, n := range callers {
		if e, ok := byName[n]; ok && !e.Exclusive {
			problems = append(problems, fmt.Sprintf("%s: reaches a bed lifecycle call (%s) and so must be exclusive: true with an exclusive_reason",
				n, strings.Join(BedLifecycleCalls, " / ")))
		}
	}
	return problems
}

// CheckParallelConsistency reports every live test whose t.Parallel() use
// disagrees with its class: a shared test must call t.Parallel() (otherwise it
// would silently run serially at the head of the shared phase, with no gain) and
// a default-base-train or exclusive test must not (it is serialised by its phase,
// and a stray t.Parallel() would let it overlap its phase peers). parallel is the
// set from ScanParallelTests.
func CheckParallelConsistency(reg *Registry, parallel []string) []string {
	par := toSet(parallel)
	var problems []string
	for _, e := range reg.Tests {
		switch cls := e.Isolation(); {
		case cls == IsolationShared && !par[e.Name]:
			problems = append(problems, fmt.Sprintf("%s: is shared but does not call t.Parallel() (it would run serially in the shared phase)", e.Name))
		case cls != IsolationShared && par[e.Name]:
			problems = append(problems, fmt.Sprintf("%s: is %s but calls t.Parallel() (it must run serially in its phase)", e.Name, cls))
		}
	}
	return problems
}

// checkFullTraversalSet fails when no entry is marked full_traversal (R3): the
// named set that drives each pipeline path end to end must never become empty.
func checkFullTraversalSet(reg *Registry) []string {
	for _, e := range reg.Tests {
		if e.FullTraversal {
			return nil
		}
	}
	return []string{"no registry entry is marked full_traversal: the named full-traversal set (R3) must not be empty"}
}

// CheckEntryLiterals reports every live test whose declared entry is not Specify
// but whose same-package reference closure (specifyDrivers, from
// ScanSpecifyDrivers) places an item at Specify — a stale or dishonest entry
// claim, the signature of a late-subject test that still drives the pipeline from
// its start. exempt names tests allowed to do so (a cheap primer filing, not a
// traversal of the subject), each with a reason. Entries that do not exist are
// Check's concern.
func CheckEntryLiterals(reg *Registry, specifyDrivers []string, exempt map[string]string) []string {
	byName := map[string]Entry{}
	for _, e := range reg.Tests {
		byName[e.Name] = e
	}
	var problems []string
	for _, n := range specifyDrivers {
		e, ok := byName[n]
		if !ok || e.EntryStage == StageSpecify {
			continue
		}
		if _, ok := exempt[n]; ok {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: declares entry %q but reaches SetIssueStatus(..., %q) — either it enters at %s, or the Specify traversal must be replaced by a seed",
			n, e.EntryStage, "Specify", StageSpecify))
	}
	return problems
}

// CheckIdentityCallers reports every live test in callers (the set of tests that
// reach an author-identity assertion, from ScanIdentityAssertCallers) whose
// registry entry is not auth: sensitive. Entries that do not exist are Check's
// concern, not this one's.
func CheckIdentityCallers(reg *Registry, callers []string) []string {
	byName := map[string]Entry{}
	for _, e := range reg.Tests {
		byName[e.Name] = e
	}
	var problems []string
	for _, n := range callers {
		if e, ok := byName[n]; ok && e.Auth != Sensitive {
			problems = append(problems, fmt.Sprintf("%s: reaches an author-identity assertion (%s) and so must be auth: %s",
				n, strings.Join(IdentityAssertions, " / "), Sensitive))
		}
	}
	return problems
}

func validStage(s Stage) bool {
	for _, v := range Stages {
		if v == s {
			return true
		}
	}
	return false
}

func validLegPattern(p string) bool {
	auth, train, ok := strings.Cut(p, "/")
	if !ok {
		return false
	}
	return (auth == "*" || auth == "pat" || auth == "app") && (train == "*" || train == "off" || train == "on")
}

func validReason(r Reason) bool {
	for _, v := range Reasons {
		if v == r {
			return true
		}
	}
	return false
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// SortedNames returns reg's entry names sorted (helper for callers/tests).
func SortedNames(reg *Registry) []string {
	out := make([]string, 0, len(reg.Tests))
	for _, e := range reg.Tests {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}
