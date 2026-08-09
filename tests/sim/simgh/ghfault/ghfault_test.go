package ghfault

import (
	"errors"
	"strings"
	"testing"
)

// The authoritative assertion that these constructors are shaped correctly is
// engine/simgh_fault_classification_test.go, which runs them through the
// engine's real (unexported) classifier. These tests are the local guard: they
// pin the substring each constructor promises, so a reworded message fails
// here — in the package that owns the wording — rather than only in a distant
// engine test.
func TestConstructorsCarryTheirClassificationPhrase(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // lowercased substring the engine matches on
	}{
		{"RateLimit", RateLimit(), "api rate limit exceeded"},
		{"GraphQLRateLimit", GraphQLRateLimit(), "api rate limit already exceeded"},
		{"SecondaryRateLimit", SecondaryRateLimit(), "secondary rate limit"},
		{"AbuseDetection", AbuseDetection(), "abuse detection"},
		{"TooManyRequests", TooManyRequests(), "github api returned 429"},
		{"ServerError", ServerError(), "github api returned 5"},
		{"ConnectionReset", ConnectionReset(), "connection reset"},
		{"IOTimeout", IOTimeout(), "i/o timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("constructor returned nil")
			}
			if got := strings.ToLower(tc.err.Error()); !strings.Contains(got, tc.want) {
				t.Errorf("%s() = %q, want it to contain %q", tc.name, got, tc.want)
			}
		})
	}
}

// NotFound is the control case: it must not carry any transient phrase, or
// every "this error escalates" assertion built on it would be testing the
// defer path instead.
func TestNotFoundCarriesNoTransientPhrase(t *testing.T) {
	msg := strings.ToLower(NotFound().Error())
	for _, phrase := range []string{
		"api rate limit", "secondary rate limit", "abuse detection",
		"github api returned 429", "github api returned 5",
		"connection reset", "i/o timeout",
	} {
		if strings.Contains(msg, phrase) {
			t.Errorf("NotFound() = %q, must not contain transient phrase %q", msg, phrase)
		}
	}
}

func TestWrapPreservesTheSentinel(t *testing.T) {
	sentinel := errors.New("not mergeable")
	err := Wrap(sentinel, "simgh: merging PR #7")

	if !errors.Is(err, sentinel) {
		t.Fatalf("Wrap(%v) = %v, want errors.Is to match the sentinel", sentinel, err)
	}
	if !strings.Contains(err.Error(), "simgh: merging PR #7") {
		t.Errorf("Wrap() = %q, want it to carry the caller's message", err)
	}
	if !strings.Contains(err.Error(), "not mergeable") {
		t.Errorf("Wrap() = %q, want it to carry the sentinel's message", err)
	}
}

// A nil sentinel must still yield a usable error rather than a nil the
// injector would read as "no fault" — silently turning a registered fault into
// a call that succeeds is the exact false pass this layer exists to prevent.
func TestWrapWithNilSentinelStillReturnsAnError(t *testing.T) {
	err := Wrap(nil, "boom")
	if err == nil {
		t.Fatal("Wrap(nil, …) = nil, want a non-nil error")
	}
	if err.Error() != "boom" {
		t.Errorf("Wrap(nil, \"boom\") = %q, want %q", err, "boom")
	}
}
