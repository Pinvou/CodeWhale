package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type recoveryPassRunner interface {
	RunOnce(context.Context, int) (account.ApprovalRecoveryPass, error)
}

func runRecoveryPasses(ctx context.Context, runtime recoveryPassRunner, interval time.Duration, limit int, ready *atomic.Bool, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		summary, err := runtime.RunOnce(passCtx, limit)
		cancel()
		ready.Store(err == nil)
		if ctx.Err() != nil {
			return
		}
		slog.InfoContext(ctx, "approval recovery pass", "candidates", summary.Candidates, "confirmed", summary.Confirmed, "invalidated", summary.Invalidated, "pending", summary.Pending, "unavailable", summary.Unavailable, "more_candidates", summary.MoreCandidates)
		if err != nil {
			slog.ErrorContext(ctx, "approval recovery failed", "error_code", "APPROVAL_RECOVERY_UNAVAILABLE")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func main() {
	if err := httpserver.ConfigureProcessLogging("account-approval-recovery"); err != nil {
		log.Fatal("RECOVERY_LOG_CONFIGURATION_INVALID")
	}
	if err := run(); err != nil {
		log.Fatal("RECOVERY_PROCESS_FAILED")
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := loadRecoveryConfiguration()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	pool, runtime, err := connectRecovery(startup, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	var ready atomic.Bool
	done := make(chan struct{})
	go runRecoveryPasses(ctx, runtime, cfg.interval, cfg.limit, &ready, done)
	err = httpserver.RunWithApplicationContext(ctx, "account-approval-recovery", http.NotFoundHandler(), func(ctx context.Context) error {
		if !ready.Load() {
			return errors.New("APPROVAL_RECOVERY_NOT_READY")
		}
		return pool.Ping(ctx)
	})
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		return errors.New("APPROVAL_RECOVERY_DRAIN_FAILED")
	}
	return err
}
