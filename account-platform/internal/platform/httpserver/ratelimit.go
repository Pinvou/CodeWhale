package httpserver

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

type rateLimitVisitor struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]rateLimitVisitor
	rate     float64
	burst    float64
	now      func() time.Time
	lastGC   time.Time
}

func withRateLimit(cfg config.Config, next http.Handler) http.Handler {
	if cfg.RateLimitRPS <= 0 || cfg.RateLimitBurst <= 0 {
		return next
	}
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: cfg.RateLimitRPS, burst: float64(cfg.RateLimitBurst), now: time.Now}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Client traffic must not consume the health probes' admission budget.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.URL.Path == "/health/live" || r.URL.Path == "/health/ready") {
			next.ServeHTTP(w, r)
			return
		}
		if !limiter.allow(remoteHost(r.RemoteAddr)) {
			w.Header().Set("Retry-After", "1")
			api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "request rate limit exceeded", RequestID(r.Context()))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (limiter *rateLimiter) allow(key string) bool {
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.lastGC.IsZero() || now.Sub(limiter.lastGC) >= 10*time.Minute {
		for visitorKey, visitor := range limiter.visitors {
			if now.Sub(visitor.last) >= 10*time.Minute {
				delete(limiter.visitors, visitorKey)
			}
		}
		limiter.lastGC = now
	}
	visitor, found := limiter.visitors[key]
	if !found {
		visitor = rateLimitVisitor{tokens: limiter.burst, last: now}
	} else {
		visitor.tokens = min(limiter.burst, visitor.tokens+now.Sub(visitor.last).Seconds()*limiter.rate)
		visitor.last = now
	}
	allowed := visitor.tokens >= 1
	if allowed {
		visitor.tokens--
	}
	limiter.visitors[key] = visitor
	return allowed
}

func remoteHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil && host != "" {
		return host
	}
	if address == "" {
		return "unknown"
	}
	return address
}
