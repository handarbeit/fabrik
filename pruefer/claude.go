package pruefer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeprofile"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// reviewShellAllow lists the read-only shell commands the reviewer may run
// (#2192, R2). Each is a prefix rule; Claude Code path-checks the operands of
// these commands (and redirect targets) against the Read rules, so they reach
// only the clone and the staged diff. Deliberately absent, each with a form a
// prefix rule cannot exclude (probed against claude 2.1.296, adrs/2192):
//   - sed: `sed -n -i 1p file` passed `Bash(sed -n *)`; `w`/`e` script commands
//     write and execute.
//   - rg: `--pre <cmd>` executes a program.
//   - find: `-exec`/`-delete`; Glob covers it. (The CLI's built-in read-only
//     set would otherwise allow it in the clone, hence reviewShellDeny.)
//   - wc: `--files0-from=<file>` reads a NUL-separated file list from an
//     arbitrary path, and GNU getopt_long accepts any unambiguous prefix of the
//     option (`--files0=`, `--files0-f=`), so no deny on one spelling can close
//     it. Read offset/limit, head and tail cover paging; wc is denied outright
//     (reviewShellDeny) because the CLI's built-in read-only set would
//     otherwise still admit it.
//   - git: diff/grep `--no-index` and `blame --contents` read outside the
//     scope, `--output=`/`-O<cmd>` write or execute, and the depth-1 clone has
//     no history for log/blame/diff to report.
var reviewShellAllow = []string{"cat", "head", "tail", "ls"}

// reviewShellDeny are deny rules passed via --disallowedTools. Deny beats
// allow, so these only remove capability the CLI would otherwise grant.
var reviewShellDeny = []string{
	"Bash(find *)",
	"Bash(wc *)", // --files0-from (and its abbreviations) reads a file list from any path
}

// escapeRulePath escapes a resolved absolute path for use inside a Claude Code
// permission rule: gitignore metacharacters (\ [ ] * ? !, leading #) and the
// rule delimiters ( ).
func escapeRulePath(p string) string {
	var b strings.Builder
	for i, r := range p {
		switch r {
		case '\\', '[', ']', '*', '?', '!', '(', ')':
			b.WriteByte('\\')
		case '#':
			if i == 0 {
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveScopePaths returns the symlink-resolved absolute clone directory and
// (when non-empty) staged diff directory. The CLI applies an allow rule only
// when both the requested path and its resolved target match, and on macOS the
// temp dir is a symlink (/var -> /private/var), so unresolved paths would
// match nothing and every read would fail. Any failure is returned: Review
// fails closed rather than running without a scope.
func resolveScopePaths(workDir, stagedDir string) (clone, staged string, err error) {
	if workDir == "" {
		return "", "", errors.New("review scope: empty work dir")
	}
	clone, err = resolveAbs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("review scope: resolving clone %q: %w", workDir, err)
	}
	if stagedDir != "" {
		staged, err = resolveAbs(stagedDir)
		if err != nil {
			return "", "", fmt.Errorf("review scope: resolving staged diff dir %q: %w", stagedDir, err)
		}
	}
	for _, p := range []string{clone, staged} {
		if strings.ContainsAny(p, "\n\r\x00") {
			return "", "", fmt.Errorf("review scope: path %q contains a control character", p)
		}
	}
	return clone, staged, nil
}

func resolveAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// skillRef is the qualified review skill this request runs.
func (r ReviewRequest) skillRef() string {
	if r.SkillRef != "" {
		return r.SkillRef
	}
	return reviewplugin.DefaultRef
}

// buildReviewToolRules builds the permission rules for a review from the
// already-resolved clone and staged-diff-directory paths (#2192, R1-R3; the
// diff is a directory of entries since #2190). Scope is expressed as
// path-scoped ALLOWS only — Claude Code's deny rules beat allows, so "deny
// everything, allow the clone" cannot be written. There is deliberately no
// bare Read, Grep or Glob entry: an unscoped allow admits every path, and
// probing showed Read(...) rules do not scope the Grep/Glob tools, so each
// gets its own path rule. Task (subagents) inherits these rules.
func buildReviewToolRules(clone, stagedDir, skillRef string) (allow, deny []string) {
	root := "//" + strings.TrimPrefix(escapeRulePath(clone), "/") + "/**"
	allow = append(allow, "Read("+root+")", "Grep("+root+")", "Glob("+root+")")
	if stagedDir != "" {
		staged := "//" + strings.TrimPrefix(escapeRulePath(stagedDir), "/") + "/**"
		allow = append(allow, "Read("+staged+")", "Grep("+staged+")", "Glob("+staged+")")
	}
	for _, c := range reviewShellAllow {
		allow = append(allow, "Bash("+c+" *)")
	}
	allow = append(allow, "Task", "Skill("+skillRef+")")
	return allow, append([]string(nil), reviewShellDeny...)
}

// checkReviewArgsScoped reports an error when args grant anything beyond the
// scoped read-only set: a bare Read/Grep/Glob/Bash, an unanchored path rule,
// a write/execute/network tool, or a missing --disallowedTools for find. The
// tests apply it to the built args and, for non-vacuity, to the legacy
// unscoped list, where it must report a violation.
func checkReviewArgsScoped(args []string) error {
	var allows, denies []string
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "--allowedTools":
			allows = append(allows, args[i+1])
		case "--disallowedTools":
			denies = append(denies, args[i+1])
		}
	}
	shell := map[string]bool{}
	for _, c := range reviewShellAllow {
		shell["Bash("+c+" *)"] = true
	}
	for _, r := range allows {
		switch {
		case r == "Task" || shell[r]:
		case strings.HasPrefix(r, "Skill("+reviewplugin.PluginName+":") && strings.HasSuffix(r, ")") && !strings.Contains(r, "*"):
		case strings.HasPrefix(r, "Read(//") || strings.HasPrefix(r, "Grep(//") || strings.HasPrefix(r, "Glob(//"):
		default:
			return fmt.Errorf("allow rule %q is not a scoped read-only rule", r)
		}
	}
	for _, want := range reviewShellDeny {
		if !slices.Contains(denies, want) {
			return fmt.Errorf("missing --disallowedTools %q", want)
		}
	}
	return nil
}

// reviewInactivityTimeout mirrors engine/claude.go's hardcoded 15-minute
// inactivity watchdog: if claude produces no stdout for this long, the
// process is presumed stuck and killed so it doesn't hold a concurrency
// slot forever. Var (not const) so tests can shrink it.
var reviewInactivityTimeout = 15 * time.Minute

// reviewWaitDelay mirrors engine/claude.go's claudeWaitDelay: how long to
// wait for stdout pipe drain after the process exits before giving up on
// recovering output held open by a grandchild process.
var reviewWaitDelay = 30 * time.Second

// reviewKillGrace is the SIGINT/SIGTERM grace window used by Review's kill
// escalation. Fixed (unlike Fabrik's per-stage configurable kill_grace)
// since Pruefer has no stage YAML to source an override from.
var reviewKillGrace = 10 * time.Second

// ReviewRequest bundles the inputs for a single Claude review invocation.
// The PR's head commit is expected to already be checked out at WorkDir
// (see worktree.go) — Claude inspects it directly rather than receiving the
// diff purely as prompt text, so it can use git/Read/Grep/Glob for
// surrounding-code context.
type ReviewRequest struct {
	Owner      string
	Repo       string
	PRNumber   int
	Title      string
	Body       string
	HeadSHA    string
	BaseBranch string
	Model      string
	Effort     string
	// ClaudeConfigDir is the resolved Claude profile directory this review runs
	// under (#2294), copied from Config at dispatch so a review already running
	// keeps the value it started with across a reload. Empty means nothing
	// resolved: the review process then carries no CLAUDE_CONFIG_DIR at all.
	ClaudeConfigDir string
	WorkDir         string // ephemeral clone directory; claude's cwd
	// PluginDir is the per-review directory holding the embedded pruefer-review
	// plugin, passed as the only --plugin-dir (#2306). SkillRef is the
	// qualified review skill resolved against it; empty means the default,
	// pruefer-review:review. Review fills both; tests building args or prompts
	// directly may leave them empty.
	PluginDir   string
	SkillRef    string
	MaxWallTime time.Duration // 0 = no wall-time cap
	// ReviewThreads carries existing review threads (resolved and
	// unresolved) on the PR, fetched by ReviewPR via
	// GitHubReviewer.FetchPRReviewThreads (R1). nil when the fetch failed
	// (non-fatal degrade) or the PR genuinely has no threads yet —
	// buildReviewPrompt renders no thread-context section in either case.
	ReviewThreads []gh.PRReviewThread
	// ReviewThreadsTruncated is true when the PR has more review threads
	// than FetchPRReviewThreads' own fetch-layer page size returned (distinct
	// from selectPromptThreads' smaller prompt-level cap, and from any
	// individual thread's CommentsTruncated). Always false on a degraded
	// (nil ReviewThreads) fetch. buildReviewPrompt notes this so a PR that
	// has grown past the fetch ceiling doesn't silently look complete.
	ReviewThreadsTruncated bool
	// OmittedExcludedPaths lists files review.go's per-file exclusion
	// pipeline dropped via cfg.ExcludedPaths before measuring the diff
	// against max_diff_bytes (R2). Disjoint from OmittedTrimmedPaths — a
	// file is dropped by exactly one of the two passes, never both.
	OmittedExcludedPaths []string
	// OmittedTrimmedPaths lists files review.go's size guard additionally
	// dropped (largest first) because the diff still exceeded
	// max_diff_bytes after exclusion (R4). Both fields are disclosed to the
	// model so it never silently assumes it saw the whole change — see
	// renderOmittedPaths.
	OmittedTrimmedPaths []string
	// OmittedReviewedPaths lists the files of an engine-authored merge-train
	// trial PR that review.go withheld because each is the already-reviewed
	// patch of exactly one member PR (#2230); only the remaining files — the
	// conflict-resolution edits — are in the staged diff.
	OmittedReviewedPaths []string
	// OperatorGuidance/OperatorGuidanceMode carry #1446's operator-level
	// review-guidance override (Config.ReviewGuidance/ReviewGuidanceMode),
	// composed onto the embedded default guidance by resolveGuidance before
	// RepoGuidance is composed on top of that result. Empty
	// OperatorGuidance is a no-op regardless of mode.
	OperatorGuidance     string
	OperatorGuidanceMode string
	// RepoGuidance/RepoGuidanceMode carry #1446's repo-resident
	// review-guidance skill (.pruefer/skills/review/SKILL.md, resolved at
	// the PR's base ref — never the head, see C1/ADR-1446), composed by
	// resolveGuidance as the last (outermost) guidance layer. Empty
	// RepoGuidance is a no-op regardless of mode — the common case, since
	// most repos have no skill file at all.
	RepoGuidance     string
	RepoGuidanceMode string
	// Staged describes the directory the PR's diff is staged in — the same
	// bytes review.go already fetched for the size guard, after exclusion and
	// trimming, so it is exactly the change the model is being asked to review
	// and nothing more. One entry file per changed path (or bounded chunk of a
	// large one) plus an index, so no part of the diff sits behind a single
	// Read call's limit (#2190, see diffstage.go).
	//
	// It exists because the review clone structurally cannot produce that
	// diff itself: CloneForReview does a depth-1 fetch of
	// refs/pull/<N>/head and nothing else, so the base branch is not a ref
	// in that repository and `git diff <base>...HEAD` cannot resolve. nil (the
	// degraded case: no diff text, or staging failed) falls back to the older
	// wording so a review still runs, and its coverage is `partial`.
	Staged *StagedDiff
}

// ClaudeInvoker defines the interface for invoking Claude Code to produce
// review text for a single PR. Deliberately narrower than
// engine.ClaudeInvoker — no stages.Stage, no gh.ProjectItem — since Pruefer
// has no board/stage concept.
type ClaudeInvoker interface {
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
}

// ReviewResult carries a completed review invocation's output text plus the
// turn/cost instrumentation the TUI's cost-visibility requirement depends
// on, mirroring engine/claude.go's TokenUsage capture (NumTurns/CostUSD
// parsed from claude's stream-json "result" envelope).
type ReviewResult struct {
	Text     string
	NumTurns int
	CostUSD  float64
	// Opened is the set of staged entry IDs the invocation's transcript proves
	// were read in full, and OpenedAuthoritative whether that transcript shows
	// every subagent's traffic (see deriveOpened). Both are zero for an
	// invoker that cannot observe a transcript, which leaves coverage resting
	// on the model's own report.
	Opened              map[string]bool
	OpenedAuthoritative bool
	// UnobservedSubagents is set when a Task/Agent call's traffic was not
	// visible in the transcript (see deriveOpened): coverage then refuses to
	// credit what only an invisible subagent could have read.
	UnobservedSubagents bool
	// Transcript is the invocation's raw stream-json stdout, unmodified. It is
	// empty for an invoker that has no transcript. ReviewPR keeps it only when
	// coverage is partial (see maybeKeepTranscript, #2303).
	Transcript []byte
}

// RealClaudeInvoker invokes the real claude CLI via exec.CommandContext,
// mirroring engine/claude.go's runClaude: non-interactive permission mode,
// a fixed read-only tool allowlist, an inactivity watchdog, an optional
// wall-time deadline, WaitDelay for buffered-stdout recovery, and graceful
// SIGINT→SIGTERM→SIGKILL process-group kill on cancellation.
type RealClaudeInvoker struct{}

// maxPromptThreads caps how many existing review threads buildReviewPrompt
// renders (R4). A fixed constant, not operator-configurable: no evidence yet
// that this needs tuning, and it keeps buildReviewPrompt a pure function of
// ReviewRequest with no config dependency. See selectPromptThreads for the
// selection/ordering policy applied when a PR has more threads than this.
const maxPromptThreads = 20

// selectPromptThreads chooses which of threads to render in the prompt and
// reports how many were left out, implementing R4's cap. Ordering keeps the
// highest-signal context under the cap: unresolved threads before resolved
// ones (an open finding is more actionable than a closed one), and within
// each group, non-outdated (still-current) threads before outdated (stale)
// ones, most-recently-commented first. Truncation therefore drops resolved
// threads before unresolved ones, and stale threads before active ones
// within each group.
func selectPromptThreads(threads []gh.PRReviewThread, limit int) (selected []gh.PRReviewThread, omitted int) {
	if len(threads) <= limit {
		return threads, 0
	}
	ordered := make([]gh.PRReviewThread, len(threads))
	copy(ordered, threads)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.IsResolved != b.IsResolved {
			return !a.IsResolved // unresolved first
		}
		if a.IsOutdated != b.IsOutdated {
			return !a.IsOutdated // non-outdated first
		}
		return threadLastCommentTime(a).After(threadLastCommentTime(b))
	})
	return ordered[:limit], len(ordered) - limit
}

// threadLastCommentTime returns the CreatedAt of a thread's most recent
// comment, or the zero time if it has none.
func threadLastCommentTime(t gh.PRReviewThread) time.Time {
	var latest time.Time
	for _, c := range t.Comments {
		if c.CreatedAt.After(latest) {
			latest = c.CreatedAt
		}
	}
	return latest
}

// maxThreadCommentBodyChars caps how many characters of a single review
// thread comment body renderReviewThreads renders (R4). maxPromptThreads
// bounds the number of threads and FetchPRReviewThreads's per-thread page
// size bounds the number of comments, but neither bounds the size of an
// individual comment body — a single large pasted comment (e.g. a log or
// long discussion) could still make the prompt unbounded without this.
// Truncation is noted inline, consistent with the CommentsTruncated signal
// below, so the reviewer knows content was cut rather than trusting a
// silently partial body.
const maxThreadCommentBodyChars = 2000

// truncateCommentBody bounds body to at most maxThreadCommentBodyChars
// runes, appending a note naming how much was cut when truncation occurs.
func truncateCommentBody(body string) string {
	runes := []rune(body)
	if len(runes) <= maxThreadCommentBodyChars {
		return body
	}
	return string(runes[:maxThreadCommentBodyChars]) + fmt.Sprintf(" […truncated, %d more characters]", len(runes)-maxThreadCommentBodyChars)
}

// renderReviewThreads writes the "## Existing review threads" section plus
// the R2 policy paragraph that governs it. Both belong to buildReviewPrompt's
// eventual Go-owned "contract" half once #1446 splits the function into an
// overridable guidance half and a Go-owned contract half — a repo using
// `mode: replace` on the guidance half must not be able to silently drop
// this context and reintroduce the duplicate findings this exists to
// eliminate (see adrs/1497-pruefer-prior-review-thread-context.md).
func renderReviewThreads(b *strings.Builder, threads []gh.PRReviewThread, threadsTruncated bool) {
	if len(threads) == 0 {
		return
	}
	selected, omitted := selectPromptThreads(threads, maxPromptThreads)

	b.WriteString("## Existing review threads\n\n")
	b.WriteString("These are review threads already on this PR, from prior review passes.\n\n")
	for _, t := range selected {
		status := "[OPEN]"
		if t.IsResolved {
			status = "[RESOLVED]"
		}
		if t.IsOutdated {
			status += " (outdated — the commented lines have since changed)"
		}
		fmt.Fprintf(b, "- %s:%d %s\n", t.Path, t.Line, status)
		for _, c := range t.Comments {
			author := c.Author
			if author == "" {
				author = "unknown"
			}
			fmt.Fprintf(b, "  - @%s: %s\n", author, truncateCommentBody(c.Body))
		}
		if t.CommentsTruncated {
			b.WriteString("  - (this thread has more replies than shown here — a later reply, possibly the one that resolved it, may be missing)\n")
		}
	}
	if omitted > 0 {
		fmt.Fprintf(b, "\n(%d additional thread(s) omitted to keep this prompt bounded; unresolved and current threads were prioritized over resolved and outdated ones.)\n", omitted)
	}
	if threadsTruncated {
		b.WriteString("\n(This PR has more review threads than could be fetched; the newest threads — disproportionately likely to be open — may be missing from the list above entirely.)\n")
	}
	b.WriteString("\n")

	b.WriteString("Policy for the threads above: do not raise a finding that restates an [OPEN] thread — it is already tracked. Do not restate a [RESOLVED] thread unless the fix (or the current diff) introduces a new defect, in which case say so explicitly and reference the prior thread. Where a thread was answered with a reasoned reply, engage with that reasoning or drop the point — do not simply repeat the original observation.\n\n")
}

// renderOmittedPaths writes a short, additive prompt section naming files
// review.go's diff-size guard omitted from what it measured and from
// inline-comment anchoring (R4). This is load-bearing, not cosmetic: unlike
// the diff text itself, Claude never receives it as prompt content — it reads
// the clone and the staged diff with its own tools, so a passive "FYI,
// these were excluded" note is what stops it from reading an omitted file
// directly, which in the motivating scenario (a 17 MB JSONL corpus) could blow
// its own context. It is backstopped by validRightAnchors demoting any finding
// Claude reports on an omitted file anyway (R6) rather than trusting the
// instruction alone. (#2192: the old `git diff ... -- . ':!path'` example is
// gone — git is not available to the reviewer and the clone has no base ref.)
func renderOmittedPaths(b *strings.Builder, excluded, trimmed, reviewed []string) {
	if len(excluded) == 0 && len(trimmed) == 0 && len(reviewed) == 0 {
		return
	}
	b.WriteString("## Files omitted from this review\n\n")
	b.WriteString("Pruefer's size guard omitted the following files from what it measured against max_diff_bytes and from inline-comment anchoring. They are not in the staged diff. Do not review them, and do not read them from the working tree:\n\n")
	for _, p := range excluded {
		fmt.Fprintf(b, "- %s (excluded by `excluded_paths`)\n", p)
	}
	for _, p := range trimmed {
		fmt.Fprintf(b, "- %s (dropped to fit within max_diff_bytes)\n", p)
	}
	for _, p := range reviewed {
		fmt.Fprintf(b, "- %s (identical to a member PR's patch that was already reviewed; this merge-train trial PR is being reviewed only for what the members' patches do not already cover)\n", p)
	}
	b.WriteString("\n")
}

// resolveGuidance composes the operator and repo guidance layers (#1446, R3)
// that sit on top of the default review skill (#2306). The default layer is
// no longer text: it is the embedded pruefer-review:review skill, which the
// prompt tells the reviewer to invoke. Each overridable layer still folds onto
// the one below with its own independent append/replace mode — the operator
// layer first, then the repo layer onto that result. An absent layer (empty
// guidance text) is a no-op regardless of its mode.
//
// replacesDefault reports that some non-empty layer is in replace mode, i.e.
// the guidance replaces the skill's own criteria rather than adding to them.
func resolveGuidance(req ReviewRequest) (guidance string, replacesDefault bool) {
	guidance = composeGuidanceLayer("", req.OperatorGuidance, req.OperatorGuidanceMode)
	guidance = composeGuidanceLayer(guidance, req.RepoGuidance, req.RepoGuidanceMode)
	replacesDefault = (strings.TrimSpace(req.OperatorGuidance) != "" && req.OperatorGuidanceMode == GuidanceModeReplace) ||
		(strings.TrimSpace(req.RepoGuidance) != "" && req.RepoGuidanceMode == GuidanceModeReplace)
	return guidance, replacesDefault
}

// composeGuidanceLayer folds overlay onto base according to mode (R2/R4):
// GuidanceModeReplace substitutes base entirely with overlay;
// GuidanceModeAppend (and any value normalizeGuidanceMode has already
// reduced to it, including the empty default) concatenates overlay after
// base, separated by a blank line. An empty (or all-whitespace) overlay is
// always a no-op — the layer is treated as absent — regardless of mode,
// since there is nothing to append or to replace with; this is what makes
// AC1's zero-config prompt byte-identical to the pre-#1446 output despite
// resolveGuidance always calling this function twice.
func composeGuidanceLayer(base, overlay, mode string) string {
	if strings.TrimSpace(overlay) == "" {
		return base
	}
	if mode == GuidanceModeReplace {
		return overlay
	}
	if base == "" {
		return overlay
	}
	return base + "\n\n" + overlay
}

// renderReviewTools writes the exact tools and shell forms the reviewer has
// (#2192, R5), so it does not guess at commands that will be denied. The shell
// command list is generated from reviewShellAllow, and a test asserts every
// command form offered here is covered by the permission rules.
func renderReviewTools(b *strings.Builder, req ReviewRequest) {
	b.WriteString("The PR's head commit is already checked out in your working directory. You are a read-only reader and have exactly these tools; anything else is denied:\n\n")
	b.WriteString("- Read: read a file by absolute path. Use its offset and limit to page through a large file.\n")
	b.WriteString("- Grep and Glob: search file contents and file names.\n")
	b.WriteString("- Shell, only these read-only commands, one plain command at a time: ")
	for i, c := range reviewShellAllow {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "`%s`", c)
	}
	b.WriteString(". Pipes between them are fine; redirection, command substitution and chaining with `;`, `&&` or `||` are not.\n")
	b.WriteString("- Skill: invoke the review skill named below. It only loads instructions; it grants no other access.\n")
	b.WriteString("- Task: delegate part of a large diff (by file or area) to a subagent. Subagents are bound by exactly the same tools and path limits, and have no other tools. Only your own final message is the review, so merge their findings into it yourself.\n\n")
	b.WriteString("Path limits: Read, Grep, Glob and the shell commands reach only the working directory")
	if req.Staged != nil {
		b.WriteString(" and the staged diff directory named below")
	}
	b.WriteString(". Any other path, including `..` or symlinks that lead outside, is denied.\n\n")
	b.WriteString("Not available, so do not try them: find, wc, sed, rg, git of any kind, and any command that writes files, runs code (tests, builds, linters, package managers) or uses the network. You have no write access.\n\n")
}

// renderDynamicContext writes buildReviewPrompt's Go-supplied dynamic
// context (C7): the reviewer-identity/instructions preamble, the base-branch
// diff instruction, omitted-paths disclosure, the PR body, and prior review-
// thread context. None of this is "guidance" in the R2 sense — it is
// per-review runtime data — so it is never affected by any guidance layer or
// mode; buildReviewPrompt calls this unconditionally, before resolveGuidance
// is even consulted. Moved verbatim from the pre-#1446 buildReviewPrompt, no
// behavior change.
func renderDynamicContext(b *strings.Builder, req ReviewRequest) {
	fmt.Fprintf(b, "You are Pruefer, an automated code reviewer for pull request %s/%s#%d: %q.\n\n", req.Owner, req.Repo, req.PRNumber, req.Title)
	renderReviewTools(b, req)
	if req.Staged != nil {
		// The clone has no base ref (depth-1 head-only fetch), so a
		// three-dot diff against the base cannot resolve here. Hand over
		// the diff Pruefer already has instead of naming a command that
		// will fail.
		fmt.Fprintf(b, "This PR's complete unified diff — exactly the change under review, already filtered to what you should review — is staged as %d entries in %s. Read %s first: it lists every entry with its ID, file, original path, chunk and size. Each entry is one small file holding one changed path's diff (a large path is cut into numbered chunks that each repeat the file header, and hunk headers carry the true line numbers). Together they are the authoritative statement of what changed. The working directory holds the PR's head commit, so use Grep, Glob and Read on it for surrounding context the diff alone doesn't give you.\n\n", len(req.Staged.Entries), req.Staged.Dir, req.Staged.IndexPath)
		fmt.Fprintf(b, "Entry files are named <ID>.diff, IDs 0001 through %04d, so if INDEX.txt itself is cut off you can still open the rest by name.\n\n", len(req.Staged.Entries))
		b.WriteString("Open every entry in full: use Read, or `cat`. Read can silently stop before the end of a file, so check that its last numbered line matches the entry's line count in the index and, if it stops early, Read again from the next line with offset. An entry flagged wide-line holds lines Read cuts mid-line: open those with `cat` only. If you split the diff across subagents, every index entry must be opened by some reader. Do not stop after the first entries — tests and later files are listed last, and an unread entry is reported to the PR as unread.\n\n")
		if req.BaseBranch != "" {
			fmt.Fprintf(b, "The PR targets %q. The checkout is a shallow, single-commit fetch of the PR head with no git history or base ref, so rely on the staged diff above rather than trying to reconstruct the change.\n\n", req.BaseBranch)
		}
	} else if req.BaseBranch != "" {
		fmt.Fprintf(b, "The PR targets %q. The checkout is a shallow, single-commit fetch of the PR head with no git history or base ref, and no staged diff is available: read the changed files in the working tree directly.\n\n", req.BaseBranch)
	}
	renderOmittedPaths(b, req.OmittedExcludedPaths, req.OmittedTrimmedPaths, req.OmittedReviewedPaths)
	if req.Body != "" {
		b.WriteString("## PR description\n\n")
		b.WriteString(req.Body)
		b.WriteString("\n\n")
	}
	renderReviewThreads(b, req.ReviewThreads, req.ReviewThreadsTruncated)
}

// renderContract writes the Go-owned, never-overridable output-contract half
// of the review prompt (C5): the no-approval-language rule and the two-part
// output format (PRUEFER_SUMMARY_BEGIN/END delimiters, the fenced JSON
// findings schema, the severity enum, and line-anchoring rules). No
// guidance layer — embedded default, operator override, or repo skill, in
// either append or replace mode — can reach this function's output;
// buildReviewPrompt calls it unconditionally, after resolveGuidance. Moved
// verbatim from the pre-#1446 buildReviewPrompt, no behavior change.
func renderContract(b *strings.Builder, withCoverage bool) {
	b.WriteString("You do not decide whether this PR is approved or blocked — that is computed automatically from the severity you assign each finding below, never from anything you write in prose. Do not use approval/rejection language such as \"LGTM\" or \"requesting changes\" in your summary; just describe what you found.\n\n")
	if withCoverage {
		b.WriteString("Output has three parts, in this exact order:\n\n")
	} else {
		b.WriteString("Output has two parts, in this exact order:\n\n")
	}
	b.WriteString("1. A short prose summary: what you reviewed and your overall assessment. This is the only text GitHub shows outside of inline comments, so it must stand on its own. Wrap it in PRUEFER_SUMMARY_BEGIN and PRUEFER_SUMMARY_END marker lines, each alone on its own line, with nothing else on those lines. Nothing — no narration, no meta-commentary, no investigation notes — may appear before PRUEFER_SUMMARY_BEGIN; anything there is discarded and never shown to anyone. The ```json findings block described in part 2 below must come after PRUEFER_SUMMARY_END, never between the two markers. For example:\n\n")
	b.WriteString("PRUEFER_SUMMARY_BEGIN\nReviewed the changes to X. Found one medium-severity issue; see inline comment.\nPRUEFER_SUMMARY_END\n\n")
	b.WriteString("2. A single fenced ```json code block containing a JSON array of your findings, each anchored to the exact file and line it concerns, with a \"severity\" classification:\n\n")
	b.WriteString("```json\n[{\"path\": \"engine/claude.go\", \"line\": 954, \"body\": \"...\", \"severity\": \"low\"}]\n```\n\n")
	b.WriteString("\"severity\" must be exactly one of:\n\n")
	b.WriteString("- \"low\": style, minor nit, or a suggestion — not a defect.\n")
	b.WriteString("- \"medium\": a real defect, but scoped and low-impact.\n")
	b.WriteString("- \"high\": a bug or design issue that will likely cause incorrect behavior.\n")
	b.WriteString("- \"critical\": a security vulnerability, data loss, or severe correctness bug.\n\n")
	b.WriteString("Each entry's \"path\" must be a file path exactly as it appears in the diff, and \"line\" must be a line number in the new (post-change) version of that file — i.e. a line you can see in `git diff` output prefixed with `+` or unprefixed (context), never a line that only existed in the old version. If you have no findings, emit an empty array `[]`. Do not put findings only in the prose — every specific, actionable finding belongs in the JSON array so it can be attached to its exact line; use the prose summary for overall assessment only. Report each distinct underlying finding once — if the same defect is visible at more than one line, pick the most relevant anchor rather than emitting a separate entry per line.\n\n")
	if withCoverage {
		b.WriteString("3. After the findings block, a coverage block listing the ID of every staged diff entry that was opened and read in full, by you or by a subagent you delegated it to. Put each marker alone on its own line, with a JSON object between them:\n\n")
		b.WriteString("PRUEFER_COVERAGE_BEGIN\n{\"opened\": [\"0001\", \"0002\"]}\nPRUEFER_COVERAGE_END\n\n")
		b.WriteString("List only entries you actually read in full; an entry you do not list is reported to the PR as unread, and listing one you did not read defeats the review. If you delegate entries to subagents, give each the entry IDs it must open and tell it to report back the IDs it opened in full, and include in your list exactly the IDs the subagents report. If you opened nothing, report an empty list.\n\n")
	}
	b.WriteString("Output ONLY the review text itself: no preamble, no meta-commentary about what you are about to do.\n")
}

// buildReviewPrompt constructs the prompt sent to claude via stdin, in three
// sequential parts (#1446): Go-supplied dynamic context, the composed
// guidance layers, then the Go-owned output contract — the same order the
// monolithic pre-#1446 function wrote them in, so a zero-config
// ReviewRequest produces a byte-identical prompt (AC1/C2).
func buildReviewPrompt(req ReviewRequest) string {
	var b strings.Builder
	renderDynamicContext(&b, req)
	guidance, replaces := resolveGuidance(req)
	renderSkillSection(&b, req.skillRef(), guidance, replaces)
	renderContract(&b, req.Staged != nil)
	return b.String()
}

// buildReviewArgs constructs the claude CLI argument list for a review
// invocation. Always non-interactive (--permission-mode dontAsk) and always
// the fixed read-only allowlist — there is no unrestricted/label-driven
// escape hatch, unlike Fabrik stages.
//
// Together with buildReviewToolRules above and --permission-mode dontAsk
// below, an empty --setting-sources is the third piece of Pruefer's
// security posture: it loads NO settings layer at all — not the reviewed
// PR's .claude/settings.json or settings.local.json, and not the
// operator's own ~/.claude/settings.json either.
//
// Excluding the PR's own files is the original reason (they come from code
// that has not been reviewed yet, so a malicious or careless PR could
// otherwise widen the reviewer's tool grants via permissions.allow). That
// remains additive to the untrusted-workspace default which already blocks
// them — the clone is never marked trusted — and it still suppresses the
// "Ignoring N permissions.allow entries" warning.
//
// Excluding the *operator's* settings is newer, and is about
// reproducibility rather than trust. This previously passed
// "--setting-sources user", which made every review depend on whichever
// profile the daemon happened to inherit via CLAUDE_CONFIG_DIR (the profile is now
// declared explicitly: see ReviewRequest.ClaudeConfigDir). Switching
// accounts silently rewired the reviewer: one profile carried a PreToolUse
// hook (an output-compressing proxy applied to the reviewer's own git and
// file reads), "permissions.defaultMode": "auto", and an "effortLevel" —
// none of which Pruefer chose, none of which appear in its log, and all of
// which change how thoroughly a PR is read. Reviews degraded fleet-wide to
// ~20s with no error anywhere.
//
// Everything Pruefer actually needs from settings it already passes
// explicitly (--model, CLAUDE_CODE_EFFORT_LEVEL, --allowedTools,
// --permission-mode), so loading none makes a review a function of the PR
// and Pruefer's own config alone. Authentication is unaffected: it comes
// from the config directory, not from any settings layer.
func buildReviewArgs(req ReviewRequest) ([]string, error) {
	stagedDir := ""
	if req.Staged != nil {
		stagedDir = req.Staged.Dir
	}
	clone, staged, err := resolveScopePaths(req.WorkDir, stagedDir)
	if err != nil {
		return nil, err
	}
	allow, deny := buildReviewToolRules(clone, staged, req.skillRef())
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "dontAsk",
		"--setting-sources", "",
	}
	if req.PluginDir != "" {
		args = append(args, "--plugin-dir", req.PluginDir)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	for _, tool := range allow {
		args = append(args, "--allowedTools", tool)
	}
	for _, tool := range deny {
		args = append(args, "--disallowedTools", tool)
	}
	return args, nil
}

// buildReviewEnv returns the environment variable overrides applied on top
// of os.Environ() for a review invocation.
func buildReviewEnv(req ReviewRequest) []string {
	effort := req.Effort
	if effort == "" {
		effort = DefaultEffort
	}
	env := []string{
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1",
		"CLAUDE_CODE_EFFORT_LEVEL=" + effort,
	}
	if req.ClaudeConfigDir != "" {
		// The resolved profile is the only source of this variable
		// (reviewProcessEnv drops the inherited one), so the account a review
		// bills to is declared, not whatever the daemon happened to inherit.
		env = append(env, claudeprofile.BareEnv+"="+req.ClaudeConfigDir)
	}
	return env
}

// mergeEnv builds a subprocess environment from base (typically
// os.Environ()), with overrides applied on top. Keys present in overrides
// are removed from base first so overrides take effect even when base
// already contains the same key — mirrors engine/claude.go's mergeEnv.
func mergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	keys := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[kv[:i]] = true
		}
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && keys[kv[:i]] {
			continue
		}
		result = append(result, kv)
	}
	return append(result, overrides...)
}

// activityWriter wraps an io.Writer and updates a shared atomic timestamp on
// every Write call. Used by the inactivity watchdog — mirrors
// engine/claude.go's activityWriter.
type activityWriter struct {
	inner        io.Writer
	lastActivity *atomic.Int64
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.lastActivity.Store(time.Now().UnixNano())
	return w.inner.Write(p)
}

// claudeReviewResponse mirrors the subset of claude --output-format
// stream-json's final result object that Pruefer needs. NumTurns/CostUSD
// mirror engine/claude.go's claudeResponse fields of the same JSON keys.
type claudeReviewResponse struct {
	Result   string  `json:"result"`
	IsError  bool    `json:"is_error"`
	NumTurns int     `json:"num_turns"`
	CostUSD  float64 `json:"total_cost_usd"`
}

// parseClaudeReviewJSON parses claude's NDJSON stream output, handling both
// a single result object and a conversation array (only the last
// {"type":"result",...} line matters in the array case).
func parseClaudeReviewJSON(output []byte) (claudeReviewResponse, bool) {
	var resp claudeReviewResponse
	if err := json.Unmarshal(output, &resp); err == nil && resp.Result != "" {
		return resp, true
	}
	var lastResult *claudeReviewResponse
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type     string  `json:"type"`
			Result   string  `json:"result"`
			IsError  bool    `json:"is_error"`
			NumTurns int     `json:"num_turns"`
			CostUSD  float64 `json:"total_cost_usd"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Type != "result" {
			continue
		}
		r := claudeReviewResponse{Result: envelope.Result, IsError: envelope.IsError, NumTurns: envelope.NumTurns, CostUSD: envelope.CostUSD}
		lastResult = &r
	}
	if lastResult != nil {
		return *lastResult, true
	}
	return claudeReviewResponse{}, false
}

// Review invokes the claude CLI once, in req.WorkDir, and returns its review
// text plus turn/cost instrumentation. On any failure — process error,
// timeout, unparseable output, or an is_error response — it returns a
// non-nil error and a zero-value ReviewResult. Per the issue's "on
// invocation failure, post nothing" requirement, callers must not submit a
// review when err != nil.
func (r *RealClaudeInvoker) Review(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
	logf(req.PRNumber, "claude", "reviewing %s/%s#%d (head %s) in %s\n", req.Owner, req.Repo, req.PRNumber, req.HeadSHA, req.WorkDir)

	stageCtx := ctx
	cancel := context.CancelFunc(func() {})
	if req.MaxWallTime > 0 {
		stageCtx, cancel = context.WithTimeout(ctx, req.MaxWallTime)
	}
	defer cancel()

	var stdout bytes.Buffer
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var stdoutWriter io.Writer = &activityWriter{inner: &stdout, lastActivity: &lastActivity}

	// Fail closed: never run a review without its path scope. The resolved
	// paths replace the request's so the prompt, the cwd and the rules all
	// name the same symlink-free paths (macOS /var -> /private/var).
	stagedDir := ""
	if req.Staged != nil {
		stagedDir = req.Staged.Dir
	}
	clone, staged, err := resolveScopePaths(req.WorkDir, stagedDir)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("building review arguments: %w", err)
	}
	req.WorkDir = clone
	if req.Staged != nil {
		resolvedStaged := *req.Staged
		resolvedStaged.Dir = staged
		resolvedStaged.IndexPath = filepath.Join(staged, stagedIndexName)
		req.Staged = &resolvedStaged
	}
	// R1/R2: a fresh plugin directory for this review alone, removed on every
	// return path, with the default skill resolved against exactly it.
	rp, err := setupReviewPlugin()
	if err != nil {
		return ReviewResult{}, fmt.Errorf("preparing review plugin: %w", err)
	}
	defer rp.Close()
	req.PluginDir, req.SkillRef = rp.Dir, rp.Ref
	args, err := buildReviewArgs(req)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("building review arguments: %w", err)
	}
	_, replaces := resolveGuidance(req)
	logf(req.PRNumber, "claude", "%s\n", rp.logLine(replaces))
	logf(req.PRNumber, "claude", "review tool rules: %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(stageCtx, "claude", args...)
	cmd.Dir = req.WorkDir
	cmd.Env = reviewProcessEnv(os.Environ(), req)
	cmd.Stdin = strings.NewReader(buildReviewPrompt(req))
	cmd.Stderr = os.Stderr
	cmd.Stdout = stdoutWriter
	waitDelay := reviewWaitDelay
	if waitDelay <= 0 {
		waitDelay = 30 * time.Second
	}
	cmd.WaitDelay = waitDelay
	setCmdProcAttr(cmd)

	watchdogCtx, watchdogCancel := context.WithCancel(context.Background())
	defer watchdogCancel()
	var inactivityFired atomic.Bool

	cmd.Cancel = func() error {
		if cmd.Process != nil {
			reason := "context_cancel"
			if req.MaxWallTime > 0 && stageCtx.Err() == context.DeadlineExceeded {
				reason = "max_wall_time"
			}
			killProcGroupGraceful(cmd.Process.Pid, req.PRNumber, "review", reason, reviewKillGrace, reviewKillGrace)
		}
		return nil
	}

	if err := cmd.Start(); err != nil {
		watchdogCancel()
		return ReviewResult{}, fmt.Errorf("starting claude: %w", err)
	}
	pid := cmd.Process.Pid
	// #1989: sample the command sessions the Bash tool creates for itself.
	stopSessionTracking := trackReviewSessions(watchdogCtx, pid, req.PRNumber)

	// Snapshot the tunables before the goroutine starts: it may first run after
	// Review has returned, and tests reassign these package vars between calls.
	idleLimit, killGrace := reviewInactivityTimeout, reviewKillGrace
	go func(pid int) {
		timer := time.NewTimer(idleLimit)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				since := time.Since(time.Unix(0, lastActivity.Load()))
				if since >= idleLimit {
					logf(req.PRNumber, "warn", "review invocation idle for %s with no output — killing\n", idleLimit)
					inactivityFired.Store(true)
					killProcGroupGraceful(pid, req.PRNumber, "review", "inactivity_timeout", killGrace, killGrace)
					return
				}
				timer.Reset(idleLimit - since)
			case <-watchdogCtx.Done():
				return
			}
		}
	}(pid)

	runErr := cmd.Wait()
	watchdogCancel()
	killProcGroup(cmd, req.PRNumber, "review")
	// #1989: reap the session too — separate-group members the group kill misses.
	exitKind := "clean_exit"
	if inactivityFired.Load() || stageCtx.Err() != nil {
		exitKind = "after_stop"
	}
	reapReviewSession(pid, req.PRNumber, exitKind)
	stopSessionTracking()

	if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil {
		logf(req.PRNumber, "warn", "WaitDelay fired: claude exited but grandchild processes held stdout pipe open; processing buffered output (%d bytes)\n", stdout.Len())
		runErr = nil
	}

	if runErr != nil {
		return ReviewResult{}, fmt.Errorf("claude exited with error: %w", runErr)
	}

	resp, ok := parseClaudeReviewJSON(bytes.TrimSpace(stdout.Bytes()))
	if !ok {
		return ReviewResult{}, fmt.Errorf("could not parse claude output (%d bytes)", stdout.Len())
	}
	if resp.IsError {
		return ReviewResult{}, fmt.Errorf("claude reported an error: %s", resp.Result)
	}
	if strings.TrimSpace(resp.Result) == "" {
		return ReviewResult{}, fmt.Errorf("claude produced an empty review")
	}
	if err := checkSkillRan(stdout.Bytes(), rp.Ref); err != nil {
		return ReviewResult{}, err
	}
	opened, authoritative := deriveOpened(stdout.Bytes(), req.Staged)
	return ReviewResult{Text: resp.Result, NumTurns: resp.NumTurns, CostUSD: resp.CostUSD, Opened: opened, OpenedAuthoritative: authoritative,
		UnobservedSubagents: req.Staged != nil && !authoritative, Transcript: stdout.Bytes()}, nil
}
