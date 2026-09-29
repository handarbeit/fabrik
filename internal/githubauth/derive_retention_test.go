package githubauth

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Tests for #1951: a failed installation repo listing must not be treated as
// an empty grant (R1), and a rate-limited one must not be retried before the
// reported reset (R3).

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var retentionEpoch = time.Date(2026, 9, 29, 17, 33, 37, 0, time.UTC)

// rateLimit403 is the response GitHub sent in the incident: a primary-limit
// 403 carrying x-ratelimit-remaining: 0 and a reset.
func rateLimit403(reset time.Time) listOverride {
	return listOverride{
		status: 403,
		headers: map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     fmt.Sprint(reset.Unix()),
		},
		body: `{"message":"API rate limit exceeded for installation ID 111."}`,
	}
}

type retentionRig struct {
	r     *Reconciler
	fake  *fakeAppServer
	clock *fakeClock
	logs  func() []string
	logf  func(string, ...any)
}

// newRetentionRig builds a Reconciler over a fake App server. setup runs
// before Reconcile so a test can make the very first derivation fail.
func newRetentionRig(t *testing.T, installs []gh.AppInstallation, repos map[int64][]string, setup func(*fakeAppServer)) *retentionRig {
	t.Helper()
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	t.Cleanup(func() { runManifestFlow = oldFlow })

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", installs, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = repos
	t.Cleanup(srv.Close)
	if setup != nil {
		setup(fake)
	}

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	clock := &fakeClock{t: retentionEpoch}
	r.mu.Lock()
	r.nowFn = clock.Now
	r.mu.Unlock()
	return &retentionRig{r: r, fake: fake, clock: clock, logs: lines, logf: logf}
}

func (g *retentionRig) derive(t *testing.T, filter []string) DerivedRepoSet {
	t.Helper()
	set, _, err := g.r.Derive(context.Background(), filter, 0, g.logf)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return set
}

func repoNames(set DerivedRepoSet) []string {
	out := make([]string, len(set.Repos))
	for i, r := range set.Repos {
		out[i] = r.Repo
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func inst(set DerivedRepoSet, id int64) DerivedInstallation {
	for _, i := range set.Installations {
		if i.InstallationID == id {
			return i
		}
	}
	return DerivedInstallation{}
}

var oneInstall = []gh.AppInstallation{{ID: 111, Account: "verveguy", RepositorySelection: "all"}}

func threeRepos() map[int64][]string {
	return map[int64][]string{111: {"verveguy/a", "verveguy/b", "verveguy/c"}}
}

// AC1: a rate-limited listing leaves the derived set unchanged, marked stale,
// and the log names the reset time.
func TestDerive_RateLimitedListing_RetainsLastGood(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	want := repoNames(g.derive(t, nil))
	if len(want) != 3 {
		t.Fatalf("setup: %v", want)
	}

	reset := g.clock.Now().Add(10 * time.Minute)
	g.fake.setListOverride(111, rateLimit403(reset))
	set := g.derive(t, nil)

	if got := repoNames(set); !sameStrings(got, want) {
		t.Fatalf("repos = %v, want the retained %v", got, want)
	}
	in := inst(set, 111)
	if !in.Stale || in.RepoCount != 3 || in.RepoListError == "" {
		t.Errorf("installation = %+v, want Stale, RepoCount 3, RepoListError set", in)
	}
	if !in.RetryNotBefore.Equal(reset) {
		t.Errorf("RetryNotBefore = %v, want %v", in.RetryNotBefore, reset)
	}
	logged := strings.Join(g.logs(), "\n")
	if !strings.Contains(logged, "STALE") || !strings.Contains(logged, reset.UTC().Format(time.RFC3339)) {
		t.Errorf("log must say the set is stale and name the reset %s; got:\n%s", reset.UTC().Format(time.RFC3339), logged)
	}
}

// AC2: only a successful listing may shrink the set.
func TestDerive_SuccessfulListingShrinksSet(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)

	g.fake.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`})
	if got := len(g.derive(t, nil).Repos); got != 3 {
		t.Fatalf("failed listing changed the set to %d repos", got)
	}
	g.fake.clearListOverride(111)
	g.fake.selectedRepos = map[int64][]string{111: {"verveguy/a"}}
	set := g.derive(t, nil)
	if got := repoNames(set); !sameStrings(got, []string{"verveguy/a"}) {
		t.Errorf("repos = %v, want the shrunk [verveguy/a]", got)
	}
	if in := inst(set, 111); in.Stale || in.RepoListError != "" {
		t.Errorf("a successful listing must not be stale: %+v", in)
	}
}

// AC5/R3: no listing call is made before the reported reset, one is made after.
func TestDerive_RateLimitHold_NoListingBeforeReset(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)

	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))
	g.derive(t, nil)
	hits := g.fake.listHitsFor(111)

	// The window is still exhausted; a listing now would have succeeded here,
	// so a call before the reset would visibly un-stale the installation.
	g.fake.clearListOverride(111)
	g.clock.Advance(5 * time.Minute)
	set := g.derive(t, nil)
	if got := g.fake.listHitsFor(111); got != hits {
		t.Errorf("listing called %d time(s) before the reset, want 0", got-hits)
	}
	if in := inst(set, 111); !in.Stale || len(set.Repos) != 3 {
		t.Errorf("during the hold the retained set must stay: %+v repos=%v", in, repoNames(set))
	}

	g.clock.Advance(5*time.Minute + time.Second)
	set = g.derive(t, nil)
	if got := g.fake.listHitsFor(111); got != hits+1 {
		t.Errorf("listing calls after the reset = %d, want exactly 1", got-hits)
	}
	if in := inst(set, 111); in.Stale || !in.RetryNotBefore.IsZero() {
		t.Errorf("after the reset a successful listing clears the hold: %+v", in)
	}
}

// R3: an unusable reset (past, or no signal at all) sets no hold — the normal
// cadence applies and the very next derive re-lists.
func TestDerive_NoHoldWithoutFutureReset(t *testing.T) {
	cases := map[string]listOverride{
		"reset in the past": rateLimit403(retentionEpoch.Add(-time.Hour)),
		"plain 500":         {status: 500, body: `{"message":"boom"}`},
		"rate limit, no reset headers": {
			status: 403, headers: map[string]string{"X-RateLimit-Remaining": "0"},
			body: `{"message":"API rate limit exceeded"}`,
		},
	}
	for name, ov := range cases {
		t.Run(name, func(t *testing.T) {
			g := newRetentionRig(t, oneInstall, threeRepos(), nil)
			g.derive(t, nil)
			g.fake.setListOverride(111, ov)
			set := g.derive(t, nil)
			if in := inst(set, 111); !in.RetryNotBefore.IsZero() {
				t.Errorf("RetryNotBefore = %v, want none", in.RetryNotBefore)
			}
			hits := g.fake.listHitsFor(111)
			g.derive(t, nil)
			if g.fake.listHitsFor(111) != hits+1 {
				t.Errorf("the next derive must re-list when no hold applies")
			}
		})
	}
}

// R3: a hostile far-future reset is clamped to one hour.
func TestDerive_HoldClampedToOneHour(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)
	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(48*time.Hour)))
	set := g.derive(t, nil)
	if want := g.clock.Now().Add(time.Hour); !inst(set, 111).RetryNotBefore.Equal(want) {
		t.Errorf("RetryNotBefore = %v, want clamp to %v", inst(set, 111).RetryNotBefore, want)
	}
}

// AC6: a cold-start failure is an error for that installation, never a silent
// zero, and is retried.
func TestDerive_ColdStartRateLimit_IsErrorNotZero_AndRetried(t *testing.T) {
	reset := retentionEpoch.Add(10 * time.Minute)
	g := newRetentionRig(t, oneInstall, threeRepos(), func(f *fakeAppServer) {
		f.setListOverride(111, rateLimit403(reset))
	})
	// Reconcile derived on the real clock; whether or not that first attempt
	// set a hold, the fake-clock derive below is still a cold-start failure.
	set := g.derive(t, nil)
	in := inst(set, 111)
	if in.Stale || in.RepoListError == "" || in.RepoCount != 0 || len(set.Repos) != 0 {
		t.Fatalf("cold start: %+v repos=%v; want no repos, !Stale, RepoListError set", in, repoNames(set))
	}
	var lines []string
	logDerivedSet(set, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "ERROR") {
		t.Errorf("cold start must be logged as an error:\n%s", joined)
	}
	if strings.Contains(joined, "0 repo(s) accessible") {
		t.Errorf("a failed listing must never read as zero accessible repos:\n%s", joined)
	}

	// Retried: once the window recovers a later derive lists and derives the repos.
	g.fake.clearListOverride(111)
	g.clock.Advance(20 * time.Minute)
	set = g.derive(t, nil)
	if len(set.Repos) != 3 || inst(set, 111).RepoListError != "" {
		t.Errorf("cold start was not recovered by a later listing: %v", repoNames(set))
	}
}

// R3 on a cold start: a future reset holds the next attempt too.
func TestDerive_ColdStartRateLimit_HoldsUntilReset(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	// Fresh reconciler state: drop the successful first listing to simulate a
	// process that never had one.
	g.r.mu.Lock()
	g.r.lastGood = nil
	g.r.mu.Unlock()

	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))
	g.derive(t, nil)
	hits := g.fake.listHitsFor(111)
	g.fake.clearListOverride(111)
	g.clock.Advance(time.Minute)
	set := g.derive(t, nil)
	if g.fake.listHitsFor(111) != hits {
		t.Error("cold-start installation was re-listed inside the hold")
	}
	if in := inst(set, 111); in.Stale || in.RepoListError == "" {
		t.Errorf("held cold start must still report a listing error: %+v", in)
	}
	g.clock.Advance(10 * time.Minute)
	if got := len(g.derive(t, nil).Repos); got != 3 {
		t.Errorf("after the reset the cold start recovers, got %d repos", got)
	}
}

// R1: any failed listing retains — a 5xx here, and a transport error below.
func TestDerive_ServerErrorListing_RetainsLastGood(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)
	g.fake.setListOverride(111, listOverride{status: 502, body: `bad gateway`})
	set := g.derive(t, nil)
	if len(set.Repos) != 3 || !inst(set, 111).Stale {
		t.Errorf("5xx must retain: repos=%v %+v", repoNames(set), inst(set, 111))
	}
}

// R1: a listing that fails partway through pagination is a failed listing;
// the partial first page must never become the grant.
func TestDerive_MidPaginationFailure_RetainsLastGood_NeverPartial(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	want := repoNames(g.derive(t, nil))

	page1 := make([]string, 100) // a full page, so the client asks for page 2
	for i := range page1 {
		page1[i] = fmt.Sprintf("verveguy/partial-%03d", i)
	}
	g.fake.selectedRepos = map[int64][]string{111: page1}
	g.fake.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`, fromPage: 2})
	set := g.derive(t, nil)

	if got := repoNames(set); !sameStrings(got, want) {
		t.Fatalf("repos = %v (len %d), want the retained %v — a partial page set leaked", got, len(got), want)
	}
	if !inst(set, 111).Stale {
		t.Errorf("mid-pagination failure must be a stale listing: %+v", inst(set, 111))
	}
}

// A listing that hits the pagination ceiling is still a *successful* one and
// overwrites the retained set (the Truncated warning already flags it).
func TestDerive_TruncatedListing_OverwritesRetainedSet(t *testing.T) {
	g := newRetentionRig(t, oneInstall, map[int64][]string{111: {"verveguy/old"}}, nil)
	g.derive(t, nil)

	full := make([]string, 100)
	for i := range full {
		full[i] = fmt.Sprintf("verveguy/new-%03d", i)
	}
	g.fake.selectedRepos = map[int64][]string{111: full}
	g.fake.neverShortPage = true
	if set := g.derive(t, nil); !set.Truncated {
		t.Fatal("setup: expected a truncated listing")
	}

	g.fake.neverShortPage = false
	g.fake.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`})
	set := g.derive(t, nil)
	names := repoNames(set)
	hasNew, hasOld := false, false
	for _, n := range names {
		hasNew = hasNew || n == "verveguy/new-099"
		hasOld = hasOld || n == "verveguy/old"
	}
	if !hasNew || hasOld {
		t.Errorf("retained set should be the truncated success, not the older listing (new=%v old=%v)", hasNew, hasOld)
	}
}

// R1/R3: retention and holds are scoped per installation.
func TestDerive_FailureOnOneInstallationLeavesOthersAlone(t *testing.T) {
	installs := []gh.AppInstallation{
		{ID: 111, Account: "verveguy", RepositorySelection: "all"},
		{ID: 222, Account: "handarbeit", RepositorySelection: "all"},
	}
	repos := map[int64][]string{111: {"verveguy/a", "verveguy/b"}, 222: {"handarbeit/x"}}
	g := newRetentionRig(t, installs, repos, nil)
	g.derive(t, nil)

	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))
	g.fake.selectedRepos = map[int64][]string{111: {"verveguy/a", "verveguy/b"}, 222: {"handarbeit/x", "handarbeit/y"}}
	hits222 := g.fake.listHitsFor(222)
	set := g.derive(t, nil)

	if in := inst(set, 222); in.Stale || in.RepoCount != 2 {
		t.Errorf("installation 222 must be listed fresh: %+v", in)
	}
	if in := inst(set, 111); !in.Stale || in.RepoCount != 2 {
		t.Errorf("installation 111 must be retained: %+v", in)
	}
	if len(set.Repos) != 4 {
		t.Errorf("repos = %v, want 2 retained + 2 fresh", repoNames(set))
	}

	// 111 is held; 222 keeps its own cadence and is re-listed on every derive.
	g.clock.Advance(time.Minute)
	g.derive(t, nil)
	if g.fake.listHitsFor(222) != hits222+2 {
		t.Errorf("installation 222 must be listed on every derive despite 111's hold")
	}
}

// An installation that leaves the list, or is narrowed out by watched_repos,
// loses its retained set: a revoked installation must not keep its repos.
func TestDerive_RetainedSetPrunedWhenInstallationNoLongerWanted(t *testing.T) {
	t.Run("installation removed", func(t *testing.T) {
		g := newRetentionRig(t, oneInstall, threeRepos(), nil)
		g.derive(t, nil)
		g.fake.installations = nil
		g.derive(t, nil)

		g.fake.installations = oneInstall
		g.fake.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`})
		set := g.derive(t, nil)
		if in := inst(set, 111); in.Stale || len(set.Repos) != 0 {
			t.Errorf("a re-appearing installation must start cold, got %+v repos=%v", in, repoNames(set))
		}
	})
	t.Run("narrowed out by watched_repos", func(t *testing.T) {
		g := newRetentionRig(t, oneInstall, threeRepos(), nil)
		g.derive(t, nil)
		g.derive(t, []string{"otherowner/z"}) // verveguy is no longer needed

		g.fake.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`})
		set := g.derive(t, nil)
		if in := inst(set, 111); in.Stale || len(set.Repos) != 0 {
			t.Errorf("a re-needed installation must start cold, got %+v repos=%v", in, repoNames(set))
		}
	})
}

// The retained set is the raw listing, so a watched_repos change applies to it.
func TestDerive_WatchedReposFilterAppliesToRetainedSet(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)
	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))

	set := g.derive(t, []string{"verveguy/b"})
	if got := repoNames(set); !sameStrings(got, []string{"verveguy/b"}) {
		t.Errorf("repos = %v, want the filter applied to retained data", got)
	}
	if !inst(set, 111).Stale {
		t.Error("expected stale")
	}
}

// Permission shortfalls (ADR-1709) are still reported for a stale installation.
func TestDerive_StaleInstallationStillReportsPermissionShortfalls(t *testing.T) {
	installs := []gh.AppInstallation{{ID: 111, Account: "verveguy", RepositorySelection: "all", Permissions: map[string]string{"issues": "read"}}}
	g := newRetentionRig(t, installs, threeRepos(), nil)
	g.r.mu.Lock()
	g.r.requiredPermissions = map[string]string{"issues": "write"}
	g.r.mu.Unlock()
	g.derive(t, nil)
	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))
	set := g.derive(t, nil)

	var lines []string
	logDerivedSet(set, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "STALE") || !strings.Contains(joined, `"write"`) {
		t.Errorf("expected both the stale line and the shortfall line:\n%s", joined)
	}
	if strings.Contains(joined, "0 repo(s) accessible") {
		t.Errorf("stale installation logged as zero accessible:\n%s", joined)
	}
}

// Overlapping Derive calls (ticker, webhook, SIGHUP) must not race on the
// retention state.
func TestDerive_ConcurrentCallsAreRaceFree(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), nil)
	g.derive(t, nil)
	g.fake.setListOverride(111, rateLimit403(g.clock.Now().Add(10*time.Minute)))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if i%2 == 0 {
					g.clock.Advance(time.Second)
				}
				if _, _, err := g.r.Derive(context.Background(), nil, 0, func(string, ...any) {}); err != nil {
					t.Errorf("Derive: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
}

// A watched repo under an owner whose listing failed cold is unknown, not
// "not covered by any installation's grant" (the incident printed that line
// eleven times for repos that were in fact still granted).
func TestDerive_ColdStartFailure_DoesNotClaimWatchedRepoUncovered(t *testing.T) {
	g := newRetentionRig(t, oneInstall, threeRepos(), func(f *fakeAppServer) {
		f.setListOverride(111, listOverride{status: 500, body: `{"message":"boom"}`})
	})
	set := g.derive(t, []string{"verveguy/a", "elsewhere/z"})
	if got := set.FilteredOut; !sameStrings(got, []string{"elsewhere/z"}) {
		t.Errorf("FilteredOut = %v, want only the entry no installation could cover", got)
	}
}
