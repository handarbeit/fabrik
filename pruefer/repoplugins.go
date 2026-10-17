package pruefer

import (
	"fmt"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
)

// Caps on repo plugin trees (#2366, FR-008). They are fixed constants, not
// operator settings: a tree over any cap is refused whole and reported, and
// every check is decided from tree metadata before a single blob is read. The
// values carry over from the first repo-plugin implementation (#2307).
const (
	maxRepoPluginFiles     = 500
	maxRepoPluginBytes     = 4 << 20 // one plugin, all files
	maxRepoPluginFileBytes = 1 << 20 // one file
	maxRepoPluginsAllBytes = 8 << 20 // every plugin one review loads, together
	maxRepoPluginsAllFiles = 1000    // ... and one blob request per file, every uncached review
	maxRepoPluginPathBytes = 400     // a whole relative path
	maxRepoPluginSegment   = 120     // one path segment
)

// The named validation rules. Each is a separate check so a test can neutralise
// exactly one (disableRepoPluginRuleForTest) and prove its fixture is refused
// only because of it (FR-017).
const (
	ruleTruncated     = "truncated"
	ruleSymlink       = "symlink"
	ruleGitlink       = "gitlink"
	ruleUnsupported   = "unsupported-mode"
	ruleCleanPath     = "clean-path"
	ruleCaseCollision = "case-collision"
	ruleFileBytes     = "file-bytes"
	rulePluginBytes   = "plugin-bytes"
	rulePluginFiles   = "plugin-files"
	ruleTotalBytes    = "total-bytes"
	ruleTotalFiles    = "total-files"
)

// repoPluginRules lists every validation rule, for the neutralisation tests.
var repoPluginRules = []string{
	ruleTruncated, ruleSymlink, ruleGitlink, ruleUnsupported, ruleCleanPath,
	ruleCaseCollision, ruleFileBytes, rulePluginBytes, rulePluginFiles,
	ruleTotalBytes, ruleTotalFiles,
}

var disabledRepoPluginRules = struct {
	mu    sync.Mutex
	rules map[string]bool
}{rules: map[string]bool{}}

// repoPluginRuleOn reports whether rule is enforced. Always true outside tests.
func repoPluginRuleOn(rule string) bool {
	disabledRepoPluginRules.mu.Lock()
	defer disabledRepoPluginRules.mu.Unlock()
	return !disabledRepoPluginRules.rules[rule]
}

// repoPluginFile is one regular file of a repo plugin, held in memory.
type repoPluginFile struct {
	Path string // slash-separated, relative to the plugin directory, validated
	Data []byte
	Exec bool // git mode 100755: hook and MCP scripts must stay executable
}

// repoPluginTree is one validated plugin, fetched from a fixed commit.
type repoPluginTree struct {
	Files []repoPluginFile
	Bytes int64
}

// pluginEntry is a validated regular-file entry of one plugin, path relative to
// the plugin directory.
type pluginEntry struct {
	Path string
	Mode string
	SHA  string
	Size int64
}

// cleanPluginPath reports why p is not a plain relative path, or "" if it is:
// non-empty, slash-separated, no absolute prefix, no empty, "." or ".." segment,
// no backslash, NUL or control character, no ".git" segment (a plugin must not
// carry a nested repository's metadata), bounded in length.
func cleanPluginPath(p string) string {
	switch {
	case p == "":
		return "the path is empty"
	case len(p) > maxRepoPluginPathBytes:
		return "the path is longer than the cap"
	case strings.HasPrefix(p, "/"):
		return "the path is absolute"
	}
	for _, r := range p {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return "the path holds a backslash or control character"
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "", seg == ".", seg == "..":
			return fmt.Sprintf("the path has the segment %q", seg)
		case len(seg) > maxRepoPluginSegment:
			return "a path segment is longer than the cap"
		case strings.EqualFold(seg, ".git"):
			return "the path has a .git segment"
		}
	}
	return ""
}

// validateRepoPluginTree decides from tree metadata alone whether a plugin
// listing may be loaded (#2366, FR-008): the listing must be complete, hold only
// regular files and directories (no symlink, no gitlink, no other mode), use
// only clean relative paths with no two that differ only in case (they would be
// written into each other on a case-insensitive filesystem), and stay inside the
// per-file, per-plugin byte and file caps. Any violation refuses the whole
// plugin — never "skip the offending file". It returns the files to read, or the
// reason the plugin is refused. Pure: no blob has been read.
func validateRepoPluginTree(entries []gh.TreeEntry, truncated bool) (files []pluginEntry, refusal string) {
	if truncated && repoPluginRuleOn(ruleTruncated) {
		return nil, "the tree listing was truncated by GitHub"
	}
	seen := map[string]string{} // lower-cased path -> the path that claimed it
	var bytes int64
	for _, e := range entries {
		if repoPluginRuleOn(ruleCleanPath) {
			if why := cleanPluginPath(e.Path); why != "" {
				return nil, fmt.Sprintf("%q is not a clean relative path (%s)", e.Path, why)
			}
		}
		if repoPluginRuleOn(ruleCaseCollision) {
			key := strings.ToLower(e.Path)
			if prev, dup := seen[key]; dup {
				return nil, fmt.Sprintf("%q and %q differ only in case", prev, e.Path)
			}
			seen[key] = e.Path
		}
		if e.Type == "tree" {
			continue // a directory carries nothing; its files are listed on their own
		}
		if e.Type == "commit" || e.Mode == "160000" {
			if repoPluginRuleOn(ruleGitlink) {
				return nil, fmt.Sprintf("%q is a submodule, which is not followed", e.Path)
			}
			continue // neutralised: there is no blob to read
		}
		if e.Mode == "120000" {
			if repoPluginRuleOn(ruleSymlink) {
				return nil, fmt.Sprintf("%q is a symlink, which is not followed", e.Path)
			}
			continue
		}
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
			if repoPluginRuleOn(ruleUnsupported) {
				return nil, fmt.Sprintf("%q has the unsupported mode %q (type %q)", e.Path, e.Mode, e.Type)
			}
			continue
		}
		if e.Size > maxRepoPluginFileBytes && repoPluginRuleOn(ruleFileBytes) {
			return nil, fmt.Sprintf("%q is %d bytes, over the %d byte file cap", e.Path, e.Size, maxRepoPluginFileBytes)
		}
		bytes += e.Size
		files = append(files, pluginEntry{Path: e.Path, Mode: e.Mode, SHA: e.SHA, Size: e.Size})
		if len(files) > maxRepoPluginFiles && repoPluginRuleOn(rulePluginFiles) {
			return nil, fmt.Sprintf("the plugin has more than %d files", maxRepoPluginFiles)
		}
		if bytes > maxRepoPluginBytes && repoPluginRuleOn(rulePluginBytes) {
			return nil, fmt.Sprintf("the plugin is over the %d byte plugin cap", maxRepoPluginBytes)
		}
	}
	if len(files) == 0 {
		return nil, "the plugin directory holds no files"
	}
	return files, ""
}

// repoPluginBudget enforces the across-plugins caps of one review: the plugins a
// review loads together stay inside a total byte and file count, so the blob
// reads one uncached review can cost are bounded however many plugins the repo
// names.
type repoPluginBudget struct {
	bytes int64
	files int
}

// fits reports why a plugin of the given size would not fit in the budget
// ("" if it would), without charging it. fetchTree calls it with the sizes the
// validated listing already carries, so the cross-plugin caps are decided from
// tree metadata before any blob is read.
func (b *repoPluginBudget) fits(bytes int64, files int) string {
	if b.bytes+bytes > maxRepoPluginsAllBytes && repoPluginRuleOn(ruleTotalBytes) {
		return fmt.Sprintf("the repo's plugins together exceed %d bytes", maxRepoPluginsAllBytes)
	}
	if b.files+files > maxRepoPluginsAllFiles && repoPluginRuleOn(ruleTotalFiles) {
		return fmt.Sprintf("the repo's plugins together exceed %d files", maxRepoPluginsAllFiles)
	}
	return ""
}

// admit charges a plugin of the given size to the budget and reports why it
// does not fit ("" if it does). A refused plugin is not charged.
func (b *repoPluginBudget) admit(bytes int64, files int) string {
	if why := b.fits(bytes, files); why != "" {
		return why
	}
	b.bytes += bytes
	b.files += files
	return ""
}
