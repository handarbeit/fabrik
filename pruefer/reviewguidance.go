package pruefer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	gh "github.com/handarbeit/fabrik/github"
)

// GuidanceModeAppend and GuidanceModeReplace are the two recognized values
// for #1446's guidance-layer composition mode (R2/R3): append (the default)
// concatenates a layer's overlay text after the layer below it; replace
// substitutes the layer below entirely. Never affects the Go-owned contract
// half (C5) or Go-supplied dynamic context (C7) in either direction — see
// resolveGuidance/composeGuidanceLayer in claude.go.
const (
	GuidanceModeAppend  = "append"
	GuidanceModeReplace = "replace"
)

// DefaultMaxReviewSkillBytes caps the size of a repo-resident review-
// guidance skill this issue will parse — double #1642's
// DefaultMaxRepoConfigBytes (32 KB): prose guidance is reasonably expected
// to run longer than a handful of glob patterns and a severity tier, while
// staying well under DefaultMaxDiffBytes (500 KB, governing a full unified
// diff).
const DefaultMaxReviewSkillBytes = 64 * 1024 // 64 KB

// frontmatterDelim is the YAML frontmatter fence line, mirroring Fabrik's
// own SKILL.md convention (---\n<yaml>\n---\n<body>).
const frontmatterDelim = "---"

// skillFrontmatter is the shape of a review-guidance skill's optional YAML
// frontmatter block — currently just the composition mode.
type skillFrontmatter struct {
	Mode string `yaml:"mode"`
}

// RepoSkillProvenance records what happened when resolving a repo-resident
// review-guidance skill, for R5's observability requirement — mirroring
// RepoConfigProvenance's (#1642) shape.
type RepoSkillProvenance struct {
	Ref string // the base ref/SHA the lookup was performed at

	// Found is true iff a skill file exists at Ref — regardless of whether
	// it turned out to be usable. False is the overwhelmingly common case
	// (most repos have no skill) and produces no warning at all.
	Found bool
	Bytes int // the file's size in bytes, set whenever Found is true

	// Invalid is true iff a file was Found but rejected wholesale
	// (oversized, non-UTF-8, or malformed frontmatter) — R4's
	// degrade-to-absent path. Reason explains why.
	Invalid bool
	Reason  string

	// ModeInvalid is true iff the frontmatter was otherwise valid but its
	// mode value was not recognized — a narrower, correctable mistake that
	// degrades only the mode (to GuidanceModeAppend), not the guidance text
	// itself. Distinct from Invalid, which degrades the whole file. Reason
	// explains the fallback.
	ModeInvalid bool

	// Mode is the normalized mode actually applied (GuidanceModeAppend or
	// GuidanceModeReplace) when Found and not Invalid; the zero value
	// otherwise.
	Mode string
}

// parseSkillFrontmatter splits data into an optional YAML frontmatter mode
// declaration and the guidance body that follows it. Three outcomes:
//
//   - No leading "---" line at all: the entire file is the body, mode is
//     empty (ok=true) — a repo skill needn't declare any frontmatter.
//   - A leading "---" line, a closing "---" line, and YAML between them
//     that parses successfully: mode is whatever the block declared (may be
//     empty), body is everything after the closing delimiter (ok=true).
//   - A leading "---" line with no closing delimiter, or YAML between the
//     delimiters that fails to parse: ok=false. Per Plan's fail-safe
//     direction, this degrades the *entire* file to absent (see
//     fetchRepoGuidance) rather than risk silently treating unparseable
//     content as a mode declaration — the caller must not, for example,
//     fall back to treating the whole file as an append-mode body, since
//     "garbage between --- fences" is a stronger signal of author error
//     than "no fences at all".
func parseSkillFrontmatter(data []byte) (mode, body string, ok bool) {
	text := string(data)
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\n") != frontmatterDelim {
		return "", text, true
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\n") != frontmatterDelim {
			continue
		}
		var fm skillFrontmatter
		block := strings.Join(lines[1:i], "")
		if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
			return "", "", false
		}
		return fm.Mode, strings.Join(lines[i+1:], ""), true
	}
	// Opened but never closed.
	return "", "", false
}

// normalizeGuidanceMode reduces a raw frontmatter (or config) mode value to
// one of the two recognized constants. An empty or already-recognized value
// passes through unchanged with ok=true; anything else defaults to
// GuidanceModeAppend (the safe, non-destructive direction — a mode typo
// should not cost a repo its guidance text) with ok=false so the caller can
// warn.
func normalizeGuidanceMode(mode string) (normalized string, ok bool) {
	switch mode {
	case "", GuidanceModeAppend:
		return GuidanceModeAppend, true
	case GuidanceModeReplace:
		return GuidanceModeReplace, true
	default:
		return GuidanceModeAppend, false
	}
}

// fetchRepoGuidance resolves owner/repo's repo-resident review-guidance
// skill (DefaultReviewSkillPath) at ref — the PR's base ref, C1's security
// invariant, never the head. It never returns an error: every failure mode
// (fetch error other than "not found", oversized file, invalid UTF-8,
// malformed frontmatter) degrades to empty guidance with Invalid set on the
// returned provenance (R4) — the caller proceeds with whatever other
// guidance layers resolved, and the review is never blocked on this repo's
// skill file. An unrecognized-but-present mode degrades only the mode (to
// GuidanceModeAppend), not the guidance text — see ModeInvalid. Mirrors
// fetchRepoConfig's (#1642) shape.
func fetchRepoGuidance(client GitHubReviewer, owner, repo, ref string) (text, mode string, prov RepoSkillProvenance) {
	prov = RepoSkillProvenance{Ref: ref}

	data, err := client.FetchFileAtRef(owner, repo, DefaultReviewSkillPath, ref)
	if err != nil {
		if errors.Is(err, gh.ErrNotFound) {
			return "", "", prov // R3-equivalent: absent file, not an error, no warning.
		}
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("fetching %s: %v", DefaultReviewSkillPath, err)
		return "", "", prov
	}
	prov.Found = true
	prov.Bytes = len(data)

	if len(data) > DefaultMaxReviewSkillBytes {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("%s is %d bytes, exceeds the %d byte cap", DefaultReviewSkillPath, len(data), DefaultMaxReviewSkillBytes)
		return "", "", prov
	}
	if !utf8.Valid(data) {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("%s is not valid UTF-8", DefaultReviewSkillPath)
		return "", "", prov
	}

	rawMode, body, ok := parseSkillFrontmatter(data)
	if !ok {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("%s has malformed frontmatter — degrading to no guidance", DefaultReviewSkillPath)
		return "", "", prov
	}

	normalized, modeOK := normalizeGuidanceMode(rawMode)
	prov.Mode = normalized
	if !modeOK {
		prov.ModeInvalid = true
		prov.Reason = fmt.Sprintf("%s frontmatter mode %q is not recognized — defaulting to %q", DefaultReviewSkillPath, rawMode, GuidanceModeAppend)
	}

	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		// A frontmatter-only (or otherwise empty-after-frontmatter) file has
		// no guidance text to contribute. Not itself invalid — just nothing
		// to compose — so composeGuidanceLayer's own empty-overlay no-op
		// handles this identically to "no skill file at all".
		return "", normalized, prov
	}

	return trimmedBody, normalized, prov
}

// logReviewGuidanceResolution implements R5: logs which repo skill (if any)
// was applied, at which base ref/SHA and size — the security-relevant
// provenance, since C1's whole guarantee rests on resolving at the base ref
// rather than the head — mirroring logRepoConfigResolution's (#1642)
// established convention.
func logReviewGuidanceResolution(prNumber int, owner, repo string, prov RepoSkillProvenance) {
	switch {
	case !prov.Found:
		// The overwhelmingly common case — no compliance-drift signal,
		// nothing to log.
	case prov.Invalid:
		logf(prNumber, "warn", "%s/%s review skill at %s: %s\n", owner, repo, prov.Ref, prov.Reason)
	default:
		logf(prNumber, "config", "%s/%s: applied review skill %s at %s (%d bytes, mode=%s)\n", owner, repo, DefaultReviewSkillPath, prov.Ref, prov.Bytes, prov.Mode)
		if prov.ModeInvalid {
			logf(prNumber, "warn", "%s/%s review skill at %s: %s\n", owner, repo, prov.Ref, prov.Reason)
		}
	}
}
