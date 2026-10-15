package pruefer

import (
	"sync"
	"testing"

	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

func TestDaemonRecordSkip_CountsOnlySavingsAndEmitsCumulativeTotals(t *testing.T) {
	var mu sync.Mutex
	var got []ptui.SkipEvent
	d := &Daemon{Emit: func(ev ptui.Event) {
		if s, ok := ev.(ptui.SkipEvent); ok {
			mu.Lock()
			got = append(got, s)
			mu.Unlock()
		}
	}}
	d.recordSkip(1, SkipPatchUnchanged)
	d.recordSkip(1, SkipPatchUnchanged)
	d.recordSkip(2, SkipTrialEquivalent)
	d.recordSkip(3, SkipCatchUp)
	d.recordSkip(4, SkipDraft)           // not a saving
	d.recordSkip(5, SkipAlreadyReviewed) // not a saving

	want := map[string]int{"patch_unchanged": 2, "trial_equivalent": 1, "catch_up": 1}
	counts := d.SkipCounts()
	if len(counts) != len(want) {
		t.Fatalf("SkipCounts() = %v, want %v", counts, want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("SkipCounts()[%s] = %d, want %d", k, counts[k], v)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 || got[0].Total != 1 || got[1].Total != 2 || got[1].Reason != "patch_unchanged" {
		t.Errorf("events = %+v, want 4 with cumulative totals 1,2 for patch_unchanged", got)
	}
}

func TestDaemonRecordSkip_ConcurrentCallsAreExact(t *testing.T) {
	d := &Daemon{}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				d.recordSkip(1, SkipPatchUnchanged)
			}
		}()
	}
	wg.Wait()
	if got := d.SkipCounts()["patch_unchanged"]; got != 400 {
		t.Errorf("count = %d, want 400", got)
	}
}
