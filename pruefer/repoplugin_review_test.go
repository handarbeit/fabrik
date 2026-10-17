package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// repoDeepTree is a repo plugin named "mine" with one skill, "deep", and an
// executable hook script.
func repoDeepTree() *repoPluginTree {
	return &repoPluginTree{Files: []repoPluginFile{
		{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"mine","version":"3.1.0"}`)},
		{Path: "skills/deep/SKILL.md", Data: []byte("---\ndescription: x\n---\nReview deeply.\n")},
		{Path: "hooks/run.sh", Data: []byte("#!/bin/sh\n"), Exec: true},
	}}
}

func repoDeepPlugin() loadedRepoPlugin {
	return loadedRepoPlugin{
		ID: "mine@acme-tools", Name: "mine", Origin: originInRepo, Repo: "owner/repo",
		Commit: "basesha", Path: "plugins/mine", TreeSHA: "treesha", Tree: repoDeepTree(),
	}
}

func TestReview_RepoPluginSkillRuns(t *testing.T) {
	logs := capturePluginLogs(t)
	rec := filepath.Join(t.TempDir(), "rec")
	selFile := rec + ".sel"
	if err := os.WriteFile(selFile, []byte(selStream+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The script looks inside the repo plugin directory while the review runs.
	writeFakeClaude(t, `echo "$*" >> "`+rec+`.calls"
for a in "$@"; do
  case "$a" in
    */repo/00) ls -l "$a/hooks/run.sh" >> "`+rec+`.ls"; cat "$a/skills/deep/SKILL.md" >> "`+rec+`.skill"; echo "$a" > "`+rec+`.dir" ;;
  esac
done
cat "`+selFile+`"
`)
	op := writeTestPlugin(t, filepath.Join(t.TempDir(), "op-dir"), "op", "1", "x")
	rpl := repoDeepPlugin()
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
		PRNumber: 1, WorkDir: t.TempDir(), ReviewSkill: "mine:deep", BaseSHA: "basesha",
		OperatorPluginDirs: []string{op}, RepoPlugins: []loadedRepoPlugin{rpl},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "selected review" || res.FallbackFrom != "" {
		t.Fatalf("res = %+v", res)
	}
	calls := recordedCalls(t, rec)
	if len(calls) != 1 {
		t.Fatalf("claude ran %d times, want 1", len(calls))
	}
	// Order: embedded, operator, then the repo plugin (R6); the grant names the run's skill only.
	i1, i2, i3 := strings.Index(calls[0], "pruefer-review"), strings.Index(calls[0], op), strings.Index(calls[0], "/repo/00")
	if strings.Count(calls[0], "--plugin-dir") != 3 || !(i1 >= 0 && i1 < i2 && i2 < i3) {
		t.Errorf("want embedded, operator, repo --plugin-dir in that order: %s", calls[0])
	}
	if !strings.Contains(calls[0], "Skill(mine:deep)") || strings.Contains(calls[0], "Skill(pruefer-review:review)") || strings.Contains(calls[0], "--dangerously") {
		t.Errorf("the Skill grant must name the selected skill only: %s", calls[0])
	}
	if !strings.Contains(calls[0], `--setting-sources`) {
		t.Errorf("--setting-sources must stay: %s", calls[0])
	}
	// While the review ran the plugin was on disk with its executable bit.
	ls, _ := os.ReadFile(rec + ".ls")
	if !strings.HasPrefix(string(ls), "-rwx") {
		t.Errorf("hook script lost its executable bit: %q", ls)
	}
	if skill, _ := os.ReadFile(rec + ".skill"); !strings.Contains(string(skill), "Review deeply.") {
		t.Errorf("skill content not written: %q", skill)
	}
	// ...and the per-review root, repo plugin included, is gone afterwards.
	dirB, _ := os.ReadFile(rec + ".dir")
	if d := strings.TrimSpace(string(dirB)); d == "" {
		t.Error("the repo plugin directory was never passed")
	} else if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("repo plugin directory survived the review: %v", err)
	}
	// R8: the log names the repo plugin with its origin, repository, commit and tree.
	want := "review plugin: mine v3.1.0 (repo plugin mine@acme-tools, in-repo owner/repo at commit basesha, path \"plugins/mine\", tree treesha"
	if countContaining(logs(), want) != 1 {
		t.Errorf("repo plugin log line missing %q in %q", want, logs())
	}
	if countContaining(logs(), "review skill: mine:deep (plugin mine v3.1.0, repo,") != 1 {
		t.Errorf("skill log line missing: %q", logs())
	}
}

func TestReview_SkippedRepoPluginIsNamedInTheFallbackNotice(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selStream, defStream)
	notes := []string{"repo plugin mine in marketplace acme-tools not loaded: its source repository evil/x is not on the operator's repo_plugin_marketplaces allowlist"}
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
		PRNumber: 1, WorkDir: t.TempDir(), ReviewSkill: "mine:deep", BaseSHA: "basesha", RepoPluginNotes: notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "default review" || res.FallbackFrom != "mine:deep" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.FallbackReason, "not on the operator's repo_plugin_marketplaces allowlist") {
		t.Errorf("the reason must name why the plugin was skipped: %q", res.FallbackReason)
	}
	notice := fallbackNotice(res.FallbackFrom, "basesha", res.FallbackReason)
	if strings.Contains(notice, os.TempDir()) || !strings.Contains(notice, "allowlist") {
		t.Errorf("notice = %q", notice)
	}
	if calls := recordedCalls(t, rec); len(calls) != 1 || strings.Contains(calls[0], "/repo/") {
		t.Errorf("a skipped plugin must not reach argv: %q", calls)
	}
}

func TestBuildPluginSet_RepoPluginsAfterOperatorFirstWinsAndReservedName(t *testing.T) {
	rp, err := setupReviewPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	op := writeTestPlugin(t, filepath.Join(t.TempDir(), "mine-op"), "mine", "1.0", "x")
	reserved := repoDeepPlugin()
	reserved.ID = "sneaky@acme-tools"
	reserved.Tree = &repoPluginTree{Files: []repoPluginFile{{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"` + reviewplugin.PluginName + `"}`)}}}
	other := repoDeepPlugin()
	other.ID = "other@acme-tools"
	other.Tree = &repoPluginTree{Files: []repoPluginFile{{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"other","version":"2"}`)}}}

	// A repo plugin named like the operator's "mine" loses to it; the reserved name is refused.
	set := buildPluginSet(rp, []string{op}, []loadedRepoPlugin{repoDeepPlugin(), reserved, other}, []string{"repo plugin zzz@m not loaded: because"})
	var names []string
	for _, p := range set.Plugins {
		names = append(names, p.Name+":"+p.Source)
	}
	if got, want := strings.Join(names, ","), "pruefer-review:embedded,mine:operator,other:repo"; got != want {
		t.Errorf("load order = %s, want %s", got, want)
	}
	joined := strings.Join(set.Notes, "\n")
	for _, w := range []string{"is already loaded from a operator plugin", "reserved for the embedded plugin", "zzz@m not loaded: because"} {
		if !strings.Contains(joined, w) {
			t.Errorf("notes lack %q:\n%s", w, joined)
		}
	}
	for _, n := range set.Notes {
		if strings.Contains(n, os.TempDir()) || strings.Contains(n, rp.Root) {
			t.Errorf("note leaks a local path: %s", n)
		}
	}
	// Repo plugins sit under the per-review root in numbered, not repo-named, directories.
	for _, p := range set.Plugins {
		if p.Source == sourceRepo && !strings.HasPrefix(p.Dir, rp.RepoRoot()+string(filepath.Separator)) {
			t.Errorf("repo plugin outside the repo root: %s", p.Dir)
		}
	}
	if set.Plugins[2].Repo == nil || set.Plugins[2].Repo.ID != "other@acme-tools" {
		t.Errorf("repo provenance lost: %+v", set.Plugins[2])
	}
}

func TestWriteRepoPluginTree_RefusesEscapesAndOverwrites(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../escape", "/abs", "a/../../b"} {
		err := writeRepoPluginTree(filepath.Join(root, "x"), &repoPluginTree{Files: []repoPluginFile{{Path: p, Data: []byte("x")}}})
		if err == nil {
			t.Errorf("%q was written", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
		t.Error("a file escaped the plugin directory")
	}
	// An existing file is never overwritten.
	d := filepath.Join(root, "y")
	if err := writeRepoPluginTree(d, &repoPluginTree{Files: []repoPluginFile{{Path: "a", Data: []byte("1")}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeRepoPluginTree(d, &repoPluginTree{Files: []repoPluginFile{{Path: "a", Data: []byte("2")}}}); err == nil {
		t.Error("an existing file was overwritten")
	}
	if err := writeRepoPluginTree(filepath.Join(root, "z"), &repoPluginTree{}); err == nil {
		t.Error("an empty tree was written")
	}
}

// --- ReviewPR end to end (mock invoker) ---

const repoCfgSelecting = "plugins:\n  - acme-review@acme-tools\nreview_skill: acme-review:review\n"

func reviewWithRepoPlugins(t *testing.T, f *fakeReviewer, cfg Config, pr gh.PRDetails) (ReviewRequest, ReviewOutcome) {
	t.Helper()
	// ReviewPR uses the process-wide cache; give each test its own so one
	// test's fetch can never satisfy another's.
	old := defaultRepoPluginCache
	defaultRepoPluginCache = newRepoPluginCache(64 << 20)
	t.Cleanup(func() { defaultRepoPluginCache = old })
	f.repoConfigData = []byte(repoCfgSelecting)
	if pr.Number == 0 {
		pr = gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "main", DefaultBranch: "main"}
	}
	return reviewWithPR(t, f, cfg, pr)
}

// R5 / SC-001: with repo_plugins off, a repo that declares and selects a
// plugin causes no repo-plugin reads and no repo plugin reaches the invoker.
func TestReviewPR_RepoPluginsOffReadsNothing(t *testing.T) {
	f := inRepoWorld()
	req, out := reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true}, gh.PRDetails{})
	if f.git.totalReads() != 0 {
		t.Errorf("repo plugin reads with the capability off: %+v", f.git)
	}
	if len(req.RepoPlugins) != 0 || len(req.RepoPluginNotes) != 0 {
		t.Errorf("repo plugins reached the invoker: %+v %v", req.RepoPlugins, req.RepoPluginNotes)
	}
	if req.ReviewSkill != "acme-review:review" {
		t.Errorf("the selection must be unchanged (it takes the visible fallback): %q", req.ReviewSkill)
	}
	if !out.Conclusive {
		t.Errorf("an off switch is a clean verdict: %+v", out)
	}
}

func TestReviewPR_RepoPluginsOnLoadsFromTheBaseSHA(t *testing.T) {
	logs := capturePluginLogs(t)
	f := inRepoWorld()
	cfg := Config{RepoReviewSkill: true, RepoPlugins: true}
	req, out := reviewWithRepoPlugins(t, f, cfg, gh.PRDetails{})
	if len(req.RepoPlugins) != 1 || req.RepoPlugins[0].ID != "acme-review@acme-tools" || req.RepoPlugins[0].Commit != "basesha" {
		t.Fatalf("RepoPlugins = %+v", req.RepoPlugins)
	}
	if len(req.RepoPluginNotes) != 0 || !out.Conclusive {
		t.Errorf("notes=%v outcome=%+v", req.RepoPluginNotes, out)
	}
	for _, r := range append(append([]string{}, f.git.SubtreeReads...), f.git.FileReads...) {
		if strings.Contains(r, "head-sha") {
			t.Errorf("a repo plugin read used the PR head: %s", r)
		}
	}
	if countContaining(logs(), "repo plugin acme-review@acme-tools will load from in-repo owner/repo at commit basesha") != 1 {
		t.Errorf("the load was not logged (R8): %q", logs())
	}
}

func TestReviewPR_RepoPluginsNotOnTheDefaultBranchReadNothing(t *testing.T) {
	logs := capturePluginLogs(t)
	f := inRepoWorld()
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "feature/x", DefaultBranch: "main"}
	req, _ := reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, pr)
	if f.git.totalReads() != 0 || len(req.RepoPlugins) != 0 {
		t.Errorf("a stacked PR read or loaded repo plugins: reads=%d plugins=%v", f.git.totalReads(), req.RepoPlugins)
	}
	if countContaining(logs(), "is not the repository's default branch main") != 1 {
		t.Errorf("the refusal was not logged: %q", logs())
	}
	// An unknown default branch fails closed too.
	f = inRepoWorld()
	pr.BaseRef, pr.DefaultBranch = "main", ""
	req, _ = reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, pr)
	if f.git.totalReads() != 0 || len(req.RepoPlugins) != 0 {
		t.Errorf("an unknown default branch read or loaded repo plugins")
	}
}

func TestReviewPR_UnresolvedBaseSHALoadsNoRepoPlugins(t *testing.T) {
	f := inRepoWorld()
	f.baseSHAErr = context.DeadlineExceeded
	req, out := reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, gh.PRDetails{})
	if f.git.totalReads() != 0 || len(req.RepoPlugins) != 0 {
		t.Errorf("plugin code was read without a pinned base SHA")
	}
	if out.Conclusive {
		t.Error("an unresolved base is already non-conclusive")
	}
}

func TestReviewPR_RepoPluginNotNamedIsNotLoaded(t *testing.T) {
	f := inRepoWorld()
	f.git.repo("owner/repo").put("basesha", ".claude/settings.json", fakeFile{Data: `{"enabledPlugins":{"acme-review@acme-tools":true}}`})
	f.repoConfigData = []byte("review_skill: acme-review:review\n") // enabled for the developers, not named for Pruefer
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "main", DefaultBranch: "main"}
	req, _ := reviewWithPR(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, pr)
	if f.git.totalReads() != 0 || len(req.RepoPlugins) != 0 {
		t.Errorf("an enabled but unnamed plugin was read or loaded: reads=%d", f.git.totalReads())
	}
}

func TestReviewPR_SkippedRepoPluginsAreNotesAndTransientFailuresAreNotConclusive(t *testing.T) {
	// Deterministic refusal: noted, conclusive.
	f := inRepoWorld()
	delete(f.git.repo("owner/repo").commits["basesha"], "plugins/acme-review/.claude-plugin/plugin.json")
	req, out := reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, gh.PRDetails{})
	if len(req.RepoPlugins) != 0 || len(req.RepoPluginNotes) != 1 || !strings.Contains(req.RepoPluginNotes[0], "acme-review in marketplace acme-tools") {
		t.Errorf("plugins=%v notes=%v", req.RepoPlugins, req.RepoPluginNotes)
	}
	if !out.Conclusive {
		t.Error("a deterministic refusal must stay conclusive")
	}
	// Transport failure: not memoised.
	f = inRepoWorld()
	f.git.blobErr = context.DeadlineExceeded
	_, out = reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: true, RepoPlugins: true}, gh.PRDetails{})
	if out.Conclusive {
		t.Error("a failed repo plugin read must make the outcome non-conclusive (ADR 1952)")
	}
}

func TestReviewPR_RepoPluginsAreIndependentOfTheOperatorVeto(t *testing.T) {
	// repo_review_skill: false stops the repo choosing a skill; repo_plugins is
	// the switch for loading plugins. Both are honoured independently.
	f := inRepoWorld()
	req, _ := reviewWithRepoPlugins(t, f, Config{RepoReviewSkill: false, RepoPlugins: true}, gh.PRDetails{})
	if len(req.RepoPlugins) != 1 {
		t.Errorf("repo_plugins on should load the named plugin regardless of repo_review_skill")
	}
	if req.ReviewSkill != DefaultReviewSkill {
		t.Errorf("the veto must still keep the repo from choosing a skill: %q", req.ReviewSkill)
	}
}
