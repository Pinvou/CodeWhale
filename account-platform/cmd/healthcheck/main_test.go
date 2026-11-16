package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadinessChecksOnlyConfiguredEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name               string
		ordinary, approval int
		enableApproval     bool
		wantResult         string
		wantRequests       []string
	}{
		{"ordinary only", 200, 503, false, "", []string{"/ordinary"}},
		{"both ready", 200, 200, true, "", []string{"/ordinary", "/approval"}},
		{"approval unavailable", 200, 503, true, "APPROVAL_RELAY_READINESS_CHECK_NOT_READY", []string{"/ordinary", "/approval"}},
		{"ordinary unavailable", 503, 200, true, "READINESS_CHECK_NOT_READY", []string{"/ordinary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise the real request builder without allocating a listener.
			var requests []string
			client := &http.Client{Transport: readinessTransport(func(req *http.Request) *http.Response {
				requests = append(requests, req.URL.Path)
				writer := httptest.NewRecorder()
				status := tc.ordinary
				if req.URL.Path == "/approval" {
					status = tc.approval
				}
				writer.WriteHeader(status)
				return writer.Result()
			})}
			approval := ""
			if tc.enableApproval {
				approval = "http://example.test/approval"
			}
			if got := readinessFailure(context.Background(), client, "http://example.test/ordinary", approval); got != tc.wantResult {
				t.Fatalf("readiness result %q, want %q", got, tc.wantResult)
			}
			if !reflect.DeepEqual(requests, tc.wantRequests) {
				t.Fatalf("requests %v, want %v", requests, tc.wantRequests)
			}
		})
	}
}

type readinessTransport func(*http.Request) *http.Response

func (transport readinessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request), nil
}
