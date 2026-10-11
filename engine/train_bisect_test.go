package engine

import (
	"math/rand"
	"reflect"
	"testing"
)

// refBisectRecursive is a verbatim-in-shape copy of the pre-#2051 recursive bisect, over
// plain integers, kept here as the oracle bisectState must equal (R4).
func refBisectRecursive(red []int, used *int, costCap int, trial func(half []int) (isRed bool, survivors []int), seq *[][]int) (poisoner int, fellBack bool) {
	if len(red) == 1 {
		return red[0], false
	}
	mid := len(red) / 2
	for _, half := range [][]int{red[:mid], red[mid:]} {
		if *used >= costCap {
			return 0, true
		}
		*seq = append(*seq, append([]int(nil), half...))
		isRed, survivors := trial(half)
		*used++
		if isRed && len(survivors) > 0 {
			return refBisectRecursive(survivors, used, costCap, trial, seq)
		}
	}
	return 0, true
}

func driveBisectState(b *bisectState, trial func(half []int) (bool, []int), seq *[][]int) (poisoner int, fellBack bool) {
	for {
		act, half := b.next()
		switch act {
		case bisectIsolated:
			return b.poisoner(), false
		case bisectFallbackCap, bisectFallbackSplit:
			return 0, true
		}
		*seq = append(*seq, append([]int(nil), half...))
		isRed, survivors := trial(half)
		b.spent()
		b.apply(isRed, survivors, nil)
	}
}

func TestBisectState_MatchesRecursiveReference(t *testing.T) {
	rng := rand.New(rand.NewSource(2051))
	for iter := 0; iter < 20000; iter++ {
		n := 2 + rng.Intn(9)
		red := make([]int, n)
		for i := range red {
			red[i] = i + 1
		}
		costCap := 1 + rng.Intn(9)
		// A random but deterministic-per-call verdict: each distinct half gets a stable
		// answer, and some halves "lose" members during assembly (survivors subset).
		answers := map[string]struct {
			red  bool
			drop int
		}{}
		trial := func(half []int) (bool, []int) {
			key := reflectKey(half)
			a, ok := answers[key]
			if !ok {
				a.red = rng.Intn(3) != 0
				if rng.Intn(5) == 0 && len(half) > 1 {
					a.drop = 1 + rng.Intn(len(half)-1)
				}
				answers[key] = a
			}
			return a.red, append([]int(nil), half[:len(half)-a.drop]...)
		}

		var refSeq, gotSeq [][]int
		refUsed := 1
		refP, refFB := refBisectRecursive(red, &refUsed, costCap, trial, &refSeq)

		b := newBisectState(red, nil, costCap)
		gotP, gotFB := driveBisectState(b, trial, &gotSeq)

		if refP != gotP || refFB != gotFB || !reflect.DeepEqual(refSeq, gotSeq) || refUsed != b.Used {
			t.Fatalf("iter %d n=%d cap=%d: reference (poisoner=%d fellBack=%v used=%d seq=%v) != state machine (poisoner=%d fellBack=%v used=%d seq=%v)",
				iter, n, costCap, refP, refFB, refUsed, refSeq, gotP, gotFB, b.Used, gotSeq)
		}
	}
}

func reflectKey(half []int) string {
	b := make([]byte, 0, len(half)*3)
	for _, v := range half {
		b = append(b, byte(v), ',')
	}
	return string(b)
}

func TestBisectState_BorsOrderAndCap(t *testing.T) {
	// 4 members, poisoner #3: A={1,2} green, B={3,4} red, then {3} (len 1 after halving {3,4}→{3},{4}):
	// {3} red → isolated. Sequence must be [1 2] [3 4] [3].
	var seq [][]int
	b := newBisectState([]int{1, 2, 3, 4}, nil, 99)
	p, fb := driveBisectState(b, func(half []int) (bool, []int) {
		for _, v := range half {
			if v == 3 {
				return true, half
			}
		}
		return false, half
	}, &seq)
	if p != 3 || fb {
		t.Fatalf("poisoner=%d fellBack=%v, want 3/false", p, fb)
	}
	want := [][]int{{1, 2}, {3, 4}, {3}}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("sequence = %v, want %v", seq, want)
	}
	if b.Used != 4 {
		t.Fatalf("used = %d, want 4 (initial red + 3 halves)", b.Used)
	}

	// Cost cap: the check is BEFORE each half, with Used starting at 1.
	b = newBisectState([]int{1, 2, 3, 4}, nil, 1)
	if act, _ := b.next(); act != bisectFallbackCap {
		t.Fatalf("cap 1 must fall back before any half, got %v", act)
	}
	b = newBisectState([]int{1, 2, 3, 4}, nil, 2)
	if act, half := b.next(); act != bisectTrial || !reflect.DeepEqual(half, []int{1, 2}) {
		t.Fatalf("cap 2 allows exactly one half, got %v %v", act, half)
	}
	b.spent()
	b.apply(false, nil, nil)
	if act, _ := b.next(); act != bisectFallbackCap {
		t.Fatalf("after one half the cap-2 episode must fall back, got %v", act)
	}
}

func TestBisectState_BothHalvesGreenIsSplitFallback(t *testing.T) {
	b := newBisectState([]int{1, 2}, nil, 9)
	for i := 0; i < 2; i++ {
		act, _ := b.next()
		if act != bisectTrial {
			t.Fatalf("half %d: act=%v", i, act)
		}
		b.spent()
		b.apply(false, nil, nil)
	}
	if act, _ := b.next(); act != bisectFallbackSplit {
		t.Fatalf("both halves green must report the split fallback, got %v", act)
	}
}
