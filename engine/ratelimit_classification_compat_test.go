package engine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestIsTransientAPIError_ClassifiedRateLimitsStayTransient pins the coupling
// #1951 introduced: github now returns a typed *gh.RateLimitError for
// rate-limited 403/429 responses, but isTransientAPIError (#1313) still
// classifies by substring against err.Error(). If the rate-limit error text
// ever drops the "GitHub API returned %d: <body>" framing, the engine would
// silently stop deferring rate limits.
func TestIsTransientAPIError_ClassifiedRateLimitsStayTransient(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
	}{
		{"primary 403", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1790000000"}, `{"message":"API rate limit exceeded for user ID 1."}`},
		{"secondary 403", 403, map[string]string{"Retry-After": "60"}, `{"message":"You have exceeded a secondary rate limit."}`},
		{"429 unknown body", 429, nil, `{"message":"slow down"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := gh.NewClientWithBaseURL("tok", srv.URL)
			_, err := c.AddComment("o", "r", 1, "hi")
			if !errors.Is(err, gh.ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
			if !isTransientAPIError(err) {
				t.Errorf("isTransientAPIError(%q) = false; the engine would stop deferring this rate limit", err)
			}
			if strings.Contains(err.Error(), "fine-grained access token") {
				t.Errorf("rate-limited error carries the PAT hint: %v", err)
			}
		})
	}
}
