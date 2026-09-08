package queue

import (
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
)

// isNetworkError returns true for transient connectivity failures that should
// NOT count against the retry budget — EOF, connection reset, timeout, etc.
// Server-side rejections (4xx/5xx responses) are NOT network errors and do
// consume the budget so they eventually surface as Failed in the worklist.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Unwrap url.Error wrappers from the net/http client.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return isNetworkError(urlErr.Err)
	}
	// net.Error covers timeouts, temporary DNS failures, connection resets.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Bare EOF / unexpected EOF from a prematurely closed connection.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// String-match for errors the standard library surfaces without a
	// distinct type (syscall errors on Windows / Linux).
	msg := err.Error()
	for _, substr := range []string{
		"connection reset by peer",
		"connection refused",
		"no route to host",
		"broken pipe",
		"use of closed network connection",
		"forcibly closed by the remote host", // Windows wording
		"EOF",
	} {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// networkBackoff returns the wait before retrying a network-error attempt.
// Much gentler than the data-error backoff: 5s / 15s / 30s / 60s / 120s+.
// We don't count network retries, so this is indexed by a separate counter
// stored in QueueEntry.NetworkRetries (not Retries).
func networkBackoff(networkRetries int) int64 {
	switch networkRetries {
	case 0:
		return 5
	case 1:
		return 15
	case 2:
		return 30
	case 3:
		return 60
	default:
		return 120
	}
}
