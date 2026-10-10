package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

type DeadLetterCandidate struct {
	EventID          uuid.UUID `json:"event_id"`
	EventType        string    `json:"event_type"`
	AggregateID      uuid.UUID `json:"aggregate_id"`
	AggregateVersion int64     `json:"aggregate_version"`
	EventDigest      string    `json:"event_digest"`
	Attempts         int       `json:"attempts"`
	LastError        string    `json:"last_error"`
	DeadLetteredAt   time.Time `json:"dead_lettered_at"`
	Replayable       bool      `json:"replayable"`
}

var ErrAuthorizationEventReplayForbidden = errors.New("AUTHORIZATION_EVENT_REPLAY_FORBIDDEN")

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
	if event.Type == "account.approval-command.authorized.v1" {
		candidate.Replayable = false
		candidate.LastError = ""
		err = store.pool.QueryRow(ctx, `SELECT event_digest FROM public.inspect_authorization_dead_letter($1)`, eventID).Scan(&candidate.EventDigest)
		return candidate, err
	}
	candidate.Replayable = true
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
	// Authorization events are permanently non-replayable. Resolve this from
	// the persisted event before consulting an old idempotency record, so a
	// prior generic result can never bypass the prohibition.
	var persistedEventType string
	if err := tx.QueryRow(ctx, `SELECT event_type FROM outbox_events WHERE event_id=$1`, request.EventID).Scan(&persistedEventType); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, err
	} else if err == nil && persistedEventType == "account.approval-command.authorized.v1" {
		return ReplayResult{}, ErrAuthorizationEventReplayForbidden
	}
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
	command, err := tx.Exec(ctx, `UPDATE outbox_events SET dead_lettered_at=NULL,publication_generation=publication_generation+1,attempts=0,last_error=$2,next_attempt_at=now(),locked_by=NULL,locked_until=NULL WHERE event_id=$1 AND published_at IS NULL AND dead_lettered_at IS NOT NULL`, request.EventID, "audited replay queued: "+replayID.String())
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
