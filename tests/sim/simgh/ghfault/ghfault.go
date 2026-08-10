// Package ghfault constructs the error shapes Fabrik's engine classifies
// specially, for injection through simgh's fault table.
//
// # Why this is its own package
//
// It is a deliberately tiny, stdlib-only leaf: it imports nothing from this
// repository, so `engine` can import it from a test without a cycle.
// `tests/sim/simgh` imports `engine` (it satisfies engine.GitHubClient), so an
// internal `package engine` test cannot reach the constructors if they live
// there — and the classifier they exist to pin, isTransientAPIError, is
// unexported and reached only through unexported code, so an external
// `package engine_test` cannot see it either. Splitting the constructors out
// is what lets `engine/simgh_fault_classification_test.go` assert against the
// real classifier without exporting it and without touching production code.
//
// # Why the strings matter
//
// The engine classifies these failures by substring match against err.Error()
// (engine/item.go: isTransientError, rateLimitErrorPatterns,
// isTransientAPIError). An injected error whose text the classifier does not
// recognise exercises the *non*-transient branch, so a fault-injection test
// built on it silently proves the opposite of what it claims. Each constructor
// below therefore documents the exact production pattern it is built to match,
// and engine/simgh_fault_classification_test.go asserts the match against the
// real predicates — so a rewording of either side fails loudly rather than
// quietly re-routing every scenario built on it.
//
// The messages imitate GitHub's own framing (REST's "GitHub API returned %d:
// %s", GraphQL's error prose) rather than being minimal strings that merely
// happen to contain the pattern. A test built on a bare "abuse detection"
// would keep passing if the classifier were narrowed to require surrounding
// context that real GitHub always supplies.
package ghfault

import (
	"errors"
	"fmt"
)

// RateLimit returns GitHub's REST primary rate-limit exhaustion error.
//
// Matches the "api rate limit exceeded" entry of engine/item.go's
// rateLimitErrorPatterns, so isTransientAPIError classifies it as transient
// (defer indefinitely, do not consume an escalation budget). Note that
// isTransientError — the narrower, bounded-retry predicate — does *not*
// recognise it; that asymmetry is the point of the two predicates and is
// asserted in engine/simgh_fault_classification_test.go.
func RateLimit() error {
	return errors.New("GitHub API returned 403: API rate limit exceeded for user ID 12345")
}

// GraphQLRateLimit returns GitHub's GraphQL primary rate-limit exhaustion
// error, whose wording differs from REST's.
//
// Matches the "api rate limit already exceeded" entry of rateLimitErrorPatterns.
// It exists separately from RateLimit because the two phrases are separate
// entries in that table — deliberately split so a bare "api rate limit" prefix
// cannot match unrelated prose — and a constructor for only one of them would
// leave the other unpinned.
func GraphQLRateLimit() error {
	return errors.New("GraphQL error: API rate limit already exceeded for user ID 12345")
}

// SecondaryRateLimit returns GitHub's secondary rate-limit error, the one
// returned for bursty writes rather than for quota exhaustion.
//
// Matches the "secondary rate limit" entry of rateLimitErrorPatterns.
func SecondaryRateLimit() error {
	return errors.New("GitHub API returned 403: You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
}

// AbuseDetection returns GitHub's abuse-detection error.
//
// Matches the "abuse detection" entry of rateLimitErrorPatterns.
func AbuseDetection() error {
	return errors.New("GitHub API returned 403: You have triggered an abuse detection mechanism. Please wait a few minutes before you try again.")
}

// TooManyRequests returns a bare HTTP 429 in the engine's REST framing.
//
// Matches isTransientAPIError's "github api returned 429" substring rather
// than any phrase-table entry: GitHub uses 429 exclusively for rate limiting,
// so the engine treats the status code alone as sufficient. This is the one
// rate-limit shape that carries no recognisable body prose, which is exactly
// why it is worth being able to inject.
func TooManyRequests() error {
	return errors.New("GitHub API returned 429: Too Many Requests")
}

// ServerError returns a GitHub 5xx in the engine's REST framing.
//
// Matches isTransientError's "GitHub API returned 5" substring, so it is
// transient under *both* predicates — unlike the rate-limit shapes, which only
// isTransientAPIError recognises. Injecting this exercises the bounded-retry
// paths; injecting RateLimit exercises the defer-indefinitely ones.
func ServerError() error {
	return errors.New("GitHub API returned 502: Bad Gateway")
}

// ConnectionReset returns a transport-layer connection reset.
//
// Matches isTransientError's "connection reset" substring. It is a plain
// string error rather than a real *net.OpError on purpose: the engine's string
// branch and its errors.As branch are separate classification routes, and this
// constructor exists to pin the string one. A *net.OpError would be classified
// by the other route and would leave the substring untested.
func ConnectionReset() error {
	return errors.New("Post \"https://api.github.com/graphql\": read tcp 10.0.0.1:54321->140.82.121.6:443: connection reset by peer")
}

// IOTimeout returns a transport-layer read timeout.
//
// Matches isTransientError's "i/o timeout" substring. Plain string error for
// the same reason as ConnectionReset.
func IOTimeout() error {
	return errors.New("Post \"https://api.github.com/graphql\": dial tcp 140.82.121.6:443: i/o timeout")
}

// NotFound returns a GitHub 404 in the engine's REST framing.
//
// Deliberately *not* transient under either predicate: it is here so a
// scenario can inject a failure the engine must escalate rather than defer,
// which is the control case every transient-classification assertion needs.
func NotFound() error {
	return errors.New("GitHub API returned 404: Not Found")
}

// Wrap produces an error carrying sentinel in its chain, so that a caller
// matching with errors.Is (as the engine does for github.ErrNotMergeable,
// ErrNotMergeableCI and ErrNotFound) recognises the injected failure as that
// specific condition.
//
// The sentinel is supplied by the caller rather than named here, because those
// sentinels live in the `github` package and this one imports nothing from
// this repository — see the package doc.
func Wrap(sentinel error, msg string) error {
	if sentinel == nil {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %w", msg, sentinel)
}
