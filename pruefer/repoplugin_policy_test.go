package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// preapprovingRepoPlugin is a repo plugin "mine" whose skill pre-approves Bash
// (allowed-tools frontmatter, which the CLI honours past the review grant) and
// which carries a hook script.
func preapprovingRepoPlugin() loadedRepoPlugin {
	p := repoDeepPlugin()
	p.Tree = &repoPluginTree{Files: []repoPluginFile{
		{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"mine","version":"3.1.0"}`)},
		{Path: "skills/deep/SKILL.md", Data: []byte("---\ndescription: x\nallowed-tools: Bash(curl:*)\n---\nReview deeply.\n")},
		{Path: "hooks/hooks.json", Data: []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"sh hooks/run.sh"}]}]}}`)},
		{Path: "hooks/run.sh", Data: []byte("#!/bin/sh\n"), Exec: true},
	}}
	return p
}

// reviewWithPolicyPlugin runs a Review whose single repo plugin pre-approves
// Bash and has a hook, and returns what the repo plugin directory the CLI was
// handed contained: its SKILL.md and whether the hook script was present.
func reviewWithPolicyPlugin(t *testing.T, grant reviewGrant) (skill string, hookPresent bool, res ReviewResult) {
	t.Helper()
	rec := filepath.Join(t.TempDir(), "rec")
	selFile := rec + ".sel"
	if err := os.WriteFile(selFile, []byte(selStream+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeClaude(t, `echo "$*" >> "`+rec+`.calls"
for a in "$@"; do
  case "$a" in
    */pruefer-repo-plugins-*|*/repo/*|*/policy-*)
      cat "$a/skills/deep/SKILL.md" >> "`+rec+`.skill" 2>/dev/null
      if [ -e "$a/hooks/run.sh" ] || [ -e "$a/hooks/hooks.json" ]; then echo yes > "`+rec+`.hook"; fi ;;
  esac
done
cat "`+selFile+`"
`)
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
		PRNumber: 1, WorkDir: t.TempDir(), ReviewSkill: "mine:deep", BaseSHA: "basesha",
		RepoPlugins: []loadedRepoPlugin{preapprovingRepoPlugin()}, Grant: grant,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rec + ".skill")
	_, herr := os.Stat(rec + ".hook")
	return string(b), herr == nil, res
}

// ADR 2308 applies to a repo plugin as it does to an operator plugin: an outside
// author's review loads it without hooks, MCP servers and tool pre-approvals.
func TestReview_RepoPluginIsReadOnlyForAnOutsideAuthor(t *testing.T) {
	skill, hook, res := reviewWithPolicyPlugin(t, reviewGrant{StripPluginHooks: true})
	if hook {
		t.Error("an outside author's review loaded the repo plugin's hooks")
	}
	if !strings.Contains(skill, "Review deeply.") || strings.Contains(skill, "allowed-tools") {
		t.Errorf("skill should load without its pre-approval: %q", skill)
	}
	if res.FallbackFrom != "" {
		t.Errorf("the stripped plugin must still run its skill: %+v", res)
	}
	if len(res.PluginsStripped) != 1 || !strings.Contains(res.PluginsStripped[0], "mine") {
		t.Errorf("the posted notice must name the stripped repo plugin: %v", res.PluginsStripped)
	}
}

// A trusted author keeps the plugin's hooks, but a pre-approval outside the
// review grant is dropped.
func TestReview_RepoPluginPreapprovalOutsideTheGrantIsDropped(t *testing.T) {
	skill, hook, res := reviewWithPolicyPlugin(t, reviewGrant{})
	if !hook {
		t.Error("a trusted author's review should keep the repo plugin's hooks")
	}
	if !strings.Contains(skill, "Review deeply.") || strings.Contains(skill, "curl") {
		t.Errorf("an out-of-grant pre-approval survived: %q", skill)
	}
	if res.FallbackFrom != "" {
		t.Errorf("res = %+v", res)
	}
}
