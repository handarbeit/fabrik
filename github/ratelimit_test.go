package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const patHintFragment = "fine-grained access token"

func pinRateLimitNow(t *testing.T, now time.Time) {
	t.Helper()
	old := rateLimitNow
	rateLimitNow = func() time.Time { return now }
	t.Cleanup(func() { rateLimitNow = old })
}

func TestClassifyRateLimit_Matrix(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	pinRateLimitNow(t, now)

	primaryBody := `{"message":"API rate limit exceeded for installation ID 1."}`
	secondaryBody := `{"message":"You have exceeded a secondary rate limit. Please wait."}`
	abuseBody := `{"message":"You have triggered an abuse detection mechanism."}`
	permBody := `{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`

	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		want    bool
	}{
		{"403 remaining 0", 403, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"x"}`, true},
		{"403 remaining nonzero", 403, map[string]string{"X-RateLimit-Remaining": "12"}, permBody, false},
		{"403 retry-after", 403, map[string]string{"Retry-After": "60"}, `{"message":"x"}`, true},
		{"403 primary message", 403, nil, primaryBody, true},
		{"403 secondary message", 403, nil, secondaryBody, true},
		{"403 abuse message", 403, nil, abuseBody, true},
		{"403 message case-insensitive", 403, nil, `{"message":"API Rate Limit Exceeded"}`, true},
		{"bare 403", 403, nil, `{"message":"forbidden"}`, false},
		{"403 permissions quoting rate-limit docs", 403, nil, permBody, false},
		{"429 bare", 429, nil, `{"message":"slow down"}`, true},
		{"429 with reset", 429, map[string]string{"X-RateLimit-Reset": "1790000000"}, `{}`, true},
		{"401", 401, map[string]string{"X-RateLimit-Remaining": "0"}, primaryBody, false},
		{"500 remaining 0", 500, map[string]string{"X-RateLimit-Remaining": "0"}, primaryBody, false},
		{"404", 404, nil, primaryBody, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := classifyRateLimit("GitHub API", tc.status, h, []byte(tc.body))
			if (got != nil) != tc.want {
				t.Fatalf("classifyRateLimit = %v, want rate-limited=%v", got, tc.want)
			}
		})
	}
}

func TestClassifyRateLimit_ResetParsing(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	pinRateLimitNow(t, now)

	httpDate := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name    string
		headers map[string]string
		want    time.Time
	}{
		{"none", nil, time.Time{}},
		{"x-ratelimit-reset epoch", map[string]string{"X-RateLimit-Reset": "1790000000"}, time.Unix(1790000000, 0)},
		{"retry-after seconds", map[string]string{"Retry-After": "45"}, now.Add(45 * time.Second)},
		{"retry-after http-date", map[string]string{"Retry-After": httpDate}, now.Add(90 * time.Second).Truncate(time.Second)},
		{"retry-after preferred over reset", map[string]string{"Retry-After": "10", "X-RateLimit-Reset": "1790000000"}, now.Add(10 * time.Second)},
		{"retry-after zero falls back to reset", map[string]string{"Retry-After": "0", "X-RateLimit-Reset": "1790000000"}, time.Unix(1790000000, 0)},
		{"retry-after negative, no reset", map[string]string{"Retry-After": "-5"}, time.Time{}},
		{"retry-after malformed", map[string]string{"Retry-After": "soon"}, time.Time{}},
		{"reset malformed", map[string]string{"X-RateLimit-Reset": "abc"}, time.Time{}},
		{"reset zero", map[string]string{"X-RateLimit-Reset": "0"}, time.Time{}},
		{"reset negative", map[string]string{"X-RateLimit-Reset": "-1"}, time.Time{}},
		{"retry-after huge is capped, no overflow", map[string]string{"Retry-After": strconv.FormatInt(1<<62, 10)}, now.Add(maxRetryAfterSeconds * time.Second)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-RateLimit-Remaining", "0")
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := classifyRateLimit("GitHub API", 403, h, []byte(`{}`))
			if got == nil {
				t.Fatal("expected rate-limited")
			}
			if !got.ResetAt.Equal(tc.want) {
				t.Errorf("ResetAt = %v, want %v", got.ResetAt, tc.want)
			}
		})
	}
}

func TestRateLimitError_IsAsAndText(t *testing.T) {
	reset := time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC)
	rl := &RateLimitError{StatusCode: 403, ResetAt: reset, Message: `{"message":"API rate limit exceeded"}`, prefix: "GitHub API"}
	wrapped := fmt.Errorf("listing: %w", rl)

	if !errors.Is(wrapped, ErrRateLimited) {
		t.Error("errors.Is(err, ErrRateLimited) = false")
	}
	if errors.Is(wrapped, ErrAppUnauthorized) {
		t.Error("rate-limit error must not match ErrAppUnauthorized")
	}
	var got *RateLimitError
	if !errors.As(wrapped, &got) || !got.ResetAt.Equal(reset) {
		t.Errorf("errors.As failed or wrong ResetAt: %v", got)
	}
	msg := rl.Error()
	for _, want := range []string{"GitHub API returned 403", "API rate limit exceeded", "2026-09-29T13:00:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, patHintFragment) {
		t.Errorf("message carries the PAT hint: %q", msg)
	}
	noReset := (&RateLimitError{StatusCode: 429, Message: "x", prefix: "GitHub API"}).Error()
	if strings.Contains(noReset, "resets at") {
		t.Errorf("message with no reset must not name one: %q", noReset)
	}
}

// rateLimitedServer answers every request with the given status, headers and body.
func rateLimitedServer(status int, headers map[string]string, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

var primaryLimitHeaders = map[string]string{
	"X-RateLimit-Remaining": "0",
	"X-RateLimit-Reset":     "1790000000",
}

const primaryLimitBody = `{"message":"API rate limit exceeded for installation ID 149677864."}`

func assertRateLimited(t *testing.T, err error, wantReset time.Time) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRateLimited)", err)
	}
	if errors.Is(err, ErrAppUnauthorized) {
		t.Errorf("err = %v must not match ErrAppUnauthorized", err)
	}
	if strings.Contains(err.Error(), patHintFragment) {
		t.Errorf("rate-limited error carries the PAT hint: %v", err)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("errors.As(*RateLimitError) failed for %v", err)
	}
	if !rl.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v, want %v", rl.ResetAt, wantReset)
	}
}

func TestAppRequest_RateLimited403(t *testing.T) {
	srv := rateLimitedServer(403, primaryLimitHeaders, primaryLimitBody)
	defer srv.Close()

	_, _, err := FetchInstallationRepositories(srv.URL, "inst-token")
	assertRateLimited(t, err, time.Unix(1790000000, 0))
	if !strings.Contains(err.Error(), "GitHub App API returned 403") {
		t.Errorf("App framing lost: %v", err)
	}
}

func TestAppRequest_RateLimited429(t *testing.T) {
	srv := rateLimitedServer(429, map[string]string{"Retry-After": "60"}, `{"message":"slow down"}`)
	defer srv.Close()
	pinRateLimitNow(t, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))

	_, _, err := FetchAppInstallations(srv.URL, "jwt")
	assertRateLimited(t, err, time.Date(2026, 9, 29, 12, 1, 0, 0, time.UTC))
}

func TestAppRequest_RateLimitedSecondary403(t *testing.T) {
	srv := rateLimitedServer(403, map[string]string{"Retry-After": "30"}, `{"message":"You have exceeded a secondary rate limit."}`)
	defer srv.Close()
	pinRateLimitNow(t, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))

	_, _, err := MintInstallationToken(srv.URL, "jwt", 1)
	assertRateLimited(t, err, time.Date(2026, 9, 29, 12, 0, 30, 0, time.UTC))
}

func TestAppRequest_Genuine401KeepsErrAppUnauthorizedWithoutHint(t *testing.T) {
	srv := rateLimitedServer(401, nil, `{"message":"Bad credentials"}`)
	defer srv.Close()

	_, _, err := FetchAppInstallations(srv.URL, "jwt")
	if !errors.Is(err, ErrAppUnauthorized) {
		t.Fatalf("err = %v, want ErrAppUnauthorized", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("401 must not be rate-limited: %v", err)
	}
	if strings.Contains(err.Error(), patHintFragment) {
		t.Errorf("App client must never emit the PAT hint: %v", err)
	}
}

func TestAppRequest_BareForbiddenStaysUnauthorizedWithoutHint(t *testing.T) {
	srv := rateLimitedServer(403, nil, `{"message":"Resource not accessible by integration"}`)
	defer srv.Close()

	_, _, err := FetchAppInstallations(srv.URL, "jwt")
	if !errors.Is(err, ErrAppUnauthorized) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrAppUnauthorized only", err)
	}
	if strings.Contains(err.Error(), patHintFragment) {
		t.Errorf("App client must never emit the PAT hint: %v", err)
	}
}

func TestDo_RateLimited403HasNoHint(t *testing.T) {
	srv := rateLimitedServer(403, primaryLimitHeaders, primaryLimitBody)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("GET", srv.URL+"/x", nil)
	assertRateLimited(t, err, time.Unix(1790000000, 0))
	if !strings.Contains(err.Error(), "GitHub API returned 403: ") ||
		!strings.Contains(strings.ToLower(err.Error()), "api rate limit exceeded") {
		t.Errorf("engine-facing framing lost: %v", err)
	}
}

func TestDo_RateLimited429(t *testing.T) {
	srv := rateLimitedServer(429, nil, `{"message":"slow down"}`)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("GET", srv.URL+"/x", nil)
	assertRateLimited(t, err, time.Time{})
	if !strings.Contains(err.Error(), "GitHub API returned 429") {
		t.Errorf("429 framing lost: %v", err)
	}
}

func TestDo_GenuineAuthFailuresKeepHint(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `{"message":"Bad credentials"}`},
		{403, `{"message":"Resource not accessible by personal access token"}`},
	} {
		srv := rateLimitedServer(tc.status, nil, tc.body)
		c := NewClientWithBaseURL("token", srv.URL)
		_, _, err := c.do("GET", srv.URL+"/x", nil)
		srv.Close()
		if err == nil || errors.Is(err, ErrRateLimited) {
			t.Fatalf("status %d: err = %v, want a plain auth error", tc.status, err)
		}
		if !strings.Contains(err.Error(), patHintFragment) {
			t.Errorf("status %d: genuine auth failure lost its hint: %v", tc.status, err)
		}
	}
}

func TestGraphqlRequest_RateLimited403HasNoHint(t *testing.T) {
	srv := rateLimitedServer(403, primaryLimitHeaders, primaryLimitBody)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	c.graphqlURL = srv.URL
	var out struct{}
	err := c.graphqlRequest("query{}", nil, &out)
	assertRateLimited(t, err, time.Unix(1790000000, 0))
	if !strings.Contains(err.Error(), "GitHub API returned 403: ") {
		t.Errorf("engine-facing framing lost: %v", err)
	}
}

func TestGraphqlRequest_RateLimited429(t *testing.T) {
	srv := rateLimitedServer(429, map[string]string{"Retry-After": "5"}, `{"message":"slow down"}`)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	c.graphqlURL = srv.URL
	var out struct{}
	err := c.graphqlRequest("query{}", nil, &out)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}
