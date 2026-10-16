package pruefer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// checkIsolated reports whether args load no settings layer and exactly one
// plugin directory, the one Pruefer passed (#2306, R3). The tests apply it to
// real args and, for non-vacuity, to args with the guarantee removed.
func checkIsolated(args []string, pluginDir string) error {
	var plugins []string
	sawSources := false
	for i, a := range args {
		switch a {
		case "--setting-sources":
			if i+1 < len(args) && args[i+1] == "" {
				sawSources = true
			}
		case "--plugin-dir":
			if i+1 < len(args) {
				plugins = append(plugins, args[i+1])
			}
		}
	}
	if !sawSources {
		return fmt.Errorf("--setting-sources \"\" is absent: the profile's and the tree's settings (and plugins) would load")
	}
	if len(plugins) != 1 || plugins[0] != pluginDir {
		return fmt.Errorf("plugin dirs = %v, want exactly [%s]", plugins, pluginDir)
	}
	return nil
}

func withoutPair(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// R3: under two different CLAUDE_CONFIG_DIR values, in both the daemon's
// environment and the request, the args still load no settings layer and only
// Pruefer's own plugin, and the review process carries only the resolved dir.
func TestBuildReviewArgs_OnlyPrueferPluginUnderAnyProfile(t *testing.T) {
	for _, profile := range []string{"/profiles/one", "/profiles/two"} {
		t.Run(profile, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", profile)
			rp, err := setupReviewPlugin()
			if err != nil {
				t.Fatal(err)
			}
			defer rp.Close()
			req := reviewTestScope(t)
			req.ClaudeConfigDir = profile
			req.PluginDirs, req.SkillRef = []string{rp.Dir}, rp.Ref
			args := mustReviewArgs(t, req)
			if err := checkIsolated(args, rp.Dir); err != nil {
				t.Fatal(err)
			}
			// Non-vacuity: removing the flag pair, or adding a second plugin
			// dir, must be caught.
			if checkIsolated(withoutPair(args, "--setting-sources"), rp.Dir) == nil {
				t.Error("checker accepted args without --setting-sources")
			}
			if checkIsolated(append(slices.Clone(args), "--plugin-dir", "/elsewhere"), rp.Dir) == nil {
				t.Error("checker accepted a second plugin dir")
			}
			// The review process sees only the resolved profile.
			env := reviewProcessEnv([]string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/daemon/inherited"}, req)
			var got []string
			for _, kv := range env {
				if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
					got = append(got, kv)
				}
			}
			if want := []string{"CLAUDE_CONFIG_DIR=" + profile}; !slices.Equal(got, want) {
				t.Errorf("CLAUDE_CONFIG_DIR entries = %v, want %v", got, want)
			}
		})
	}
}

func TestBuildReviewToolRules_GrantsOnlyTheScopedSkill(t *testing.T) {
	allow, _ := buildReviewToolRules("/clone", "", reviewplugin.DefaultRef)
	if !slices.Contains(allow, "Skill(pruefer-review:review)") {
		t.Errorf("scoped Skill grant missing: %v", allow)
	}
	if slices.Contains(allow, "Skill") {
		t.Error("bare Skill grant present")
	}
}

func TestCheckReviewArgsScoped_RejectsWiderSkillGrants(t *testing.T) {
	good := mustReviewArgs(t, reviewTestScope(t))
	if err := checkReviewArgsScoped(good, reviewplugin.DefaultRef); err != nil {
		t.Fatal(err)
	}
	for _, widen := range []string{"Skill", "Skill(*)", "Skill(pruefer-review:*)", "Skill(other-plugin:review)", "Skill()"} {
		if checkReviewArgsScoped(append(slices.Clone(good), "--allowedTools", widen), reviewplugin.DefaultRef) == nil {
			t.Errorf("checker accepted %q", widen)
		}
	}
}

func TestSetupReviewPlugin_DistinctDirsAndCleanup(t *testing.T) {
	const n = 8
	plugins := make([]*reviewPlugin, n)
	var wg sync.WaitGroup
	for i := range plugins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := setupReviewPlugin()
			if err != nil {
				t.Error(err)
				return
			}
			plugins[i] = p
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range plugins {
		if p == nil {
			t.FailNow()
		}
		if seen[p.Dir] {
			t.Errorf("directory %s shared between reviews", p.Dir)
		}
		seen[p.Dir] = true
		if p.Ref != "pruefer-review:review" || p.Plugin != "pruefer-review" {
			t.Errorf("ref/plugin = %q/%q", p.Ref, p.Plugin)
		}
		if _, err := os.Stat(filepath.Join(p.Dir, ".claude-plugin", "plugin.json")); err != nil {
			t.Errorf("manifest missing: %v", err)
		}
	}
	for _, p := range plugins {
		p.Close()
		p.Close() // idempotent
		if _, err := os.Stat(p.Dir); !os.IsNotExist(err) {
			t.Errorf("%s still present after Close: %v", p.Dir, err)
		}
	}
}

// fakeClaudeRecording installs a fake claude that records its arguments (one
// "[arg]" per line), the CLAUDE_CONFIG_DIR it saw, and whether the plugin
// manifest existed while it ran, then emits stream.
func fakeClaudeRecording(t *testing.T, rec, stream string) {
	t.Helper()
	script := `prev=""
for a in "$@"; do
  printf '[%s]\n' "$a" >> "` + rec + `.args"
  if [ "$prev" = "--plugin-dir" ]; then
    echo "$a" > "` + rec + `.plugindir"
    if [ -f "$a/.claude-plugin/plugin.json" ] && [ -f "$a/skills/review/SKILL.md" ]; then echo present > "` + rec + `.during"; else echo missing > "` + rec + `.during"; fi
  fi
  prev="$a"
done
echo "$CLAUDE_CONFIG_DIR" > "` + rec + `.profile"
cat <<'STREAM'
` + stream + `
STREAM
`
	writeFakeClaude(t, script)
}

const resultLine = `{"type":"result","result":"ok","is_error":false,"num_turns":3,"total_cost_usd":0.01}`
const skillOK = `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"pruefer-review:review"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"s1","is_error":false,"content":"loaded"}]}}
` + resultLine

func TestRealClaudeInvoker_Review_PluginDirLifecycleAndLog(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeRecording(t, rec, skillOK)
	var mu sync.Mutex
	var lines []string
	old := Logf
	Logf = func(pr int, tag, format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	t.Cleanup(func() { Logf = old })

	_, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc", WorkDir: t.TempDir(), ClaudeConfigDir: "/profiles/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(suffix string) string {
		b, err := os.ReadFile(rec + suffix)
		if err != nil {
			t.Fatalf("reading %s: %v", suffix, err)
		}
		return strings.TrimSpace(string(b))
	}
	dir := read(".plugindir")
	if read(".during") != "present" {
		t.Error("the plugin was not on disk while claude ran")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("plugin dir %s not removed after the review: %v", dir, err)
	}
	args := read(".args")
	if !strings.Contains(args, "[--setting-sources]\n[]") || !strings.Contains(args, "[Skill(pruefer-review:review)]") {
		t.Errorf("args lack isolation or the scoped Skill grant:\n%s", args)
	}
	if got := read(".profile"); got != "/profiles/x" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q", got)
	}
	// R6
	found := false
	for _, l := range lines {
		if strings.Contains(l, "review skill: pruefer-review:review (plugin pruefer-review v"+reviewplugin.Version()+", embedded, dir "+dir+")") {
			found = true
		}
	}
	if !found {
		t.Errorf("R6 log line missing; got %q", lines)
	}
}

func TestRealClaudeInvoker_Review_PluginDirRemovedOnEveryExit(t *testing.T) {
	for name, script := range map[string]string{
		"process error": "exit 1\n",
		"unparseable":   "printf 'not json\\n'\n",
		"is_error":      `printf '%s\n' '{"type":"result","result":"x","is_error":true}'` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec")
			writeFakeClaude(t, `for a in "$@"; do if [ "$prev" = "--plugin-dir" ]; then echo "$a" > "`+rec+`"; fi; prev="$a"; done`+"\n"+script)
			_, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
			if err == nil {
				t.Fatal("expected an error")
			}
			b, rerr := os.ReadFile(rec)
			if rerr != nil {
				t.Fatalf("fake claude never saw --plugin-dir: %v", rerr)
			}
			if _, err := os.Stat(strings.TrimSpace(string(b))); !os.IsNotExist(err) {
				t.Errorf("plugin dir leaked after %s: %v", name, err)
			}
		})
	}
}

func TestRealClaudeInvoker_Review_FailsWhenSkillNotRun(t *testing.T) {
	cases := map[string]string{
		"never invoked": `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/x"}}]}}
` + resultLine,
		"skill errored": `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"pruefer-review:review"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"s1","is_error":true,"content":"Unknown skill"}]}}
` + resultLine,
		"only a subagent invoked it": `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"pruefer-review:review"}}]}}
` + resultLine,
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			fakeClaudeRecording(t, filepath.Join(t.TempDir(), "rec"), stream)
			res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
			if err == nil {
				t.Fatalf("expected the review to fail, got %+v", res)
			}
			if res.Text != "" {
				t.Errorf("a failed review must carry no text, got %q", res.Text)
			}
		})
	}
	// Non-vacuity: the same fixtures pass once the skill really ran.
	fakeClaudeRecording(t, filepath.Join(t.TempDir(), "rec"), skillOK)
	if _, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()}); err != nil {
		t.Errorf("a review that ran the skill failed: %v", err)
	}
}

func TestCheckSkillRan(t *testing.T) {
	if err := checkSkillRan([]byte(resultLine+"\n"), "pruefer-review:review"); err != nil {
		t.Errorf("a bare result object cannot be judged and must pass: %v", err)
	}
	if err := checkSkillRan([]byte(skillOK+"\n"), "pruefer-review:review"); err != nil {
		t.Errorf("ran skill rejected: %v", err)
	}
	other := strings.ReplaceAll(skillOK, "pruefer-review:review", "someone-else:review")
	if checkSkillRan([]byte(other+"\n"), "pruefer-review:review") == nil {
		t.Error("a same-named skill from another plugin counted as ours")
	}
}

// R4/R5 end to end: with no customization the request reaches the invoker with
// no guidance layer, and the prompt it produces invokes the default skill; a
// repo whose only customization is a replace-mode review skill reaches it as
// replacement guidance, with the contract unchanged.
func TestReviewPR_DefaultAndRepoOnlyGuidanceShapes(t *testing.T) {
	run := func(skill string) ReviewRequest {
		client := newFakeReviewer()
		if skill != "" {
			client.repoSkillData = []byte(skill)
		}
		claude := &mockClaudeInvoker{}
		clone, _ := fakeClone(t, nil)
		pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", BaseRef: "main"}
		out := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, nil)
		if !out.Reviewed || out.Err != nil {
			t.Fatalf("outcome = %+v", out)
		}
		return claude.callsSnapshot()[0]
	}

	def := run("")
	if def.OperatorGuidance != "" || def.RepoGuidance != "" {
		t.Errorf("zero-config request carries guidance: %+v", def)
	}
	p := buildReviewPrompt(def)
	if !strings.Contains(p, "invoking the `pruefer-review:review` skill") || strings.Contains(p, "Additional review guidance") {
		t.Errorf("zero-config prompt wrong:\n%s", p)
	}

	repl := run("---\nmode: replace\n---\nOnly check for %w wrapping.\n")
	p = buildReviewPrompt(repl)
	if !strings.Contains(p, "Only check for %w wrapping.") || !strings.Contains(p, "replaces the skill's criteria") {
		t.Errorf("replace-only repo prompt wrong:\n%s", p)
	}
	var contract strings.Builder
	renderContract(&contract, def.Staged != nil)
	if !strings.HasSuffix(p, contract.String()) || !strings.HasSuffix(buildReviewPrompt(def), contract.String()) {
		t.Error("the output contract differs between the default and replace prompts")
	}
}
