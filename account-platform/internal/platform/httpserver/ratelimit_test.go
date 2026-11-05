package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func TestRateLimitUsesDirectPeerAndRecoversTokens(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: 1, burst: 2, now: func() time.Time { return now }}
	if !limiter.allow("10.0.0.1") || !limiter.allow("10.0.0.1") || limiter.allow("10.0.0.1") {
		t.Fatal("token bucket did not enforce the configured burst")
	}
	now = now.Add(time.Second)
	if !limiter.allow("10.0.0.1") {
		t.Fatal("token bucket did not replenish")
	}
}

func TestRateLimitReturnsStableError(t *testing.T) {
	handler := withRequestID(withRateLimit(config.Config{RateLimitRPS: 1, RateLimitBurst: 1}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "10.0.0.1:1234"
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusNoContent {
			t.Fatalf("first request status = %d", response.Code)
		}
		if attempt == 1 && (response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1") {
			t.Fatalf("limited request response = %d %#v", response.Code, response.Header())
		}
	}
}
