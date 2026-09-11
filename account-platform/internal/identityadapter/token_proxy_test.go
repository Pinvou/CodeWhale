package identityadapter

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type admissionFake struct {
	retry       time.Duration
	dimensions  tokenRiskDimensions
	success     bool
	stableUser  string
	completions int
}

func (f *admissionFake) Admit(_ context.Context, dimensions tokenRiskDimensions) (time.Duration, error) {
	f.dimensions = dimensions
	return f.retry, nil
}

func (f *admissionFake) Complete(_ context.Context, _ tokenRiskDimensions, success bool, stableUser string) (time.Duration, error) {
	f.completions++
	f.success = success
	f.stableUser = stableUser
	return 0, nil
}

func TestTokenProxyAppliesDimensionsAndNormalizesCredentialFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("password") != "secret-value" {
			t.Fatal("password was not relayed in the form body")
		}
		http.Error(w, `{"error":"user_not_found"}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()
	admission := &admissionFake{}
	proxy, err := NewTokenProxy(upstream.URL, admission)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0

	request := httptest.NewRequest(http.MethodPost, "/realms/pinvou/protocol/openid-connect/token", strings.NewReader("grant_type=password&username=Alice&password=secret-value"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 192.0.2.20")
	request.Header.Set("X-Device-ID", "device-1")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Body.String() != "{\"error\":\"invalid_grant\"}\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if admission.dimensions.source != "192.0.2.20" || admission.dimensions.device != "device-1" || admission.dimensions.identifier != "alice" || admission.success || admission.completions != 1 {
		t.Fatalf("dimensions/result = %+v success=%v completions=%d", admission.dimensions, admission.success, admission.completions)
	}
}

func TestTokenProxyPreservesClientConfigurationFailuresWithoutCountingCredentialFailure(t *testing.T) {
	for _, upstreamError := range []string{"invalid_client", "unauthorized_client"} {
		t.Run(upstreamError, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"` + upstreamError + `"}`))
			}))
			defer upstream.Close()
			admission := &admissionFake{}
			proxy, _ := NewTokenProxy(upstream.URL, admission)
			proxy.floor = 0
			request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&client_id=missing&username=alice&password=secret"))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()

			proxy.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized || response.Body.String() != "{\"error\":\"unauthorized_client\"}\n" {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if admission.completions != 0 {
				t.Fatalf("client configuration failure counted as credential failure: completions=%d", admission.completions)
			}
		})
	}
}

func TestTokenProxyMapsUpstreamAvailabilityFailuresWithoutCountingCredentialFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}))
	defer upstream.Close()
	admission := &admissionFake{}
	proxy, _ := NewTokenProxy(upstream.URL, admission)
	proxy.floor = 0
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&client_id=desktop&username=alice&password=secret"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("response = %d %q retry=%q", response.Code, response.Body.String(), response.Header().Get("Retry-After"))
	}
	if admission.completions != 0 {
		t.Fatalf("availability failure counted as credential failure: completions=%d", admission.completions)
	}
}

func TestTokenProxyPreservesBoundedUpstreamRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "23")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}))
	defer upstream.Close()
	admission := &admissionFake{}
	proxy, _ := NewTokenProxy(upstream.URL, admission)
	proxy.floor = 0
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&client_id=desktop&username=alice&password=secret"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || response.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" || response.Header().Get("Retry-After") != "23" {
		t.Fatalf("response = %d %q retry=%q", response.Code, response.Body.String(), response.Header().Get("Retry-After"))
	}
	if admission.completions != 0 {
		t.Fatalf("upstream rate limit counted as credential failure: completions=%d", admission.completions)
	}
}

func TestUpstreamRetryAfterIsSafeAndBounded(t *testing.T) {
	now := time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC)
	for name, input := range map[string]struct {
		raw  string
		want int
	}{
		"missing": {raw: "", want: 1},
		"invalid": {raw: "later", want: 1},
		"zero":    {raw: "0", want: 1},
		"seconds": {raw: "23", want: 23},
		"bounded": {raw: "999", want: maxUpstreamRetryAfterSeconds},
		"date":    {raw: now.Add(12 * time.Second).Format(http.TimeFormat), want: 12},
		"old date": {
			raw:  now.Add(-12 * time.Second).Format(http.TimeFormat),
			want: 1,
		},
		"far date": {
			raw:  time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat),
			want: maxUpstreamRetryAfterSeconds,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := upstreamRetryAfter(input.raw, now); got != input.want {
				t.Fatalf("upstreamRetryAfter(%q) = %d, want %d", input.raw, got, input.want)
			}
		})
	}
}

func TestTokenProxyRejectsBeforeUpstreamWhenLimited(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	proxy, _ := NewTokenProxy(upstream.URL, &admissionFake{retry: 17 * time.Second})
	proxy.floor = 0
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=refresh_token&refresh_token=opaque"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if called || response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "17" {
		t.Fatalf("called=%v response=%d retry=%q", called, response.Code, response.Header().Get("Retry-After"))
	}
}

func TestTokenProxyForwardsTrustedRiskDimensionsAndLearnsStableSubject(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"stable-user-7"}`))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Pinvou-Client-Source") != "192.0.2.8" || r.Header.Get("X-Device-ID") != "device-7" {
			t.Fatalf("risk headers = source %q device %q", r.Header.Get("X-Pinvou-Client-Source"), r.Header.Get("X-Device-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"header.` + payload + `.signature","refresh_token":"opaque"}`))
	}))
	defer upstream.Close()
	admission := &admissionFake{}
	proxy, _ := NewTokenProxy(upstream.URL, admission)
	proxy.floor = 0
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&client_id=desktop&username=alice&password=secret"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Forwarded-For", "192.0.2.8")
	request.Header.Set("X-Device-ID", "device-7")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !admission.success || admission.stableUser != "stable-user-7" {
		t.Fatalf("response=%d success=%v stable=%q", response.Code, admission.success, admission.stableUser)
	}
}
