package pruefer

import (
	"context"
	"fmt"
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
	data := []byte("review_skill: mine:deep\nplugins:\n  - acme@tools\nrepo_review_skill: true\nrepo_plugins: true\nrepo_plugin_marketplaces:\n  - evil/x\nreview_guidance: be nice\nmodel: opus\n")
	rc, prov := fetchRepoConfig(newFakeContentsClient(data, nil), "o", "r", "sha")
	if rc.ReviewSkill == nil || *rc.ReviewSkill != "mine:deep" {
		t.Errorf("review_skill not parsed: %+v", rc)
	}
	// plugins (#2366) is a repo key (<plugin>@<marketplace> entries); the
	// operator switches that gate and widen it are not.
	if !equalStrings(rc.Plugins, []string{"acme@tools"}) {
		t.Errorf("plugins not parsed: %+v", rc.Plugins)
	}
	want := []string{"model", "repo_plugin_marketplaces", "repo_plugins", "repo_review_skill", "review_guidance"}
	if !equalStrings(prov.UnknownKeys, want) {
		t.Errorf("UnknownKeys = %v, want %v (operator-only keys must not be recognised from a repo)", prov.UnknownKeys, want)
	}
	// applyRepoNarrowing never carries the selection into Config.
	eff, _ := applyRepoNarrowing(Config{}, rc)
	if eff.ReviewSkill != "" || eff.Plugins != nil || eff.RepoPlugins || eff.RepoPluginMarketplaces != nil {
		t.Errorf("the repo reached operator-only Config fields: %+v", eff)
	}
}

// --- ReviewPR end to end (mock invoker) ---

func reviewWith(t *testing.T, client *fakeReviewer, cfg Config) (ReviewRequest, ReviewOutcome) {
	t.Helper()
	return reviewWithPR(t, client, cfg, gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "head-sha", BaseRef: "main"})
}

func reviewWithPR(t *testing.T, client *fakeReviewer, cfg Config, pr gh.PRDetails) (ReviewRequest, ReviewOutcome) {
	t.Helper()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "mock review", NumTurns: 2, CostUSD: 0.01}, nil
	}}
	clone, _ := fakeClone(t, nil)
	out := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !out.Reviewed || out.Err != nil {
		t.Fatalf("outcome = %+v", out)
	}
	return claude.callsSnapshot()[0], out
}

func TestReviewPR_PassesPinnedBaseSHAAndSelectionToTheInvoker(t *testing.T) {
	client := newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	op := writeTestPlugin(t, filepath.Join(t.TempDir(), "mine"), "mine", "1", "deep")
	req, _ := reviewWith(t, client, Config{RepoReviewSkill: true, Plugins: []string{op}})
	if req.BaseSHA != "basesha" || req.ReviewSkill != "mine:deep" {
		t.Errorf("BaseSHA/ReviewSkill = %q/%q", req.BaseSHA, req.ReviewSkill)
	}
	if len(req.OperatorPluginDirs) != 1 {
		t.Errorf("OperatorPluginDirs = %v, want the operator's one plugin", req.OperatorPluginDirs)
	}
	if client.resolveCalls != 1 {
		t.Errorf("the base was resolved %d times, want once per review", client.resolveCalls)
	}
}

// The operator veto: the repo's review_skill is ignored and the log says so.
func TestReviewPR_RepoReviewSkillFalseIgnoresTheRepoSelection(t *testing.T) {
	logs := capturePluginLogs(t)
	client := newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	req, _ := reviewWith(t, client, Config{ReviewSkill: "op:skill", RepoReviewSkill: false})
	if req.ReviewSkill != "op:skill" {
		t.Errorf("ReviewSkill = %q, want the operator's", req.ReviewSkill)
	}
	if countContaining(logs(), `repo review_skill "mine:deep" ignored: repo_review_skill is false`) != 1 {
		t.Errorf("the log must say the repo's value was ignored: %q", logs())
	}
	// Non-vacuity: with the veto off the same fixture selects the repo's skill.
	client = newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	req, _ = reviewWith(t, client, Config{ReviewSkill: "op:skill", RepoReviewSkill: true})
	if req.ReviewSkill != "mine:deep" {
		t.Errorf("fixture selects nothing even without the veto: %q", req.ReviewSkill)
	}
}

// With repo_plugins off (the default) no code from the reviewed repo is loaded
// (#2307, #2366): a repo's review_skill can only name a skill of the embedded
// plugin or an operator plugin, so with none configured the invoker is handed
// no plugin directory at all, whatever the repo's base holds.
func TestReviewPR_NoRepoSuppliedPluginReachesTheInvoker(t *testing.T) {
	client := newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	req, _ := reviewWith(t, client, Config{RepoReviewSkill: true})
	if len(req.OperatorPluginDirs) != 0 {
		t.Errorf("plugin directories reached the invoker with none configured: %v", req.OperatorPluginDirs)
	}
	if req.ReviewSkill != "mine:deep" {
		t.Errorf("selection must be unchanged: %q", req.ReviewSkill)
	}
}

// A repo config present only on the PR head cannot alter selection.
func TestReviewPR_HeadOnlyConfigHasNoEffect(t *testing.T) {
	client := newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	client.repoConfigOnlyAtRef = "head-sha"
	req, _ := reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.ReviewSkill != DefaultReviewSkill {
		t.Errorf("head-only config reached the review: skill=%q", req.ReviewSkill)
	}
	// Non-vacuity: with the same content at the base SHA it takes effect.
	client = newFakeReviewer()
	client.repoConfigData = []byte("review_skill: mine:deep\n")
	client.repoConfigOnlyAtRef = "basesha"
	req, _ = reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.ReviewSkill != "mine:deep" {
		t.Errorf("fixture has no effect even at the base: skill=%q", req.ReviewSkill)
	}
}

func TestReviewPR_UnresolvableBaseFallsBackToTheBranchAndIsNotConclusive(t *testing.T) {
	client := newFakeReviewer()
	client.baseSHAErr = context.DeadlineExceeded
	req, out := reviewWith(t, client, Config{RepoReviewSkill: true})
	if req.BaseSHA != "" {
		t.Errorf("no base SHA expected: %+v", req)
	}
	if out.Conclusive {
		t.Error("an outcome shaped by a failed base read must not be memoised")
	}
	for _, c := range client.fileAtRefCallArgs() {
		if c.ref != "main" {
			t.Errorf("without a pin the reads fall back to the branch name, got %q", c.ref)
		}
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

// The reason keeps its quotes, parentheses, commas and semicolons, sits in a code
// span, and still cannot carry a backtick, mention or issue reference.
func TestFallbackNotice_ReasonStaysReadableAndSafe(t *testing.T) {
	reason := "it did not resolve: plugin \"mine\" is not loaded (operator plugin a skipped: x; operator plugin b skipped: y, z)"
	n := fallbackNotice("mine:deep", "abc", reason)
	if !strings.Contains(n, "(`"+reason+"`)") {
		t.Errorf("reason not kept verbatim in a code span: %s", n)
	}
	hostile := fallbackNotice("mine:deep", "abc", "bad `x` @evil #12\n> quote")
	for _, bad := range []string{"@evil", "#12", "`x`", "\n"} {
		if strings.Contains(hostile, bad) {
			t.Errorf("hostile reason leaked %q: %s", bad, hostile)
		}
	}
}

func TestFallbackNotice_KeepsPluginNotesBeyondTheReferenceCap(t *testing.T) {
	reason := "it did not resolve: plugin \"mine\" is not loaded (operator plugin mine-dir skipped: its manifest name \"x\" is reserved; operator plugin other skipped: \"shared\" is already loaded from a operator plugin, which is not replaced)"
	n := fallbackNotice("mine:deep", "abc", reason)
	for _, want := range []string{"mine-dir skipped", "other skipped", "is not replaced"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice lost %q: %s", want, n)
		}
	}
	if long := fallbackNotice("x:y", "abc", strings.Repeat("a", 2000)); len(long) > 900 {
		t.Errorf("reason not capped: %d", len(long))
	}
}

// The repo_review_skill env var is the operator's veto, so a value that does
// not parse must close the veto (and say so) rather than leave it open.
func TestLoadConfig_UnparseableRepoReviewSkillEnvFailsClosed(t *testing.T) {
	var logged strings.Builder
	prev := Logf
	Logf = func(_ int, tag, format string, args ...any) { fmt.Fprintf(&logged, "["+tag+"] "+format, args...) }
	t.Cleanup(func() { Logf = prev })

	t.Setenv("PRUEFER_REPO_REVIEW_SKILL", "flase")
	cfg, err := LoadConfig([]string{"-config", writeYAMLConfig(t, t.TempDir(), "review_skill: ours:deep\n")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoReviewSkill {
		t.Error("an unparseable PRUEFER_REPO_REVIEW_SKILL left the repo override enabled")
	}
	if !strings.Contains(logged.String(), "PRUEFER_REPO_REVIEW_SKILL") {
		t.Errorf("no warning logged: %q", logged.String())
	}
}
