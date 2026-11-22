package httpserver

import (
	"container/list"
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
	node   *list.Element
}

const (
	rateLimitMaxVisitors = 10000
	rateLimitPruneBatch  = 32
	rateLimitIdleTTL     = 10 * time.Minute
)

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]rateLimitVisitor
	rate     float64
	burst    float64
	now      func() time.Time
	idle     list.List
	capacity int
}

func withRateLimit(cfg config.Config, next http.Handler) http.Handler {
	return withApplicationRateLimit(cfg, next, nil)
}

func withApplicationRateLimit(cfg config.Config, next, application http.Handler) http.Handler {
	if cfg.RateLimitRPS <= 0 || cfg.RateLimitBurst <= 0 {
		return next
	}
	limiter := &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: cfg.RateLimitRPS, burst: float64(cfg.RateLimitBurst), now: time.Now}
	newBucket := func(rate float64, burst int) *rateLimiter {
		return &rateLimiter{visitors: map[string]rateLimitVisitor{}, rate: rate, burst: float64(burst), now: time.Now}
	}
	// NFR-41 has an independent >=200/s authenticated callback contract.
	// Token issuance and invalid traffic cannot spend the notification budget.
	callbacks, tokens := newBucket(200, 200), newBucket(200, 200)
	invalid := newBucket(cfg.RateLimitRPS, cfg.RateLimitBurst)
	policy, _ := application.(AdmissionPolicy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Client traffic must not consume the health probes' admission budget.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.URL.Path == "/health/live" || r.URL.Path == "/health/ready") {
			next.ServeHTTP(w, r)
			return
		}
		bucket, key := limiter, rateLimitSource(r, cfg.HTTPTrustedProxyIPs)
		r = withAdmittedRateLimitSource(r, key)
		if policy != nil {
			class, principal := policy.RateLimitAdmission(r)
			switch class {
			case InvalidCallbackAdmission:
				bucket = invalid
			case VerifiedCallbackAdmission, VerifiedCallbackTokenAdmission:
				if principal == "" {
					bucket = invalid
					break
				}
				if class == VerifiedCallbackAdmission {
					bucket = callbacks
				} else {
					bucket = tokens
				}
				key += "\x00" + principal
			}
		}
		if !bucket.allow(key) {
			w.Header().Set("Retry-After", "1")
			api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "request rate limit exceeded", RequestID(r.Context()))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (limiter *rateLimiter) allow(key string) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	// Touch order makes the oldest idle entry available without scanning the
	// map. Even after a long idle period, one request retires at most one batch.
	for range rateLimitPruneBatch {
		oldest := limiter.idle.Front()
		if oldest == nil {
			break
		}
		oldKey := oldest.Value.(string)
		if now.Sub(limiter.visitors[oldKey].last) < rateLimitIdleTTL {
			break
		}
		delete(limiter.visitors, oldKey)
		limiter.idle.Remove(oldest)
	}
	visitor, found := limiter.visitors[key]
	if !found {
		capacity := limiter.capacity
		if capacity <= 0 {
			capacity = rateLimitMaxVisitors
		}
		if len(limiter.visitors) >= capacity {
			// Never evict an active bucket to give a new source fresh credits.
			return false
		}
		visitor = rateLimitVisitor{tokens: limiter.burst, last: now, node: limiter.idle.PushBack(key)}
	} else {
		if !now.Before(visitor.last) {
			visitor.tokens = min(limiter.burst, visitor.tokens+now.Sub(visitor.last).Seconds()*limiter.rate)
			visitor.last = now
		}
		limiter.idle.MoveToBack(visitor.node)
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
