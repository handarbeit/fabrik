package github

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
)

// StatusError is the generic non-2xx GitHub API failure (#2393): the status
// code is typed so a caller can tell a 5xx from a 4xx without matching the
// message. Its text is exactly the pre-#2393 errors.New text, so the string
// classifiers (isTransientError, ghfault) keep working unchanged.
type StatusError struct {
	Code int
	msg  string
}

func (e *StatusError) Error() string { return e.msg }

// serverErrorText matches the text form of a 5xx, which also covers errors
// built outside this package (ghfault.ServerError in the sim) and errors that
// crossed a string boundary.
var serverErrorText = regexp.MustCompile(`GitHub API returned 5\d\d`)

// IsTransportError reports whether err says the forge could not be reached or
// failed server-side: a timeout, a dial/connection error, a truncated
// response or a 5xx (#2393). A 4xx, a rate limit and a cancelled context are
// not transport errors — the first two prove the server answered, the last is
// the engine's own shutdown. A nil error is not one either.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	return serverErrorText.MatchString(err.Error())
}

// ResultObserver is told the final outcome of every HTTP request a Client
// issues (nil on success). The engine uses it to track forge reachability.
type ResultObserver func(err error)

// SetResultObserver installs fn (nil clears it). Call before the client is
// shared across goroutines.
func (c *Client) SetResultObserver(fn ResultObserver) {
	c.mu.Lock()
	c.observer = fn
	c.mu.Unlock()
}

func (c *Client) observe(err error) {
	c.mu.Lock()
	fn := c.observer
	c.mu.Unlock()
	if fn != nil {
		fn(err)
	}
}
