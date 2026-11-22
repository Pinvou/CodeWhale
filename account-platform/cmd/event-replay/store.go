package main

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type replayStore interface {
	InspectDeadLetter(context.Context, uuid.UUID) (events.DeadLetterCandidate, error)
	ReplayDeadLetter(context.Context, events.ReplayRequest) (events.ReplayResult, error)
	Close()
}

type replayStoreConstructor func(context.Context, string) (replayStore, error)

func configuredReplayStore(ctx context.Context, databaseURL string) (replayStore, error) {
	return selectReplayStore(ctx, databaseURL,
		func(ctx context.Context, dsn string) (replayStore, error) {
			return events.NewAccountReplayPostgresStore(ctx, dsn)
		},
		func(ctx context.Context, dsn string) (replayStore, error) {
			return events.NewPostgresStore(ctx, dsn)
		})
}

// The deployed Account runtime identity is frozen by its migrations. Choose
// the protocol once from the existing parsed configuration, then let the
// Account constructor verify both actual database identities. Neither a new
// mandatory source setting nor a missing-function fallback is introduced.
func selectReplayStore(ctx context.Context, databaseURL string, account, generic replayStoreConstructor) (replayStore, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if config.User == "account_service" {
		return account(ctx, databaseURL)
	}
	return generic(ctx, databaseURL)
}
