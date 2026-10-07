package tokengovernance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActivationClientClosedAuthoritativeRead(t *testing.T) {
	digest := strings.Repeat("a", 64)
	value := Snapshot{State: "ACTIVE", Epoch: 1, PolicySetDigest: digest, HandlerSetDigest: digest, ApprovedRuntimeRoles: []string{"release_a"}, UpdatedAt: time.Now().UTC().Add(-time.Second)}
	encoded, _ := json.Marshal(value)
	valid := string(encoded)
	tests := []struct {
		name, body, cache string
		status            int
		success           bool
	}{
		{"active", valid, "no-store", 200, true},
		{"safe", strings.Replace(valid, `"ACTIVE"`, `"SAFE_ROLLBACK"`, 1), "no-store", 200, true},
		{"duplicate state", strings.Replace(valid, `"state":"ACTIVE"`, `"state":"PRE_ACTIVATION","state":"ACTIVE"`, 1), "no-store", 200, false},
		{"duplicate epoch", strings.Replace(valid, `"epoch":1`, `"epoch":0,"epoch":1`, 1), "no-store", 200, false},
		{"unknown field", strings.Replace(valid, `{`, `{"extra":true,`, 1), "no-store", 200, false},
		{"case alias", strings.Replace(valid, `"state"`, `"State"`, 1), "no-store", 200, false},
		{"null roles", strings.Replace(valid, `["release_a"]`, `null`, 1), "no-store", 200, false},
		{"duplicate roles", strings.Replace(valid, `["release_a"]`, `["release_a","release_a"]`, 1), "no-store", 200, false},
		{"unknown state", strings.Replace(valid, `"ACTIVE"`, `"FUTURE"`, 1), "no-store", 200, false},
		{"wrong digest", strings.Replace(valid, digest, strings.Repeat("b", 64), 1), "no-store", 200, false},
		{"trailing object", valid + `{}`, "no-store", 200, false},
		{"cacheable", valid, "max-age=30", 200, false},
		{"service unavailable", valid, "no-store", 503, false},
		{"oversized", valid + strings.Repeat(" ", 16<<10), "no-store", 200, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/token-governance/activation" || r.Header.Get("Authorization") == "" {
					t.Error("activation request identity or method missing")
				}
				w.Header().Set("Cache-Control", test.cache)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "test-service-token", digest, digest)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Snapshot(context.Background())
			if (err == nil) != test.success {
				t.Fatalf("success=%v error=%v", test.success, err)
			}
			if err != nil && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestActivationClientPreAndAbsoluteDeadline(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pre := Snapshot{State: "PRE_ACTIVATION", ApprovedRuntimeRoles: []string{}, UpdatedAt: time.Now().UTC()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(pre)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-service-token", digest, digest)
	if got, err := client.Snapshot(context.Background()); err != nil || got.Epoch != 0 || got.State != "PRE_ACTIVATION" {
		t.Fatalf("PRE read: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Snapshot(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled request must fail closed: %v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, _ = NewClient(redirect.URL, "test-service-token", digest, digest)
	if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect accepted: %v", err)
	}
}
