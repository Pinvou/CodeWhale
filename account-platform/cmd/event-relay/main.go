package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func main() {
	source := strings.ToLower(strings.TrimSpace(os.Getenv("EVENT_SOURCE")))
	if source != "account" && source != "billing" && source != "token" {
		log.Fatal("EVENT_SOURCE must be account, billing, or token")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "event-relay", "source", source)
	metrics := newRelayMetrics(source)
	metricsServer := metrics.server(":9090")
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
		result, err := relay.RunOnce(ctx)
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
