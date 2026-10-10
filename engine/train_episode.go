package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// trainEpisode is the per-run state of one merge-train worker (one (repo,base)
// partition, #2050, ADR 2050): the live membership the TUI row title shows, the
// current phase and when it was entered, and the outcome facts that become the
// single History entry when the worker ends.
//
// Sites only RECORD facts here — they never emit. The one completion event is
// emitted by runMergeTrainWorker's deferred func from resolve(), which runs on
// every exit path, so "exactly one History entry per episode" holds by
// construction. All methods are nil-receiver safe so helpers that run without
// an episode (tests calling land*/catch-up paths directly) need no guards, and
// every method takes the episode's own small lock, which is never held while an
// event is emitted.
type trainEpisode struct {
	mu            sync.Mutex
	partitionBase string
	original      int // dispatched batch size (the "M" in "N of M")

	active   []int // issue numbers the train currently holds, in batch order
	ejected  []trainEpisodeNote
	deferred []trainEpisodeNote

	phase          string
	phaseStartedAt time.Time

	// Outcome facts.
	landed       []int
	landedPR     int // integration / singleton / member PR that landed them (0 = unknown)
	poisoner     int
	oneAtATime   bool
	abandonCause string
	dissolved    bool // the train ran to completion with nothing left to land
}

// trainEpisodeNote is one member that left the train, with a short reason.
type trainEpisodeNote struct {
	num        int
	reason     string
	atAssembly bool // ejected while forming/assembling the trial, before any CI wait
}

// Outcome vocabulary of a History entry (docs/state-machine.md §7.17.2).
const (
	trainOutcomeLanded       = "landed"
	trainOutcomeBisected     = "red → bisected"
	trainOutcomeEjectedAsm   = "ejected at assembly"
	trainOutcomeEjected      = "ejected"
	trainOutcomeOneAtATime   = "one-at-a-time"
	trainOutcomeAbandoned    = "abandoned"
	trainOutcomeDissolved    = "dissolved"
	trainOutcomeNothingLands = "ended — nothing landed"
)

// maxTitleNotes bounds how many ejected / deferred members the live title
// names before collapsing the rest to "+k".
const maxTitleNotes = 3

func newTrainEpisode(partitionBase string, nums []int) *trainEpisode {
	return &trainEpisode{
		partitionBase: partitionBase,
		original:      len(nums),
		active:        append([]int(nil), nums...),
	}
}

func issueRefs(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, " ")
}

func noteRefs(notes []trainEpisodeNote) string {
	nums := make([]int, 0, len(notes))
	for _, n := range notes {
		nums = append(nums, n.num)
	}
	if len(nums) > maxTitleNotes {
		return issueRefs(nums[:maxTitleNotes]) + fmt.Sprintf(" +%d", len(nums)-maxTitleNotes)
	}
	return issueRefs(nums)
}

// titleLocked renders the live row title, e.g.
// "3 of 5: #1555 #1562 #1576 (ejected #1549 #1560)", with a "[base] " prefix
// for a non-default partition (the shared row names its partition).
func (ep *trainEpisode) titleLocked() string {
	t := fmt.Sprintf("%d of %d", len(ep.active), ep.original)
	if len(ep.active) > 0 {
		t += ": " + issueRefs(ep.active)
	}
	if len(ep.ejected) > 0 {
		t += " (ejected " + noteRefs(ep.ejected) + ")"
	}
	if len(ep.deferred) > 0 {
		t += " (deferred " + noteRefs(ep.deferred) + ")"
	}
	if ep.partitionBase != defaultPartitionBase {
		t = fmt.Sprintf("[%s] %s", ep.partitionBase, t)
	}
	return t
}

func (ep *trainEpisode) title() string {
	if ep == nil {
		return ""
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.titleLocked()
}

// setActive replaces the held-member list (a phase that holds a definite set).
func (ep *trainEpisode) setActive(members []trainMember) {
	if ep == nil || members == nil {
		return
	}
	nums := make([]int, len(members))
	for i, m := range members {
		nums[i] = m.item.Number
	}
	ep.mu.Lock()
	ep.active = nums
	ep.mu.Unlock()
}

func removeNum(nums []int, n int) []int {
	out := nums[:0:0]
	for _, v := range nums {
		if v != n {
			out = append(out, v)
		}
	}
	return out
}

func hasNote(notes []trainEpisodeNote, n int) bool {
	for _, v := range notes {
		if v.num == n {
			return true
		}
	}
	return false
}

// recordEjected / recordDeferred record the fact and report whether it was new.
func (ep *trainEpisode) recordEjected(num int, reason string, atAssembly bool) bool {
	if ep == nil {
		return false
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if hasNote(ep.ejected, num) {
		return false
	}
	ep.active = removeNum(ep.active, num)
	ep.ejected = append(ep.ejected, trainEpisodeNote{num: num, reason: reason, atAssembly: atAssembly})
	return true
}

func (ep *trainEpisode) recordDeferred(num int, reason string) bool {
	if ep == nil {
		return false
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if hasNote(ep.deferred, num) {
		return false
	}
	ep.active = removeNum(ep.active, num)
	ep.deferred = append(ep.deferred, trainEpisodeNote{num: num, reason: reason})
	return true
}

func (ep *trainEpisode) noteLanded(num, prNum int) {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	for _, n := range ep.landed {
		if n == num {
			return
		}
	}
	ep.landed = append(ep.landed, num)
	if prNum != 0 {
		ep.landedPR = prNum
	}
}

func (ep *trainEpisode) notePoisoner(num int) {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	ep.poisoner = num
	ep.mu.Unlock()
}

func (ep *trainEpisode) noteOneAtATime() {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	ep.oneAtATime = true
	ep.mu.Unlock()
}

// noteAbandoned records why the episode gave up. First writer wins: the
// earliest cause is the root one.
func (ep *trainEpisode) noteAbandoned(cause string) {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	if ep.abandonCause == "" {
		ep.abandonCause = cause
	}
	ep.mu.Unlock()
}

func (ep *trainEpisode) noteDissolved() {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	ep.dissolved = true
	ep.mu.Unlock()
}

// nothingRecorded reports whether no outcome fact has been recorded yet.
func (ep *trainEpisode) nothingRecorded() bool {
	if ep == nil {
		return true
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return len(ep.landed) == 0 && ep.poisoner == 0 && !ep.oneAtATime &&
		ep.abandonCause == "" && !ep.dissolved && len(ep.ejected) == 0
}

// trainOutcome is the resolved terminal result of one episode.
type trainOutcome struct {
	Outcome string
	Detail  string
	Success bool
}

// resolve folds the recorded facts into the single History outcome. Fixed
// precedence (ADR 2050): abandoned (nothing landed and a fault was recorded) >
// one-at-a-time > red → bisected > landed > ejected at assembly / ejected >
// dissolved > the catch-all. Ejections and deferrals are always appended to the
// detail. Success is true for every outcome in which the train did its job and
// false for abandoned and the catch-all.
func (ep *trainEpisode) resolve() trainOutcome {
	if ep == nil {
		return trainOutcome{Outcome: trainOutcomeNothingLands}
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()

	var parts []string
	landedDetail := func() string {
		s := issueRefs(ep.landed)
		if ep.landedPR != 0 {
			s += fmt.Sprintf(" via PR #%d", ep.landedPR)
		}
		return s
	}

	var out trainOutcome
	switch {
	case len(ep.landed) == 0 && ep.abandonCause != "":
		out = trainOutcome{Outcome: trainOutcomeAbandoned, Success: false}
		parts = append(parts, ep.abandonCause)
	case ep.oneAtATime:
		out = trainOutcome{Outcome: trainOutcomeOneAtATime, Success: true}
		if len(ep.landed) > 0 {
			parts = append(parts, "landed "+landedDetail())
		}
		if ep.abandonCause != "" {
			parts = append(parts, ep.abandonCause)
		}
	case ep.poisoner != 0:
		out = trainOutcome{Outcome: trainOutcomeBisected, Success: true}
		parts = append(parts, fmt.Sprintf("poisoner #%d ejected", ep.poisoner))
		if len(ep.landed) > 0 {
			parts = append(parts, "landed "+landedDetail())
		}
	case len(ep.landed) > 0:
		out = trainOutcome{Outcome: trainOutcomeLanded, Success: true}
		parts = append(parts, landedDetail())
	case len(ep.ejected) > 0:
		name := trainOutcomeEjected
		for _, n := range ep.ejected {
			if n.atAssembly {
				name = trainOutcomeEjectedAsm
				break
			}
		}
		out = trainOutcome{Outcome: name, Success: true}
	case ep.dissolved:
		out = trainOutcome{Outcome: trainOutcomeDissolved, Success: true}
		parts = append(parts, "nothing to land")
	default:
		out = trainOutcome{Outcome: trainOutcomeNothingLands, Success: false}
	}

	// Ejections that are not already the headline poisoner, and deferrals.
	var ej []string
	for _, n := range ep.ejected {
		if n.num == ep.poisoner {
			continue
		}
		ej = append(ej, fmt.Sprintf("#%d (%s)", n.num, n.reason))
	}
	if len(ej) > 0 {
		parts = append(parts, "ejected "+strings.Join(ej, ", "))
	}
	var df []string
	for _, n := range ep.deferred {
		df = append(df, fmt.Sprintf("#%d (%s)", n.num, n.reason))
	}
	if len(df) > 0 {
		parts = append(parts, "deferred "+strings.Join(df, ", "))
	}
	out.Detail = strings.Join(parts, "; ")
	return out
}

// currentPhaseLabel returns the TUI label of the phase the train is in now.
func (ep *trainEpisode) currentPhaseLabel() string {
	if ep == nil {
		return ""
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.phase
}
