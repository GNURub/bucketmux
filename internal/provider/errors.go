package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type FailureKind string

const (
	FailureQuota       FailureKind = "quota"
	FailureThrottled   FailureKind = "throttled"
	FailureCredentials FailureKind = "credentials"
	FailureUnavailable FailureKind = "unavailable"
	FailurePermanent   FailureKind = "permanent"
)

type Error struct {
	Op         string
	Kind       FailureKind
	StatusCode int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s provider request failed (%s, status=%d): %v", e.Op, e.Kind, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("%s provider request failed (%s): %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func Failure(err error) (FailureKind, time.Duration, bool) {
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		return "", 0, false
	}
	return providerErr.Kind, providerErr.RetryAfter, true
}

func FailoverEligible(err error) bool {
	kind, _, ok := Failure(err)
	return ok && kind != FailurePermanent
}

func HTTPError(op string, response *http.Response, responseBody string) error {
	status := response.StatusCode
	body := strings.ToLower(responseBody)
	kind := FailurePermanent
	switch {
	case status == http.StatusInsufficientStorage || strings.Contains(body, "quotaexceeded") ||
		strings.Contains(body, "insufficientstorage") || strings.Contains(body, "storagefull"):
		kind = FailureQuota
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		strings.Contains(body, "invalidaccesskeyid") || strings.Contains(body, "signaturedoesnotmatch") ||
		strings.Contains(body, "expiredtoken") || strings.Contains(body, "authenticationfailed"):
		kind = FailureCredentials
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable ||
		strings.Contains(body, "slowdown") || strings.Contains(body, "throttl") || strings.Contains(body, "serverbusy"):
		kind = FailureThrottled
	case status >= 500:
		kind = FailureUnavailable
	}
	return &Error{Op: op, Kind: kind, StatusCode: status, RetryAfter: retryAfter(response.Header.Get("Retry-After")), Err: errors.New(strings.TrimSpace(responseBody))}
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}
