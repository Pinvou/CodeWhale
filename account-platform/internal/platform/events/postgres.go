package events

import (
	"context"

	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool               *pgxpool.Pool
	excludedEventTypes []string
}

type OutboxStats struct {
	Pending          int64
	DeadLettered     int64
	OldestPendingAge time.Duration
}

func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	return newPostgresStore(ctx, databaseURL, nil)
}

// NewPartitionedPostgresStore creates a generic outbox store whose claim and
// stats queries exclude a closed set of event types. The regular constructor
// retains the original unfiltered SQL path used by Billing and Token.
func NewPartitionedPostgresStore(ctx context.Context, databaseURL string, excludedEventTypes []string) (*PostgresStore, error) {
	excluded := make([]string, 0, len(excludedEventTypes))
	seen := map[string]bool{}
	for _, eventType := range excludedEventTypes {
		eventType = strings.TrimSpace(eventType)
		if eventType == "" || len(eventType) > 160 || strings.ContainsAny(eventType, " \t\r\n*>/") || seen[eventType] {
			return nil, errors.New("event store partition is invalid")
		}
		seen[eventType] = true
		excluded = append(excluded, eventType)
	}
	if len(excluded) == 0 || len(excluded) > 16 {
		return nil, errors.New("event store partition is invalid")
	}
	return newPostgresStore(ctx, databaseURL, excluded)
}

func newPostgresStore(ctx context.Context, databaseURL string, excludedEventTypes []string) (*PostgresStore, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse event store database URL: %w", err)
	}
	configuration.MaxConns = 4
	configuration.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("open event store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping event store: %w", err)
	}
	return &PostgresStore{pool: pool, excludedEventTypes: excludedEventTypes}, nil
}

func (store *PostgresStore) Close() { store.pool.Close() }

func (store *PostgresStore) Ping(ctx context.Context) error { return store.pool.Ping(ctx) }

func (store *PostgresStore) Stats(ctx context.Context) (OutboxStats, error) {
	var stats OutboxStats
	var oldestSeconds float64
	var err error
	if len(store.excludedEventTypes) == 0 {
		err = store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL),count(*) FILTER (WHERE dead_lettered_at IS NOT NULL),COALESCE(EXTRACT(EPOCH FROM now()-min(occurred_at) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL)),0) FROM outbox_events`).Scan(&stats.Pending, &stats.DeadLettered, &oldestSeconds)
	} else {
		err = store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL),count(*) FILTER (WHERE dead_lettered_at IS NOT NULL),COALESCE(EXTRACT(EPOCH FROM now()-min(occurred_at) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL)),0) FROM outbox_events WHERE NOT(event_type=ANY($1::text[]))`, store.excludedEventTypes).Scan(&stats.Pending, &stats.DeadLettered, &oldestSeconds)
	}
	stats.OldestPendingAge = time.Duration(oldestSeconds * float64(time.Second))
	return stats, err
}

func (store *PostgresStore) Claim(ctx context.Context, workerID string, limit int, lease time.Duration, allowedSubjects []string) ([]Event, error) {
	if strings.TrimSpace(workerID) == "" || limit < 1 || limit > 1000 || lease <= 0 || !validSubjectScope(allowedSubjects) {
		return nil, errors.New("event outbox claim parameters are invalid")
	}
	exact, prefixes := []string{}, []string{}
	for _, subject := range allowedSubjects {
		if strings.HasSuffix(subject, ".>") {
			prefixes = append(prefixes, strings.TrimSuffix(subject, ">"))
		} else {
			exact = append(exact, subject)
		}
	}
	// Scope and partition exclusion both apply before acquiring any lease.
	// Prefix equality uses left/length rather than LIKE so _ and % are literal.
	query := `
WITH candidates AS (
    SELECT event_id FROM outbox_events
    WHERE published_at IS NULL AND dead_lettered_at IS NULL AND next_attempt_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
      AND NOT(event_type=ANY($4::text[]))
      AND (event_type=ANY($5::text[]) OR EXISTS (
          SELECT 1 FROM unnest($6::text[]) AS scope(prefix)
          WHERE left(event_type,length(scope.prefix))=scope.prefix
      ))
    ORDER BY occurred_at, event_id
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE outbox_events event
SET locked_by=$2, locked_until=now() + ($3 * interval '1 second')
FROM candidates
WHERE event.event_id=candidates.event_id
RETURNING event.event_id,event.event_type,event.aggregate_id,event.aggregate_version,event.payload,event.trace_id,event.occurred_at,event.attempts,event.publication_generation,event.locked_until`
	excluded := store.excludedEventTypes
	if excluded == nil {
		excluded = []string{}
	}
	rows, err := store.pool.Query(ctx, query, limit, workerID, max(1, int(lease.Seconds())), excluded, exact, prefixes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claimed := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt, &event.Attempts, &event.PublicationGeneration, &event.LeaseUntil); err != nil {
			return nil, err
		}
		claimed = append(claimed, event)
	}
	return claimed, rows.Err()
}

func (store *PostgresStore) MarkPublished(ctx context.Context, workerID string, event Event) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempts=attempts+1,last_error=NULL,locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND publication_generation=$3 AND locked_until=$4 AND locked_until>now() AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID, event.PublicationGeneration, event.LeaseUntil)
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before publish acknowledgement")
	}
	return err
}

func (store *PostgresStore) MarkFailed(ctx context.Context, workerID string, event Event, retryAt time.Time, message string) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET attempts=attempts+1,last_error=$3,next_attempt_at=$4,locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND publication_generation=$5 AND locked_until=$6 AND locked_until>now() AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID, message, retryAt.UTC(), event.PublicationGeneration, event.LeaseUntil)
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before failure acknowledgement")
	}
	return err
}

func (store *PostgresStore) MarkDeadLettered(ctx context.Context, workerID string, event Event, message string) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET attempts=attempts+1,last_error=$3,dead_lettered_at=now(),locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND publication_generation=$4 AND locked_until=$5 AND locked_until>now() AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID, message, event.PublicationGeneration, event.LeaseUntil)
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before dead-letter acknowledgement")
	}
	return err
}

type InboxHandler func(context.Context, pgx.Tx, Event) error

func (store *PostgresStore) ProcessInbox(ctx context.Context, event Event, handler InboxHandler) (bool, error) {
	if err := event.Validate(); err != nil || handler == nil {
		return false, ErrInvalidEvent
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `INSERT INTO inbox_messages(event_id,event_type,aggregate_id,aggregate_version,received_at) VALUES ($1,$2,$3,$4,now()) ON CONFLICT (event_id) DO NOTHING`, event.ID, event.Type, event.AggregateID, event.AggregateVersion)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT last_version FROM inbox_aggregate_versions WHERE event_type=$1 AND aggregate_id=$2 FOR UPDATE`, event.Type, event.AggregateID).Scan(&current)
	hasCurrent := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	switch DecideVersion(hasCurrent, current, event.AggregateVersion) {
	case Stale:
		_, err = tx.Exec(ctx, `UPDATE inbox_messages SET processed_at=now(),last_error='stale aggregate version ignored' WHERE event_id=$1`, event.ID)
		if err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	case Gap:
		return false, ErrVersionGap
	}
	if err := handler(ctx, tx, event); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO inbox_aggregate_versions(event_type,aggregate_id,last_version,updated_at) VALUES ($1,$2,$3,now()) ON CONFLICT (event_type,aggregate_id) DO UPDATE SET last_version=EXCLUDED.last_version,updated_at=EXCLUDED.updated_at`, event.Type, event.AggregateID, event.AggregateVersion)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE inbox_messages SET processed_at=now(),last_error=NULL WHERE event_id=$1`, event.ID)
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
