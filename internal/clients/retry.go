// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

// The wait before the first retry, and the longest any wait grows to.
const (
	retryFirstWait = time.Second
	retryMaxWait   = 30 * time.Second
)

// newRetryFunc returns the gophercloud.RetryFunc that max_retries installs. It
// retries a GET, HEAD or DELETE up to maxRetries times when the request draws
// a 429 or a 5xx, or gets no answer at all (a refused or dropped connection,
// for example), unless waiting cannot help (see isRetryable). Before each
// retry it waits twice as long as before, from retryFirstWait up to
// retryMaxWait. sleep does the waiting and returns the context's error if the
// context ends first.
//
// A POST, PUT or PATCH is never retried. The service may already have acted on
// a request whose answer was lost or replaced by a gateway error. A replayed
// POST then creates a second object that nothing in state points at, and
// several OpenStack PUTs, such as add_router_interface, are not idempotent.
func newRetryFunc(maxRetries int, sleep func(context.Context, time.Duration) error) gophercloud.RetryFunc {
	return func(ctx context.Context, method, _ string, _ *gophercloud.RequestOpts, err error, failCount uint) error {
		// failCount is how many times this request has failed, starting at 1,
		// so it has been retried failCount-1 times.
		if failCount > uint(maxRetries) || !isRetryable(ctx, method, err) {
			return err
		}
		// A nil return tells gophercloud to send the request again. If the
		// context ends during the wait, keep what the last attempt drew in the
		// error, so the reason for the retry is not lost.
		if serr := sleep(ctx, retryWait(failCount)); serr != nil {
			return fmt.Errorf("%w (last attempt: %v)", serr, err)
		}
		return nil
	}
}

// isRetryable reports whether a request sent with method that failed with err
// may be sent again.
func isRetryable(ctx context.Context, method string, err error) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
	default:
		return false
	}
	// gophercloud reports a status it did not expect as an
	// ErrUnexpectedResponseCode, after it has handled a 401 by reauthenticating.
	var status gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &status) {
		return status.Actual == http.StatusTooManyRequests || status.Actual >= http.StatusInternalServerError
	}
	// http.Client reports every request that got no answer as a *url.Error,
	// including one cut short because its context ended, which is final.
	// gophercloud also calls the RetryFunc when it cannot decode an answer;
	// that error is neither kind, so it is not retried.
	var transport *url.Error
	if !errors.As(err, &transport) || ctx.Err() != nil {
		return false
	}
	// A certificate the provider does not trust, TLS spoken to a port that
	// does not speak it (net/http reports a plain-HTTP answer as
	// http.ErrSchemeMismatch), and a host name that does not resolve fail the
	// same way every time, so retrying them only delays the error. A DNS
	// lookup that timed out may work when sent again.
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &certErr), errors.As(err, &recordErr), errors.Is(err, http.ErrSchemeMismatch):
		return false
	case errors.As(err, &dnsErr):
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	return true
}

// retryWait returns how long to wait before retrying a request that has failed
// failCount times.
func retryWait(failCount uint) time.Duration {
	wait := retryFirstWait
	for i := uint(1); i < failCount && wait < retryMaxWait; i++ {
		wait *= 2
	}
	return min(wait, retryMaxWait)
}

// sleepContext waits for d, or returns ctx's error as soon as ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
