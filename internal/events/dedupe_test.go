package events

import (
	"fmt"
	"sync"
	"testing"
	"time"
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

func TestDedupe_SeenDoesNotRecord(t *testing.T) {
	d := NewDedupe(4)
	if d.Seen("a") {
		t.Fatal("Seen on empty ring = true")
	}
	if d.SeenBefore("a") {
		t.Fatal("Seen recorded the id: SeenBefore reported a duplicate")
	}
	if !d.Seen("a") {
		t.Fatal("Seen after SeenBefore = false")
	}
	if d.Seen("") {
		t.Fatal("empty id must never be seen")
	}
}

func TestDedupe_NewestAndReceivedAt(t *testing.T) {
	d := NewDedupe(2)
	base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	cur := base
	d.SetClockForTest(func() time.Time { return cur })
	if !d.Newest().IsZero() {
		t.Fatal("Newest on empty ring must be zero")
	}
	d.SeenBefore("a")
	cur = base.Add(time.Minute)
	d.SeenBefore("b")
	if got := d.Newest(); !got.Equal(base.Add(time.Minute)) {
		t.Errorf("Newest = %v", got)
	}
	if got, ok := d.ReceivedAt("a", cur); !ok || !got.Equal(base) {
		t.Errorf("ReceivedAt(a) = %v,%v", got, ok)
	}
	// A repeat lookup does not move the record time.
	d.SeenBefore("a")
	if got, _ := d.ReceivedAt("a", cur); !got.Equal(base) {
		t.Errorf("repeat SeenBefore moved ReceivedAt to %v", got)
	}
	// Eviction keeps map and ring consistent.
	cur = base.Add(2 * time.Minute)
	d.SeenBefore("c") // evicts a
	if _, ok := d.ReceivedAt("a", cur); ok {
		t.Error("evicted id still has a ReceivedAt")
	}
	if got := d.Newest(); !got.Equal(cur) {
		t.Errorf("Newest after eviction = %v", got)
	}
	if _, ok := d.ReceivedAt("", cur); ok {
		t.Error("empty id has a ReceivedAt")
	}
}
