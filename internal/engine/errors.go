package engine

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ErrDigestMismatch is returned when a source keeps serving content that does
// not match the requested digest.
var ErrDigestMismatch = errors.New("digest mismatch")

var (
	errRangeUnsupported = errors.New("server does not support range requests")
	errAgain            = errors.New("download state changed")
	errStalled          = errors.New("connection stalled")
)

// permanentError marks failures that retrying the same request cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func isPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// statusError is an unexpected HTTP response status.
type statusError struct {
	code       int
	host       string
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d %s from %s", e.code, http.StatusText(e.code), e.host)
}

// classifyStatus turns an unexpected response into an error, marking the ones
// that won't go away on retry as permanent. origHost is the host the request
// was sent to before any redirects.
func classifyStatus(resp *http.Response, origHost string) error {
	se := &statusError{
		code:       resp.StatusCode,
		host:       resp.Request.URL.Host,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if se.host != origHost {
			// Usually an expired pre-signed CDN URL; asking the registry
			// again hands out a fresh one.
			return se
		}
		return &permanentError{se}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusGone, http.StatusRequestedRangeNotSatisfiable:
		return &permanentError{se}
	}
	return se
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
