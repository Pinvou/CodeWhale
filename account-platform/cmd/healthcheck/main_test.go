package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfiguredReadinessPort(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health/ready" {
					t.Errorf("unexpected readiness path: %s", r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PORT", port)
			t.Setenv("SHUTDOWN_TIMEOUT", "")
			if err := checkReady(); (err == nil) != (status == http.StatusOK) {
				t.Fatalf("status=%d error=%v", status, err)
			}
		})
	}
}

func TestInvalidReadinessPort(t *testing.T) {
	t.Setenv("PORT", "0")
	if err := checkReady(); err == nil || !strings.Contains(err.Error(), "invalid PORT") {
		t.Fatalf("invalid port was not rejected: %v", err)
	}
}
