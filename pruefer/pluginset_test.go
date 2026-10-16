package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// writeTestPlugin creates a plugin directory with a manifest and one skill.
func writeTestPlugin(t *testing.T, dir, name, version, skill string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"name":%q,"version":%q}`, name, version)
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if skill != "" {
		sd := filepath.Join(dir, "skills", skill)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte("---\ndescription: x\n---\nDo it.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func capturePluginLogs(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	old := Logf
	Logf = func(pr int, tag, format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf("[%s] ", tag)+fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	t.Cleanup(func() { Logf = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func countContaining(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestResolveOperatorPluginDirs_ReportsOncePerPathAndAgainAfterRecovery(t *testing.T) {
	logs := capturePluginLogs(t)
	good := writeTestPlugin(t, t.TempDir(), "good", "1", "review")
	missing := filepath.Join(t.TempDir(), "gone")
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		got := resolveOperatorPluginDirs([]string{missing, good, file}, 1)
		if len(got) != 1 {
			t.Fatalf("round %d: dirs = %v, want only the readable one", i, got)
		}
		if want, _ := filepath.EvalSymlinks(good); got[0] != want {
			t.Errorf("dir = %q, want symlink-resolved %q", got[0], want)
		}
	}
	if n := countContaining(logs(), "gone"); n != 1 {
		t.Errorf("missing directory reported %d times, want once", n)
	}
	if n := countContaining(logs(), "afile"); n != 1 {
		t.Errorf("non-directory reported %d times, want once", n)
	}

	// Fixed, then broken again: reported again.
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveOperatorPluginDirs([]string{missing}, 1); len(got) != 1 {
		t.Fatalf("recovered path not used: %v", got)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	resolveOperatorPluginDirs([]string{missing}, 1)
	if n := countContaining(logs(), "gone"); n != 2 {
		t.Errorf("breakage after recovery reported %d times in total, want 2", n)
	}
}

func TestResolveOperatorPluginDirs_ExpandsHomeAndRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestPlugin(t, filepath.Join(home, "plugs", "p"), "p", "1", "")
	got := resolveOperatorPluginDirs([]string{"~/plugs/p"}, 1)
	want, _ := filepath.EvalSymlinks(filepath.Join(home, "plugs", "p"))
	if len(got) != 1 || got[0] != want {
		t.Errorf("~ not expanded: %v, want [%s]", got, want)
	}
	t.Chdir(home)
	got = resolveOperatorPluginDirs([]string{"plugs/p"}, 1)
	if len(got) != 1 || got[0] != want {
		t.Errorf("relative path not resolved against the working directory: %v", got)
	}
}

func TestBuildPluginSet_OrderReservedNameAndDuplicates(t *testing.T) {
	rp, err := setupReviewPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	opA := writeTestPlugin(t, filepath.Join(t.TempDir(), "a"), "opa", "2.0", "x")
	opReserved := writeTestPlugin(t, filepath.Join(t.TempDir(), "res"), reviewplugin.PluginName, "9", "review")
	opDupOfRepo := writeTestPlugin(t, filepath.Join(t.TempDir(), "d"), "shared", "1", "x")
	opNoManifest := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(opNoManifest, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := func(name string) repoPluginFile {
		return repoPluginFile{Path: ".claude-plugin/plugin.json", Data: []byte(fmt.Sprintf(`{"name":%q,"version":"3"}`, name))}
	}
	trees := []repoPluginTree{
		{Dir: "r1", Files: []repoPluginFile{manifest("shared")}},         // same manifest name as an operator plugin
		{Dir: "r2", Files: []repoPluginFile{manifest("repo-two")}},       // fine
		{Dir: "r3", Files: []repoPluginFile{manifest("pruefer-review")}}, // reserved
	}
	set := buildPluginSet(rp, []string{opA, opReserved, opDupOfRepo, opNoManifest}, trees)

	var names []string
	for _, p := range set.Plugins {
		names = append(names, p.Name+":"+p.Source)
	}
	want := []string{"pruefer-review:embedded", "opa:operator", "shared:operator", "plain:operator", "repo-two:repo"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("load order = %v, want %v", names, want)
	}
	if p, _ := set.byName("opa"); p.Version != "2.0" {
		t.Errorf("version = %q", p.Version)
	}
	joined := strings.Join(set.Notes, "\n")
	for _, w := range []string{"reserved for the embedded plugin", `"shared" is already loaded from a operator plugin`} {
		if !strings.Contains(joined, w) {
			t.Errorf("notes lack %q:\n%s", w, joined)
		}
	}
	if got := countContaining(set.Notes, "reserved"); got != 2 {
		t.Errorf("reserved name reported %d times, want 2 (operator and repo)", got)
	}
	// Notes are posted in fallback text: no local directory leaks into them.
	for _, n := range set.Notes {
		if strings.Contains(n, os.TempDir()) || strings.Contains(n, rp.Root) {
			t.Errorf("note leaks a local path: %s", n)
		}
	}
	// The embedded plugin is still the one passed first and was not replaced.
	if set.Plugins[0].Dir != rp.Dir {
		t.Errorf("embedded plugin not first: %v", set.Dirs())
	}
}

func TestBuildPluginSet_WritesRepoTreeUnderTheReviewRoot(t *testing.T) {
	rp, err := setupReviewPlugin()
	if err != nil {
		t.Fatal(err)
	}
	tree := repoPluginTree{Dir: "mine", Files: []repoPluginFile{
		{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"mine"}`)},
		{Path: "hooks/run.sh", Data: []byte("#!/bin/sh\n"), Exec: true},
		{Path: "skills/review/SKILL.md", Data: []byte("---\n---\nx")},
	}}
	set := buildPluginSet(rp, nil, []repoPluginTree{tree})
	p, ok := set.byName("mine")
	if !ok {
		t.Fatalf("repo plugin not loaded: %+v", set.Notes)
	}
	if !strings.HasPrefix(p.Dir, rp.Root+string(filepath.Separator)) {
		t.Errorf("repo plugin dir %s is outside the per-review root %s", p.Dir, rp.Root)
	}
	if fi, err := os.Stat(filepath.Join(p.Dir, "hooks", "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("executable bit not preserved: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(p.Dir, ".claude-plugin", "plugin.json")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("regular file mode wrong: %v %v", fi, err)
	}
	rp.Close()
	if _, err := os.Stat(p.Dir); !os.IsNotExist(err) {
		t.Errorf("repo plugin dir leaked after Close: %v", err)
	}
}

func TestWriteRepoPluginTree_RefusesEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../escape", "/abs", "a/../../b", "a//b", ""} {
		_, err := writeRepoPluginTree(root, repoPluginTree{Dir: "p", Files: []repoPluginFile{{Path: bad, Data: []byte("x")}}})
		if err == nil {
			t.Errorf("path %q was written", bad)
		}
	}
	if _, err := writeRepoPluginTree(root, repoPluginTree{Dir: "../up"}); err == nil {
		t.Error("a traversing directory name was accepted")
	}
	if entries, _ := os.ReadDir(filepath.Dir(root)); len(entries) == 0 {
		t.Fatal("sanity")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Error("a file escaped the root")
	}
}

func TestSanitizeNoticeText(t *testing.T) {
	got := sanitizeNoticeText("`evil`:[x](http://e) @user\n# head")
	for _, bad := range []string{"`", "[", "]", "(", ")", "@", "\n", "#", "http://"} {
		if bad != "http://" && strings.Contains(got, bad) {
			t.Errorf("%q survived: %q", bad, got)
		}
	}
	if long := sanitizeNoticeText(strings.Repeat("a", 500)); len(long) > 130 {
		t.Errorf("not capped: %d", len(long))
	}
	if got := sanitizeNoticeText("my-plugin:review.v2"); got != "my-plugin:review.v2" {
		t.Errorf("a plain reference changed: %q", got)
	}
}

func TestWellFormedSkillRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"pruefer-review:review": true, "a:b": true,
		"review": false, ":x": false, "x:": false, "": false, "a b:c": false, "a:`b`": false,
		"p:foo*": false, "p:foo(x)": false, "p:*": false, "p*:s": false,
	} {
		if got := wellFormedSkillRef(ref); got != want {
			t.Errorf("wellFormedSkillRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// A repo-controlled skill directory name must not widen or break the
// Skill(<ref>) allow rule: a name carrying "*", "(" or ")" falls back to the
// default skill instead of being spliced into the rule.
func TestChooseReviewSkill_RejectsRuleDelimitersInSkillName(t *testing.T) {
	root := t.TempDir()
	// One plugin holds all three skills so a single manifest name resolves them.
	dir := filepath.Join(root, "mine")
	for _, skill := range []string{"foo*", "foo(x)", "plain"} {
		writeTestPlugin(t, dir, "mine", "1", skill)
	}
	set := &pluginSet{Plugins: []loadedPlugin{{Name: "mine", Dir: dir}}}
	def := reviewplugin.DefaultRef

	if ref, reason := chooseReviewSkill(set, "mine:plain", def); ref != "mine:plain" || reason != "" {
		t.Fatalf("plain skill: got %q, %q", ref, reason)
	}
	for _, req := range []string{"mine:foo*", "mine:foo(x)"} {
		ref, reason := chooseReviewSkill(set, req, def)
		if ref != def || reason == "" {
			t.Errorf("%q: got %q, %q; want the default with a reason", req, ref, reason)
		}
	}
}
