package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// specSlugMaxLen caps the title-derived part of a spec directory name.
const specSlugMaxLen = 50

// specSlugFallback names the directory when the title slugifies to nothing
// (all punctuation, non-ASCII).
const specSlugFallback = "spec"

// specSlug derives the directory slug from an issue title: lowercase ASCII
// alphanumerics, every other run collapsed to a single "-", trimmed, and cut
// to specSlugMaxLen on a "-" boundary where one exists. An empty result falls
// back to specSlugFallback.
func specSlug(title string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
		} else {
			pendingDash = true
		}
	}
	slug := b.String()
	if len(slug) > specSlugMaxLen {
		slug = slug[:specSlugMaxLen]
		if i := strings.LastIndexByte(slug, '-'); i > 0 {
			slug = slug[:i]
		}
		slug = strings.Trim(slug, "-")
	}
	if slug == "" {
		return specSlugFallback
	}
	return slug
}

// existingSpecDir returns the worktree-relative specs/<N>-* directory already
// present for the issue (lexically first match), or "". The filesystem is the
// slug lock (FR-003): itemstate is in-memory, so reuse of an existing
// directory is the only restart-safe way to keep a title change from forking
// the spec into a second directory.
func existingSpecDir(workDir string, issueNumber int) string {
	matches, err := filepath.Glob(filepath.Join(workDir, "specs", fmt.Sprintf("%d-*", issueNumber)))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			return filepath.ToSlash(filepath.Join("specs", filepath.Base(m)))
		}
	}
	return ""
}

// stripOpenQuestions removes the "## Open Questions" section — the heading
// through the line before the next level-2 heading, or EOF. Fenced code blocks
// are not parsed; a body without the section is returned unchanged.
func stripOpenQuestions(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(line, "## ") || trimmed == "##" {
			skipping = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "##")), "Open Questions")
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// isSpecOnlyCommit reports whether every file a commit touched is the item's own
// persisted spec (specs/<issueNumber>-*/spec.md). A commit touching no files
// (empty, merge) is not spec-only. Used by commitsAheadOfBase so Specify's
// engine-written spec commit does not defeat the empty-coordinator rule (#921).
func isSpecOnlyCommit(files []string, issueNumber int) bool {
	n := 0
	prefix := fmt.Sprintf("specs/%d-", issueNumber)
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok || strings.Count(rest, "/") != 1 || !strings.HasSuffix(rest, "/spec.md") {
			return false
		}
		n++
	}
	return n > 0
}

// persistSpec projects the stage's canonical issue body to
// specs/<N>-<slug>/spec.md in workDir and commits that one file (ADR 2034).
// It is a no-op unless the stage opted in with persist_spec, the body is
// non-empty, and the content differs from what is already on disk. The commit
// is pathspec-scoped, so other dirty worktree state is never captured. All
// failures are logged and non-fatal. It reports whether a commit was created.
func (e *Engine) persistSpec(item gh.ProjectItem, stage *stages.Stage, workDir, body string) bool {
	if stage == nil || !stage.PersistSpec || strings.TrimSpace(body) == "" || workDir == "" {
		return false
	}

	dir := existingSpecDir(workDir, item.Number)
	isNew := dir == ""
	if isNew {
		dir = fmt.Sprintf("specs/%d-%s", item.Number, specSlug(item.Title))
	}
	rel := dir + "/spec.md"
	abs := filepath.Join(workDir, filepath.FromSlash(rel))

	content := strings.TrimRight(stripOpenQuestions(body), " \t\r\n") + "\n"

	existing, readErr := os.ReadFile(abs)
	if readErr != nil && !os.IsNotExist(readErr) {
		e.logf(item.Number, "warn", "could not read %s: %v\n", rel, readErr)
		return false
	}
	// The file matching on disk is not enough: a previous round may have written
	// it and then failed to commit (index.lock, hook failure), leaving it dirty
	// or untracked. Only a clean, tracked match means there is nothing to do.
	if readErr == nil && string(existing) == content && specCleanInHead(workDir, rel) {
		return false
	}

	if readErr != nil || string(existing) != content {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			e.logf(item.Number, "warn", "could not create %s: %v\n", dir, err)
			return false
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			e.logf(item.Number, "warn", "could not write %s: %v\n", rel, err)
			return false
		}
	}

	verb := "update"
	if _, err := runGitIn(workDir, "cat-file", "-e", "HEAD:"+rel); err != nil {
		verb = "add"
	}
	if out, err := runGitIn(workDir, "add", "--", rel); err != nil {
		e.logf(item.Number, "warn", "could not stage %s: %v: %s\n", rel, err, strings.TrimSpace(out))
		return false
	}
	msg := fmt.Sprintf("docs(spec): %s %s for #%d", verb, rel, item.Number)
	if out, err := runGitIn(workDir, "commit", "-m", msg, "--", rel); err != nil {
		e.logf(item.Number, "warn", "could not commit %s: %v: %s\n", rel, err, strings.TrimSpace(out))
		return false
	}
	e.logf(item.Number, "spec", "%s %s\n", verb, rel)
	return true
}

// specCleanInHead reports whether rel is tracked and has no staged or unstaged
// changes, i.e. the on-disk spec is exactly what HEAD holds. Any git error
// reports false so the caller falls through to (idempotent) add and commit.
func specCleanInHead(workDir, rel string) bool {
	if _, err := runGitIn(workDir, "cat-file", "-e", "HEAD:"+rel); err != nil {
		return false
	}
	out, err := runGitIn(workDir, "status", "--porcelain", "--", rel)
	return err == nil && strings.TrimSpace(out) == ""
}

// runGitIn runs git in dir and returns its combined output.
func runGitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
