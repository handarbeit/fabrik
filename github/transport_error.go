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

// answerText matches the text form of a response the server actually sent: a
// 1xx-4xx status or a GraphQL error. It covers errors built outside this
// package (ghfault in the sim) and errors that crossed a string boundary.
var answerText = regexp.MustCompile(`GitHub API returned [1-4]\d\d|GraphQL error: `)

// IsForgeAnswer reports whether err is evidence that the forge answered: nil
// (a success), or a response with a non-5xx status (a 4xx, a rate limit, a
// GraphQL error). A transport error is not an answer, and neither is an error
// that never reached the forge or says nothing about it — a cancelled context
// (the engine's own shutdown) or a local request-build failure. Callers that
// track reachability ignore outcomes that are neither a transport error nor an
// answer (#2393).
func IsForgeAnswer(err error) bool {
	if err == nil {
		return true
	}
	if IsTransportError(err) || errors.Is(err, context.Canceled) {
		return false
	}
	var rl *RateLimitError
	var se *StatusError
	var fe *forbiddenError
	var ge *GraphQLError
	switch {
	case errors.As(err, &rl), errors.As(err, &fe), errors.As(err, &ge):
		return true
	case errors.As(err, &se):
		return se.Code < 500
	}
	for _, sentinel := range []error{
		ErrNotFound, ErrForbidden, ErrMethodNotAllowed, ErrConflict,
		ErrUnprocessableEntity, ErrDiffTooLarge, ErrRateLimited,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return answerText.MatchString(err.Error())
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
