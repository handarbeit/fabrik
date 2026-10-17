package pruefer

import (
	"strconv"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// disableRepoPluginRuleForTest turns one validation rule off until the test
// ends (FR-017). Tests that use it must not run in parallel.
func disableRepoPluginRuleForTest(t *testing.T, rule string) {
	t.Helper()
	disabledRepoPluginRules.mu.Lock()
	disabledRepoPluginRules.rules[rule] = true
	disabledRepoPluginRules.mu.Unlock()
	t.Cleanup(func() {
		disabledRepoPluginRules.mu.Lock()
		delete(disabledRepoPluginRules.rules, rule)
		disabledRepoPluginRules.mu.Unlock()
	})
}

func blobEntry(path string, size int64) gh.TreeEntry {
	return gh.TreeEntry{Path: path, Mode: "100644", Type: "blob", SHA: "sha-" + path, Size: size}
}

func okEntries() []gh.TreeEntry {
	return []gh.TreeEntry{
		{Path: ".claude-plugin", Mode: "040000", Type: "tree", SHA: "t1"},
		blobEntry(".claude-plugin/plugin.json", 40),
		{Path: "hooks/run.sh", Mode: "100755", Type: "blob", SHA: "sh", Size: 10},
	}
}

func TestValidateRepoPluginTree_AcceptsAPlainTree(t *testing.T) {
	files, refusal := validateRepoPluginTree(okEntries(), false)
	if refusal != "" || len(files) != 2 {
		t.Fatalf("files=%v refusal=%q", files, refusal)
	}
	if files[1].Mode != "100755" {
		t.Errorf("exec mode lost: %+v", files[1])
	}
}

// Every rule has a fixture that is refused with the rule in place and accepted
// with only that rule neutralised (FR-017, SC-006): the fixture must be refused
// because of the rule under test, not another.
func TestValidateRepoPluginTree_EachRuleIsLoadBearing(t *testing.T) {
	many := okEntries()
	for i := 0; i < maxRepoPluginFiles; i++ {
		many = append(many, blobEntry("f"+strconv.Itoa(i), 1))
	}
	big := okEntries()
	for i := 0; i < 5; i++ { // 5 x 1 MiB: each file under the file cap, the plugin over its own
		big = append(big, blobEntry("b"+strconv.Itoa(i), maxRepoPluginFileBytes))
	}
	cases := []struct {
		rule      string
		entries   []gh.TreeEntry
		truncated bool
		reason    string
	}{
		{ruleTruncated, okEntries(), true, "truncated"},
		{ruleSymlink, append(okEntries(), gh.TreeEntry{Path: "link", Mode: "120000", Type: "blob", SHA: "s"}), false, "symlink"},
		{ruleGitlink, append(okEntries(), gh.TreeEntry{Path: "sub", Mode: "160000", Type: "commit", SHA: "s"}), false, "submodule"},
		{ruleUnsupported, append(okEntries(), gh.TreeEntry{Path: "odd", Mode: "100000", Type: "blob", SHA: "s"}), false, "unsupported"},
		{ruleCleanPath, append(okEntries(), blobEntry("a/../x", 1)), false, "clean relative path"},
		{ruleCaseCollision, append(okEntries(), blobEntry("Skills/a", 1), blobEntry("skills/a", 1)), false, "differ only in case"},
		{ruleFileBytes, append(okEntries(), blobEntry("huge", maxRepoPluginFileBytes+1)), false, "file cap"},
		{rulePluginFiles, many, false, "more than"},
		{rulePluginBytes, big, false, "plugin cap"},
	}
	covered := map[string]bool{ruleTotalBytes: true, ruleTotalFiles: true} // exercised by the budget test below
	for _, tc := range cases {
		covered[tc.rule] = true
		t.Run(tc.rule, func(t *testing.T) {
			_, refusal := validateRepoPluginTree(tc.entries, tc.truncated)
			if !strings.Contains(refusal, tc.reason) {
				t.Fatalf("refusal = %q, want it to contain %q", refusal, tc.reason)
			}
			disableRepoPluginRuleForTest(t, tc.rule)
			if _, refusal := validateRepoPluginTree(tc.entries, tc.truncated); refusal != "" {
				t.Errorf("with %s neutralised the fixture is still refused (%q): the rule is not what refuses it", tc.rule, refusal)
			}
		})
	}
	for _, r := range repoPluginRules {
		if !covered[r] {
			t.Errorf("rule %s has no neutralisation fixture", r)
		}
	}
}

func TestCleanPluginPath(t *testing.T) {
	for _, bad := range []string{
		"", "/abs", "a//b", "a/./b", "a/..", "../a", "a\\b", "a\x00b", "a\nb", ".git/config", "x/.GIT/y",
		strings.Repeat("a", maxRepoPluginSegment+1), strings.Repeat("a/", maxRepoPluginPathBytes),
	} {
		if cleanPluginPath(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"a", "a/b.txt", ".claude-plugin/plugin.json", "x/.gitignore", "Dockerfile"} {
		if why := cleanPluginPath(good); why != "" {
			t.Errorf("%q refused: %s", good, why)
		}
	}
}

func TestValidateRepoPluginTree_EmptyIsRefused(t *testing.T) {
	if _, refusal := validateRepoPluginTree(nil, false); refusal == "" {
		t.Error("an empty plugin directory must be refused")
	}
	if _, refusal := validateRepoPluginTree([]gh.TreeEntry{{Path: "d", Mode: "040000", Type: "tree"}}, false); refusal == "" {
		t.Error("a directory-only listing must be refused")
	}
}

func TestRepoPluginBudget(t *testing.T) {
	var b repoPluginBudget
	if why := b.admit(maxRepoPluginsAllBytes, 10); why != "" {
		t.Fatalf("first plugin refused: %s", why)
	}
	if why := b.admit(1, 1); !strings.Contains(why, "bytes") {
		t.Errorf("over the byte total: %q", why)
	}
	disableRepoPluginRuleForTest(t, ruleTotalBytes)
	if why := b.admit(1, 1); why != "" {
		t.Errorf("byte total neutralised but still refused: %s", why)
	}

	var f repoPluginBudget
	if why := f.admit(1, maxRepoPluginsAllFiles); why != "" {
		t.Fatalf("first plugin refused: %s", why)
	}
	if why := f.admit(1, 1); !strings.Contains(why, "files") {
		t.Errorf("over the file total: %q", why)
	}
	disableRepoPluginRuleForTest(t, ruleTotalFiles)
	if why := f.admit(1, 1); why != "" {
		t.Errorf("file total neutralised but still refused: %s", why)
	}
}
