package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/internal/skillref"
	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// DefaultReviewSkill is the review skill used when neither the operator nor the
// repo selects one: the default skill of the embedded pruefer-review plugin.
var DefaultReviewSkill = reviewplugin.DefaultRef

// wellFormedSkillRef reports whether ref is a non-empty plugin:skill pair with
// no whitespace, backtick, "*", "(" or ")". Pruefer accepts only qualified
// references: a bare name would resolve by load order, which for a repo-chosen
// reference would let plugin order decide which skill reviews the PR. The
// reference is spliced into the reviewer's Skill(<ref>) allow rule, so a
// character that is a pattern or rule delimiter there (a skill directory named
// "foo*" in a repo plugin) must never pass.
func wellFormedSkillRef(ref string) bool {
	pn, skill, ok := plugin.Split(ref)
	return ok && pn != "" && skill != "" && !strings.ContainsAny(ref, " \t\r\n`*()")
}

// sanitizeNoticeText makes a repo-controlled string safe to render in a posted
// review body and a log line: only [A-Za-z0-9._:/ -] survive (anything else,
// including markdown, mentions and newlines, becomes "?"), capped at 120
// characters.
func sanitizeNoticeText(s string) string { return sanitizeNoticeTextN(s, 120) }

// maxNoticeReasonLen caps the reason in a posted fallback notice. It is larger
// than the reference's cap because the reason carries the plugin notes that
// explain why a repo plugin was skipped, which a 120-character cut would drop.
const maxNoticeReasonLen = 480

// sanitizeNoticeTextN is sanitizeNoticeText with an explicit character cap.
func sanitizeNoticeTextN(s string, limit int) string { return sanitizeNoticeRunes(s, limit, "") }

// noticeReasonPunctuation is what a fallback reason may carry beyond the
// reference alphabet so the resolver's message and the plugin notes stay
// readable: quotes, parentheses, commas and semicolons. The reason is always
// rendered in a code span, where none of them can form markdown, and the
// backtick that could close the span, "@" and "#" stay excluded.
const noticeReasonPunctuation = `"'(),;`

// sanitizeNoticeReason is sanitizeNoticeText for the reason of a fallback
// notice, with the wider alphabet and cap that reason needs.
func sanitizeNoticeReason(s string) string {
	return sanitizeNoticeRunes(s, maxNoticeReasonLen, noticeReasonPunctuation)
}

func sanitizeNoticeRunes(s string, limit int, extra string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			b.WriteString("...")
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._:/ -", r), strings.ContainsRune(extra, r):
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
		n++
	}
	return b.String()
}

// operatorPluginReports remembers which operator plugin paths have already been
// reported as unusable, so a missing directory is reported once rather than on
// every review. A path that becomes usable again is forgotten, so a later
// breakage reports again.
var operatorPluginReports = struct {
	mu       sync.Mutex
	reported map[string]bool
}{reported: map[string]bool{}}

// resolveOperatorPluginDirs turns the operator's plugins: list into directories
// --plugin-dir and the resolver can both see (#2307, R1): "~/" expanded, a
// relative path taken against the working directory (as DefaultConfigPath is),
// symlinks resolved. These are trusted local paths used in place, so no size or
// symlink limit applies. A path that is missing, unreadable or not a directory
// is reported once and skipped; the review never fails on it.
func resolveOperatorPluginDirs(paths []string, prNumber int) []string {
	var out []string
	for _, p := range paths {
		dir, err := resolveOperatorPluginDir(p)
		operatorPluginReports.mu.Lock()
		already := operatorPluginReports.reported[p]
		if err != nil {
			operatorPluginReports.reported[p] = true
		} else {
			delete(operatorPluginReports.reported, p)
		}
		operatorPluginReports.mu.Unlock()
		if err != nil {
			if !already {
				logf(prNumber, "warn", "review plugin %q skipped: %v\n", p, err)
			}
			continue
		}
		out = append(out, dir)
	}
	return out
}

func resolveOperatorPluginDir(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %w", err)
		}
		p = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Plugin sources, for the log.
const (
	sourceEmbedded = "embedded"
	sourceOperator = "operator"
	sourceRepo     = "repo"
)

// loadedPlugin is one plugin a review passes with --plugin-dir.
type loadedPlugin struct {
	Name    string
	Version string
	Dir     string
	Source  string
}

func (p loadedPlugin) version() string {
	if p.Version == "" {
		return "unknown"
	}
	return p.Version
}

// logLine is the per-plugin line every review logs (#2307, R1).
func (p loadedPlugin) logLine() string {
	return fmt.Sprintf("review plugin: %s v%s (%s, dir %s)", p.Name, p.version(), p.Source, p.Dir)
}

// pluginSet is the ordered set of plugin directories one review passes.
type pluginSet struct {
	Plugins []loadedPlugin
	// Notes records every plugin that was not loaded after all — a duplicate
	// name, the reserved name, a tree that could not be written — for the log and
	// any fallback notice.
	Notes []string
	// Details carries the full error behind a note whose text must not reach a
	// posted review (it can name the per-review temp directory). It is logged
	// only, never appended to a fallback reason.
	Details []string
}

// Dirs returns the plugin directories in load order.
func (s *pluginSet) Dirs() []string {
	dirs := make([]string, len(s.Plugins))
	for i, p := range s.Plugins {
		dirs[i] = p.Dir
	}
	return dirs
}

// byName returns the loaded plugin with the given manifest name.
func (s *pluginSet) byName(name string) (loadedPlugin, bool) {
	for _, p := range s.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return loadedPlugin{}, false
}

// buildPluginSet assembles the plugins a review passes, in the fixed order of
// #2307 FR-019: the embedded pruefer-review, then operator plugins in listed
// order, then repo plugins in directory-name order. Names are manifest names.
// The embedded plugin is never replaced: any other plugin declaring its name is
// skipped and reported, and for every other duplicate the earlier plugin wins.
// Repo plugin trees are written under rp.RepoRoot(), part of the per-review
// directory removed on every exit path.
func buildPluginSet(rp *reviewPlugin, operatorDirs []string, repoTrees []repoPluginTree) *pluginSet {
	set := &pluginSet{}
	seen := map[string]string{} // manifest name -> source that holds it
	add := func(dir, source, what string) {
		p := skillref.Discover(dir)
		switch {
		case source != sourceEmbedded && p.Name == reviewplugin.PluginName:
			set.Notes = append(set.Notes, fmt.Sprintf("%s plugin %s skipped: its manifest name %q is reserved for the embedded plugin", source, what, p.Name))
		case seen[p.Name] != "":
			set.Notes = append(set.Notes, fmt.Sprintf("%s plugin %s skipped: manifest name %q is already loaded from a %s plugin", source, what, p.Name, seen[p.Name]))
		default:
			seen[p.Name] = source
			set.Plugins = append(set.Plugins, loadedPlugin{Name: p.Name, Version: p.Version, Dir: dir, Source: source})
		}
	}
	add(rp.Dir, sourceEmbedded, reviewplugin.PluginName)
	for _, d := range operatorDirs {
		add(d, sourceOperator, filepath.Base(d))
	}
	for _, t := range repoTrees {
		dir, err := writeRepoPluginTree(rp.RepoRoot(), t)
		if err != nil {
			// The error is usually an *os.PathError naming the per-review temp
			// directory; notes reach the posted fallback notice, so they carry a
			// fixed phrase and the detail is only logged.
			set.Notes = append(set.Notes, fmt.Sprintf("repo plugin %s skipped: could not be written", t.Dir))
			set.Details = append(set.Details, fmt.Sprintf("repo plugin %s could not be written: %v", t.Dir, err))
			continue
		}
		add(dir, sourceRepo, t.Dir)
	}
	return set
}

// writeRepoPluginTree writes one validated tree below root and returns its
// directory. The paths were validated when the tree was read, and are checked
// again here: nothing may land outside root/<name>. Files are written 0644, or
// 0755 where the base commit marked them executable.
func writeRepoPluginTree(root string, t repoPluginTree) (string, error) {
	dest := filepath.Join(root, t.Dir)
	if !repoPluginNameRE.MatchString(t.Dir) {
		return "", fmt.Errorf("directory name %q is not allowed", t.Dir)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	for _, f := range t.Files {
		if !cleanRelativePath(f.Path) || !filepath.IsLocal(filepath.FromSlash(f.Path)) {
			return "", fmt.Errorf("path %q is not a clean relative path", f.Path)
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := os.WriteFile(target, f.Data, mode); err != nil {
			return "", err
		}
		if err := os.Chmod(target, mode); err != nil {
			return "", err
		}
	}
	return dest, nil
}
