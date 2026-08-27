package pruefer

import (
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	gh "github.com/handarbeit/fabrik/github"
)

// DefaultMaxRepoConfigBytes caps the size of a repo-resident
// .pruefer/config.yaml this issue will parse. Generous for a small
// preferences file (a handful of glob patterns, a byte cap, a severity
// tier) while still well below DefaultMaxDiffBytes (500 KB, governing a
// full unified diff) — 32 KB is orders of magnitude more than any
// legitimate config of this shape would ever need.
const DefaultMaxRepoConfigBytes = 32 * 1024 // 32 KB

// recognizedRepoConfigKeys is the exhaustive set of YAML keys a repo-resident
// config file may set — the "May" list from the issue's classification rule.
// Every other key (whether a real operator-scoped Config field or simply
// unknown) is reported via RepoConfigProvenance.UnknownKeys and otherwise
// ignored — see yamlRepoConfig's doc comment for why this is enforced as a
// distinct, narrower struct rather than a convention layered on yamlConfig.
var recognizedRepoConfigKeys = map[string]bool{
	"excluded_paths":            true,
	"excluded_labels":           true,
	"excluded_authors":          true,
	"max_diff_bytes":            true,
	"request_changes_threshold": true,
}

// yamlRepoConfig is the schema a repo-resident .pruefer/config.yaml is
// parsed against — a strict, standalone 5-field subset of yamlConfig's
// fields, not a reuse of the full operator struct. Reusing yamlConfig would
// make every operator-only field (model, github_app_id, watched_repos, ...)
// structurally *parseable* from repo YAML even if downstream code chooses to
// ignore it — a weaker property than the issue's explicit classification
// rule, which wants the schema itself to not recognize an operator-scoped
// key. With this struct, an operator-scoped key simply has nowhere to land;
// it is caught only as a diagnostic (see fetchRepoConfig's second,
// map-shaped parse), never as a value that reaches Config.
//
// MaxDiffBytes and RequestChangesThreshold are pointers so "the repo didn't
// set this key" (nil) is distinguishable from "the repo explicitly set the
// zero value" (e.g. request_changes_threshold: "") — the latter is itself a
// widening attempt (turning an operator-configured threshold back off) that
// applyRepoNarrowing must be able to see and reject; a plain string/int64
// field couldn't represent that distinction at all.
type yamlRepoConfig struct {
	ExcludedPaths           []string `yaml:"excluded_paths"`
	ExcludedLabels          []string `yaml:"excluded_labels"`
	ExcludedAuthors         []string `yaml:"excluded_authors"`
	MaxDiffBytes            *int64   `yaml:"max_diff_bytes"`
	RequestChangesThreshold *string  `yaml:"request_changes_threshold"`
}

// RepoConfigProvenance records what happened when resolving a repo-resident
// config, for R5's observability requirement: which repo config was applied
// (or wasn't), and at which base ref/SHA.
type RepoConfigProvenance struct {
	Ref string // the base ref/SHA the lookup was performed at

	// Found is true iff a .pruefer/config.yaml exists at Ref — regardless of
	// whether it turned out to be usable. False is the overwhelmingly common
	// case (R3) and produces no warning at all: an absent file is not a
	// compliance-drift signal, it's the default state.
	Found bool

	// Invalid is true iff a file was Found but rejected (oversized,
	// non-UTF-8, or unparseable YAML) — R4's fall-back-to-operator-config
	// path. Reason explains why.
	Invalid bool
	Reason  string

	// UnknownKeys lists any top-level YAML key present in the file that
	// recognizedRepoConfigKeys does not recognize — whether a real
	// operator-scoped Config field (e.g. "model", "watched_repos") or simply
	// unknown. Sorted for deterministic log/test output. Never populated
	// when Invalid is true (the file was never successfully parsed at all).
	UnknownKeys []string
}

// fetchRepoConfig resolves owner/repo's repo-resident .pruefer/config.yaml
// at ref (the PR's base ref — R1's security invariant, never the head).
// It never returns an error: every failure mode (fetch error other than
// "not found", oversized file, invalid UTF-8, malformed YAML) degrades to a
// zero-value yamlConfig with Invalid set on the returned provenance (R4) —
// the caller falls back to the operator's own config and the review
// proceeds. Unrecognized/operator-scoped keys are detected via a second,
// map-shaped parse and reported on the provenance rather than treated as an
// error (AC4) — recognizedRepoConfigKeys governs which fields yamlRepoConfig
// itself has room for, so an operator-scoped value can never reach the
// returned struct regardless of this diagnostic.
func fetchRepoConfig(client GitHubReviewer, owner, repo, ref string) (yamlRepoConfig, RepoConfigProvenance) {
	prov := RepoConfigProvenance{Ref: ref}

	data, err := client.FetchFileAtRef(owner, repo, DefaultConfigPath, ref)
	if err != nil {
		if errors.Is(err, gh.ErrNotFound) {
			return yamlRepoConfig{}, prov // R3: absent file, not an error, no warning.
		}
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("fetching %s: %v", DefaultConfigPath, err)
		return yamlRepoConfig{}, prov
	}
	prov.Found = true

	if len(data) > DefaultMaxRepoConfigBytes {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("%s is %d bytes, exceeds the %d byte cap", DefaultConfigPath, len(data), DefaultMaxRepoConfigBytes)
		return yamlRepoConfig{}, prov
	}
	if !utf8.Valid(data) {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("%s is not valid UTF-8", DefaultConfigPath)
		return yamlRepoConfig{}, prov
	}

	var rc yamlRepoConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		prov.Invalid = true
		prov.Reason = fmt.Sprintf("parsing %s: %v", DefaultConfigPath, err)
		return yamlRepoConfig{}, prov
	}

	// Best-effort unknown-key detection: a second parse into a generic map
	// so any key recognizedRepoConfigKeys doesn't know about can be named in
	// the log (AC4) without making yamlRepoConfig itself any less strict. If
	// this second unmarshal somehow fails despite the first succeeding, skip
	// the diagnostic rather than treating it as a new invalidity mode — the
	// real (recognized-field) values above are already usable.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err == nil {
		for k := range raw {
			if !recognizedRepoConfigKeys[k] {
				prov.UnknownKeys = append(prov.UnknownKeys, k)
			}
		}
		sort.Strings(prov.UnknownKeys)
	}

	return rc, prov
}

// narrowingWarning records one field the repo config attempted to set that
// applyRepoNarrowing rejected as a widening attempt (AC3), or an invalid
// value it ignored — for R5 logging.
type narrowingWarning struct {
	Field  string
	Detail string
}

// narrowingRank ranks a RequestChangesThreshold value for the purpose of
// comparing a repo's requested value against the operator's, in the
// *opposite* direction from severityRank: here, "" (off) is the most
// lenient possible state and ranks *above* every real tier (5, versus
// critical's 4), because turning the gate off is the most permissive thing
// a threshold can be. severityRank, by contrast, ranks "" at 0 (least
// severe) for its own unrelated purpose — a fail-closed per-finding
// comparison in decideEvent, where an untrusted/missing per-finding
// severity must never be trusted to escalate toward REQUEST_CHANGES.
// Conflating the two would silently re-invert the exact direction the
// issue's "Correction to the severity-threshold direction" section fixed:
// a repo could raise its own rank (or unset an operator's threshold back to
// off) and call it "narrowing". Do not unify these two functions.
func narrowingRank(s Severity) int {
	if s == "" {
		return 5
	}
	return severityRank(s)
}

// displaySeverity renders s for a log/warning message, spelling out "off"
// for the empty value rather than an empty string.
func displaySeverity(s Severity) string {
	if s == "" {
		return "off"
	}
	return string(s)
}

// unionDedupe returns operatorList with any entries of repoList not already
// present appended, in repoList's order, deduplicated. Returns operatorList
// unmodified (same slice, no allocation) when repoList is empty — preserving
// byte-identical behavior when a repo has no narrowing to contribute (R3).
func unionDedupe(operatorList, repoList []string) []string {
	if len(repoList) == 0 {
		return operatorList
	}
	seen := make(map[string]bool, len(operatorList)+len(repoList))
	out := make([]string, 0, len(operatorList)+len(repoList))
	for _, v := range operatorList {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range repoList {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// applyRepoNarrowing merges repo's narrowable preferences onto operator,
// implementing the issue's "What a repo may and may not set" rules
// verbatim: excluded_paths/excluded_labels/excluded_authors are unioned
// (never removing an operator entry); max_diff_bytes may only be lowered,
// never raised or uncapped; request_changes_threshold may only move to an
// equal-or-stricter tier (see narrowingRank), never to a more lenient one or
// back to off. Every rejected widening attempt is returned as a
// narrowingWarning (AC3) instead of applied — operator's value is kept
// untouched on rejection. repo fields not set by the repo config (nil/empty)
// leave the corresponding operator value completely unchanged, so an absent
// repo config (the zero-value yamlRepoConfig{}) is guaranteed to return
// operator unchanged and no warnings (R3/AC5).
func applyRepoNarrowing(operator Config, repo yamlRepoConfig) (Config, []narrowingWarning) {
	effective := operator
	var warnings []narrowingWarning

	effective.ExcludedPaths = unionDedupe(operator.ExcludedPaths, repo.ExcludedPaths)
	effective.ExcludedLabels = unionDedupe(operator.ExcludedLabels, repo.ExcludedLabels)
	effective.ExcludedAuthors = unionDedupe(operator.ExcludedAuthors, repo.ExcludedAuthors)

	if repo.MaxDiffBytes != nil {
		repoVal := *repo.MaxDiffBytes
		switch {
		case operator.MaxDiffBytes <= 0 && repoVal > 0:
			// Operator uncapped; repo requests a real cap — narrowing.
			effective.MaxDiffBytes = repoVal
		case operator.MaxDiffBytes <= 0 && repoVal <= 0:
			// Both uncapped: no-op, not a widen — no warning.
		case repoVal > 0 && repoVal <= operator.MaxDiffBytes:
			// Repo requests an equal-or-lower cap — narrowing (equal is a
			// no-op, not a widen, so it's allowed).
			effective.MaxDiffBytes = repoVal
		default:
			// repoVal <= 0 (uncapped) while the operator has a real cap, or
			// repoVal exceeds the operator's cap: a widening attempt.
			warnings = append(warnings, narrowingWarning{
				Field:  "max_diff_bytes",
				Detail: fmt.Sprintf("repo requested %d, which would widen beyond operator's %d — rejected", repoVal, operator.MaxDiffBytes),
			})
		}
	}

	if repo.RequestChangesThreshold != nil {
		repoThreshold := Severity(*repo.RequestChangesThreshold)
		switch {
		case repoThreshold != "" && !validSeverity(repoThreshold):
			warnings = append(warnings, narrowingWarning{
				Field:  "request_changes_threshold",
				Detail: fmt.Sprintf("repo value %q is not a recognized severity tier — ignored", repoThreshold),
			})
		case narrowingRank(repoThreshold) > narrowingRank(operator.RequestChangesThreshold):
			warnings = append(warnings, narrowingWarning{
				Field:  "request_changes_threshold",
				Detail: fmt.Sprintf("repo requested %q, which would widen beyond operator's %q — rejected", displaySeverity(repoThreshold), displaySeverity(operator.RequestChangesThreshold)),
			})
		default:
			effective.RequestChangesThreshold = repoThreshold
		}
	}

	return effective, warnings
}

// logRepoConfigResolution implements R5: logs which repo config was applied
// (or why it wasn't) and at which base ref — the provenance is the
// security-relevant fact, since R1's whole guarantee rests on resolving at
// the base ref rather than the head. Follows logSummaryParseInfo's
// established convention: the resolution/merge functions above stay pure,
// this is the one call site (with pr.Number in scope) that logs.
func logRepoConfigResolution(prNumber int, owner, repo string, prov RepoConfigProvenance, warnings []narrowingWarning) {
	switch {
	case !prov.Found:
		// R3: the overwhelmingly common case — no compliance-drift signal,
		// nothing to log.
	case prov.Invalid:
		logf(prNumber, "warn", "%s/%s repo config at %s: %s — falling back to operator config\n", owner, repo, prov.Ref, prov.Reason)
	default:
		logf(prNumber, "config", "%s/%s: applied repo config %s at %s\n", owner, repo, DefaultConfigPath, prov.Ref)
	}
	for _, k := range prov.UnknownKeys {
		logf(prNumber, "warn", "%s/%s repo config at %s: key %q is not repo-settable — ignored\n", owner, repo, prov.Ref, k)
	}
	for _, w := range warnings {
		logf(prNumber, "warn", "%s/%s repo config at %s: %s: %s\n", owner, repo, prov.Ref, w.Field, w.Detail)
	}
}
