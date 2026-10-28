package trainlog

import "testing"

// Verbatim engine lines (with the log prefix) — see the package comment.
const (
	batch2      = `[merge-train] landing complete for acme/alpha:e2e-cold (integration PR #321, 2 members)`
	singleton   = `[merge-train] landing complete for acme/alpha:e2e-cold (singleton fast path, PR #322, 1 member)`
	ejectOwned  = `[#1278 queued-review-settle] 1 unprocessed review finding(s) (threads or review bodies) on Queued member #1278 — owned by the live batch for acme/alpha:e2e-cold, flagging pending eject`
	ejectDirect = `[#1278 queued-review-settle] 1 unprocessed review finding(s) (threads or review bodies) on Queued member #1278 — not owned by any live batch for acme/alpha:e2e-cold, ejecting directly`
	ejectHuman  = `[#1278 queued-review-settle] 2 unprocessed human comment(s) on Queued member #1278 — owned by the live batch for acme/alpha:e2e-cold, flagging pending eject`
	ejectOld    = `[#1278 queued-review-settle] 1 review-thread finding(s) on Queued member #1278 — ejecting directly`
)

const key = "acme/alpha:e2e-cold"

func TestParseLanding(t *testing.T) {
	cases := []struct {
		name, line, key string
		want            Landing
		ok              bool
	}{
		{"batch", batch2, key, Landing{Batch, 321, 2}, true},
		{"singleton", singleton, key, Landing{Singleton, 322, 1}, true},
		{"any key", singleton, "", Landing{Singleton, 322, 1}, true},
		{"other key", batch2, "acme/alpha", Landing{}, false},
		{"longer key", `landing complete for acme/alpha:e2e-cold-2 (singleton fast path, PR #3, 1 member)`, key, Landing{}, false},
		{"unrelated", `[poll] nothing to do`, key, Landing{}, false},
		{"no kind", `landing complete for acme/alpha:e2e-cold`, key, Landing{}, false},
	}
	for _, c := range cases {
		got, ok := ParseLanding(c.line, c.key)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%+v, %v), want (%+v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestMemberEjected(t *testing.T) {
	for _, l := range []string{ejectOwned, ejectDirect, ejectHuman, ejectOld} {
		if !MemberEjected(l, 1278) {
			t.Errorf("not recognised: %s", l)
		}
		if MemberEjected(l, 127) || MemberEjected(l, 12780) || MemberEjected(l, 1280) {
			t.Errorf("matched wrong member: %s", l)
		}
	}
	if MemberEjected(batch2, 1278) || MemberEjected(`[#1278 merge-train] something on member #1278 `, 1278) {
		t.Error("unrelated line matched")
	}
}

func TestDecide(t *testing.T) {
	split := Opts{TrainKey: key, Members: []int{1278, 1280}, SingletonSplits: true}
	shared := Opts{TrainKey: "", Members: []int{1278, 1280}}
	cases := []struct {
		name  string
		lines []string
		o     Opts
		want  Outcome
	}{
		{"empty", nil, split, Pending},
		{"unrelated", []string{"[poll] x"}, split, Pending},
		{"batch lands", []string{batch2}, split, Landed},
		{"singleton splits", []string{singleton}, split, NotBatched},
		{"singleton other key ignored", []string{`landing complete for acme/alpha (singleton fast path, PR #9, 1 member)`}, split, Pending},
		{"eject splits", []string{ejectOwned, singleton}, split, NotBatched},
		{"eject without singleton opt", []string{ejectDirect}, shared, NotBatched},
		{"batch before eject wins", []string{batch2, ejectOwned}, split, Landed},
		{"singleton before batch", []string{singleton, batch2}, split, NotBatched},
		{"shared: singleton counts as landed", []string{singleton}, shared, Landed},
		{"other member eject ignored", []string{ejectOwned}, Opts{TrainKey: key, Members: []int{5}}, Pending},
	}
	for _, c := range cases {
		got, line := Decide(c.lines, c.o)
		if got != c.want {
			t.Errorf("%s: got %v (%q), want %v", c.name, got, line, c.want)
		}
	}
}
