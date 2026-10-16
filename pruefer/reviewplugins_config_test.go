package pruefer

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

func TestLoadConfig_ReviewPluginSettings(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")

	cfg, err := LoadConfig([]string{"-config", missing})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewSkill != "" || len(cfg.Plugins) != 0 || !cfg.RepoReviewSkill {
		t.Errorf("defaults = %q %v repo=%v, want empty, none, true", cfg.ReviewSkill, cfg.Plugins, cfg.RepoReviewSkill)
	}

	path := writeYAMLConfig(t, dir, "plugins:\n  - /a\n  - /b\nreview_skill: ours:deep\nrepo_review_skill: false\n")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Plugins, []string{"/a", "/b"}) || cfg.ReviewSkill != "ours:deep" || cfg.RepoReviewSkill {
		t.Errorf("yaml = %v %q repo=%v", cfg.Plugins, cfg.ReviewSkill, cfg.RepoReviewSkill)
	}

	t.Setenv("PRUEFER_PLUGINS", "/c, /d")
	t.Setenv("PRUEFER_REVIEW_SKILL", "env:skill")
	t.Setenv("PRUEFER_REPO_REVIEW_SKILL", "true")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Plugins, []string{"/c", "/d"}) || cfg.ReviewSkill != "env:skill" || !cfg.RepoReviewSkill {
		t.Errorf("env = %v %q repo=%v, want env to override yaml", cfg.Plugins, cfg.ReviewSkill, cfg.RepoReviewSkill)
	}

	cfg, err = LoadConfig([]string{"-config", path, "-plugins", "/e", "-review-skill", "flag:skill", "-repo-review-skill=false"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Plugins, []string{"/e"}) || cfg.ReviewSkill != "flag:skill" || cfg.RepoReviewSkill {
		t.Errorf("flags = %v %q repo=%v, want flags to override env", cfg.Plugins, cfg.ReviewSkill, cfg.RepoReviewSkill)
	}
}

func TestLoadConfig_RejectsAMalformedReviewSkill(t *testing.T) {
	path := writeYAMLConfig(t, t.TempDir(), "review_skill: just-a-name\n")
	if _, err := LoadConfig([]string{"-config", path}); err == nil || !strings.Contains(err.Error(), "review_skill") {
		t.Errorf("err = %v, want a review_skill refusal", err)
	}
}

func TestReload_ReviewPluginSettingsAreLive(t *testing.T) {
	old := Config{RepoReviewSkill: true}
	cand := Config{Plugins: []string{"/p"}, ReviewSkill: "a:b", RepoReviewSkill: false}
	merged, diff := applyConfigReload(old, cand)
	if !slices.Equal(merged.Plugins, cand.Plugins) || merged.ReviewSkill != "a:b" || merged.RepoReviewSkill {
		t.Errorf("merged = %+v", merged)
	}
	got := map[string]bool{}
	for _, c := range diff.FieldsChanged {
		got[c.Field] = true
	}
	for _, f := range []string{"Plugins", "ReviewSkill", "RepoReviewSkill"} {
		if !got[f] {
			t.Errorf("%s not applied live", f)
		}
	}
	if len(diff.RestartOnlyChanged) != 0 {
		t.Errorf("restart-only changes: %+v", diff.RestartOnlyChanged)
	}
}

func strPtr(s string) *string { return &s }

func TestSelectReviewSkill(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		repo     yamlRepoConfig
		wantRef  string
		wantNote string
	}{
		{"default", Config{RepoReviewSkill: true}, yamlRepoConfig{}, DefaultReviewSkill, ""},
		{"operator choice", Config{ReviewSkill: "op:s", RepoReviewSkill: true}, yamlRepoConfig{}, "op:s", ""},
		{"repo overrides", Config{ReviewSkill: "op:s", RepoReviewSkill: true}, yamlRepoConfig{ReviewSkill: strPtr("repo:s")}, "repo:s", ""},
		{"repo overrides the default", Config{RepoReviewSkill: true}, yamlRepoConfig{ReviewSkill: strPtr(" repo:s ")}, "repo:s", ""},
		{"veto", Config{ReviewSkill: "op:s", RepoReviewSkill: false}, yamlRepoConfig{ReviewSkill: strPtr("repo:s")}, "op:s", "ignored: repo_review_skill is false"},
		{"veto with default operator", Config{}, yamlRepoConfig{ReviewSkill: strPtr("repo:s")}, DefaultReviewSkill, "ignored: repo_review_skill is false"},
		{"blank repo value is no override", Config{RepoReviewSkill: true}, yamlRepoConfig{ReviewSkill: strPtr("  ")}, DefaultReviewSkill, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, note := selectReviewSkill(tc.cfg, tc.repo)
			if ref != tc.wantRef || (tc.wantNote == "") != (note == "") || !strings.Contains(note, tc.wantNote) {
				t.Errorf("selectReviewSkill = %q, %q; want %q, note containing %q", ref, note, tc.wantRef, tc.wantNote)
			}
		})
	}
}

func TestSelectReviewSkill_RepoValueIsSanitisedInTheLog(t *testing.T) {
	_, note := selectReviewSkill(Config{}, yamlRepoConfig{ReviewSkill: strPtr("x:y`@bob\n# hi")})
	if strings.ContainsAny(note, "`@\n#") {
		t.Errorf("note not sanitised: %q", note)
	}
}

// ADR 1642 AC4 still holds: review_skill is the only new repo key, and every
// operator-only key stays unparseable from a repo file.
func TestFetchRepoConfig_ReviewSkillRecognisedOperatorKeysStillNot(t *testing.T) {
	data := []byte("review_skill: mine:deep\nplugins:\n  - /etc\nrepo_review_skill: true\nreview_guidance: be nice\nmodel: opus\n")
	rc, prov := fetchRepoConfig(newFakeContentsClient(data, nil), "o", "r", "sha")
	if rc.ReviewSkill == nil || *rc.ReviewSkill != "mine:deep" {
		t.Errorf("review_skill not parsed: %+v", rc)
	}
	want := []string{"model", "plugins", "repo_review_skill", "review_guidance"}
	if !equalStrings(prov.UnknownKeys, want) {
		t.Errorf("UnknownKeys = %v, want %v (operator-only keys must not be recognised from a repo)", prov.UnknownKeys, want)
	}
	// applyRepoNarrowing never carries the selection into Config.
	eff, _ := applyRepoNarrowing(Config{}, rc)
	if eff.ReviewSkill != "" || eff.Plugins != nil {
		t.Errorf("the repo reached operator-only Config fields: %+v", eff)
	}
}

// --- ReviewPR end to end (mock invoker) ---

func reviewWith(t *testing.T, client *fakeReviewer, cfg Config) (ReviewRequest, ReviewOutcome) {
	t.Helper()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "mock review", NumTurns: 2, CostUSD: 0.01}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "main"}
	out := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !out.Reviewed || out.Err != nil {
		t.Fatalf("outcome = %+v", out)
	}
	return claude.callsSnapshot()[0], out
}

func TestReviewPR_PassesBaseRefPluginsAndSelectionToTheInvoker(t *testing.T) {
	client := newFakeReviewer()
	client.setRepoPlugins(goodPluginFiles())
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	req, _ := reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.BaseSHA != "basesha" || req.ReviewSkill != "mine:deep" {
		t.Errorf("BaseSHA/ReviewSkill = %q/%q", req.BaseSHA, req.ReviewSkill)
	}
	if len(req.RepoPlugins) != 1 || req.RepoPlugins[0].Dir != "mine" {
		t.Errorf("RepoPlugins = %+v", req.RepoPlugins)
	}
	if !slices.Equal(client.treeCalls, []string{"basesha"}) {
		t.Errorf("plugin tree read at %v, want only the pinned base SHA (never the head)", client.treeCalls)
	}
}

// The operator veto: the repo's review_skill is ignored, but its plugins still
// load (the veto governs selection, not loading).
func TestReviewPR_RepoReviewSkillFalseIgnoresTheRepoSelection(t *testing.T) {
	logs := capturePluginLogs(t)
	client := newFakeReviewer()
	client.setRepoPlugins(goodPluginFiles())
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	req, _ := reviewWith(t, client, Config{ReviewSkill: "op:skill", RepoReviewSkill: false})
	if req.ReviewSkill != "op:skill" {
		t.Errorf("ReviewSkill = %q, want the operator's", req.ReviewSkill)
	}
	if len(req.RepoPlugins) != 1 {
		t.Errorf("repo plugins must still load under the veto: %+v", req.RepoPlugins)
	}
	if countContaining(logs(), `repo review_skill "mine:deep" ignored: repo_review_skill is false`) != 1 {
		t.Errorf("the log must say the repo's value was ignored: %q", logs())
	}
}

// A repo config present only on the PR head cannot alter selection, and a plugin
// present only on the head never reaches the invoker.
func TestReviewPR_HeadOnlyConfigAndPluginsHaveNoEffect(t *testing.T) {
	client := newFakeReviewer()
	client.setRepoPlugins(goodPluginFiles())
	client.pluginsOnlyAtSHA = "head-sha"
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	client.repoConfigOnlyAtRef = "head-sha"
	req, _ := reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.ReviewSkill != DefaultReviewSkill || len(req.RepoPlugins) != 0 {
		t.Errorf("head-only content reached the review: skill=%q plugins=%+v", req.ReviewSkill, req.RepoPlugins)
	}
	// Non-vacuity: with the same content at the base SHA both take effect.
	client = newFakeReviewer()
	client.setRepoPlugins(goodPluginFiles())
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	client.pluginsOnlyAtSHA, client.repoConfigOnlyAtRef = "basesha", "basesha"
	req, _ = reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.ReviewSkill != "mine:deep" || len(req.RepoPlugins) != 1 {
		t.Errorf("fixture has no effect even at the base: skill=%q plugins=%+v", req.ReviewSkill, req.RepoPlugins)
	}
}

func TestReviewPR_UnresolvableBaseLoadsNoRepoPluginsAndIsNotConclusive(t *testing.T) {
	client := newFakeReviewer()
	client.setRepoPlugins(goodPluginFiles())
	client.baseSHAErr = context.DeadlineExceeded
	req, out := reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.BaseSHA != "" || len(req.RepoPlugins) != 0 || len(client.treeCalls) != 0 {
		t.Errorf("no base SHA must mean no repo plugin read: %+v / %v", req, client.treeCalls)
	}
	if out.Conclusive {
		t.Error("an outcome shaped by a failed base read must not be memoised")
	}
	if !strings.Contains(strings.Join(req.PluginNotes, ";"), "base commit could not be resolved") {
		t.Errorf("notes = %v", req.PluginNotes)
	}
	for _, c := range client.fileAtRefCallArgs() {
		if c.ref != "main" {
			t.Errorf("without a pin the reads fall back to the branch name, got %q", c.ref)
		}
	}
}

func TestReviewPR_UnavailableRepoPluginTreeIsNotConclusive(t *testing.T) {
	client := newFakeReviewer()
	client.pluginTreeErr = context.DeadlineExceeded
	_, out := reviewWith(t, client, Config{})
	if out.Conclusive {
		t.Error("a failed plugin-tree read must make the outcome non-conclusive")
	}
}

func TestReviewPR_PostsTheFallbackNotice(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "mock review", FallbackFrom: "mine:`@evil`", FallbackReason: "it did not resolve: plugin \"mine\" is not loaded"}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "main"}
	out := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !out.Reviewed || out.FallbackFrom == "" {
		t.Fatalf("outcome = %+v", out)
	}
	body := client.submitCalls[0].body
	for _, want := range []string{"Review skill fallback", "`basesha`", "plugin \"mine\" is not loaded"[:6], "did not resolve", "`" + reviewplugin.DefaultRef + "`"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@evil") || strings.Contains(body, "`@") {
		t.Errorf("repo-controlled text not neutralised:\n%s", body)
	}
	if !strings.HasPrefix(body, "> ") {
		t.Errorf("notice should lead the body:\n%s", body)
	}
	// No notice on an ordinary review.
	client2 := newFakeReviewer()
	reviewWith(t, client2, Config{})
	if strings.Contains(client2.submitCalls[0].body, "fallback") {
		t.Errorf("an ordinary review carries a fallback notice:\n%s", client2.submitCalls[0].body)
	}
}

func TestFallbackNotice_NamesRefSHAAndReason(t *testing.T) {
	n := fallbackNotice("mine:deep", "0123456789abcdef0123456789abcdef01234567", "it failed to load")
	for _, want := range []string{"`mine:deep`", "`0123456789abcdef0123456789abcdef01234567`", "it failed to load", "`pruefer-review:review`"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice lacks %q: %s", want, n)
		}
	}
	if !strings.Contains(fallbackNotice("x:y", "", "r"), "`unknown`") {
		t.Error("an unknown base SHA must be stated as unknown")
	}
}

func TestCheckReviewArgsScoped_AcceptsOnlyTheNamedSkills(t *testing.T) {
	req := reviewTestScope(t)
	req.SkillRef = "mine:deep"
	args := mustReviewArgs(t, req)
	if err := checkReviewArgsScoped(args, "mine:deep"); err != nil {
		t.Fatal(err)
	}
	// The selected skill's grant is not acceptable where only the default is allowed.
	if checkReviewArgsScoped(args, reviewplugin.DefaultRef) == nil {
		t.Error("a grant for a skill outside the allowed set was accepted")
	}
	if checkReviewArgsScoped(args) == nil {
		t.Error("with no allowed skill every Skill grant must be rejected")
	}
	for _, widen := range []string{"Skill", "Skill(*)", "Skill(mine:*)", "Skill(other:deep)", "Skill()"} {
		if checkReviewArgsScoped(append(slices.Clone(args), "--allowedTools", widen), "mine:deep") == nil {
			t.Errorf("checker accepted %q", widen)
		}
	}
	// A wildcard in an "allowed" ref never becomes an accepted rule.
	wild := append(slices.Clone(args), "--allowedTools", "Skill(mine:*)")
	if checkReviewArgsScoped(wild, "mine:*") == nil {
		t.Error("a wildcard ref was accepted as an allowed skill")
	}
}
