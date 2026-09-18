package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// toolchainStaleLabel is applied when checkToolchainDrift detects a
// declared-vs-resolved toolchain mismatch (#1786). See ADR-1786.
const toolchainStaleLabel = "fabrik:toolchain-stale"

// toolchainSubprocessTimeout bounds each `node --version`/`go version` shell-out.
// Resolution is cached per tool for the daemon's process lifetime (the
// premise of #1786 is that the inherited PATH cannot change without a
// restart), so this timeout is paid at most once per tool per daemon run.
const toolchainSubprocessTimeout = 5 * time.Second

// versionReq is a deliberately narrow, fail-closed toolchain version
// requirement. granularity controls how many components of the resolved
// version are compared: 1 = major only (.nvmrc, .tool-versions,
// package.json engines.node — Research's false-positive risk means these
// stay coarse), 2 = major.minor (go.mod toolchain/go directive, which state
// a minimum version Go itself already auto-satisfies via GOTOOLCHAIN=auto
// in the common case).
//
// op applies only at granularity 1: "" / "^" / "~" all collapse to an exact
// major-version match at this granularity (their distinction only matters
// at minor/patch precision, which this checker deliberately does not
// compare — see Plan's Key Decisions), ">=" requires the resolved major to
// be at least the declared major. Granularity 2 always uses >= semantics
// (a go.mod declares a minimum, never a pin).
type versionReq struct {
	op          string
	granularity int
	major       int
	minor       int
}

// satisfies reports whether resolved satisfies req. Called only on a
// successfully resolved binary version — an unresolved tool (missing from
// PATH, or an unparseable version) is handled upstream as "not comparable"
// and never reaches here.
func (req versionReq) satisfies(resolved resolvedVersion) bool {
	switch req.granularity {
	case 2:
		if resolved.major != req.major {
			return resolved.major > req.major
		}
		return resolved.minor >= req.minor
	default: // granularity 1
		if req.op == ">=" {
			return resolved.major >= req.major
		}
		return resolved.major == req.major
	}
}

// toolchainDeclaration is one declared toolchain constraint found in a
// worktree, alongside where it was declared (for the eventual warning) and
// which resolved binary it should be checked against.
type toolchainDeclaration struct {
	Tool     string // "node" or "go" — the resolveToolVersion key
	Source   string // human-readable origin, e.g. ".nvmrc" or "go.mod (toolchain)"
	Declared string // the raw declared version string, for display
	req      versionReq
}

// toolchainMismatch is one declaration whose resolved binary does not
// satisfy it, ready for display in the warning comment.
type toolchainMismatch struct {
	Source   string
	Declared string
	Resolved string
}

// versionCoreRE matches a bare, optionally "v"-prefixed numeric version with
// up to three dotted components and nothing else — used for .nvmrc and
// .tool-versions, both of which may otherwise carry a non-numeric alias
// (lts/*, lts/iron, system) that must be treated as not comparable rather
// than parsed incorrectly.
var versionCoreRE = regexp.MustCompile(`^v?(\d+)(?:\.\d+){0,2}$`)

// parseNvmrc reads .nvmrc from workDir. Returns nil if the file is absent,
// or if its content isn't a clean numeric version (e.g. an alias like
// lts/* or lts/iron) — both are "not comparable", never a mismatch.
func parseNvmrc(workDir string) *toolchainDeclaration {
	data, err := os.ReadFile(filepath.Join(workDir, ".nvmrc"))
	if err != nil {
		return nil
	}
	content := strings.TrimSpace(string(data))
	m := versionCoreRE.FindStringSubmatch(content)
	if m == nil {
		return nil
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &toolchainDeclaration{
		Tool:     "node",
		Source:   ".nvmrc",
		Declared: content,
		req:      versionReq{granularity: 1, major: major},
	}
}

// packageJSONFile is the subset of package.json this checker cares about.
type packageJSONFile struct {
	Engines map[string]string `json:"engines"`
}

// enginesRangeRE matches the narrow subset of semver-range syntax this
// checker supports for package.json's engines.node: an optional "^", "~",
// or ">=" prefix, then a bare numeric version. Anything else (||, x,
// multiple clauses, whitespace-separated compound ranges) doesn't match and
// is treated as not comparable — a deliberate scope narrowing per Research's
// false-positive risk, not a bug.
var enginesRangeRE = regexp.MustCompile(`^(\^|~|>=)?\s*v?(\d+)(?:\.\d+){0,2}$`)

// parsePackageJSONEngines reads package.json from workDir and extracts
// engines.node, if present and expressible in the narrow supported range
// syntax. Malformed JSON, a missing engines.node key, or an unsupported
// range all return nil (not comparable), never a mismatch.
func parsePackageJSONEngines(workDir string) *toolchainDeclaration {
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg packageJSONFile
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	raw, ok := pkg.Engines["node"]
	if !ok {
		return nil
	}
	raw = strings.TrimSpace(raw)
	m := enginesRangeRE.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	return &toolchainDeclaration{
		Tool:     "node",
		Source:   "package.json (engines.node)",
		Declared: raw,
		req:      versionReq{op: m[1], granularity: 1, major: major},
	}
}

// goToolchainRE matches go.mod's "toolchain goX.Y[.Z]" directive line.
var goToolchainRE = regexp.MustCompile(`(?m)^toolchain\s+go(\d+)\.(\d+)(?:\.\d+)?\s*$`)

// goDirectiveRE matches go.mod's "go X.Y[.Z]" minimum-version directive
// line (distinct from the module's own "module ..." line and from the
// toolchain directive above).
var goDirectiveRE = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)(?:\.\d+)?\s*$`)

// parseGoMod reads go.mod from workDir and extracts a minimum-version
// requirement: the "toolchain" directive if present (it states the exact
// toolchain the module wants), else the "go" directive (the module's
// minimum language version) as a fallback. Both are minimums, not pins —
// GOTOOLCHAIN=auto already self-corrects in the common case (see #1786
// Research), so this only catches the GOTOOLCHAIN=local / auto-download-
// disabled edge case where the resolved go binary is older than declared.
func parseGoMod(workDir string) *toolchainDeclaration {
	data, err := os.ReadFile(filepath.Join(workDir, "go.mod"))
	if err != nil {
		return nil
	}
	content := string(data)
	if m := goToolchainRE.FindStringSubmatch(content); m != nil {
		major, errMaj := strconv.Atoi(m[1])
		minor, errMin := strconv.Atoi(m[2])
		if errMaj != nil || errMin != nil {
			return nil
		}
		return &toolchainDeclaration{
			Tool:     "go",
			Source:   "go.mod (toolchain)",
			Declared: fmt.Sprintf("go%d.%d", major, minor),
			req:      versionReq{granularity: 2, major: major, minor: minor},
		}
	}
	if m := goDirectiveRE.FindStringSubmatch(content); m != nil {
		major, errMaj := strconv.Atoi(m[1])
		minor, errMin := strconv.Atoi(m[2])
		if errMaj != nil || errMin != nil {
			return nil
		}
		return &toolchainDeclaration{
			Tool:     "go",
			Source:   "go.mod (go directive)",
			Declared: fmt.Sprintf("%d.%d", major, minor),
			req:      versionReq{granularity: 2, major: major, minor: minor},
		}
	}
	return nil
}

// toolVersionsToolMap maps the asdf tool names this checker understands to
// the toolchain binary key resolveToolVersion uses. Any other tool line
// (ruby, python, terraform, ...) is silently ignored — mapping every
// possible asdf tool name to a binary is explicit scope creep this issue
// doesn't take on.
var toolVersionsToolMap = map[string]string{
	"nodejs": "node",
	"golang": "go",
}

// parseToolVersions reads .tool-versions (asdf format) from workDir and
// returns one declaration per recognized (nodejs/golang) line. Unrecognized
// tool lines and malformed version tokens are silently skipped.
func parseToolVersions(workDir string) []*toolchainDeclaration {
	data, err := os.ReadFile(filepath.Join(workDir, ".tool-versions"))
	if err != nil {
		return nil
	}
	var decls []*toolchainDeclaration
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tool, ok := toolVersionsToolMap[strings.ToLower(fields[0])]
		if !ok {
			continue
		}
		versionStr := fields[1]
		m := versionCoreRE.FindStringSubmatch(versionStr)
		if m == nil {
			continue
		}
		major, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		decls = append(decls, &toolchainDeclaration{
			Tool:     tool,
			Source:   fmt.Sprintf(".tool-versions (%s)", fields[0]),
			Declared: versionStr,
			req:      versionReq{granularity: 1, major: major},
		})
	}
	return decls
}

// parseToolchainDeclarations reads every declaration format R1 names from
// workDir. A format whose file is absent, or whose content isn't
// comparable, contributes nothing — this never errors.
func parseToolchainDeclarations(workDir string) []*toolchainDeclaration {
	var decls []*toolchainDeclaration
	if d := parseNvmrc(workDir); d != nil {
		decls = append(decls, d)
	}
	if d := parsePackageJSONEngines(workDir); d != nil {
		decls = append(decls, d)
	}
	if d := parseGoMod(workDir); d != nil {
		decls = append(decls, d)
	}
	decls = append(decls, parseToolVersions(workDir)...)
	return decls
}

// resolvedVersion is a parsed toolchain binary version, or a resolution
// failure (binary missing from PATH, subprocess error, or unparseable
// output) — the latter is always "cannot compare", never a mismatch.
type resolvedVersion struct {
	raw          string
	major, minor int
	err          error
}

var (
	toolchainVersionCacheMu sync.Mutex
	toolchainVersionCache   = map[string]resolvedVersion{}
)

var nodeVersionOutputRE = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)
var goVersionOutputRE = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// toolchainVersionCommand names the executable and args used to resolve a
// tool's version, and the regexp used to parse its output. A package-level
// var (rather than hardcoding inside resolveToolVersionUncached) so tests
// can point "node"/"go" at a shim script on a temp PATH without needing to
// fake the daemon's real inherited PATH.
var toolchainVersionCommand = map[string]struct {
	args []string
	re   *regexp.Regexp
}{
	"node": {args: []string{"node", "--version"}, re: nodeVersionOutputRE},
	"go":   {args: []string{"go", "version"}, re: goVersionOutputRE},
}

// resolveToolVersion resolves tool's version via the daemon's own inherited
// PATH, memoized for the process's lifetime: the entire premise of #1786 is
// that this PATH cannot change without a daemon restart, so a successful
// resolution is invariant for the process's life and a repeated
// per-invocation shell-out would be pure waste (R4's "no added latency
// worth noticing"). Only successful resolutions are cached — a resolution
// failure (subprocess timeout, transient exec error, or a momentarily
// unavailable binary) is deliberately NOT cached, since unlike PATH itself,
// nothing guarantees a failure is invariant; caching it would let a single
// bad moment (e.g. a timeout under load) permanently and silently disable
// drift detection for that tool for the rest of the daemon's life, with no
// way to observe or recover from it (Pruefer, #1786 PR review). The retry
// cost on a persistent failure is bounded the same way the happy path's
// cost is: only paid when a worktree actually declares that tool.
func resolveToolVersion(ctx context.Context, tool string) resolvedVersion {
	toolchainVersionCacheMu.Lock()
	if v, ok := toolchainVersionCache[tool]; ok {
		toolchainVersionCacheMu.Unlock()
		return v
	}
	toolchainVersionCacheMu.Unlock()

	v := resolveToolVersionUncached(ctx, tool)

	if v.err == nil {
		toolchainVersionCacheMu.Lock()
		toolchainVersionCache[tool] = v
		toolchainVersionCacheMu.Unlock()
	}
	return v
}

// resetToolchainVersionCacheForTest clears the process-lifetime resolution
// cache. Test-only — production code never needs to invalidate it, since
// the daemon's PATH is invariant for its own lifetime by construction.
func resetToolchainVersionCacheForTest() {
	toolchainVersionCacheMu.Lock()
	defer toolchainVersionCacheMu.Unlock()
	toolchainVersionCache = map[string]resolvedVersion{}
}

func resolveToolVersionUncached(ctx context.Context, tool string) resolvedVersion {
	cmd, ok := toolchainVersionCommand[tool]
	if !ok {
		return resolvedVersion{err: fmt.Errorf("unknown toolchain %q", tool)}
	}
	if _, err := exec.LookPath(cmd.args[0]); err != nil {
		// Absent from PATH entirely is a distinct, pre-existing condition
		// (see the issue's Risks) — never a mismatch, never a failure.
		return resolvedVersion{err: err}
	}
	cctx, cancel := context.WithTimeout(ctx, toolchainSubprocessTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, cmd.args[0], cmd.args[1:]...).Output()
	if err != nil {
		return resolvedVersion{err: err}
	}
	m := cmd.re.FindStringSubmatch(string(out))
	if m == nil {
		return resolvedVersion{err: fmt.Errorf("could not parse %s output: %q", cmd.args[0], strings.TrimSpace(string(out)))}
	}
	major, errMaj := strconv.Atoi(m[1])
	minor, errMin := strconv.Atoi(m[2])
	if errMaj != nil || errMin != nil {
		return resolvedVersion{err: fmt.Errorf("could not parse %s version numbers from %q", cmd.args[0], strings.TrimSpace(string(out)))}
	}
	return resolvedVersion{raw: strings.TrimSpace(string(out)), major: major, minor: minor}
}

// detectToolchainDrift is the pure detection core: for every comparable
// declaration found in workDir, resolve the corresponding binary via the
// daemon's inherited PATH and report a mismatch when the resolved version
// does not satisfy the declaration. A declaration that can't be resolved
// (binary missing from PATH) or isn't comparable (unparseable/ambiguous
// syntax) contributes nothing — fail closed, never a false-positive
// warning.
func detectToolchainDrift(ctx context.Context, workDir string) []toolchainMismatch {
	var mismatches []toolchainMismatch
	for _, decl := range parseToolchainDeclarations(workDir) {
		resolved := resolveToolVersion(ctx, decl.Tool)
		if resolved.err != nil {
			continue
		}
		if decl.req.satisfies(resolved) {
			continue
		}
		mismatches = append(mismatches, toolchainMismatch{
			Source:   decl.Source,
			Declared: decl.Declared,
			Resolved: resolved.raw,
		})
	}
	return mismatches
}

// checkToolchainDrift is the shared, best-effort entry point called from
// both invocation paths (stage dispatch's runInvocationWithExtension and
// comment review's processComments), immediately before Claude is invoked.
// It detects, at worker-invocation time, whether this worktree declares a
// toolchain version the daemon's own inherited PATH does not satisfy, and —
// unlike claudeUsageLimitError/apiKeyHelperDetectedError — never skips the
// invocation. There is nothing for Fabrik to do about a detected mismatch
// except tell someone (R2/R3): re-resolving PATH or driving a version
// manager is explicitly out of scope. See issue #1786 and ADR-1786.
//
// Episode-scoped per issue, gated on toolchainStaleLabel's own absence
// (R5) — mirroring fabrik:claude-limit/fabrik:api-key-helper-detected. Cost
// (Research's "Cost of detection" risk) is bounded separately, by
// resolveToolVersion's process-lifetime cache: this function's own per-call
// cost is a few small file reads plus a cache hit in the common case.
func (e *Engine) checkToolchainDrift(ctx context.Context, item gh.ProjectItem, workDir string) {
	mismatches := detectToolchainDrift(ctx, workDir)

	if len(mismatches) == 0 {
		if hasLabel(item.Labels, toolchainStaleLabel) {
			e.removeLabel(item, toolchainStaleLabel)
		}
		return
	}

	if hasLabel(item.Labels, toolchainStaleLabel) {
		// Episode already signaled — don't repeat on every invocation (R5).
		return
	}

	lines := make([]string, 0, len(mismatches))
	for _, m := range mismatches {
		lines = append(lines, fmt.Sprintf(
			"- **%s** declares `%s`, but the resolved binary on the daemon's inherited `PATH` reports `%s`",
			m.Source, m.Declared, m.Resolved,
		))
	}
	e.logf(item.Number, "warn", "toolchain drift detected: %d mismatch(es)\n", len(mismatches))

	comment := fmt.Sprintf(
		"🏭 **Fabrik — toolchain drift detected**\n\nThis worktree declares a toolchain version that the worker's resolved binary — found via the daemon's own inherited `PATH` — does not satisfy:\n\n%s\n\nThis is a signal, not a fix: Fabrik does not re-resolve `PATH` or drive a version manager (`nvm use`, `asdf install`, etc.) on your behalf. A long-lived daemon inherits `PATH` once, at the moment its hosting shell started — restarting `fabrik` alone re-inherits the same stale `PATH` from the same parent shell; only restarting the hosting shell (or the daemon's own environment) picks up a newer toolchain. This warning is posted once per issue while the mismatch persists and clears automatically once a later invocation observes a satisfied declaration.",
		strings.Join(lines, "\n"),
	)
	e.postItemComment(item, comment, false)
	e.addLabel(item, toolchainStaleLabel)
}
