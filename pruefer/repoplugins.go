package pruefer

import (
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
		res.Plugins = append(res.Plugins, tree)
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Dir < res.Skipped[j].Dir })
	return res
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
	for _, e := range entries {
		dir, _, nested := strings.Cut(e.Path, "/")
		if !nested {
			// A bare file or a plugin directory entry itself. A file at the top of
			// .pruefer/plugins is not a plugin; the directory entry carries nothing.
			continue
		}
		groups[dir] = append(groups[dir], e)
	}

	out := map[string][]pluginEntry{}
	var skipped []repoPluginSkip
	for _, dir := range sortedKeys(groups) {
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
