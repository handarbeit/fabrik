package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestIsTransportError(t *testing.T) {
	timeoutErr := &url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline", fmt.Errorf("executing request: %w", timeoutErr), true},
		{"bare deadline", context.DeadlineExceeded, true},
		{"dial", fmt.Errorf("executing request: %w", &net.OpError{Op: "dial", Err: errors.New("refused")}), true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"502 typed", &StatusError{Code: 502, msg: "GitHub API returned 502: bad"}, true},
		{"503 text", errors.New("GitHub API returned 503: unavailable"), true},
		{"404", fmt.Errorf("GitHub API returned 404: x: %w", ErrNotFound), false},
		{"422", fmt.Errorf("GitHub API returned 422: x: %w", ErrUnprocessableEntity), false},
		{"400 typed", &StatusError{Code: 400, msg: "GitHub API returned 400: x"}, false},
		{"403", &forbiddenError{msg: "GitHub API returned 403: x"}, false},
		{"rate limit", &RateLimitError{}, false},
		{"canceled", fmt.Errorf("executing request: %w", &url.Error{Op: "Get", Err: context.Canceled}), false},
		{"graphql", &GraphQLError{Message: "boom"}, false},
	}
	for _, tc := range cases {
		if got := IsTransportError(tc.err); got != tc.want {
			t.Errorf("%s: IsTransportError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStatusErrorKeepsText(t *testing.T) {
	err := apiStatusError(502, http.Header{}, []byte("oops"))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 502 {
		t.Fatalf("want *StatusError 502, got %T %v", err, err)
	}
	if err.Error() != "GitHub API returned 502: oops" {
		t.Errorf("text changed: %q", err.Error())
	}
}

func TestResultObserver_HTTPOutcomes(t *testing.T) {
	status := 200
	slow := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow {
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"labels":[]}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("tok", srv.URL)
	c.httpClient.Timeout = 50 * time.Millisecond
	var mu sync.Mutex
	var got []bool
	c.SetResultObserver(func(err error) {
		mu.Lock()
		got = append(got, IsTransportError(err))
		mu.Unlock()
	})

	run := func() { _, _ = c.FetchLabels("o", "r", 1) }
	status = 200
	run()
	status = 404
	run()
	status = 502
	run()
	status = 200
	slow = true
	run()

	want := []bool{false, false, true, true}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("observer calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d transport = %v, want %v (all %v)", i, got[i], want[i], got)
		}
	}
}
