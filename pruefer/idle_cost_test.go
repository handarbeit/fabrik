package pruefer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// R4 (#1952): idle polling cost, measured. A synthetic GitHub (httptest) with
// R repos × P open PRs — every PR already reviewed by the bot at its head and
// untouched — is polled by a real Daemon through a real *github.Client, once
// cold, once idle, and once with a single PR receiving a new (non-command)
// comment. Three configurations are compared:
//
//	baseline  conditional requests off, no memo   (pre-#1952 behavior)
//	cond      conditional requests on,  no memo   (R2 alone)
//	memo+cond conditional requests on,  memo on   (R1 + R2, the shipped default)
//
// "billed" is requests minus 304s: GitHub does not count a 304 against the
// primary rate limit. Run with -v to see the table. The server-side count is
// asserted equal to the client-side RequestStats so the numbers cannot drift
// from what actually crossed the wire.

type synthGitHub struct {
	mu       sync.Mutex
	repos    int
	prs      int
	updated  map[string]string // "repo#n" → updated_at
	comments map[string]string // "repo#n" → extra comment body ("" = none)
	total    int
	notMod   int
}

func newSynthGitHub(repos, prs int) *synthGitHub {
	return &synthGitHub{repos: repos, prs: prs, updated: map[string]string{}, comments: map[string]string{}}
}

func (s *synthGitHub) head(repo string, n int) string { return fmt.Sprintf("sha-%s-%d", repo, n) }

func (s *synthGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	path := r.URL.Path
	var body any
	switch {
	case strings.HasSuffix(path, "/contents/.pruefer/config.yaml"):
		w.WriteHeader(404) // the common case: no repo config; a 404 carries no ETag
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	case strings.HasSuffix(path, "/pulls"):
		repo := strings.Split(path, "/")[3]
		var list []map[string]any
		for n := 1; n <= s.prs; n++ {
			key := fmt.Sprintf("%s#%d", repo, n)
			upd := s.updated[key]
			if upd == "" {
				upd = "2026-09-29T10:00:00Z"
			}
			list = append(list, map[string]any{
				"number": n, "title": "t", "state": "open", "updated_at": upd,
				"user": map[string]any{"login": "alice"},
				"head": map[string]any{"sha": s.head(repo, n), "ref": "feat"},
				"base": map[string]any{"ref": "main"},
			})
		}
		body = list
	case strings.HasSuffix(path, "/comments"):
		parts := strings.Split(path, "/")
		key := fmt.Sprintf("%s#%s", parts[3], parts[5])
		list := []map[string]any{}
		if c := s.comments[key]; c != "" {
			list = append(list, map[string]any{"id": 1, "body": c, "user": map[string]any{"login": "bob"}, "created_at": "2026-09-29T11:00:00Z"})
		}
		body = list
	case strings.HasSuffix(path, "/reviews"):
		parts := strings.Split(path, "/")
		var n int
		fmt.Sscanf(parts[5], "%d", &n)
		body = []map[string]any{{
			"id": 9, "state": "COMMENTED", "commit_id": s.head(parts[3], n), "submitted_at": "2026-09-29T10:30:00Z",
			"user": map[string]any{"login": "pruefer-bot[bot]"},
		}}
	default:
		w.WriteHeader(404)
		return
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		s.notMod++
		w.WriteHeader(304)
		return
	}
	w.Write(raw)
}

func (s *synthGitHub) snapshot() (total, notMod int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.notMod
}

type costRow struct{ requests, notModified int }

func (r costRow) billed() int { return r.requests - r.notModified }

type costScenario struct {
	cold, idle, oneChanged costRow
}

func measureIdleCost(t *testing.T, repos, prs int, conditional, memo bool) costScenario {
	t.Helper()
	synth := newSynthGitHub(repos, prs)
	srv := httptest.NewServer(synth)
	defer srv.Close()

	client := gh.NewClientWithBaseURL("tok", srv.URL)
	if conditional {
		client.EnableConditionalRequests()
	}
	var names []string
	for i := 0; i < repos; i++ {
		names = append(names, fmt.Sprintf("owner/r%d", i))
	}
	claude := okClaude()
	clone, _ := fakeClone(t, nil)
	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{ConcurrencyCap: 4},
		BotLogin: "pruefer-bot[bot]",
		Tracker:  NewReviewTracker(),
	}, names...)
	if memo {
		d.memo = newPRMemo()
	}

	poll := func() costRow {
		t0, n0 := synth.snapshot()
		c0 := client.RequestStats()
		d.poll(context.Background())
		t1, n1 := synth.snapshot()
		c1 := client.RequestStats()
		row := costRow{t1 - t0, n1 - n0}
		if got := (costRow{int(c1.Total - c0.Total), int(c1.NotModified - c0.NotModified)}); got != row {
			t.Fatalf("client-side RequestStats %+v disagree with the server's count %+v", got, row)
		}
		return row
	}
	var out costScenario
	out.cold = poll()
	out.idle = poll()
	synth.mu.Lock()
	synth.comments["r0#1"] = "thanks for the review"
	synth.updated["r0#1"] = "2026-09-29T12:00:00Z"
	synth.mu.Unlock()
	out.oneChanged = poll()
	if claude.callCount() != 0 || len(d.Clients) == 0 {
		t.Fatalf("setup: every PR is already reviewed, but claude ran %d times", claude.callCount())
	}
	return out
}

func TestIdlePollCost_ScalesWithChangesNotOpenPRs(t *testing.T) {
	const repos = 11 // the verveguy installation from #1952
	type cfg struct {
		name              string
		conditional, memo bool
	}
	configs := []cfg{{"baseline", false, false}, {"cond", true, false}, {"memo+cond", true, true}}

	t.Logf("%-10s %4s | %-22s | %-22s | %-22s", "config", "PRs", "cold req/304/billed", "idle req/304/billed", "1-changed req/304/billed")
	results := map[string]map[int]costScenario{}
	for _, prs := range []int{5, 20} {
		for _, c := range configs {
			r := measureIdleCost(t, repos, prs, c.conditional, c.memo)
			if results[c.name] == nil {
				results[c.name] = map[int]costScenario{}
			}
			results[c.name][prs] = r
			f := func(x costRow) string { return fmt.Sprintf("%4d/%4d/%4d", x.requests, x.notModified, x.billed()) }
			t.Logf("%-10s %4d | %-22s | %-22s | %-22s", c.name, repos*prs, f(r.cold), f(r.idle), f(r.oneChanged))
		}
	}

	// Baseline: idle cost is one list per repo plus three reads per PR
	// (repo config, comments, reviews).
	for _, prs := range []int{5, 20} {
		want := repos + repos*prs*3
		if got := results["baseline"][prs].idle.requests; got != want {
			t.Errorf("baseline idle requests (%d PRs/repo) = %d, want %d", prs, got, want)
		}
	}
	// Shipped default: idle billed cost is per repo, independent of the number
	// of open PRs — quadrupling PRs must not move it — and far below baseline.
	small, large := results["memo+cond"][5].idle, results["memo+cond"][20].idle
	if small.requests != large.requests || small.billed() != large.billed() {
		t.Errorf("memo+cond idle cost depends on open-PR count: %+v (5/repo) vs %+v (20/repo)", small, large)
	}
	if want := repos * 2; large.requests != want { // list + config fingerprint per repo
		t.Errorf("memo+cond idle requests = %d, want %d (one list + one config read per repo)", large.requests, want)
	}
	if base := results["baseline"][20].idle.billed(); large.billed()*5 > base {
		t.Errorf("memo+cond idle billed %d is not at least 5x below baseline %d", large.billed(), base)
	}
	// One changed PR costs a bounded, PR-count-independent handful of extra
	// requests over idle: its own re-evaluation (config re-read is a 304 or a
	// 404 floor, comments, reviews) plus the changed list page.
	extraSmall := results["memo+cond"][5].oneChanged.billed() - small.billed()
	extraLarge := results["memo+cond"][20].oneChanged.billed() - large.billed()
	if extraSmall != extraLarge {
		t.Errorf("cost of one changed PR depends on open-PR count: +%d vs +%d billed", extraSmall, extraLarge)
	}
	// R2 alone already makes the repeated reads free against the rate limit.
	if condIdle, baseIdle := results["cond"][20].idle, results["baseline"][20].idle; condIdle.billed() >= baseIdle.billed() {
		t.Errorf("conditional requests alone did not reduce billed idle cost: %d vs %d", condIdle.billed(), baseIdle.billed())
	}
}
