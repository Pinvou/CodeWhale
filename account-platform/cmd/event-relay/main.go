// event-relay publishes ordinary Account or Billing events. Only Account has
// the separately credentialed frozen authorization channel.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	logger := observability.ConfigureDefaultLogger(observability.Config{Service: "event-relay", Environment: environment, Version: "dev", Commit: "unknown"})
	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("ordinary Account relay stopped", "error", "ACCOUNT_EVENT_RELAY_UNAVAILABLE")
		os.Exit(1)
	}
}

func run(ctx context.Context, environment func(string) string, logger *slog.Logger) error {
	source := strings.TrimSpace(environment("EVENT_SOURCE"))
	if source == "" {
		source = "account"
	}
	if source != "account" && source != "billing" {
		return errors.New("ACCOUNT_EVENT_SOURCE_REQUIRED")
	}
	setup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var store *events.PostgresStore
	var err error
	if source == "account" {
		store, err = events.NewPartitionedPostgresStore(setup, environment("DATABASE_URL"), []string{account.ApprovalAuthorizationEventType})
	} else {
		store, err = events.NewPostgresStore(setup, environment("DATABASE_URL"))
	}
	if err != nil {
		return errors.New("ACCOUNT_EVENT_DATABASE_UNAVAILABLE")
	}
	defer store.Close()
	// No process-level auto provisioning or fallback identity, even in a test
	// environment. The separate operator owns stream creation and drift checks.
	broker, err := events.NewNATSBroker(setup, environment("NATS_URL"), source+"-outbox-relay", events.NATSConfig{
		Environment: environment("APP_ENV"), ResourceMode: "provisioned", CAFile: environment("NATS_CA_FILE"),
		SeedFile: environment("NATS_NKEY_SEED_FILE"), CredentialsFile: environment("NATS_CREDENTIALS_FILE"), RequireExplicitAuthentication: true,
	})
	if err != nil {
		return errors.New("ACCOUNT_EVENT_BROKER_UNAVAILABLE")
	}
	defer broker.Close()
	shutdownTracing, err := observability.SetupTracing(ctx, observability.Config{Service: "event-relay", Environment: environment("APP_ENV"), Version: "dev", Commit: "unknown"})
	if err != nil {
		return errors.New("ACCOUNT_EVENT_TRACING_UNAVAILABLE")
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = shutdownTracing(c)
	}()
	metrics := newRelayMetrics(source)
	approval := &approvalChannel{}
	if source == "account" {
		approval = openApprovalChannel(setup, environment, logger)
	}
	defer approval.close()
	address := strings.TrimSpace(environment("EVENT_RELAY_HTTP_ADDR"))
	if address == "" {
		address = ":9090"
	}
	server := metrics.serverWithApproval(address, func(c context.Context) error { return errors.Join(store.Ping(c), broker.Ping(c)) }, approval.ready)
	if source == "billing" {
		server = metrics.server(address, func(c context.Context) error { return errors.Join(store.Ping(c), broker.Ping(c)) })
	}
	defer shutdownServer(server)
	serverError := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverError <- errors.New("ACCOUNT_EVENT_HTTP_UNAVAILABLE")
		}
	}()
	approvalContext, cancelApproval := context.WithCancel(ctx)
	approvalDone := approval.start(approvalContext, logger, metrics)
	// Cancel before waiting: an HTTP listener failure must not strand the independent worker.
	defer func() { cancelApproval(); <-approvalDone }()
	relay := events.Relay{Store: store, Broker: broker, WorkerID: source + "-outbox-relay:" + uuid.NewString(), BatchSize: 100, Lease: 30 * time.Second, MaxAttempts: 12, AllowedSubjects: []string{source + ".>"}}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		jobID := uuid.NewString()
		jobContext, err := serviceauth.ContextForJob(ctx, source+"-event-relay", "OUTBOX_RELAY", jobID)
		if err != nil {
			return errors.New("ACCOUNT_EVENT_JOB_IDENTITY_INVALID")
		}
		started := time.Now()
		result, err := relay.RunOnce(jobContext)
		metrics.recordCycle(result, err, time.Since(started))
		if err != nil && ctx.Err() == nil {
			logger.Error("outbox relay cycle failed", "actor_type", "SERVICE", "service", source+"-event-relay", "job_type", "OUTBOX_RELAY", "job_run_id", jobID, "error", "ACCOUNT_EVENT_CYCLE_FAILED")
		} else if result.Claimed > 0 {
			logger.Info("outbox relay cycle completed", "actor_type", "SERVICE", "service", source+"-event-relay", "job_type", "OUTBOX_RELAY", "job_run_id", jobID, "claimed", result.Claimed, "published", result.Published, "failed", result.Failed, "dead_lettered", result.DeadLettered)
		}
		statContext, done := context.WithTimeout(ctx, 5*time.Second)
		stats, statErr := store.Stats(statContext)
		done()
		if statErr == nil {
			metrics.record(stats)
		} else if ctx.Err() == nil {
			logger.Error("outbox metrics query failed", "error", "ACCOUNT_EVENT_STATS_UNAVAILABLE")
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverError:
			return err
		case <-ticker.C:
		}
	}
}
