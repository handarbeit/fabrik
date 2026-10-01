package registry

import (
	"fmt"
	"sort"
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
	return out
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
