package github

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrRateLimited is matched (via errors.Is) by every *RateLimitError: GitHub
// refused a request because a rate limit — primary or secondary — was hit, not
// because the credential was rejected. It is deliberately distinct from
// ErrAppUnauthorized: a rate-limited 403 means "wait", never "the App's
// identity was rejected" and never "switch token type" (#1951).
var ErrRateLimited = errors.New("rate limited")

// RateLimitError is the typed form of ErrRateLimited. ResetAt is the earliest
// instant GitHub says a retry may succeed (from Retry-After, else
// X-RateLimit-Reset); the zero time means no usable reset was reported. The
// value is the raw parsed one — consumers decide how far to trust it.
type RateLimitError struct {
	StatusCode int
	ResetAt    time.Time
	// Message is the response body text, kept in the error string so callers
	// that classify by substring (engine.isTransientAPIError) still match.
	Message string
	// prefix is the "GitHub API"/"GitHub App API" framing preserved from the
	// pre-classification error text.
	prefix string
}

func (e *RateLimitError) Error() string {
	msg := fmt.Sprintf("%s returned %d: %s (rate limited", e.prefix, e.StatusCode, e.Message)
	if !e.ResetAt.IsZero() {
		msg += "; resets at " + e.ResetAt.UTC().Format(time.RFC3339)
	}
	return msg + ")"
}

// Is makes errors.Is(err, ErrRateLimited) true. It intentionally does not
// match ErrAppUnauthorized.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// rateLimitNow is the clock used to turn a Retry-After delta into an instant;
// a var so tests can pin it.
var rateLimitNow = time.Now

// maxRetryAfterSeconds caps a Retry-After delta before it is multiplied into a
// Duration, so a hostile or corrupt value cannot overflow. One year is far
// beyond anything a consumer would honour.
const maxRetryAfterSeconds = 365 * 24 * 3600

// rateLimitMessagePhrases are the narrow body phrases GitHub uses for its
// rate-limit 403s. Deliberately the same set as the engine's
// rateLimitErrorPatterns: a bare "rate limit" would reclassify a genuine
// permissions 403 whose body merely quotes rate-limit documentation.
var rateLimitMessagePhrases = []string{
	"api rate limit exceeded",
	"secondary rate limit",
	"abuse detection",
}

// classifyRateLimit returns a *RateLimitError when the response is a
// rate-limited one, else nil. A 429 is always rate-limited. A 403 is
// rate-limited only on a positive signal: X-RateLimit-Remaining present and 0,
// a Retry-After header, or one of rateLimitMessagePhrases in the body. A bare
// 403 remains an authorization failure. prefix is the error-text framing
// ("GitHub API" or "GitHub App API").
func classifyRateLimit(prefix string, status int, header http.Header, body []byte) *RateLimitError {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return nil
	}
	if status == http.StatusForbidden && !hasRateLimitSignal(header, body) {
		return nil
	}
	return &RateLimitError{
		StatusCode: status,
		ResetAt:    parseRateLimitReset(header),
		Message:    string(body),
		prefix:     prefix,
	}
}

func hasRateLimitSignal(header http.Header, body []byte) bool {
	if v := strings.TrimSpace(header.Get("X-RateLimit-Remaining")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n == 0 {
			return true
		}
	}
	if strings.TrimSpace(header.Get("Retry-After")) != "" {
		return true
	}
	lower := strings.ToLower(string(body))
	for _, p := range rateLimitMessagePhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// parseRateLimitReset returns the reset instant from Retry-After (delta
// seconds or HTTP-date), falling back to X-RateLimit-Reset (epoch seconds).
// Malformed, zero and negative values yield the zero time.
func parseRateLimitReset(header http.Header) time.Time {
	if ra := strings.TrimSpace(header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
			if secs > 0 {
				if secs > maxRetryAfterSeconds {
					secs = maxRetryAfterSeconds
				}
				return rateLimitNow().Add(time.Duration(secs) * time.Second)
			}
		} else if t, err := http.ParseTime(ra); err == nil {
			return t
		}
	}
	if v := strings.TrimSpace(header.Get("X-RateLimit-Reset")); v != "" {
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0)
		}
	}
	return time.Time{}
}
