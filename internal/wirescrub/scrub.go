// Package wirescrub detects and redacts secret-shaped substrings —
// GitHub tokens, generic bearer tokens, and email addresses — in recorded
// GraphQL/REST fixtures before they are committed. See #1453 R3: docs/
// publishes live to fabrik.handarbeit.io from this repo, so a fixture that
// slips a real token or private email into git history is a public leak,
// not just an internal one.
package wirescrub

import (
	"regexp"
	"strings"
)

// rule is one secret-shaped pattern this package knows how to detect.
type rule struct {
	// name identifies the pattern in Findings output and Redact placeholders.
	name string
	re   *regexp.Regexp
}

// rules is deliberately conservative in scope, matching what R3 calls out
// by name: GitHub's classic token prefixes (ghp_ personal access, gho_
// OAuth, ghu_ user-to-server, ghs_ server-to-server, ghr_ refresh),
// GitHub's fine-grained github_pat_ prefix, generic "Bearer <token>"
// Authorization headers (catches non-GitHub bearer tokens that might
// appear in a response body or header dump), and email addresses.
var rules = []rule{
	{
		name: "github classic token (ghp_/gho_/ghu_/ghs_/ghr_)",
		re:   regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	},
	{
		name: "github fine-grained PAT (github_pat_)",
		re:   regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	},
	{
		name: "bearer token",
		re:   regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-_.=]{20,}`),
	},
	{
		name: "email address",
		re:   regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
	},
}

// Findings scans data for secret-shaped substrings and returns one
// human-readable description per match, naming the pattern that matched
// and a masked preview — never the full matched text, so calling Findings
// (e.g. from a failing test, whose output may end up in a CI log) cannot
// itself become a secondary leak vector. A nil/empty result means nothing
// secret-shaped was found.
func Findings(data []byte) []string {
	var out []string
	for _, r := range rules {
		for _, m := range r.re.FindAll(data, -1) {
			out = append(out, r.name+": "+mask(string(m)))
		}
	}
	return out
}

// Redact returns a copy of data with every secret-shaped substring
// replaced by a fixed placeholder naming the pattern that matched, so a
// scrubbed fixture still records *that* something sensitive was removed
// and *what kind*, without retaining any part of the original value.
func Redact(data []byte) []byte {
	out := data
	for _, r := range rules {
		placeholder := []byte("[SCRUBBED:" + r.name + "]")
		out = r.re.ReplaceAll(out, placeholder)
	}
	return out
}

// mask reduces a matched secret to a preview safe to surface in test
// output or log lines: an email keeps only its domain, anything else
// keeps at most its first 4 characters (a public-safe prefix for every
// pattern above — e.g. "ghp_", "Bear") with the rest starred out.
func mask(s string) string {
	if at := strings.IndexByte(s, '@'); at >= 0 {
		return "***" + s[at:]
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}
