package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func appModeEngine(t *testing.T, slug string, botUserID int64, instanceKey string) *Engine {
	t.Helper()
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.SetGitHubAppModeForTest(map[string]bool{"owner/repo": true}, false)
	eng.SetGitHubAppIdentityForTest(slug, botUserID, instanceKey)
	return eng
}

func TestDeriveLockID(t *testing.T) {
	a := deriveLockID("my-app", "host1", "/work/a")
	if !strings.HasPrefix(a, "my-app-") || len(a) != len("my-app-")+lockIdentitySuffixLn {
		t.Errorf("lock ID = %q", a)
	}
	if a != deriveLockID("my-app", "host1", "/work/a") {
		t.Error("lock ID must be stable for the same host+dir")
	}
	if a == deriveLockID("my-app", "host1", "/work/b") || a == deriveLockID("my-app", "host2", "/work/a") {
		t.Error("lock ID must differ across directories and hosts")
	}
}

func TestLockInstanceKey_EnvOverridesHostname(t *testing.T) {
	t.Setenv(instanceIDEnv, "")
	host, _ := os.Hostname()
	if got := lockInstanceKey(); got != host {
		t.Errorf("unset override: key = %q, want hostname %q", got, host)
	}
	t.Setenv(instanceIDEnv, "  pod-a  ")
	if got := lockInstanceKey(); got != "pod-a" {
		t.Errorf("override: key = %q, want %q", got, "pod-a")
	}
	// With the override pinned, the derived label no longer depends on the
	// hostname, so a container restart (new hostname) keeps the same label.
	a := resolveAppIdentity(&fakeUserIDFetcher{id: 1}, "my-app[bot]", "/work/a")
	b := resolveAppIdentity(&fakeUserIDFetcher{id: 1}, "my-app[bot]", "/work/a")
	if a.lockID != b.lockID || a.lockID != deriveLockID("my-app", "pod-a", "/work/a") {
		t.Errorf("lock IDs %q / %q not pinned to the override", a.lockID, b.lockID)
	}
}

func TestDeriveLockID_TruncatesToLabelLimit(t *testing.T) {
	long := strings.Repeat("a", 34)
	id := deriveLockID(long, "h", "/d")
	if got := len(lockLabelPrefix + id); got > 50 {
		t.Errorf("label %q is %d chars, exceeds GitHub's 50-char limit", lockLabelPrefix+id, got)
	}
	if !strings.HasPrefix(id, strings.Repeat("a", maxLockSlugLen)+"-") {
		t.Errorf("slug not truncated deterministically: %q", id)
	}
	// A slug whose cut lands on a hyphen must not leave a doubled hyphen.
	hy := strings.Repeat("a", maxLockSlugLen-1) + "-bcdef"
	if strings.Contains(deriveLockID(hy, "h", "/d"), "--") {
		t.Errorf("doubled hyphen in %q", deriveLockID(hy, "h", "/d"))
	}
	if strings.Contains(id, "[bot]") {
		t.Errorf("lock ID must not carry [bot]: %q", id)
	}
}

func TestDeriveLockID_EmptySlugNeverYieldsEmptyLabel(t *testing.T) {
	id := deriveLockID("", "h", "/d")
	if !strings.HasPrefix(id, appLockFallbackSlug+"-") {
		t.Errorf("lock ID = %q", id)
	}
}

func TestLockIdentity_PATModeIsCfgUser(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if got := eng.lockIdentity(); got != "testuser" {
		t.Errorf("lockIdentity = %q, want testuser", got)
	}
	if got := eng.lockLabel(); got != "fabrik:locked:testuser" {
		t.Errorf("lockLabel = %q", got)
	}
	if eng.commitIdentity() != nil {
		t.Error("commitIdentity must be nil in PAT mode")
	}
	if got := eng.operatorNote(); got != "testuser" {
		t.Errorf("operatorNote = %q", got)
	}
}

func TestLockIdentity_AppModeIgnoresCfgUser(t *testing.T) {
	eng := appModeEngine(t, "my-app", 42, "instance-a")
	eng.cfg.User = "operator"
	got := eng.lockLabel()
	if !strings.HasPrefix(got, "fabrik:locked:my-app-") {
		t.Errorf("lockLabel = %q", got)
	}
	if strings.Contains(got, "operator") || strings.Contains(got, "[bot]") {
		t.Errorf("lockLabel %q must not carry the operator or [bot]", got)
	}
	if eng.selfLogin() != "my-app[bot]" {
		t.Errorf("selfLogin = %q", eng.selfLogin())
	}
	if eng.operatorNote() != "" {
		t.Errorf("operatorNote = %q, want empty under App auth", eng.operatorNote())
	}
}

func TestAppModeWithoutIdentitySeamNeverYieldsEmptyLockLabel(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.SetGitHubAppModeForTest(nil, false)
	if got := eng.lockLabel(); got == "fabrik:locked:" || !strings.HasPrefix(got, "fabrik:locked:app-") {
		t.Errorf("lockLabel = %q", got)
	}
}

func TestCommitIdentity_AppMode(t *testing.T) {
	eng := appModeEngine(t, "my-app", 42, "k")
	ci := eng.commitIdentity()
	if ci == nil || ci.Name != "my-app[bot]" || ci.Email != "42+my-app[bot]@users.noreply.github.com" {
		t.Errorf("commitIdentity = %+v", ci)
	}
	// Lookup failed: ID-less fallback, name unchanged.
	eng = appModeEngine(t, "my-app", 0, "k")
	ci = eng.commitIdentity()
	if ci == nil || ci.Name != "my-app[bot]" || ci.Email != "my-app[bot]@users.noreply.github.com" {
		t.Errorf("fallback commitIdentity = %+v", ci)
	}
}

func TestReleaseUpgradeToken_ByMode(t *testing.T) {
	if got := releaseUpgradeToken(Config{Token: "pat"}); got != "pat" {
		t.Errorf("PAT mode: %q", got)
	}
	if got := releaseUpgradeToken(Config{Token: "pat", GHESHost: "ghe.example.com"}); got != "" {
		t.Errorf("GHES: %q", got)
	}
	app := Config{Token: "pat", GitHubAppID: 1, GitHubAppPrivateKeyPath: "/k.pem", GitHubAppInstallationID: 2}
	if got := releaseUpgradeToken(app); got != "" {
		t.Errorf("App mode must not use a PAT: %q", got)
	}
}

func TestAcquireLock_AppMode_LabelAndTieBreak(t *testing.T) {
	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{Number: 10}

	// Two local instances of one App: distinct labels, so each sees the other's
	// lock as foreign and exactly one wins the tie-break.
	a := appModeEngine(t, "my-app", 1, "instance-a")
	b := appModeEngine(t, "my-app", 1, "instance-b")
	if a.lockLabel() == b.lockLabel() {
		t.Fatalf("instances must not share a lock label: %q", a.lockLabel())
	}
	lo, hi := a, b
	if lo.lockIdentity() > hi.lockIdentity() {
		lo, hi = hi, lo
	}

	run := func(eng, other *Engine) (bool, []string) {
		var added []string
		client := &mockGitHubClient{
			fetchLabelsFn: func(owner, repo string, n int) ([]string, error) {
				return []string{eng.lockLabel(), other.lockLabel()}, nil
			},
			addLabelToIssueFn: func(owner, repo string, n int, l string) error {
				added = append(added, l)
				return nil
			},
		}
		eng.client = client
		release, _, done, ok := eng.acquireLockAndVerify(context.Background(), item, stage, "owner", "repo", "owner/repo", eng.lockLabel())
		if done != nil {
			close(done)
		}
		release()
		return ok, added
	}

	okLo, added := run(lo, hi)
	if !okLo {
		t.Error("the lexicographically lower instance must win")
	}
	if len(added) == 0 || added[0] != lo.lockLabel() {
		t.Errorf("first label added = %v, want %q", added, lo.lockLabel())
	}
	if okHi, _ := run(hi, lo); okHi {
		t.Error("the lexicographically higher instance must yield")
	}
}

func TestMentionTargets_PATMode(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Assignees: []string{"alice"}, Author: "bob"}
	if got := eng.mentionTargets(item); !reflect.DeepEqual(got, []string{"testuser"}) {
		t.Errorf("PAT mode mentionTargets = %v, want [testuser] regardless of assignees", got)
	}
	eng.cfg.User = ""
	if got := eng.mentionTargets(item); got != nil {
		t.Errorf("PAT mode with no user = %v, want nil", got)
	}
}

func TestMentionTargets_AppMode(t *testing.T) {
	eng := appModeEngine(t, "my-app", 1, "k")
	eng.cfg.User = "operator"
	cases := []struct {
		name string
		item gh.ProjectItem
		want []string
	}{
		{"assignees first", gh.ProjectItem{Assignees: []string{"alice", "bob"}, Author: "carol"}, []string{"alice", "bob"}},
		{"author when unassigned", gh.ProjectItem{Author: "carol"}, []string{"carol"}},
		{"nobody", gh.ProjectItem{}, nil},
		{"bot author never mentioned", gh.ProjectItem{Author: "dependabot[bot]"}, nil},
		{"app's own GraphQL login never mentioned", gh.ProjectItem{Author: "my-app"}, nil},
		{"app's own REST login never mentioned", gh.ProjectItem{Author: "my-app[bot]", Assignees: []string{"my-app[bot]"}}, nil},
		{"bot assignee skipped, human kept", gh.ProjectItem{Assignees: []string{"copilot-swe-agent[bot]", "alice"}}, []string{"alice"}},
		{"only bot assignees falls back to human author", gh.ProjectItem{Assignees: []string{"x[bot]"}, Author: "carol"}, []string{"carol"}},
		{"never the configured operator", gh.ProjectItem{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eng.mentionTargets(tc.item); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mentionTargets = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSpawnAssignees(t *testing.T) {
	pat := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	parent := gh.ProjectItem{Assignees: []string{"alice"}, Author: "bob"}
	if got := pat.spawnAssignees(parent); !reflect.DeepEqual(got, []string{"testuser"}) {
		t.Errorf("PAT spawnAssignees = %v", got)
	}
	app := appModeEngine(t, "my-app", 1, "k")
	app.cfg.User = "operator"
	if got := app.spawnAssignees(gh.ProjectItem{Assignees: []string{"alice", "x[bot]"}, Author: "bob"}); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("App spawnAssignees = %v, want [alice] (human assignees only, not author or operator)", got)
	}
	if got := app.spawnAssignees(gh.ProjectItem{Author: "bob"}); len(got) != 0 {
		t.Errorf("App spawnAssignees with none = %v, want empty", got)
	}
}

func TestStartupCleanup_LegacyOperatorLockSweep(t *testing.T) {
	// App mode with a leftover user: — the pre-App lock label is swept; another
	// instance's App-derived label is left alone.
	client := &mockGitHubClient{}
	e := appModeEngine(t, "my-app", 1, "instance-a")
	e.client = client
	e.cfg.User = "operator"
	client.fetchLabelAppliedAtFn = func(_, _ string, _ int, _ string) (time.Time, error) {
		return time.Now().Add(-legacyLockMinAge - time.Hour), nil
	}
	bootstrapItem(t, e, 21, []string{"fabrik:locked:operator"})
	bootstrapItem(t, e, 22, []string{"fabrik:locked:my-app-abcdef"})

	e.runStartupCleanup()

	if removed := removeLabelsCalled(client, 21); !hasRemovedLabel(removed, "fabrik:locked:operator") {
		t.Errorf("legacy operator lock not swept: %v", removed)
	}
	if removed := removeLabelsCalled(client, 22); len(removed) != 0 {
		t.Errorf("another instance's lock must not be swept: %v", removed)
	}
}

func TestStartupCleanup_LegacySweepLeavesRecentOrUnreadableLock(t *testing.T) {
	// A label applied recently may belong to a live PAT-mode instance running
	// as the same user, and one whose age cannot be read cannot be shown safe:
	// neither is swept.
	client := &mockGitHubClient{}
	e := appModeEngine(t, "my-app", 1, "instance-a")
	e.client = client
	e.cfg.User = "operator"
	client.fetchLabelAppliedAtFn = func(_, _ string, n int, _ string) (time.Time, error) {
		if n == 25 {
			return time.Time{}, errors.New("events unavailable")
		}
		return time.Now().Add(-10 * time.Minute), nil
	}
	bootstrapItem(t, e, 24, []string{"fabrik:locked:operator"})
	bootstrapItem(t, e, 25, []string{"fabrik:locked:operator"})

	e.runStartupCleanup()

	for _, n := range []int{24, 25} {
		if removed := removeLabelsCalled(client, n); len(removed) != 0 {
			t.Errorf("#%d: legacy lock swept despite recent/unreadable age: %v", n, removed)
		}
	}
}

func TestStartupCleanup_LegacySweepSkipsLiveWorker(t *testing.T) {
	client := &mockGitHubClient{}
	e := appModeEngine(t, "my-app", 1, "instance-a")
	e.client = client
	e.cfg.User = "operator"
	bootstrapItem(t, e, 23, []string{"fabrik:locked:operator"})
	setWorker(e, 23, os.Getpid(), "Implement", time.Now())

	e.runStartupCleanup()

	if removed := removeLabelsCalled(client, 23); len(removed) != 0 {
		t.Errorf("legacy sweep removed a lock held by a live worker: %v", removed)
	}
}

func TestLegacyLockLabel(t *testing.T) {
	pat := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if got := pat.legacyLockLabel(); got != "" {
		t.Errorf("PAT mode: %q", got)
	}
	app := appModeEngine(t, "my-app", 1, "k")
	if got := app.legacyLockLabel(); got != "fabrik:locked:testuser" {
		t.Errorf("App mode with user: %q", got)
	}
	app.cfg.User = ""
	if got := app.legacyLockLabel(); got != "" {
		t.Errorf("App mode without user: %q", got)
	}
}

type fakeUserIDFetcher struct {
	id  int64
	err error
	got string
}

func (f *fakeUserIDFetcher) FetchUserID(login string) (int64, error) {
	f.got = login
	return f.id, f.err
}

func TestResolveAppIdentity_LookupSuccessAndSoftFailure(t *testing.T) {
	ok := &fakeUserIDFetcher{id: 99}
	id := resolveAppIdentity(ok, "my-app[bot]", t.TempDir())
	if ok.got != "my-app[bot]" || id.botUserID != 99 || id.slug != "my-app" || id.botLogin != "my-app[bot]" {
		t.Errorf("identity = %+v (lookup of %q)", id, ok.got)
	}

	// A failed lookup must not fail startup: the identity still names the bot,
	// with no ID (ID-less noreply email).
	bad := &fakeUserIDFetcher{err: errFakeLookup}
	id = resolveAppIdentity(bad, "my-app[bot]", t.TempDir())
	if id == nil || id.botUserID != 0 || id.botLogin != "my-app[bot]" || id.lockID == "" {
		t.Errorf("soft-failure identity = %+v", id)
	}
}

var errFakeLookup = errors.New("boom")

func TestBuildAwaitingInputComment_MultipleMentions(t *testing.T) {
	got := buildAwaitingInputComment([]string{"alice", "bob"}, "Plan", "")
	if !strings.Contains(got, "@alice @bob: awaiting your input on **Plan**") {
		t.Errorf("comment = %q", got)
	}
}

func TestBlockOnInput_AppMode_MentionsAssigneesNotOperator(t *testing.T) {
	eng := appModeEngine(t, "my-app", 1, "k")
	client := &mockGitHubClient{}
	eng.client = client
	eng.cfg.User = "operator"
	stage := &stages.Stage{Name: "Research", Order: 1}

	eng.blockOnInput(gh.ProjectItem{Number: 5, Assignees: []string{"alice"}, Author: "carol"}, stage, "")
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "@alice") || strings.Contains(body, "@operator") || strings.Contains(body, "@carol") {
		t.Errorf("comment = %q", body)
	}

	// Bot-authored, unassigned (e.g. a spawned child created by the App): nobody is pinged.
	client.addCommentCalls = nil
	eng.blockOnInput(gh.ProjectItem{Number: 6, Author: "my-app"}, stage, "")
	body = client.addCommentCalls[0].body
	if strings.Contains(body, "@") {
		t.Errorf("no human resolvable — comment must mention nobody, got %q", body)
	}
	if !strings.HasPrefix(body, "🏭 **Fabrik**") {
		t.Errorf("comment = %q", body)
	}
}

func spawnParentAndBoard() (gh.ProjectItem, *gh.ProjectBoard) {
	return gh.ProjectItem{ID: "I_parent", ItemID: "PVTI_parent", Number: 42, Repo: "owner/repo", Assignees: []string{"alice", "x[bot]"}, Author: "bob"},
		&gh.ProjectBoard{ProjectID: "PVT_1"}
}

func TestSpawnChildren_AssigneesByMode(t *testing.T) {
	blocks := []SpawnBlock{{Repo: "owner/repo", Title: "Child", Body: "b"}}
	for _, app := range []bool{false, true} {
		var got [][]string
		client := &mockGitHubClient{
			createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
				got = append(got, assignees)
				return 300, "I_child", nil
			},
			addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) { return "PVTI_c", nil },
		}
		eng := spawnTestEngine(t, client)
		if app {
			eng.SetGitHubAppModeForTest(nil, false)
			eng.SetGitHubAppIdentityForTest("my-app", 1, "k")
		}
		item, board := spawnParentAndBoard()
		if _, _, err := eng.spawnChildren(context.Background(), board, item, "owner", "repo", blocks, false); err != nil {
			t.Fatalf("app=%v: %v", app, err)
		}
		want := []string{"testuser"}
		if app {
			want = []string{"alice"}
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("app=%v: CreateIssue assignees = %v, want %v", app, got, want)
		}
	}
}

func TestSpawnChildren_AppMode_RetriesUnassignedOn422(t *testing.T) {
	var got [][]string
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			got = append(got, assignees)
			if len(assignees) > 0 {
				return 0, "", fmt.Errorf("creating issue: %w", gh.ErrUnprocessableEntity)
			}
			return 300, "I_child", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) { return "PVTI_c", nil },
	}
	eng := spawnTestEngine(t, client)
	eng.SetGitHubAppModeForTest(nil, false)
	eng.SetGitHubAppIdentityForTest("my-app", 1, "k")
	item, board := spawnParentAndBoard()
	spawned, _, err := eng.spawnChildren(context.Background(), board, item, "owner", "repo",
		[]SpawnBlock{{Repo: "owner/repo", Title: "Child", Body: "b"}}, false)
	if err != nil || len(spawned) != 1 {
		t.Fatalf("spawned=%v err=%v", spawned, err)
	}
	if len(got) != 2 || len(got[0]) != 1 || len(got[1]) != 0 {
		t.Errorf("CreateIssue attempts = %v, want [[alice] []]", got)
	}
}

func TestSpawnChildren_PATMode_422StillFailsLoud(t *testing.T) {
	calls := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			calls++
			return 0, "", fmt.Errorf("creating issue: %w", gh.ErrUnprocessableEntity)
		},
	}
	eng := spawnTestEngine(t, client)
	item, board := spawnParentAndBoard()
	_, _, err := eng.spawnChildren(context.Background(), board, item, "owner", "repo",
		[]SpawnBlock{{Repo: "owner/repo", Title: "Child", Body: "b"}}, false)
	if err == nil || calls != 1 {
		t.Errorf("PAT mode must fail loud on a bad assignee without retrying: err=%v calls=%d", err, calls)
	}
}
