package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// The approval channel is optional to the ordinary worker, but readiness never
// converts a missing authorization identity or artifact into success.
type approvalChannel struct {
	store     *account.ApprovalRelayStore
	broker    *events.NATSBroker
	artifacts events.NATSAuthorizationArtifactFiles
}

func openApprovalChannel(ctx context.Context, environment func(string) string, logger *slog.Logger) *approvalChannel {
	seed := environment("APPROVAL_RELAY_NATS_NKEY_SEED_FILE")
	credentials := environment("APPROVAL_RELAY_NATS_CREDENTIALS_FILE")
	channel := &approvalChannel{artifacts: events.NATSAuthorizationArtifactFiles{
		CurrentFile: environment("APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256"),
		PreviousFile: environment("APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_SHA256"),
		PrivateIdentityFiles: []string{seed, credentials},
	}}
	err := channel.artifacts.Validate(time.Now().UTC())
	if err == nil {
		channel.store, err = account.NewApprovalRelayStore(ctx, environment("APPROVAL_RELAY_DATABASE_URL"))
	}
	if err == nil {
		channel.broker, err = events.NewNATSPublishOnlyBroker(ctx, environment("NATS_URL"), "account-approval-relay", events.NATSConfig{
			Environment: environment("APP_ENV"), ResourceMode: "provisioned", CAFile: environment("NATS_CA_FILE"),
			SeedFile: seed, CredentialsFile: credentials, RequireExplicitAuthentication: true,
		})
	}
	if err != nil {
		channel.close()
		channel.store, channel.broker = nil, nil
		logger.Error("authorization relay dependency is not ready", "error", "APPROVAL_AUTHORIZATION_RELAY_UNAVAILABLE")
	}
	return channel
}

func (channel *approvalChannel) close() {
	if channel.store != nil {
		channel.store.Close()
	}
	if channel.broker != nil {
		channel.broker.Close()
	}
}

func (channel *approvalChannel) ready(ctx context.Context) error {
	if channel.store == nil || channel.broker == nil {
		return account.ErrApprovalRelayUnavailable
	}
	return errors.Join(channel.artifacts.Validate(time.Now().UTC()), channel.store.Ready(ctx), channel.broker.Ping(ctx))
}

func (channel *approvalChannel) start(ctx context.Context, logger *slog.Logger, metrics *relayMetrics) <-chan struct{} {
	done := make(chan struct{})
	if channel.store == nil || channel.broker == nil {
		close(done)
		return done
	}
	relay := events.FrozenRelay{Store: channel.store, Broker: channel.broker, WorkerID: "account-approval-relay:" + uuid.NewString(), BatchSize: 100, Lease: 30 * time.Second, MaxAttempts: 12, Observer: metrics}
	go func() { defer close(done); runApprovalRelay(ctx, logger, metrics, channel.store, relay) }()
	return done
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
			statsContext, cancelStats := context.WithTimeout(ctx, 5*time.Second)
			stats, statsErr := store.Stats(statsContext)
			cancelStats()
			if statsErr == nil {
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
