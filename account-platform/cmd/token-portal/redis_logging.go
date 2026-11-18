package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Install before either CLI or HTTP clients are created. The library logger is
// process-wide; this adapter is confined to the token-portal executable.
func configurePortalRedisLogging() { redis.SetLogger(&portalRedisLogger{}) }

type portalRedisLogger struct {
	mu   sync.Mutex
	last time.Time
}

func (l *portalRedisLogger) Printf(ctx context.Context, _ string, _ ...interface{}) {
	if l.shouldReport(time.Now()) {
		// Library format strings/arguments may contain private connection details.
		// The operation's returned error is classified separately by the caller.
		slog.ErrorContext(ctx, "portal_redis_client_event", "error_code", "PORTAL_REDIS_CLIENT_EVENT")
	}
}

func (l *portalRedisLogger) shouldReport(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && !now.Before(l.last) && now.Sub(l.last) < time.Minute {
		return false
	}
	l.last = now
	return true
}
