//go:build e2e

package e2e

import (
	"reflect"
	"testing"
)

// These are pure-function tests for the batch-cap scenario's log parsers. Their
// fixtures are the engine's own format strings (engine/poll.go, engine/merge_train.go)
// with the log prefix a real line carries, so an engine-side rewording that the
// parsers no longer match is caught here as well as by the live scenario.

const parseTestRepo = "acme/alpha"

func TestParseBatchSnapshot(t *testing.T) {
	tests := []struct {
		name string
		line string
		key  string
		want []int
		ok   bool
	}{
		{
			name: "five members",
			line: `2026-09-25 21:10:00 [merge-train] batch snapshot for acme/alpha: 5 item(s) — #11 "e2e merge-train member a (211000.123)", #12 "b", #13 "c", #14 "d", #15 "e"`,
			key:  parseTestRepo,
			want: []int{11, 12, 13, 14, 15},
			ok:   true,
		},
		{
			name: "digits and hash in title do not add members",
			line: `[merge-train] batch snapshot for acme/alpha: 2 item(s) — #6 "fix #99 and 12, done", #7 "x"`,
			key:  parseTestRepo,
			want: []int{6, 7},
			ok:   true,
		},
		{
			name: "escaped quote in title",
			line: `[merge-train] batch snapshot for acme/alpha: 1 item(s) — #6 "say \"hi\", #8 \"there\""`,
			key:  parseTestRepo,
			want: []int{6},
			ok:   true,
		},
		{
			name: "non-default base partition is not the default key",
			line: `[merge-train] batch snapshot for acme/alpha:release: 2 item(s) — #6 "x", #7 "y"`,
			key:  parseTestRepo,
			ok:   false,
		},
		{
			name: "other repo",
			line: `[merge-train] batch snapshot for acme/beta: 1 item(s) — #6 "x"`,
			key:  parseTestRepo,
			ok:   false,
		},
		{
			name: "declared count disagrees with members",
			line: `[merge-train] batch snapshot for acme/alpha: 3 item(s) — #6 "x", #7 "y"`,
			key:  parseTestRepo,
			ok:   false,
		},
		{
			name: "unrelated line",
			line: `[merge-train] batch capped for acme/alpha: 7 Queued item(s) exceed max_batch_size=5 — landing first 5 by entry order`,
			key:  parseTestRepo,
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseBatchSnapshot(tc.line, tc.key)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (members %v)", ok, tc.ok, got)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("members = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseBatchCapped(t *testing.T) {
	line := `[merge-train] batch capped for acme/alpha: 7 Queued item(s) exceed max_batch_size=5 — landing first 5 by entry order`
	q, m, ok := parseBatchCapped(line, parseTestRepo)
	if !ok || q != 7 || m != 5 {
		t.Fatalf("got (%d, %d, %v), want (7, 5, true)", q, m, ok)
	}
	if _, _, ok := parseBatchCapped(line, "acme/alpha:release"); ok {
		t.Fatal("matched a different trainKey")
	}
	// The cap and the "landing first N" figure are the same value in the engine;
	// a line where they differ is not one the engine emits.
	if _, _, ok := parseBatchCapped(`batch capped for acme/alpha: 7 Queued item(s) exceed max_batch_size=5 — landing first 4 by entry order`, parseTestRepo); ok {
		t.Fatal("accepted mismatched cap figures")
	}
	if _, _, ok := parseBatchCapped(`batch capped for acme/alpha:release: 7 Queued item(s) exceed max_batch_size=5 — landing first 5 by entry order`, parseTestRepo); ok {
		t.Fatal("default key matched a non-default partition line")
	}
}

func TestParseOpenedDraftCI(t *testing.T) {
	pr, n, ok := parseOpenedDraftCI(`[merge-train] opened draft CI PR #321 for acme/alpha (5 survivor(s))`, parseTestRepo)
	if !ok || pr != 321 || n != 5 {
		t.Fatalf("got (%d, %d, %v), want (321, 5, true)", pr, n, ok)
	}
	if _, _, ok := parseOpenedDraftCI(`opened draft CI PR #321 for acme/beta (5 survivor(s))`, parseTestRepo); ok {
		t.Fatal("matched another repo")
	}
	if _, _, ok := parseOpenedDraftCI(`opened draft CI PR #321 for acme/alpha-two (5 survivor(s))`, parseTestRepo); ok {
		t.Fatal("matched a repo sharing only a prefix")
	}
}

func TestParseMergedIntegration(t *testing.T) {
	pr, ok := parseMergedIntegration(`[merge-train] merged integration PR #321 for acme/alpha`, parseTestRepo)
	if !ok || pr != 321 {
		t.Fatalf("got (%d, %v), want (321, true)", pr, ok)
	}
	if _, ok := parseMergedIntegration(`merged integration PR #321 for acme/alpha-two`, parseTestRepo); ok {
		t.Fatal("matched a repo sharing only a prefix")
	}
	if _, ok := parseMergedIntegration(`merged singleton landing PR #321 for #12`, parseTestRepo); ok {
		t.Fatal("matched the singleton landing line")
	}
}

func TestParseLandingComplete(t *testing.T) {
	pr, n, ok := parseLandingComplete(`[merge-train] landing complete for acme/alpha (integration PR #321, 5 members)`, parseTestRepo)
	if !ok || pr != 321 || n != 5 {
		t.Fatalf("got (%d, %d, %v), want (321, 5, true)", pr, n, ok)
	}
	if _, _, ok := parseLandingComplete(`landing complete for acme/alpha (singleton fast path, PR #321, 1 member)`, parseTestRepo); ok {
		t.Fatal("matched the singleton fast path line")
	}
	if _, _, ok := parseLandingComplete(`landing complete for acme/alpha:release (integration PR #321, 2 members)`, parseTestRepo); ok {
		t.Fatal("default key matched a non-default partition line")
	}
}

func TestSameMemberSet(t *testing.T) {
	if !sameMemberSet([]int{3, 1, 2}, []int{1, 2, 3}) {
		t.Fatal("order must not matter")
	}
	if sameMemberSet([]int{1, 2}, []int{1, 2, 3}) || sameMemberSet([]int{1, 2, 4}, []int{1, 2, 3}) {
		t.Fatal("different sets compared equal")
	}
}

func TestUniqueMemberPathBatchCap(t *testing.T) {
	if got := uniqueMemberPath("e2e/train/batchcap/m1.txt", 42); got != "e2e/train/batchcap/m1-42.txt" {
		t.Fatalf("got %q", got)
	}
}
