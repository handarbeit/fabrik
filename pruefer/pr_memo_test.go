package pruefer

import (
	"fmt"
	"sync"
	"testing"
)

var baseStamp = prStamp{UpdatedAt: "2026-09-29T10:00:00Z", OpGen: 1, RepoCfg: "abc"}

func TestPRMemo_SkipRequiresFullKeyEquality(t *testing.T) {
	m := newPRMemo()
	m.Record("O", "R", 7, "sha1", baseStamp)
	if !m.Skip("o", "r", 7, "sha1", baseStamp) {
		t.Fatal("identical key (case-insensitive owner/repo) should skip")
	}
	cases := map[string]func() bool{
		"head sha": func() bool { return m.Skip("o", "r", 7, "sha2", baseStamp) },
		"updated_at": func() bool {
			s := baseStamp
			s.UpdatedAt = "2026-09-29T10:00:01Z"
			return m.Skip("o", "r", 7, "sha1", s)
		},
		"op gen":    func() bool { s := baseStamp; s.OpGen = 2; return m.Skip("o", "r", 7, "sha1", s) },
		"repo cfg":  func() bool { s := baseStamp; s.RepoCfg = "def"; return m.Skip("o", "r", 7, "sha1", s) },
		"pr number": func() bool { return m.Skip("o", "r", 8, "sha1", baseStamp) },
		"repo":      func() bool { return m.Skip("o", "other", 7, "sha1", baseStamp) },
	}
	for name, skip := range cases {
		if skip() {
			t.Errorf("a differing %s must not skip", name)
		}
	}
}

func TestPRMemo_RefusesUnknownComponents(t *testing.T) {
	m := newPRMemo()
	for name, tc := range map[string]struct {
		sha string
		st  prStamp
	}{
		"empty head":       {"", baseStamp},
		"empty updated_at": {"sha", prStamp{OpGen: 1, RepoCfg: "abc"}},
		"empty repo cfg":   {"sha", prStamp{UpdatedAt: "t", OpGen: 1}},
	} {
		m.Record("o", "r", 1, tc.sha, tc.st)
		if m.Len() != 0 {
			t.Errorf("%s: Record stored an entry", name)
		}
	}
	// And a stored entry is never matched by an unusable probe.
	m.Record("o", "r", 1, "sha", baseStamp)
	if m.Skip("o", "r", 1, "sha", prStamp{}) || m.Skip("o", "r", 1, "", baseStamp) {
		t.Error("Skip honoured an unknown probe")
	}
}

func TestPRMemo_RecordUnusableForgetsExisting(t *testing.T) {
	m := newPRMemo()
	m.Record("o", "r", 1, "sha", baseStamp)
	m.Record("o", "r", 1, "sha", prStamp{})
	if m.Len() != 0 {
		t.Fatal("recording an unusable stamp must drop the older conclusive entry")
	}
}

func TestPRMemo_Forget(t *testing.T) {
	m := newPRMemo()
	m.Record("o", "r", 1, "sha", baseStamp)
	m.Forget("o", "r", 1)
	if m.Skip("o", "r", 1, "sha", baseStamp) {
		t.Fatal("Forget did not drop the entry")
	}
}

func TestPRMemo_PruneKeepsLiveAndOtherRepos(t *testing.T) {
	m := newPRMemo()
	m.Record("o", "r", 1, "s", baseStamp)
	m.Record("o", "r", 2, "s", baseStamp)
	m.Record("o", "other", 2, "s", baseStamp)
	m.Prune("o", "r", map[int]bool{1: true})
	if !m.Skip("o", "r", 1, "s", baseStamp) {
		t.Error("live PR was pruned")
	}
	if m.Skip("o", "r", 2, "s", baseStamp) {
		t.Error("closed PR was not pruned")
	}
	if !m.Skip("o", "other", 2, "s", baseStamp) {
		t.Error("Prune touched another repo")
	}
}

func TestPRMemo_RetainRepos(t *testing.T) {
	m := newPRMemo()
	m.Record("O", "a", 1, "s", baseStamp)
	m.Record("o", "b", 1, "s", baseStamp)
	m.RetainRepos(map[string]bool{"o/a": true})
	if !m.Skip("o", "a", 1, "s", baseStamp) || m.Skip("o", "b", 1, "s", baseStamp) {
		t.Fatal("RetainRepos kept the wrong set")
	}
}

func TestPRMemo_NilSafe(t *testing.T) {
	var m *prMemo
	m.Record("o", "r", 1, "s", baseStamp)
	m.Forget("o", "r", 1)
	m.Prune("o", "r", nil)
	m.RetainRepos(nil)
	if m.Skip("o", "r", 1, "s", baseStamp) || m.Len() != 0 {
		t.Fatal("nil memo must never skip")
	}
}

func TestPRMemo_ConcurrentUse(t *testing.T) {
	m := newPRMemo()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				repo := fmt.Sprintf("r%d", g%3)
				m.Record("o", repo, i%10, "s", baseStamp)
				m.Skip("o", repo, i%10, "s", baseStamp)
				if i%17 == 0 {
					m.Prune("o", repo, map[int]bool{1: true})
					m.RetainRepos(map[string]bool{"o/r0": true, "o/r1": true, "o/r2": true})
				}
				m.Forget("o", repo, i%7)
			}
		}(g)
	}
	wg.Wait()
}
