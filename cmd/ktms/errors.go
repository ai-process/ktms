package main

import (
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exit codes, by what the caller should do about the error.
const (
	exitOK       = 0
	exitFailed   = 1 // anything else
	exitUsage    = 2 // bad flags or arguments
	exitChanges  = 3 // config plan --detailed-exitcode: the plan is not empty
	exitAuth     = 4 // no key, a wrong key or a key without the scope
	exitNotFound = 5
	exitRejected = 6 // Keystone refused the request as it stands; retrying will not help
	exitRetry    = 7 // unavailable, throttled or timed out; retry later
)

const exitCodesHelp = `Exit codes: 0 done, 1 failed, 2 bad usage, 3 plan has changes (--detailed-exitcode),
4 not authenticated or not allowed, 5 not found, 6 rejected as invalid,
7 unavailable or throttled (retry later).`

type usageError struct{ error }
type authError struct{ error }

// httpError is a non-2xx answer from the HTTP API.
type httpError struct {
	status int
	body   string
}

func (e httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var u usageError
	if errors.As(err, &u) {
		return exitUsage
	}
	var a authError
	if errors.As(err, &a) {
		return exitAuth
	}
	var h httpError
	if errors.As(err, &h) {
		switch {
		case h.status == http.StatusUnauthorized || h.status == http.StatusForbidden:
			return exitAuth
		case h.status == http.StatusNotFound:
			return exitNotFound
		case h.status == http.StatusTooManyRequests || h.status >= 500:
			return exitRetry
		case h.status >= 400:
			return exitRejected
		}
		return exitFailed
	}
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unauthenticated, codes.PermissionDenied:
			return exitAuth
		case codes.NotFound:
			return exitNotFound
		case codes.InvalidArgument, codes.FailedPrecondition, codes.AlreadyExists, codes.OutOfRange:
			return exitRejected
		case codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded, codes.Aborted:
			return exitRetry
		}
	}
	return exitFailed
}

// errText drops the "rpc error: code = ... desc =" wrapping of gRPC errors.
func errText(err error) string {
	if s, ok := status.FromError(err); ok && s.Code() != codes.OK && s.Code() != codes.Unknown {
		return fmt.Sprintf("%s: %s", s.Code(), s.Message())
	}
	return err.Error()
}
