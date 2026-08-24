package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

type OutboxStats struct {
	Pending          int64
	DeadLettered     int64
	OldestPendingAge time.Duration
}

func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
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
	return &PostgresStore{pool: pool}, nil
}

func (store *PostgresStore) Close() { store.pool.Close() }

func (store *PostgresStore) Ping(ctx context.Context) error { return store.pool.Ping(ctx) }

func (store *PostgresStore) Stats(ctx context.Context) (OutboxStats, error) {
	var stats OutboxStats
	var oldestSeconds float64
	err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL),count(*) FILTER (WHERE dead_lettered_at IS NOT NULL),COALESCE(EXTRACT(EPOCH FROM now()-min(occurred_at) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL)),0) FROM outbox_events`).Scan(&stats.Pending, &stats.DeadLettered, &oldestSeconds)
	stats.OldestPendingAge = time.Duration(oldestSeconds * float64(time.Second))
	return stats, err
}

func (store *PostgresStore) Claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]Event, error) {
	rows, err := store.pool.Query(ctx, `
WITH candidates AS (
    SELECT event_id FROM outbox_events
    WHERE published_at IS NULL AND dead_lettered_at IS NULL AND next_attempt_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY occurred_at, event_id
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE outbox_events event
SET locked_by=$2, locked_until=now() + ($3 * interval '1 second')
FROM candidates
WHERE event.event_id=candidates.event_id
RETURNING event.event_id,event.event_type,event.aggregate_id,event.aggregate_version,event.payload,event.trace_id,event.occurred_at,event.attempts`, limit, workerID, max(1, int(lease.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claimed := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt, &event.Attempts); err != nil {
			return nil, err
		}
		claimed = append(claimed, event)
	}
	return claimed, rows.Err()
}

func (store *PostgresStore) MarkPublished(ctx context.Context, workerID string, event Event) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempts=attempts+1,last_error=NULL,locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID)
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before publish acknowledgement")
	}
	return err
}

func (store *PostgresStore) MarkFailed(ctx context.Context, workerID string, event Event, retryAt time.Time, message string) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET attempts=attempts+1,last_error=$3,next_attempt_at=$4,locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID, message, retryAt.UTC())
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before failure acknowledgement")
	}
	return err
}

func (store *PostgresStore) MarkDeadLettered(ctx context.Context, workerID string, event Event, message string) error {
	command, err := store.pool.Exec(ctx, `UPDATE outbox_events SET attempts=attempts+1,last_error=$3,dead_lettered_at=now(),locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL AND dead_lettered_at IS NULL`, event.ID, workerID, message)
	if err == nil && command.RowsAffected() != 1 {
		return errors.New("event outbox lease was lost before dead-letter acknowledgement")
	}
	return err
}

type DeadLetterCandidate struct {
	EventID          uuid.UUID `json:"event_id"`
	EventType        string    `json:"event_type"`
	AggregateID      uuid.UUID `json:"aggregate_id"`
	AggregateVersion int64     `json:"aggregate_version"`
	EventDigest      string    `json:"event_digest"`
	Attempts         int       `json:"attempts"`
	LastError        string    `json:"last_error"`
	DeadLetteredAt   time.Time `json:"dead_lettered_at"`
}

type ReplayRequest struct {
	EventID        uuid.UUID
	ExpectedDigest string
	IdempotencyKey string
	Reason         string
}

type ReplayResult struct {
	ReplayID uuid.UUID `json:"replay_id"`
	EventID  uuid.UUID `json:"event_id"`
	Replayed bool      `json:"replayed"`
}

func (store *PostgresStore) InspectDeadLetter(ctx context.Context, eventID uuid.UUID) (DeadLetterCandidate, error) {
	if eventID == uuid.Nil {
		return DeadLetterCandidate{}, errors.New("event ID is required")
	}
	event, candidate, err := scanDeadLetter(store.pool.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,attempts,COALESCE(last_error,''),dead_lettered_at FROM outbox_events WHERE event_id=$1 AND published_at IS NULL AND dead_lettered_at IS NOT NULL`, eventID))
	if err != nil {
		return DeadLetterCandidate{}, err
	}
	candidate.EventDigest, err = digestEvent(event)
	return candidate, err
}

func (store *PostgresStore) ReplayDeadLetter(ctx context.Context, request ReplayRequest) (ReplayResult, error) {
	request.ExpectedDigest = strings.ToLower(strings.TrimSpace(request.ExpectedDigest))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Reason = strings.TrimSpace(request.Reason)
	identity, ok := serviceauth.FromContext(ctx)
	if request.EventID == uuid.Nil || len(request.ExpectedDigest) != 64 || len(request.IdempotencyKey) < 16 || len(request.IdempotencyKey) > 128 || len(request.Reason) < 20 || !ok || identity.ActorType != "SERVICE" || identity.Service == "" || identity.JobType != "EVENT_REPLAY" || identity.JobRunID == "" {
		return ReplayResult{}, errors.New("replay requires an event digest, stable idempotency key, reason, and EVENT_REPLAY service identity")
	}
	requestHash := replayRequestHash(request, identity)
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReplayResult{}, err
	}
	defer tx.Rollback(ctx)
	var existing ReplayResult
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT replay_id,event_id,request_hash FROM event_replay_requests WHERE idempotency_key=$1`, request.IdempotencyKey).Scan(&existing.ReplayID, &existing.EventID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return ReplayResult{}, errors.New("event replay idempotency key conflicts with another request")
		}
		existing.Replayed = false
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, err
	}
	event, _, err := scanDeadLetter(tx.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,attempts,COALESCE(last_error,''),dead_lettered_at FROM outbox_events WHERE event_id=$1 AND published_at IS NULL AND dead_lettered_at IS NOT NULL FOR UPDATE`, request.EventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, errors.New("event is not currently dead-lettered")
	}
	if err != nil {
		return ReplayResult{}, err
	}
	digest, err := digestEvent(event)
	if err != nil || digest != request.ExpectedDigest {
		return ReplayResult{}, errors.New("dead-letter event digest does not match inspected immutable envelope")
	}
	replayID := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at) VALUES($1,$2,$3,$4,$5,'SERVICE',$6,$7,$8,$9,now())`, replayID, request.EventID, digest, request.IdempotencyKey, requestHash, identity.Service, identity.JobType, identity.JobRunID, request.Reason)
	if err != nil {
		return ReplayResult{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE outbox_events SET dead_lettered_at=NULL,attempts=0,last_error=$2,next_attempt_at=now(),locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND published_at IS NULL AND dead_lettered_at IS NOT NULL`, request.EventID, "audited replay queued: "+replayID.String())
	if err != nil {
		return ReplayResult{}, err
	}
	if command.RowsAffected() != 1 {
		return ReplayResult{}, errors.New("dead-letter event changed before replay was committed")
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplayResult{}, err
	}
	return ReplayResult{ReplayID: replayID, EventID: request.EventID, Replayed: true}, nil
}

type rowScanner interface{ Scan(...any) error }

func scanDeadLetter(row rowScanner) (Event, DeadLetterCandidate, error) {
	var event Event
	var candidate DeadLetterCandidate
	err := row.Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt, &event.Attempts, &candidate.LastError, &candidate.DeadLetteredAt)
	if err != nil {
		return Event{}, DeadLetterCandidate{}, err
	}
	candidate.EventID, candidate.EventType, candidate.AggregateID, candidate.AggregateVersion, candidate.Attempts = event.ID, event.Type, event.AggregateID, event.AggregateVersion, event.Attempts
	return event, candidate, nil
}

func digestEvent(event Event) (string, error) {
	body, err := event.Marshal()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func replayRequestHash(request ReplayRequest, identity serviceauth.Identity) string {
	body, _ := json.Marshal(struct {
		EventID                          uuid.UUID `json:"event_id"`
		Digest, Reason, Service, JobType string
	}{request.EventID, request.ExpectedDigest, request.Reason, identity.Service, identity.JobType})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
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
