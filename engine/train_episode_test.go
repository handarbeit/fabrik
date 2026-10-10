package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTrainEpisode_Title(t *testing.T) {
	ep := newTrainEpisode(defaultPartitionBase, []int{1549, 1555, 1560, 1562, 1576})
	if got, want := ep.title(), "5 of 5: #1549 #1555 #1560 #1562 #1576"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	ep.recordEjected(1549, "conflict", true)
	ep.recordEjected(1560, "conflict", true)
	if got, want := ep.title(), "3 of 5: #1555 #1562 #1576 (ejected #1549 #1560)"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	ep.recordDeferred(1562, "file overlap")
	if got, want := ep.title(), "2 of 5: #1555 #1576 (ejected #1549 #1560) (deferred #1562)"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	// Recording the same member twice changes nothing and reports no change.
	if ep.recordEjected(1549, "again", true) {
		t.Error("duplicate ejection reported as new")
	}
}

func TestTrainEpisode_TitleCapsAndPartitionPrefix(t *testing.T) {
	ep := newTrainEpisode("release/1.x", []int{1, 2, 3, 4, 5, 6})
	for _, n := range []int{1, 2, 3, 4, 5} {
		ep.recordEjected(n, "x", false)
	}
	if got, want := ep.title(), "[release/1.x] 1 of 6: #6 (ejected #1 #2 #3 +2)"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	ep.recordEjected(6, "x", false)
	if got := ep.title(); !strings.HasPrefix(got, "[release/1.x] 0 of 6 ") {
		t.Fatalf("empty-membership title = %q", got)
	}
}

func TestTrainEpisode_NilSafe(t *testing.T) {
	var ep *trainEpisode
	ep.noteLanded(1, 2)
	ep.noteAbandoned("x")
	ep.noteDissolved()
	ep.notePoisoner(3)
	ep.noteOneAtATime()
	ep.setActive(nil)
	if ep.recordEjected(1, "x", true) || ep.recordDeferred(1, "x") {
		t.Error("nil episode recorded something")
	}
	if got := ep.resolve(); got.Outcome != trainOutcomeNothingLands || got.Success {
		t.Errorf("nil resolve = %+v", got)
	}
	if ep.title() != "" || ep.currentPhaseLabel() != "" || !ep.landedNothing() {
		t.Error("nil accessors not neutral")
	}
}

func TestTrainEpisode_ResolvePrecedenceAndSuccess(t *testing.T) {
	type build func(ep *trainEpisode)
	cases := []struct {
		name        string
		fill        build
		wantOutcome string
		wantSuccess bool
		wantDetail  []string
	}{
		{"landed", func(ep *trainEpisode) { ep.noteLanded(1, 50); ep.noteLanded(2, 50) },
			"landed", true, []string{"#1 #2 via PR #50"}},
		{"landed lists ejections", func(ep *trainEpisode) {
			ep.noteLanded(1, 50)
			ep.recordEjected(2, "unresolvable conflict", true)
		}, "landed", true, []string{"ejected #2 (unresolvable conflict)"}},
		{"bisected beats landed", func(ep *trainEpisode) {
			ep.notePoisoner(2)
			ep.recordEjected(2, "poisoner", false)
			ep.noteLanded(1, 50)
		}, "red → bisected", true, []string{"poisoner #2 ejected", "landed #1 via PR #50"}},
		{"one-at-a-time beats bisected", func(ep *trainEpisode) {
			ep.notePoisoner(2)
			ep.noteOneAtATime()
			ep.noteLanded(1, 51)
		}, "one-at-a-time", true, []string{"landed #1"}},
		{"ejected at assembly", func(ep *trainEpisode) { ep.recordEjected(3, "no usable linked PR", true) },
			"ejected at assembly", true, []string{"#3 (no usable linked PR)"}},
		{"ejected later is not at assembly", func(ep *trainEpisode) { ep.recordEjected(3, "review feedback", false) },
			"ejected", true, []string{"#3 (review feedback)"}},
		{"abandoned when nothing landed", func(ep *trainEpisode) { ep.noteAbandoned("CI never started") },
			"abandoned", false, []string{"CI never started"}},
		{"abandoned: first cause wins", func(ep *trainEpisode) { ep.noteAbandoned("first"); ep.noteAbandoned("second") },
			"abandoned", false, []string{"first"}},
		{"abandoned loses to a landing", func(ep *trainEpisode) { ep.noteLanded(1, 9); ep.noteAbandoned("later fault") },
			"landed", true, nil},
		{"dissolved", func(ep *trainEpisode) { ep.noteDissolved(); ep.recordDeferred(4, "live status") },
			"dissolved", true, []string{"nothing to land", "deferred #4 (live status)"}},
		{"catch-all", func(ep *trainEpisode) {}, "ended — nothing landed", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := newTrainEpisode(defaultPartitionBase, []int{1, 2, 3, 4})
			tc.fill(ep)
			got := ep.resolve()
			if got.Outcome != tc.wantOutcome || got.Success != tc.wantSuccess {
				t.Fatalf("resolve = %+v, want outcome %q success %v", got, tc.wantOutcome, tc.wantSuccess)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail %q lacks %q", got.Detail, w)
				}
			}
		})
	}
}

// landedNothing is the guard for "gave up without landing": earlier ejections,
// a poisoner, one-at-a-time or a dissolve must not count as an outcome, only a
// landing or an already-recorded abandon cause does.
func TestTrainEpisode_LandedNothing(t *testing.T) {
	ep := newTrainEpisode(defaultPartitionBase, []int{1, 2, 3})
	ep.recordEjected(1, "unresolvable conflict", true)
	ep.notePoisoner(2)
	ep.noteOneAtATime()
	ep.noteDissolved()
	if !ep.landedNothing() {
		t.Fatal("earlier non-landing facts must leave landedNothing true")
	}
	ep.noteAbandoned("batch landing did not complete")
	if ep.landedNothing() {
		t.Error("an abandon cause is already recorded")
	}
	if got := ep.resolve(); got.Outcome != trainOutcomeAbandoned || got.Success {
		t.Errorf("resolve = %+v, want abandoned / unsuccessful", got)
	}

	landed := newTrainEpisode(defaultPartitionBase, []int{1})
	landed.noteLanded(1, 9)
	if landed.landedNothing() {
		t.Error("a landing was recorded")
	}
}

// R4/FR-008: every phase write in the merge-train sources goes through the one
// hook. A direct status-line phase write elsewhere would let the board and the
// TUI row drift (the static twin of the sequence-equality test).
func TestTrainPhaseSites_GoThroughTheHook(t *testing.T) {
	phaseBuilders := regexp.MustCompile(`statusLine(Trial|Bisect|Landing|CatchUp|Queued)\b`)
	files, err := filepath.Glob("merge_train*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if phaseBuilders.MatchString(line) {
				t.Errorf("%s:%d writes a phase status line directly (route it through noteTrainPhase): %s", f, i+1, trimmed)
			}
		}
	}
}
