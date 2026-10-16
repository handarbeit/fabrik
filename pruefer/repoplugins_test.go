package pruefer

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func blobEntry(path string, size int64) gh.TreeEntry {
	return gh.TreeEntry{Path: path, Mode: "100644", Type: "blob", SHA: "sha-" + path, Size: size}
}

const goodManifest = `{"name":"mine","version":"1.0.0"}`

func goodPluginFiles() map[string]string {
	return map[string]string{
		"mine/.claude-plugin/plugin.json": goodManifest,
		"mine/skills/review/SKILL.md":     "---\ndescription: x\n---\nReview carefully.\n",
	}
}

func TestFetchRepoPlugins_ReadsTheBaseTreeAndBlobs(t *testing.T) {
	f := newFakeReviewer()
	f.setRepoPlugins(goodPluginFiles())
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if res.Unavailable != "" || len(res.Skipped) != 0 || res.Degraded {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Plugins) != 1 || res.Plugins[0].Dir != "mine" || len(res.Plugins[0].Files) != 2 {
		t.Fatalf("plugins = %+v", res.Plugins)
	}
	got := map[string]string{}
	for _, file := range res.Plugins[0].Files {
		got[file.Path] = string(file.Data)
	}
	if got[".claude-plugin/plugin.json"] != goodManifest {
		t.Errorf("manifest content = %q", got[".claude-plugin/plugin.json"])
	}
	if f.treeCalls[0] != "basesha" {
		t.Errorf("tree read at %q, want the pinned base SHA", f.treeCalls[0])
	}
}

// A plugin that exists only at the PR head is never loaded: the tree is read at
// the base SHA, where it is absent.
func TestFetchRepoPlugins_HeadOnlyPluginNeverLoads(t *testing.T) {
	f := newFakeReviewer()
	f.setRepoPlugins(goodPluginFiles())
	f.pluginsOnlyAtSHA = "head-sha"
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if len(res.Plugins) != 0 || res.Unavailable != "" {
		t.Errorf("a head-only plugin loaded from the base read: %+v", res)
	}
	// Non-vacuity: the same fixture loads when the read is at the commit that has it.
	if got := fetchRepoPlugins(f, "o", "r", "head-sha"); len(got.Plugins) != 1 {
		t.Errorf("fixture does not load at the commit that has it: %+v", got)
	}
}

func TestFetchRepoPlugins_ErrorsAndTruncation(t *testing.T) {
	f := newFakeReviewer()
	f.pluginTreeErr = os.ErrDeadlineExceeded
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if res.Unavailable == "" || !res.Degraded || len(res.Plugins) != 0 {
		t.Errorf("a failed read must be unavailable and degraded: %+v", res)
	}

	f = newFakeReviewer()
	f.setRepoPlugins(goodPluginFiles())
	f.pluginTruncated = true
	res = fetchRepoPlugins(f, "o", "r", "basesha")
	if res.Unavailable == "" || len(res.Plugins) != 0 {
		t.Errorf("a truncated listing must load nothing: %+v", res)
	}

	f = newFakeReviewer() // no tree at all: the ordinary case
	res = fetchRepoPlugins(f, "o", "r", "basesha")
	if res.Unavailable != "" || res.Degraded || len(res.Plugins) != 0 || len(res.Notes()) != 0 {
		t.Errorf("no plugin tree is not an error: %+v", res)
	}
}

func TestFetchRepoPlugins_BlobFailureSkipsThePluginAndDegrades(t *testing.T) {
	f := newFakeReviewer()
	f.setRepoPlugins(goodPluginFiles())
	f.blobErr = os.ErrClosed
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if len(res.Plugins) != 0 || len(res.Skipped) != 1 || !res.Degraded {
		t.Errorf("result = %+v", res)
	}
}

func TestValidateRepoPluginEntries_Refusals(t *testing.T) {
	big := int64(maxRepoPluginFileBytes + 1)
	many := []gh.TreeEntry{}
	for i := 0; i <= maxRepoPluginFiles; i++ {
		many = append(many, blobEntry("p/f"+strconv.Itoa(i), 1))
	}
	cases := map[string]struct {
		entries []gh.TreeEntry
		reason  string
	}{
		"symlink":        {[]gh.TreeEntry{{Path: "p/link", Mode: "120000", Type: "blob", SHA: "s"}}, "symlink"},
		"submodule":      {[]gh.TreeEntry{{Path: "p/sub", Mode: "160000", Type: "commit", SHA: "s"}}, "submodule"},
		"dotdot":         {[]gh.TreeEntry{blobEntry("p/../x", 1)}, "clean relative path"},
		"empty segment":  {[]gh.TreeEntry{blobEntry("p//x", 1)}, "clean relative path"},
		"backslash":      {[]gh.TreeEntry{blobEntry(`p/a\b`, 1)}, "clean relative path"},
		"case collision": {[]gh.TreeEntry{blobEntry("p/Skills/a", 1), blobEntry("p/skills/a", 1)}, "differ only in case"},
		"odd mode":       {[]gh.TreeEntry{{Path: "p/x", Mode: "100000", Type: "blob", SHA: "s"}}, "unsupported"},
		"file too big":   {[]gh.TreeEntry{blobEntry("p/x", big)}, "file cap"},
		"too many files": {many, "files"},
		"bad dir name":   {[]gh.TreeEntry{blobEntry("-bad name/x", 1)}, "not allowed"},
		"total too big": {[]gh.TreeEntry{
			blobEntry("p/a", maxRepoPluginFileBytes), blobEntry("p/b", maxRepoPluginFileBytes), blobEntry("p/c", maxRepoPluginFileBytes),
			blobEntry("p/d", maxRepoPluginFileBytes), blobEntry("p/e", 1)}, "exceeds"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A healthy sibling must be unaffected by the refused plugin.
			entries := append(append([]gh.TreeEntry(nil), tc.entries...), blobEntry("ok/.claude-plugin/plugin.json", 3))
			byDir, skipped := validateRepoPluginEntries(entries)
			if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, tc.reason) {
				t.Fatalf("skipped = %+v, want one reason containing %q", skipped, tc.reason)
			}
			if _, ok := byDir["ok"]; !ok || len(byDir) != 1 {
				t.Errorf("healthy sibling lost or refused plugin kept: %v", byDir)
			}
		})
	}
}

func TestValidateRepoPluginEntries_KeepsExecutableMode(t *testing.T) {
	f := newFakeReviewer()
	f.setRepoPlugins(map[string]string{"p/hooks/run.sh": "#!/bin/sh\n"})
	f.pluginEntries[0].Mode = "100755"
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if len(res.Plugins) != 1 || !res.Plugins[0].Files[0].Exec {
		t.Errorf("executable bit lost: %+v", res)
	}
}

func TestFetchRepoPlugins_AggregateCap(t *testing.T) {
	f := newFakeReviewer()
	files := map[string]string{}
	f.setRepoPlugins(files)
	for _, name := range []string{"a", "b", "c"} { // 3 x 3.5 MiB > 8 MiB; the first two fit
		for i := 0; i < 4; i++ {
			f.pluginEntries = append(f.pluginEntries, gh.TreeEntry{Path: name + "/f" + strconv.Itoa(i), Mode: "100644", Type: "blob", SHA: "b-" + name + strconv.Itoa(i), Size: 900 << 10})
			f.blobs["b-"+name+strconv.Itoa(i)] = []byte("x")
		}
	}
	res := fetchRepoPlugins(f, "o", "r", "basesha")
	if len(res.Plugins) != 2 || len(res.Skipped) != 1 || res.Skipped[0].Dir != "c" {
		t.Errorf("plugins=%d skipped=%+v", len(res.Plugins), res.Skipped)
	}
}

// repoplugins.go must never read the local clone: the clone holds the PR head,
// which is untrusted. The guard is structural — the file imports nothing that
// can touch the file system — and a file that does fails this test.
func TestRepoPluginsNeverReadTheClone(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "repoplugins.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		switch path, _ := strconv.Unquote(imp.Path.Value); path {
		case "os", "io/fs", "io/ioutil", "path/filepath", "os/exec":
			t.Errorf("repoplugins.go imports %q: repo plugins must come from the base SHA's API tree, never the file system", path)
		}
	}
	// Non-vacuity: the checker does flag a file that imports os.
	src := "package x\nimport \"os\"\nvar _ = os.Args\n"
	bad, err := parser.ParseFile(fset, "bad.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, imp := range bad.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os" {
			flagged = true
		}
	}
	if !flagged {
		t.Error("import scan would miss an os import")
	}
}

func TestRepoPluginCodeWarnings(t *testing.T) {
	trees := []repoPluginTree{
		{Dir: "plain", Files: []repoPluginFile{{Path: "skills/review/SKILL.md", Data: []byte("x")}}},
		{Dir: "hooked", Files: []repoPluginFile{{Path: "hooks/hooks.json", Data: []byte("{}")}}},
		{Dir: "mcp", Files: []repoPluginFile{{Path: ".mcp.json", Data: []byte("{}")}}},
		{Dir: "manifest", Files: []repoPluginFile{{Path: ".claude-plugin/plugin.json", Data: []byte(`{"name":"m","hooks":{},"mcpServers":{}}`)}}},
	}
	got := repoPluginCodeWarnings(trees)
	if len(got) != 3 {
		t.Fatalf("want 3 warnings (plain plugin excluded), got %d: %v", len(got), got)
	}
	for i, want := range []string{"hooked", "mcp", "manifest"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("warning %d lacks %q: %s", i, want, got[i])
		}
	}
	if !strings.Contains(got[2], "an MCP server and hooks") {
		t.Errorf("manifest warning should name both kinds: %s", got[2])
	}
}

// A plugin directory committed as a symlink or a submodule has no "/" in its
// path; it must be reported as a refused plugin, not silently dropped (FR-018).
func TestValidateRepoPluginEntries_ReportsTopLevelSymlinkAndSubmodule(t *testing.T) {
	entries := []gh.TreeEntry{
		{Path: "linked", Mode: "120000", Type: "blob", SHA: "s"},
		{Path: "subm", Mode: "160000", Type: "commit", SHA: "s"},
		{Path: "README.md", Mode: "100644", Type: "blob", SHA: "s", Size: 1},
		{Path: "ok", Mode: "040000", Type: "tree", SHA: "s"},
		blobEntry("ok/.claude-plugin/plugin.json", 3),
	}
	byDir, skipped := validateRepoPluginEntries(entries)
	if len(byDir["ok"]) != 1 {
		t.Errorf("healthy sibling affected: %v", byDir)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Dir] = s.Reason
	}
	if !strings.Contains(reasons["linked"], "symlink") || !strings.Contains(reasons["subm"], "submodule") {
		t.Errorf("top-level symlink/submodule not reported: %+v", skipped)
	}
	if len(skipped) != 2 {
		t.Errorf("a plain top-level file or tree entry must stay unreported: %+v", skipped)
	}
}

// Two plugin directories that differ only in case would be written into one
// directory on a case-insensitive filesystem; neither may load (FR-018).
func TestValidateRepoPluginEntries_RefusesCaseCollidingPluginDirs(t *testing.T) {
	entries := []gh.TreeEntry{
		blobEntry("Mine/.claude-plugin/plugin.json", 3),
		blobEntry("mine/.claude-plugin/plugin.json", 3),
		blobEntry("other/.claude-plugin/plugin.json", 3),
	}
	byDir, skipped := validateRepoPluginEntries(entries)
	if _, ok := byDir["Mine"]; ok {
		t.Error("Mine loaded despite the case collision")
	}
	if _, ok := byDir["mine"]; ok {
		t.Error("mine loaded despite the case collision")
	}
	if len(byDir["other"]) != 1 {
		t.Errorf("unrelated plugin affected: %v", byDir)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0].Reason, "differ only in case") {
		t.Errorf("collision not reported for both: %+v", skipped)
	}
}
