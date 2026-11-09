package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestRateLimitCapacityPreservesActiveCreditsAndReclaimsIdle(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: 1, burst: 2, capacity: 2, now: func() time.Time { return now }}
	if !limiter.allow("one") || !limiter.allow("two") || limiter.allow("new") {
		t.Fatal("capacity did not reject only the new source")
	}
	if !limiter.allow("one") || limiter.allow("one") || len(limiter.visitors) != 2 {
		t.Fatal("capacity reset or lost an existing bucket")
	}
	now = now.Add(time.Minute)
	if !limiter.allow("one") {
		t.Fatal("existing source cannot replenish at capacity")
	}
	now = now.Add(9 * time.Minute)
	if !limiter.allow("new") {
		t.Fatal("expired oldest source did not release capacity")
	}
	if _, present := limiter.visitors["two"]; present {
		t.Fatal("idle source was retained")
	}
	if _, present := limiter.visitors["one"]; !present || limiter.idle.Len() != len(limiter.visitors) {
		t.Fatal("active source or ordering state lost")
	}
}

func TestRateLimitPrunesOnlyOneBoundedBatch(t *testing.T) {
	now := time.Unix(100, 0)
	count := 3 * rateLimitPruneBatch
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: 1, burst: 1, capacity: count, now: func() time.Time { return now }}
	for index := range count {
		if !limiter.allow(fmt.Sprint(index)) {
			t.Fatal("initial source refused")
		}
	}
	now = now.Add(rateLimitIdleTTL)
	if !limiter.allow("next") || len(limiter.visitors) != count-rateLimitPruneBatch+1 {
		t.Fatalf("one request must prune exactly one batch: %d", len(limiter.visitors))
	}
	if !limiter.allow("following") || len(limiter.visitors) != count-2*rateLimitPruneBatch+2 || limiter.idle.Len() != len(limiter.visitors) {
		t.Fatal("subsequent request did not continue bounded retirement")
	}
}

func TestRateLimitConcurrentNewSourcesStayBounded(t *testing.T) {
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: 1, burst: 1, capacity: 64, now: func() time.Time { return time.Unix(100, 0) }}
	var workers sync.WaitGroup
	for index := range 256 {
		workers.Go(func() { limiter.allow(fmt.Sprint(index)) })
	}
	workers.Wait()
	if len(limiter.visitors) != 64 || limiter.idle.Len() != 64 {
		t.Fatal("concurrent admission exceeded capacity or lost ordering")
	}
}

func TestRateLimitTableCapacityKeepsHealthProbesReachable(t *testing.T) {
	readyCalls := 0
	handler := HandlerWithApplication(config.Config{ServiceName: "fixture", RateLimitRPS: 1, RateLimitBurst: 1},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		func(context.Context) error { readyCalls++; return nil })
	request := func(source, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = source + ":1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for index := range rateLimitMaxVisitors {
		if request(fmt.Sprintf("198.18.%d.%d", index/256, index%256), "/business").Code != http.StatusNoContent {
			t.Fatal("source below default table capacity was rejected")
		}
	}
	if response := request("203.0.113.1", "/business"); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatal("full table did not preserve stable rejection response")
	}
	for _, path := range []string{"/health/live", "/health/ready"} {
		if request("203.0.113.1", path).Code != http.StatusOK {
			t.Fatal("table capacity blocked a health probe")
		}
	}
	if readyCalls != 1 {
		t.Fatal("probe exemption bypassed readiness check")
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
