package channelevents

import (
	"strconv"
	"strings"
)

// DefaultExcludeLabels are the high-churn labels a subscription skips unless it
// opts into everything with an explicit empty exclude list (R3).
var DefaultExcludeLabels = []string{
	"fabrik:locked:*",
	"stage:*:in_progress",
	"fabrik:spawned-child:*",
	"fabrik:credited-pr:*",
	"fabrik:editing",
}

// GlobMatch reports whether name matches pattern, where `*` matches any run of
// characters (including `:` and `/`) and every other character is literal.
func GlobMatch(pattern, name string) bool {
	// Iterative two-pointer match with single-star backtracking.
	p, n := 0, 0
	star, mark := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			p = star + 1
			mark++
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if GlobMatch(p, name) {
			return true
		}
	}
	return false
}

// labelAllowed applies the include list (empty = everything) and the exclude
// list (the defaults unless explicitly set) to a label name.
func (s Subscription) labelAllowed(label string) bool {
	if len(s.Labels) > 0 && !matchAny(s.Labels, label) {
		return false
	}
	return !matchAny(s.EffectiveExclude(), label)
}

// Matches reports whether the subscription selects ev. Dimensions that are set
// are ANDed; entries within one list are ORed. Account-wide events ignore the
// repo/issue/milestone scopes and look only at the event-type filter. Label
// patterns apply to label-applied/-removed events only. An item whose
// milestone is unknown never matches a milestone scope.
func (s Subscription) Matches(ev Event) bool {
	if len(s.Events) > 0 && !containsType(s.Events, ev.Type) {
		return false
	}
	if IsAccountWide(ev.Type) {
		return true
	}
	if len(s.Repos) > 0 && !containsString(s.Repos, ev.Repo) {
		return false
	}
	if len(s.Issues) > 0 {
		ok := false
		for _, r := range s.Issues {
			if r.Number == ev.Issue && (r.Repo == "" || r.Repo == ev.Repo) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if s.Milestone != "" {
		if !ev.MilestoneKnown || !milestoneMatches(s.Milestone, ev) {
			return false
		}
	}
	if IsLabelEvent(ev.Type) && !s.labelAllowed(ev.Label) {
		return false
	}
	return true
}

// milestoneMatches compares a subscription's milestone (a title, or "#N" for a
// number) with the event's.
func milestoneMatches(want string, ev Event) bool {
	if strings.HasPrefix(want, "#") {
		return want == "#"+strconv.Itoa(ev.MilestoneNumber)
	}
	return want == ev.MilestoneTitle
}

func containsType(ts []EventType, t EventType) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
