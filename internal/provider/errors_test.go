package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPErrorClassificationForWriteFailover(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       FailureKind
		failover   bool
	}{
		{name: "expired credentials", status: http.StatusForbidden, body: `<Code>ExpiredToken</Code>`, want: FailureCredentials, failover: true},
		{name: "quota", status: http.StatusInsufficientStorage, body: `QuotaExceeded`, want: FailureQuota, failover: true},
		{name: "quota in forbidden response", status: http.StatusForbidden, body: `<Code>QuotaExceeded</Code>`, want: FailureQuota, failover: true},
		{name: "slow down", status: http.StatusServiceUnavailable, body: `<Code>SlowDown</Code>`, retryAfter: "12", want: FailureThrottled, failover: true},
		{name: "outage", status: http.StatusBadGateway, body: `upstream unavailable`, want: FailureUnavailable, failover: true},
		{name: "bad request", status: http.StatusBadRequest, body: `invalid bucket`, want: FailurePermanent, failover: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: http.Header{}}
			response.Header.Set("Retry-After", test.retryAfter)
			err := HTTPError("put", response, test.body)
			kind, retryAfter, ok := Failure(err)
			if !ok || kind != test.want || FailoverEligible(err) != test.failover {
				t.Fatalf("failure kind=%q ok=%v failover=%v err=%v", kind, ok, FailoverEligible(err), err)
			}
			if test.retryAfter != "" && retryAfter != 12*time.Second {
				t.Fatalf("retry after=%v", retryAfter)
			}
		})
	}
}
