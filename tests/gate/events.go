package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Event is one decoded `go test -json` (test2json) line.
type Event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// DecodeEvent decodes one line of the stream. Non-JSON lines — `go: downloading`
// progress, a raw panic dump, build errors merged in by 2>&1 — return ok=false
// and are skipped rather than aborting a report (jq: `fromjson? // empty`).
func DecodeEvent(line string) (Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return Event{}, false
	}
	var e Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return Event{}, false
	}
	return e, true
}

// ReadEvents decodes a whole stream, skipping non-JSON lines.
func ReadEvents(r io.Reader) ([]Event, error) {
	var out []Event
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadString('\n')
		if e, ok := DecodeEvent(line); ok {
			out = append(out, e)
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
	}
}

// topLevel is true for an event attributed to a top-level test. Subtests
// (names containing "/") are folded into their parent: per-test granularity is
// what an operator needs, not per-subtest.
func topLevel(e Event) bool { return e.Test != "" && !strings.Contains(e.Test, "/") }

// Classification is every top-level test bucketed by its LAST state-transition
// action. pass/fail/skip are completed; run/cont mean it was executing when the
// run was killed; pause means it never got a -parallel slot. A skip whose own
// message starts with the inconclusive marker (#1973) is bucketed as
// Inconclusive instead of Skip: the precondition the scenario needs never arose,
// which is neither a pass nor a failure and is never coverage.
type Classification struct {
	Pass, Fail, Skip []string
	Inconclusive     []string // skipped with the E2E-INCONCLUSIVE marker (#1973)
	Running          []string // was executing at kill time (last action run or cont)
	NeverStarted     []string // queued behind -parallel (last action pause)
}

// isInconclusiveSkip is THE predicate for the third outcome, shared by Classify
// and the ledger recorder so they can never disagree: a test's own output
// carries a "file.go:N:" log line whose message starts with the marker. It need
// not be the LAST such line — a cleanup may log after the skip — but a line that
// merely mentions the marker does not qualify, and it is only consulted for a
// test whose terminal action is skip.
func isInconclusiveSkip(testOutput string) bool {
	return inconclusiveMessage(testOutput) != ""
}

// Classify is run.sh's report_test_outcomes core. "output" events are excluded
// before taking each test's last action — every test emits output as its final
// events in practice (-v RUN/PAUSE/CONT lines, t.Log, and for the test that
// timed out the entire panic dump), so without the exclusion the last action
// would be "output" for nearly every test and the timed-out test itself would
// vanish from every bucket instead of showing up as still-running.
//
// Each top-level test's OWN output is gathered (capped like the recorder's, but never dropping a marker line) only
// to tell an inconclusive skip from an ordinary one; subtests fold into their
// parent, so a subtest's marker is never seen here.
func Classify(events []Event) Classification {
	last := map[string]string{}
	out := map[string]*testOutput{}
	for _, e := range events {
		if !topLevel(e) {
			continue
		}
		if e.Action == "output" {
			b := out[e.Test]
			if b == nil {
				b = &testOutput{}
				out[e.Test] = b
			}
			b.add(e.Output)
			continue
		}
		last[e.Test] = e.Action
	}
	var c Classification
	for t, a := range last {
		switch a {
		case "pass":
			c.Pass = append(c.Pass, t)
		case "fail":
			c.Fail = append(c.Fail, t)
		case "skip":
			if b := out[t]; b != nil && isInconclusiveSkip(b.String()) {
				c.Inconclusive = append(c.Inconclusive, t)
			} else {
				c.Skip = append(c.Skip, t)
			}
		case "run", "cont":
			c.Running = append(c.Running, t)
		case "pause":
			c.NeverStarted = append(c.NeverStarted, t)
		}
	}
	for _, s := range [][]string{c.Pass, c.Fail, c.Skip, c.Inconclusive, c.Running, c.NeverStarted} {
		sort.Strings(s)
	}
	return c
}

// Report renders the five-line completed/still-running/never-started breakdown
// exactly as the jq report did (including the trailing space after an empty
// list's colon). The inconclusive line (#1973) is added only when there is one,
// so a leg with none reads exactly as before.
func (c Classification) Report() string {
	inc := ""
	if len(c.Inconclusive) > 0 {
		inc = fmt.Sprintf("completed - inconclusive (%d): %s\n", len(c.Inconclusive), strings.Join(c.Inconclusive, ", "))
	}
	return fmt.Sprintf("completed - pass (%d): %s\n", len(c.Pass), strings.Join(c.Pass, ", ")) +
		fmt.Sprintf("completed - fail (%d): %s\n", len(c.Fail), strings.Join(c.Fail, ", ")) +
		fmt.Sprintf("completed - skip (%d): %s\n", len(c.Skip), strings.Join(c.Skip, ", ")) +
		inc +
		fmt.Sprintf("still running at kill time (%d): %s\n", len(c.Running), strings.Join(c.Running, ", ")) +
		fmt.Sprintf("never started - queued behind -parallel cap (%d): %s", len(c.NeverStarted), strings.Join(c.NeverStarted, ", "))
}

// Timing is one test's terminal result and wall-clock.
type Timing struct {
	Test    string
	Result  string
	Elapsed float64
}

func terminal(e Event) bool {
	return topLevel(e) && (e.Action == "pass" || e.Action == "fail" || e.Action == "skip")
}

// Timings is run.sh's report_test_timings core: each top-level test's terminal
// event (Elapsed is only meaningful there), slowest first; ties keep the tests'
// alphabetical order (jq's group_by sorts by test name and sort_by is stable).
func Timings(events []Event) []Timing {
	last := map[string]Timing{}
	for _, e := range events {
		if terminal(e) {
			last[e.Test] = Timing{Test: e.Test, Result: e.Action, Elapsed: e.Elapsed}
		}
	}
	out := make([]Timing, 0, len(last))
	for _, t := range last {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Test < out[j].Test })
	sort.SliceStable(out, func(i, j int) bool { return out[i].Elapsed > out[j].Elapsed })
	return out
}

// FormatTimings renders the table as `column -t -s TAB` did: columns padded to
// the widest cell plus two spaces, the last column unpadded. The elapsed column
// is jq's number-to-string followed by "s".
func FormatTimings(ts []Timing) string {
	rows := make([][3]string, len(ts))
	var w0, w1 int
	for i, t := range ts {
		rows[i] = [3]string{strconv.FormatFloat(t.Elapsed, 'f', -1, 64) + "s", t.Result, t.Test}
		if len(rows[i][0]) > w0 {
			w0 = len(rows[i][0])
		}
		if len(rows[i][1]) > w1 {
			w1 = len(rows[i][1])
		}
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %-*s  %s\n", w0, r[0], w1, r[1], r[2])
	}
	return b.String()
}

// LastCompletedTest is run.sh's last_completed_test_name: the most recently
// observed pass/fail/skip top-level test (stream order, not elapsed order), or
// "(none yet)". The stall detector names it when output goes quiet.
func LastCompletedTest(events []Event) string {
	name := "(none yet)"
	for _, e := range events {
		if terminal(e) {
			name = e.Test
		}
	}
	return name
}
