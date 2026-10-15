package pruefer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ReviewEnvPassthroughVar names the operator opt-in that forwards additional,
// explicitly named variables from the daemon's environment into the review
// process (a comma-separated list of exact names, e.g.
// "CLAUDE_CODE_OAUTH_TOKEN"). It is read from the daemon environment only —
// there is no YAML key or flag — and is itself never forwarded.
const ReviewEnvPassthroughVar = "PRUEFER_REVIEW_ENV_PASSTHROUGH"

// reviewEnvAllowExact is the exact-name allowlist for the review process
// environment (#2192, R4). The review process is handed only what `claude`
// needs to start and authenticate — the Claude profile selection, PATH, HOME,
// locale, a few host basics and TLS/proxy settings — never the daemon's whole
// environment. It runs a model over unreviewed, attacker-controllable PR
// content, and Pruefer's Go code (not the session) submits the review, so the
// session needs no GitHub credential, no .env value and no PRUEFER_* /
// HOOKDECK_* setting. The default is dropped: a variable not listed here (or
// opted in via ReviewEnvPassthroughVar) never reaches the process.
//
// ANTHROPIC_* and CLAUDE_CODE_OAUTH_TOKEN are deliberately absent: Pruefer's
// documented deployment is subscription-backed (credentials live in the
// CLAUDE_CONFIG_DIR profile; adrs/1113). A host that authenticates through a
// variable names it with ReviewEnvPassthroughVar.
var reviewEnvAllowExact = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
	"SHELL": true, "TERM": true, "TMPDIR": true, "TEMP": true, "TMP": true,
	"LANG": true, "LANGUAGE": true,
	"CLAUDE_CONFIG_DIR": true,
	"XDG_CONFIG_HOME":   true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	// Windows host basics (the package also builds there); inert elsewhere.
	"SystemRoot": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
}

// reviewEnvAllowPrefixes are allowlisted by prefix: the locale family.
var reviewEnvAllowPrefixes = []string{"LC_"}

// reviewEnvNeverPassthrough are name prefixes ReviewEnvPassthroughVar may not
// name: the credentials and daemon configuration the scrub exists to withhold.
var reviewEnvNeverPassthrough = []string{"GH_", "GITHUB_", "FABRIK_", "PRUEFER_", "HOOKDECK_"}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func reviewEnvAllowed(name string) bool {
	if reviewEnvAllowExact[name] {
		return true
	}
	for _, p := range reviewEnvAllowPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseReviewEnvPassthrough parses the opt-in list into accepted exact names
// and refused entries (malformed names, or names under a never-passthrough
// prefix — notably GH_TOKEN, GITHUB_TOKEN and FABRIK_TOKEN).
func parseReviewEnvPassthrough(raw string) (names, refused []string) {
	for _, n := range splitCSV(raw) {
		bad := !envNameRE.MatchString(n)
		for _, p := range reviewEnvNeverPassthrough {
			if strings.HasPrefix(strings.ToUpper(n), p) {
				bad = true
			}
		}
		if bad {
			refused = append(refused, n)
			continue
		}
		names = append(names, n)
	}
	return names, refused
}

var passthroughNoticeOnce sync.Once

// reviewProcessEnv builds the review process environment from environ
// (normally os.Environ()): only allowlisted names (plus any accepted
// ReviewEnvPassthroughVar names) survive, then buildReviewEnv's overrides are
// applied. Order follows environ; a name appearing twice keeps its last value.
func reviewProcessEnv(environ []string, req ReviewRequest) []string {
	var passthrough map[string]bool
	for _, kv := range environ {
		if strings.HasPrefix(kv, ReviewEnvPassthroughVar+"=") {
			names, refused := parseReviewEnvPassthrough(strings.TrimPrefix(kv, ReviewEnvPassthroughVar+"="))
			passthrough = make(map[string]bool, len(names))
			for _, n := range names {
				passthrough[n] = true
			}
			passthroughNoticeOnce.Do(func() {
				sorted := append([]string(nil), names...)
				sort.Strings(sorted)
				if len(sorted) > 0 {
					logf(0, "startup", "%s forwards %s into the review process (names only; values not logged)\n", ReviewEnvPassthroughVar, strings.Join(sorted, ", "))
				}
				if len(refused) > 0 {
					logf(0, "startup", "%s: refusing %s (not a plain variable name, or a credential/daemon-config prefix that is never forwarded)\n", ReviewEnvPassthroughVar, strings.Join(refused, ", "))
				}
			})
		}
	}
	var base []string
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if reviewEnvAllowed(name) || passthrough[name] {
			base = append(base, kv)
		}
	}
	return mergeEnv(base, buildReviewEnv(req))
}

// checkReviewEnvScrubbed reports an error when env contains a variable the
// review process must not hold: any GitHub/Fabrik/Pruefer/Hookdeck credential
// or configuration name, or any name that is neither allowlisted nor in
// extraAllowed. It is the structural assertion the env tests apply to the
// built environment — and, for non-vacuity, to a deliberately unscrubbed one,
// where it must report a violation.
func checkReviewEnvScrubbed(env []string, extraAllowed map[string]bool) error {
	var bad []string
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if name == "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING" || name == "CLAUDE_CODE_EFFORT_LEVEL" {
			continue
		}
		if reviewEnvAllowed(name) || extraAllowed[name] {
			continue
		}
		bad = append(bad, name)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("review environment holds non-allowlisted variables: %s", strings.Join(bad, ", "))
	}
	return nil
}
