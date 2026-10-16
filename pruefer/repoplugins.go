package pruefer

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// RepoPluginsDir is the repo-relative directory a repo commits review plugins
// under (#2307, R2): .pruefer/plugins/<name>/. Each is read from the PR's base
// SHA, never from the clone, which holds the untrusted PR head.
const RepoPluginsDir = ".pruefer/plugins"

// Caps on one repo's plugin trees (#2307, FR-018). A tree over any cap is
// rejected whole and reported; the repo-resident single-file review skill and
// config carry the same kind of cap (DefaultMaxRepoConfigBytes).
const (
	maxRepoPluginFiles      = 500
	maxRepoPluginBytes      = 4 << 20 // one plugin, all files
	maxRepoPluginFileBytes  = 1 << 20 // one file
	maxRepoPluginsAllBytes  = 8 << 20 // every accepted plugin together
	maxRepoPluginsAllFiles  = 1000    // every accepted plugin together: one blob request per file, every review
	repoPluginMaxNameLength = 64
)

var repoPluginNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// repoPluginFile is one regular file of a repo plugin, held in memory.
type repoPluginFile struct {
	Path string // slash-separated, relative to the plugin directory, validated
	Data []byte
	Exec bool // git mode 100755: hook and MCP scripts must stay executable
}

// repoPluginTree is one plugin read from the base SHA. Dir is the directory name
// under .pruefer/plugins; the manifest name inside decides how it is referenced.
type repoPluginTree struct {
	Dir   string
	Files []repoPluginFile
}

// repoPluginSkip is a plugin directory that was not loaded, and why.
type repoPluginSkip struct {
	Dir    string
	Reason string
}

// repoPluginResult is everything fetchRepoPlugins learned. Plugins and Skipped
// are in directory-name order. Unavailable is non-empty when the base tree
// could not be read at all (an error, or a truncated listing): no repo plugin is
// then loaded and the reason is carried into any fallback notice. Absent
// directory is not unavailable — it is the ordinary "this repo has none".
type repoPluginResult struct {
	Plugins     []repoPluginTree
	Skipped     []repoPluginSkip
	Unavailable string
	// Degraded is set when a failed read, not a deterministic verdict about the
	// repo, shaped the result: the outcome must not be memoised (#1952).
	Degraded bool
}

// Notes renders the reasons any repo plugin did not load, for a fallback notice.
func (r repoPluginResult) Notes() []string {
	var n []string
	if r.Unavailable != "" {
		n = append(n, "repo plugins not loaded: "+r.Unavailable)
	}
	for _, s := range r.Skipped {
		n = append(n, fmt.Sprintf("repo plugin %s skipped: %s", s.Dir, s.Reason))
	}
	return n
}

// fetchRepoPlugins reads .pruefer/plugins at baseSHA through the Git trees and
// blobs API and returns the validated plugin trees (#2307, R2). This file never
// touches the local clone: the plugin content a PR could rewrite is unreachable
// from here by construction (TestRepoPluginsNeverReadTheClone).
func fetchRepoPlugins(client GitHubReviewer, owner, repo, baseSHA string) repoPluginResult {
	var res repoPluginResult
	entries, truncated, err := client.FetchTreeAtRef(owner, repo, baseSHA, RepoPluginsDir)
	if err != nil {
		if errors.Is(err, gh.ErrNotFound) {
			return res
		}
		res.Unavailable = fmt.Sprintf("reading %s at %s failed: %v", RepoPluginsDir, baseSHA, err)
		res.Degraded = true
		return res
	}
	if truncated {
		res.Unavailable = fmt.Sprintf("the %s listing at %s was truncated by GitHub", RepoPluginsDir, baseSHA)
		return res
	}
	byDir, skipped := validateRepoPluginEntries(entries)
	res.Skipped = skipped

	var total int64
	var totalFiles int
	for _, dir := range sortedKeys(byDir) {
		files := byDir[dir]
		var size int64
		for _, f := range files {
			size += f.Size
		}
		if total+size > maxRepoPluginsAllBytes {
			res.Skipped = append(res.Skipped, repoPluginSkip{dir, fmt.Sprintf("the repo's plugins together exceed %d bytes", maxRepoPluginsAllBytes)})
			continue
		}
		if totalFiles+len(files) > maxRepoPluginsAllFiles {
			res.Skipped = append(res.Skipped, repoPluginSkip{dir, fmt.Sprintf("the repo's plugins together exceed %d files", maxRepoPluginsAllFiles)})
			continue
		}
		tree := repoPluginTree{Dir: dir}
		var fetchErr error
		for _, f := range files {
			data, err := client.FetchBlob(owner, repo, f.SHA, maxRepoPluginFileBytes)
			if err != nil {
				fetchErr = fmt.Errorf("reading %s: %w", f.Path, err)
				break
			}
			tree.Files = append(tree.Files, repoPluginFile{Path: f.Path, Data: data, Exec: f.Mode == "100755"})
		}
		if fetchErr != nil {
			res.Skipped = append(res.Skipped, repoPluginSkip{dir, fetchErr.Error()})
			res.Degraded = true
			continue
		}
		total += size
		totalFiles += len(files)
		res.Plugins = append(res.Plugins, tree)
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Dir < res.Skipped[j].Dir })
	return res
}

// repoPluginCodeWarnings names every loaded repo plugin that carries hooks or an
// MCP server — components that start processes as the daemon's OS user, outside
// the reviewer's path-scoped tool rules. Detection is by plugin layout
// (hooks/, .mcp.json) and by the manifest naming hooks or mcpServers.
func repoPluginCodeWarnings(trees []repoPluginTree) []string {
	var out []string
	for _, t := range trees {
		var kinds []string
		for _, f := range t.Files {
			switch {
			case f.Path == ".mcp.json":
				kinds = appendOnce(kinds, "an MCP server")
			case strings.HasPrefix(f.Path, "hooks/"):
				kinds = appendOnce(kinds, "hooks")
			case f.Path == ".claude-plugin/plugin.json":
				if bytes.Contains(f.Data, []byte(`"mcpServers"`)) {
					kinds = appendOnce(kinds, "an MCP server")
				}
				if bytes.Contains(f.Data, []byte(`"hooks"`)) {
					kinds = appendOnce(kinds, "hooks")
				}
			}
		}
		if len(kinds) > 0 {
			out = append(out, fmt.Sprintf("repo plugin %s still loads and carries %s that will run", t.Dir, strings.Join(kinds, " and ")))
		}
	}
	return out
}

func appendOnce(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// pluginEntry is a validated regular-file entry of one plugin, path relative to
// the plugin directory.
type pluginEntry struct {
	Path string
	Mode string
	SHA  string
	Size int64
}

// validateRepoPluginEntries groups the listing under .pruefer/plugins by plugin
// directory and refuses every shape that is not a plain tree of regular files
// (#2307, FR-018): a symlink, a gitlink (submodule), a path that is not a clean
// relative path, two paths that differ only in case, a name outside the allowed
// set, or a tree over a cap. A refused plugin is skipped whole with its reason;
// the others are unaffected. Pure: it decides from metadata before any blob is
// read.
func validateRepoPluginEntries(entries []gh.TreeEntry) (map[string][]pluginEntry, []repoPluginSkip) {
	groups := map[string][]gh.TreeEntry{}
	var skipped []repoPluginSkip
	for _, e := range entries {
		dir, _, nested := strings.Cut(e.Path, "/")
		if !nested {
			// A plugin directory's own tree entry carries nothing, and a regular
			// file at the top of .pruefer/plugins is not a plugin. But a plugin
			// directory committed as a symlink or a submodule is a refused plugin
			// (FR-018) and must be reported, not silently dropped.
			switch {
			case e.Type == "commit" || e.Mode == "160000":
				skipped = append(skipped, repoPluginSkip{e.Path, "the plugin directory is a submodule, which is not followed"})
			case e.Mode == "120000":
				skipped = append(skipped, repoPluginSkip{e.Path, "the plugin directory is a symlink, which is not followed"})
			}
			continue
		}
		groups[dir] = append(groups[dir], e)
	}

	// Plugin directories that differ only in case would share one directory on a
	// case-insensitive filesystem and be written into each other, producing a
	// hybrid plugin that matches neither source. Neither is loaded.
	byLower := map[string][]string{}
	for _, dir := range sortedKeys(groups) {
		byLower[strings.ToLower(dir)] = append(byLower[strings.ToLower(dir)], dir)
	}
	collided := map[string]bool{}
	for _, dirs := range byLower {
		if len(dirs) < 2 {
			continue
		}
		for _, d := range dirs {
			collided[d] = true
			skipped = append(skipped, repoPluginSkip{d, fmt.Sprintf("plugin directories %s differ only in case", strings.Join(dirs, " and "))})
		}
	}

	out := map[string][]pluginEntry{}
	for _, dir := range sortedKeys(groups) {
		if collided[dir] {
			continue
		}
		reason, files := validateOnePlugin(dir, groups[dir])
		if reason != "" {
			skipped = append(skipped, repoPluginSkip{dir, reason})
			continue
		}
		if len(files) == 0 {
			continue
		}
		out[dir] = files
	}
	return out, skipped
}

func validateOnePlugin(dir string, entries []gh.TreeEntry) (reason string, files []pluginEntry) {
	if len(dir) > repoPluginMaxNameLength || !repoPluginNameRE.MatchString(dir) || dir == "." || dir == ".." {
		return fmt.Sprintf("directory name %q is not allowed", dir), nil
	}
	seen := map[string]string{}
	var total int64
	for _, e := range entries {
		rel := strings.TrimPrefix(e.Path, dir+"/")
		if !cleanRelativePath(rel) {
			return fmt.Sprintf("path %q is not a clean relative path", rel), nil
		}
		lower := strings.ToLower(rel)
		if prev, dup := seen[lower]; dup && prev != rel {
			return fmt.Sprintf("paths %q and %q differ only in case", prev, rel), nil
		}
		seen[lower] = rel
		switch {
		case e.Type == "tree":
			continue
		case e.Type == "commit" || e.Mode == "160000":
			return fmt.Sprintf("%q is a submodule, which is not followed", rel), nil
		case e.Mode == "120000":
			return fmt.Sprintf("%q is a symlink, which is not followed", rel), nil
		case e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755" || e.Mode == "100664"):
		default:
			return fmt.Sprintf("%q has unsupported type %q mode %q", rel, e.Type, e.Mode), nil
		}
		if e.Size > maxRepoPluginFileBytes {
			return fmt.Sprintf("%q is %d bytes, over the %d byte file cap", rel, e.Size, maxRepoPluginFileBytes), nil
		}
		total += e.Size
		if total > maxRepoPluginBytes {
			return fmt.Sprintf("the plugin exceeds %d bytes", maxRepoPluginBytes), nil
		}
		if len(files) >= maxRepoPluginFiles {
			return fmt.Sprintf("the plugin exceeds %d files", maxRepoPluginFiles), nil
		}
		mode := e.Mode
		if mode == "100664" {
			mode = "100644"
		}
		files = append(files, pluginEntry{Path: rel, Mode: mode, SHA: e.SHA, Size: e.Size})
	}
	return "", files
}

// cleanRelativePath reports whether p is a non-empty slash-separated relative
// path with no empty, "." or ".." segment and no backslash or control byte.
func cleanRelativePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' || p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
