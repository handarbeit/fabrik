// Package trainlog classifies merge-train bed-log lines for the live e2e suite
// (#2481, ADR 2481): did a seeded batch land as a batch, did a single member
// land alone, or was a seeded member ejected for reviewer feedback?
//
// A live merge-train test that seeds members straight into Queued can have its
// batch split by a bot review of a member PR (the engine's queued-review-settle
// ejects the member, and the rest land alone through the singleton fast path).
// That is intended engine behaviour, so the test must end INCONCLUSIVE rather than
// wait out a long budget and FAIL. Decide is the one first-in-time classifier the
// harness polls the bed log with.
//
// The package is deliberately untagged: the harness (tests/e2e, build tag e2e) is
// only compiled by CI, so a unit test placed there would never run on a PR.
// tests/e2e/awaitvisible and tests/e2e/inconclusive are the precedent.
//
// The patterns copy the engine's own format strings verbatim:
//
//	engine/merge_train.go "landing complete for %s (integration PR #%d, %d members)"        (%s = trainKey)
//	engine/merge_train.go "landing complete for %s (singleton fast path, PR #%d, 1 member)" (%s = trainKey)
//	engine/queued_review_settle.go "%d unprocessed review finding(s) (threads or review bodies) on Queued member #%d — …"
//	engine/queued_review_settle.go "%d unprocessed human comment(s) on Queued member #%d — …"
//
// They have no compile-time link to the engine; trainlog_test.go pins them so a
// wording change fails loudly instead of silently never matching.
package trainlog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind is the form of a "landing complete" line.
type Kind int

const (
	// Batch is "(integration PR #N, M members)".
	Batch Kind = iota + 1
	// Singleton is "(singleton fast path, PR #N, 1 member)".
	Singleton
)

// Landing is one parsed "landing complete" line.
type Landing struct {
	Kind    Kind
	PR      int
	Members int
}

var (
	batchRE     = regexp.MustCompile(`landing complete for (\S+) \(integration PR #(\d+), (\d+) members?\)`)
	singletonRE = regexp.MustCompile(`landing complete for (\S+) \(singleton fast path, PR #(\d+), (\d+) members?\)`)
)

// ParseLanding parses a "landing complete" line in either form. An empty
// trainKey matches any key; otherwise the key must match exactly, so
// acme/alpha does not match acme/alpha:branch.
func ParseLanding(line, trainKey string) (Landing, bool) {
	for _, c := range []struct {
		re   *regexp.Regexp
		kind Kind
	}{{batchRE, Batch}, {singletonRE, Singleton}} {
		m := c.re.FindStringSubmatch(line)
		if m == nil || (trainKey != "" && m[1] != trainKey) {
			continue
		}
		pr, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		return Landing{Kind: c.kind, PR: pr, Members: n}, true
	}
	return Landing{}, false
}

// ejectLeads are the queued-review-settle wordings up to the member number.
// The older "review-thread finding(s)" wording is kept for older beds.
var ejectLeads = []string{
	"review finding(s) (threads or review bodies) on Queued member #",
	"review-thread finding(s) on Queued member #",
	"unprocessed human comment(s) on Queued member #",
}

// MemberEjected reports whether line is a queued-review-settle eject/flag line
// for member. The trailing space after the number keeps #12 from matching #123.
func MemberEjected(line string, member int) bool {
	if !strings.Contains(line, "queued-review-settle") && !strings.Contains(line, "Queued member #") {
		return false
	}
	for _, lead := range ejectLeads {
		if strings.Contains(line, fmt.Sprintf("%s%d ", lead, member)) {
			return true
		}
	}
	return false
}

// Outcome is the verdict Decide reaches.
type Outcome int

const (
	// Pending: nothing decisive in the log yet.
	Pending Outcome = iota
	// Landed: a landing line the scenario accepts appeared first.
	Landed
	// NotBatched: the batch will not form as seeded (a singleton landing, or a
	// seeded member ejected for reviewer feedback) — the scenario is vacuous.
	NotBatched
)

// Opts scope Decide.
type Opts struct {
	// TrainKey scopes landing lines; empty matches any key.
	TrainKey string
	// Members are the scenario's seeded issue numbers; an eject line naming one
	// of them is a split.
	Members []int
	// SingletonSplits makes a singleton landing line a split (NotBatched). When
	// false (a scenario on a shared partition, where another test's singleton
	// landing is possible) any landing line is Landed and only an eject splits.
	SingletonSplits bool
}

// Decide scans lines in order and returns the first decisive outcome plus the
// line that decided it. A batch landing seen before an eject wins: once the
// batch has landed, a later review has no effect.
func Decide(lines []string, o Opts) (Outcome, string) {
	for _, l := range lines {
		if ld, ok := ParseLanding(l, o.TrainKey); ok {
			if ld.Kind == Singleton && o.SingletonSplits {
				return NotBatched, l
			}
			return Landed, l
		}
		for _, n := range o.Members {
			if MemberEjected(l, n) {
				return NotBatched, l
			}
		}
	}
	return Pending, ""
}
