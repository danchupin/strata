package kms

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// WrapTransient wraps err with ErrKMSUnavailable when err is plausibly
// transient (network timeout, DNS, refused connection, deadline
// exceeded). Permanent errors — ErrKeyIDMismatch, ErrMissingKeyID,
// authoritative provider denials — pass through unchanged. Providers
// call this from UnwrapDEK / GenerateDataKey so the auth resolver can
// decide between HTTP 503 (transient, Retry-After:30) and HTTP 401
// (permanent KeyDenied) without a string-match on err.Error().
func WrapTransient(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrKMSUnavailable) {
		return err
	}
	if errors.Is(err, ErrKeyIDMismatch) || errors.Is(err, ErrMissingKeyID) {
		return err
	}
	if isTransient(err) {
		return fmt.Errorf("%w: %v", ErrKMSUnavailable, err)
	}
	return err
}

// isTransient inspects err for the well-known shapes a Go HTTP / TCP
// client surfaces on network-layer failure. The match is intentionally
// conservative — false negatives mean a transient error reaches the
// fail-closed path (401 KeyDenied), which is acceptable; false
// positives would hide a permanent misconfiguration behind a 503 and
// erode operator signal.
func isTransient(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return true
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Last-resort string match for SDK wrappings that strip the typed
	// error (smithy retries, aws-sdk-go-v2 internal wraps). The set is
	// deliberately small: well-known transport phrases only.
	msg := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

var transientMarkers = []string{
	"connection refused",
	"connection reset",
	"no such host",
	"network is unreachable",
	"i/o timeout",
	"server misbehaving",
	"temporarily unavailable",
	"throttlingexception",
	"requesttimeout",
	"serviceunavailable",
}
