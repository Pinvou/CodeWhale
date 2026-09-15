package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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
	var store *events.PostgresStore
	if source == "account" {
		store, err = events.NewPartitionedPostgresStore(ctx, os.Getenv("DATABASE_URL"), []string{account.ApprovalAuthorizationEventType})
	} else {
		store, err = events.NewPostgresStore(ctx, os.Getenv("DATABASE_URL"))
	}
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), source+"-outbox-relay", events.NATSConfig{Environment: environment, CAFile: os.Getenv("NATS_CA_FILE")})
	if err != nil {
		log.Fatal(err)
	}
	defer broker.Close()
	hostname, _ := os.Hostname()
	relay := events.Relay{Store: store, Broker: broker, WorkerID: hostname + ":" + uuid.NewString(), BatchSize: 100, Lease: 30 * time.Second, MaxAttempts: 12, AllowedSubjects: []string{source + ".>"}}
	logger = logger.With("source", source)
	metrics := newRelayMetrics(source)
	var approvalStore *account.ApprovalRelayStore
	var approvalBroker *events.NATSBroker
	var approvalArtifacts events.NATSAuthorizationArtifactFiles
	if source == "account" {
		approvalSeedFile := os.Getenv("APPROVAL_RELAY_NATS_NKEY_SEED_FILE")
		approvalCredentialsFile := os.Getenv("APPROVAL_RELAY_NATS_CREDENTIALS_FILE")
		approvalArtifacts = events.NATSAuthorizationArtifactFiles{
			CurrentFile: os.Getenv("APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: os.Getenv("APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256"),
			PreviousFile: os.Getenv("APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: os.Getenv("APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_SHA256"),
			PrivateIdentityFiles: []string{approvalSeedFile, approvalCredentialsFile},
		}
		err = approvalArtifacts.Validate(time.Now().UTC())
		if err == nil {
			approvalStore, err = account.NewApprovalRelayStore(ctx, os.Getenv("APPROVAL_RELAY_DATABASE_URL"))
		}
		if err == nil {
			approvalBroker, err = events.NewNATSPublishOnlyBroker(ctx, os.Getenv("NATS_URL"), "account-approval-relay", events.NATSConfig{
				Environment: environment, CAFile: os.Getenv("NATS_CA_FILE"),
				SeedFile: approvalSeedFile, CredentialsFile: approvalCredentialsFile, RequireExplicitAuthentication: true,
			})
		}
		if err != nil {
			if approvalStore != nil {
				approvalStore.Close()
				approvalStore = nil
			}
			if approvalBroker != nil {
				approvalBroker.Close()
				approvalBroker = nil
			}
			logger.Error("authorization relay dependency is not ready", "error", "APPROVAL_AUTHORIZATION_RELAY_UNAVAILABLE")
		}
	}
	if approvalStore != nil {
		defer approvalStore.Close()
	}
	if approvalBroker != nil {
		defer approvalBroker.Close()
	}
	var approvalReady func(context.Context) error
	if source == "account" {
		approvalReady = func(ctx context.Context) error {
			if approvalStore == nil || approvalBroker == nil {
				return account.ErrApprovalRelayUnavailable
			}
			return errors.Join(approvalArtifacts.Validate(time.Now().UTC()), approvalStore.Ready(ctx), approvalBroker.Ping(ctx))
		}
	}
	metricsServer := metrics.serverWithApproval(":9090", func(ctx context.Context) error {
		return errors.Join(store.Ping(ctx), broker.Ping(ctx))
	}, approvalReady)
	defer shutdownServer(metricsServer)
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("relay metrics server stopped", "error", err)
			stop()
		}
	}()
	if approvalStore != nil && approvalBroker != nil {
		approvalRelay := events.FrozenRelay{Store: approvalStore, Broker: approvalBroker, WorkerID: hostname + ":approval:" + uuid.NewString(), BatchSize: 100, Lease: 30 * time.Second, MaxAttempts: 12, Observer: metrics}
		approvalDone := make(chan struct{})
		go func() {
			defer close(approvalDone)
			runApprovalRelay(ctx, logger, metrics, approvalStore, approvalRelay)
		}()
		defer func() { <-approvalDone }()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		jobRunID := uuid.NewString()
		jobContext, jobIdentityErr := serviceauth.ContextForJob(ctx, source+"-event-relay", "OUTBOX_RELAY", jobRunID)
		if jobIdentityErr != nil {
			log.Fatal(jobIdentityErr)
		}
		cycleStarted := time.Now()
		result, err := relay.RunOnce(jobContext)
		metrics.recordCycle(result, err, time.Since(cycleStarted))
		if err != nil && ctx.Err() == nil {
			logger.Error("outbox relay cycle failed", "actor_type", "SERVICE", "service", source+"-event-relay", "job_type", "OUTBOX_RELAY", "job_run_id", jobRunID, "error", err)
		} else if result.Claimed > 0 {
			logger.Info("outbox relay cycle completed", "actor_type", "SERVICE", "service", source+"-event-relay", "job_type", "OUTBOX_RELAY", "job_run_id", jobRunID, "claimed", result.Claimed, "published", result.Published, "failed", result.Failed, "dead_lettered", result.DeadLettered)
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

func runApprovalRelay(ctx context.Context, logger *slog.Logger, metrics *relayMetrics, store *account.ApprovalRelayStore, relay events.FrozenRelay) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		jobRunID := uuid.NewString()
		jobContext, identityErr := serviceauth.ContextForJob(ctx, "account-approval-relay", "OUTBOX_RELAY", jobRunID)
		if identityErr == nil {
			cycleStarted := time.Now()
			result, cycleErr := relay.RunOnce(jobContext)
			metrics.recordApprovalCycle(result, cycleErr, time.Since(cycleStarted))
			if cycleErr != nil && ctx.Err() == nil {
				logger.Error("authorization relay cycle failed", "error", "APPROVAL_AUTHORIZATION_RELAY_UNAVAILABLE")
			}
			if stats, statsErr := store.Stats(ctx); statsErr == nil {
				metrics.recordApproval(stats)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
