package events

import (
	"fmt"
	"sync"
	"testing"
)

func TestDedupe_FreshIDNotSeen(t *testing.T) {
	d := NewDedupe(10)
	if d.SeenBefore("a") {
		t.Error("a fresh ID must not be reported as already seen")
	}
}

func TestDedupe_RepeatIDIsSeen(t *testing.T) {
	d := NewDedupe(10)
	if d.SeenBefore("a") {
		t.Fatal("first call for 'a' should be fresh")
	}
	if !d.SeenBefore("a") {
		t.Error("second call for 'a' should report already-seen")
	}
	if !d.SeenBefore("a") {
		t.Error("third call for 'a' should still report already-seen")
	}
}

func TestDedupe_EmptyIDNeverDeduped(t *testing.T) {
	d := NewDedupe(10)
	if d.SeenBefore("") {
		t.Error("empty ID should never be reported as seen (first call)")
	}
	if d.SeenBefore("") {
		t.Error("empty ID should never be reported as seen (second call)")
	}
}

func TestDedupe_EvictsOldestAtCapacity(t *testing.T) {
	d := NewDedupe(3)
	d.SeenBefore("1")
	d.SeenBefore("2")
	d.SeenBefore("3")
	// "4" pushes past capacity, evicting "1". Retained window is now [2,3,4].
	d.SeenBefore("4")

	// Hits (SeenBefore returning true) don't mutate order, so these three
	// checks can run in any order without disturbing each other.
	if !d.SeenBefore("2") {
		t.Error("expected '2' to still be within the retained window")
	}
	if !d.SeenBefore("3") {
		t.Error("expected '3' to still be within the retained window")
	}
	if !d.SeenBefore("4") {
		t.Error("expected '4' to still be within the retained window")
	}
	// "1" was evicted, so it must be reported as fresh (a miss).
	if d.SeenBefore("1") {
		t.Error("expected '1' to have been evicted and treated as fresh again")
	}
}

func TestDedupe_DefaultCapacityUsedWhenNonPositive(t *testing.T) {
	d := NewDedupe(0)
	if d.capacity != DefaultDedupeCapacity {
		t.Errorf("capacity = %d, want DefaultDedupeCapacity (%d)", d.capacity, DefaultDedupeCapacity)
	}
	d2 := NewDedupe(-5)
	if d2.capacity != DefaultDedupeCapacity {
		t.Errorf("capacity = %d, want DefaultDedupeCapacity (%d)", d2.capacity, DefaultDedupeCapacity)
	}
}

func TestDedupe_ConcurrentUseIsRaceFree(t *testing.T) {
	d := NewDedupe(1000)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.SeenBefore(fmt.Sprintf("id-%d", i%20))
		}(i)
	}
	wg.Wait()
}
