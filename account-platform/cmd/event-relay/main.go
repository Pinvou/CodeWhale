package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

func main() {
	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	logger := observability.ConfigureDefaultLogger(observability.Config{Service: "event-relay", Environment: environment, Version: "dev", Commit: "unknown"})
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"EVENT_OUTBOX_RELAY": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
	source := strings.ToLower(strings.TrimSpace(os.Getenv("EVENT_SOURCE")))
	if source != "account" && source != "billing" && source != "token" {
		log.Fatal("EVENT_SOURCE must be account, billing, or token")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := observability.SetupTracing(ctx, observability.Config{Service: "event-relay", Environment: environment, Version: "dev", Commit: "unknown"})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownContext)
	}()
	store, err := events.NewPostgresStore(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), source+"-outbox-relay")
	if err != nil {
		log.Fatal(err)
	}
	defer broker.Close()
	hostname, _ := os.Hostname()
	relay := events.Relay{Store: store, Broker: broker, WorkerID: hostname + ":" + uuid.NewString(), BatchSize: 100, Lease: 30 * time.Second, MaxAttempts: 12}
	logger = logger.With("source", source)
	metrics := newRelayMetrics(source)
	metricsServer := metrics.server(":9090", func(ctx context.Context) error {
		return errors.Join(store.Ping(ctx), broker.Ping(ctx))
	})
	defer shutdownServer(metricsServer)
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("relay metrics server stopped", "error", err)
			stop()
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		cycleStarted := time.Now()
		result, err := relay.RunOnce(ctx)
		metrics.recordCycle(result, err, time.Since(cycleStarted))
		if err != nil && ctx.Err() == nil {
			logger.Error("outbox relay cycle failed", "error", err)
		} else if result.Claimed > 0 {
			logger.Info("outbox relay cycle completed", "claimed", result.Claimed, "published", result.Published, "failed", result.Failed, "dead_lettered", result.DeadLettered)
		}
		stats, statsErr := store.Stats(ctx)
		if statsErr != nil && ctx.Err() == nil {
			logger.Error("outbox metrics query failed", "error", statsErr)
		} else {
			metrics.record(stats)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
