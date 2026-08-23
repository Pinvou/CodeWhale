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
	retry      time.Duration
	dimensions tokenRiskDimensions
	success    bool
	stableUser string
}

func (f *admissionFake) Admit(_ context.Context, dimensions tokenRiskDimensions) (time.Duration, error) {
	f.dimensions = dimensions
	return f.retry, nil
}

func (f *admissionFake) Complete(_ context.Context, _ tokenRiskDimensions, success bool, stableUser string) (time.Duration, error) {
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
	if admission.dimensions.source != "192.0.2.20" || admission.dimensions.device != "device-1" || admission.dimensions.identifier != "alice" || admission.success {
		t.Fatalf("dimensions/result = %+v success=%v", admission.dimensions, admission.success)
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
