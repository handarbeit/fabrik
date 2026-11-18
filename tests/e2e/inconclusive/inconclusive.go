// Package inconclusive is the marker contract of the third live-test outcome,
// INCONCLUSIVE (#1973, ADR-1973): "the precondition this scenario needs never
// arose". It is neither a PASS nor a FAIL nor an ordinary SKIP, and it is never
// coverage.
//
// The package is deliberately untagged: the live tests (tests/e2e, build tag
// e2e) END a test with it, and the gate runner (tests/gate, no tag) CLASSIFIES
// it from the `go test -json` stream. The runner cannot import the tagged
// package, so the string both sides must agree on lives here —
// tests/e2e/registry is the precedent for an untagged package that sits beside
// the tagged one.
package inconclusive

import (
	"fmt"
	"strings"
)

// Marker is the prefix of the skip message that declares a test inconclusive.
const Marker = "E2E-INCONCLUSIVE:"

// Message is the skip message for an inconclusive test: the marker, a space,
// then the reason.
func Message(format string, args ...any) string {
	return Marker + " " + fmt.Sprintf(format, args...)
}

// IsMarked reports whether a test's skip message declares it inconclusive. The
// marker must START the message: a log line that merely mentions it (a failure
// text quoting it, an ordinary skip citing it) is not a declaration.
func IsMarked(skipMsg string) bool {
	return strings.HasPrefix(strings.TrimSpace(skipMsg), Marker)
}

// Reason strips the marker from an inconclusive skip message.
func Reason(skipMsg string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(skipMsg), Marker))
}

// transientWriteMarkers are the gh/GitHub error texts that mean a harness seed
// write failed on GitHub's side, not because the test asked for something
// wrong. They were seen in release gates: a GraphQL "Something went wrong"
// after `gh pr create` had already created the PR (0.0.93, beta#767), and the
// usual 5xx / connection-level failures.
var transientWriteMarkers = []string{
	"something went wrong",
	"http 500", "http 502", "http 503", "http 504",
	"502 bad gateway", "503 service unavailable", "504 gateway time",
	"internal server error",
	"connection reset", "connection refused", "i/o timeout", "tls handshake timeout",
	"unexpected eof", "timeout awaiting response headers", "context deadline exceeded",
}

// IsTransientGitHubWrite reports whether a failed seed write's gh output names
// a GitHub-side or transport failure. Anything else (a missing label, a 422
// validation error, a 404) is a test bug and must stay a failure.
func IsTransientGitHubWrite(output string) bool {
	o := strings.ToLower(output)
	for _, m := range transientWriteMarkers {
		if strings.Contains(o, m) {
			return true
		}
	}
	return false
}
