package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	streamRetention    = 24 * time.Hour
	resultRetention    = 30 * 24 * time.Hour
	tombstoneRetention = 90 * 24 * time.Hour
)

func replayDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func replayAAD(usageID uuid.UUID, kind string, sequence int64) []byte {
	return []byte(fmt.Sprintf("usage-replay:%s:%s:%d", usageID, kind, sequence))
}

func (r *PostgresRepository) transitionExecution(ctx context.Context, usageID uuid.UUID, allowed []string, target, reason string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := transitionExecutionTx(ctx, tx, usageID, allowed, target, reason, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func transitionExecutionTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, allowed []string, target, reason string, now time.Time) error {
	var current string
	var version int64
	if err := tx.QueryRow(ctx, `SELECT execution_status,execution_version FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&current, &version); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if current == target {
		return nil
	}
	permitted := false
	for _, item := range allowed {
		if current == item {
			permitted = true
			break
		}
	}
	if !permitted {
		return ErrForbidden
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET execution_status=$3,execution_version=execution_version+1,updated_at=$4 WHERE id=$1 AND execution_version=$2`, usageID, version, target, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_execution_transitions(id,usage_request_id,from_status,to_status,from_version,to_version,reason,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), usageID, current, target, version, version+1, strings.TrimSpace(reason), now)
	return err
}

func (r *PostgresRepository) PrepareDispatch(ctx context.Context, usageID uuid.UUID, envelope DispatchEnvelope) error {
	if usageID == uuid.Nil || strings.TrimSpace(envelope.BaseURL) == "" || len(envelope.Credential) == 0 || len(envelope.RequestBody) == 0 || envelope.Price.ID == uuid.Nil || envelope.MaximumCost < 0 {
		return ErrForbidden
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	ciphertext, err := r.secrets.Encrypt(payload, replayAAD(usageID, "dispatch", 0))
	if err != nil {
		return err
	}
	command, err := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_preparations(usage_request_id,encrypted_envelope,key_id,envelope_integrity,created_at) SELECT $1,$2,$3,$4,$5 FROM usage_requests WHERE id=$1 AND execution_status='RECEIVED' ON CONFLICT(usage_request_id) DO NOTHING`, usageID, ciphertext, r.secrets.KeyID(), replayDigest(ciphertext), r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_dispatch_preparations WHERE usage_request_id=$1)`, usageID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrForbidden
		}
	}
	return nil
}

func (r *PostgresRepository) LoadDispatchPreparation(ctx context.Context, usageID uuid.UUID) (DispatchEnvelope, error) {
	var ciphertext []byte
	var keyID, integrity string
	if err := r.pool.QueryRow(ctx, `SELECT encrypted_envelope,key_id,envelope_integrity FROM usage_dispatch_preparations WHERE usage_request_id=$1`, usageID).Scan(&ciphertext, &keyID, &integrity); errors.Is(err, pgx.ErrNoRows) {
		return DispatchEnvelope{}, ErrNotFound
	} else if err != nil {
		return DispatchEnvelope{}, err
	}
	if keyID != r.secrets.KeyID() || replayDigest(ciphertext) != integrity {
		return DispatchEnvelope{}, ErrInvalidSecret
	}
	plaintext, err := r.secrets.Decrypt(ciphertext, replayAAD(usageID, "dispatch", 0))
	if err != nil {
		return DispatchEnvelope{}, err
	}
	defer clear(plaintext)
	var envelope DispatchEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return DispatchEnvelope{}, err
	}
	return envelope, nil
}

func (r *PostgresRepository) CompleteHoldAndDispatch(ctx context.Context, usageID, holdID, priceVersionID uuid.UUID) error {
	if usageID == uuid.Nil || holdID == uuid.Nil || priceVersionID == uuid.Nil {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var status, cancelStatus string
	if err := tx.QueryRow(ctx, `SELECT execution_status,cancel_status FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status, &cancelStatus); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status == "DISPATCH_INTENT_PERSISTED" || status == "UPSTREAM_DISPATCHING" || status == "STREAMING" || status == "SETTLEMENT_PENDING" || status == "SUCCEEDED" || status == "REVIEW" {
		return tx.Commit(ctx)
	}
	if status != "HOLD_RESERVING" {
		return ErrForbidden
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',settlement_status='HOLD_ACTIVE',hold_id=$2,price_version_id=$3,updated_at=$4 WHERE id=$1 AND status='CREATED'`, usageID, holdID, priceVersionID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"HOLD_RESERVING"}, "HOLD_RESERVED", "billing ensured one hold", now); err != nil {
		return err
	}
	if cancelStatus == "CANCEL_REQUESTED" {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrCancellationRequested
	}
	command, err = tx.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,encrypted_envelope,key_id,envelope_integrity,upstream_idempotency_key,adapter_supports_idempotency,created_at) SELECT usage_request_id,encrypted_envelope,key_id,envelope_integrity,$2,false,$3 FROM usage_dispatch_preparations WHERE usage_request_id=$1 ON CONFLICT(usage_request_id) DO NOTHING`, usageID, "pinvou-"+usageID.String(), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"HOLD_RESERVED"}, "DISPATCH_INTENT_PERSISTED", "prepared dispatch promoted after hold", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_dispatch_preparations WHERE usage_request_id=$1`, usageID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) PersistDispatchIntent(ctx context.Context, usageID uuid.UUID, envelope DispatchEnvelope) error {
	if usageID == uuid.Nil || strings.TrimSpace(envelope.BaseURL) == "" || len(envelope.Credential) == 0 || len(envelope.RequestBody) == 0 || envelope.Price.ID == uuid.Nil || envelope.MaximumCost < 0 {
		return ErrForbidden
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	ciphertext, err := r.secrets.Encrypt(payload, replayAAD(usageID, "dispatch", 0))
	if err != nil {
		return err
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,encrypted_envelope,key_id,envelope_integrity,upstream_idempotency_key,adapter_supports_idempotency,created_at) VALUES($1,$2,$3,$4,$5,false,$6) ON CONFLICT(usage_request_id) DO NOTHING`, usageID, ciphertext, r.secrets.KeyID(), replayDigest(ciphertext), "pinvou-"+usageID.String(), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var storedIntegrity string
		if err := tx.QueryRow(ctx, `SELECT envelope_integrity FROM usage_dispatch_intents WHERE usage_request_id=$1`, usageID).Scan(&storedIntegrity); err != nil {
			return err
		}
		// AES-GCM uses a random nonce, so an existing immutable intent is accepted only by state,
		// never overwritten with values resolved by a later context.
		if strings.TrimSpace(storedIntegrity) == "" {
			return ErrForbidden
		}
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"HOLD_RESERVED"}, "DISPATCH_INTENT_PERSISTED", "encrypted dispatch intent persisted", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AcquireDispatch(ctx context.Context, usageID uuid.UUID, owner string, lease time.Duration) (DispatchEnvelope, bool, error) {
	owner = strings.TrimSpace(owner)
	if usageID == uuid.Nil || owner == "" || lease < 10*time.Second || lease > 10*time.Minute {
		return DispatchEnvelope{}, false, ErrForbidden
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return DispatchEnvelope{}, false, err
	}
	defer tx.Rollback(ctx)
	var ciphertext []byte
	var keyID string
	var acquiredAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT encrypted_envelope,key_id,worker_acquired_at FROM usage_dispatch_intents WHERE usage_request_id=$1 FOR UPDATE`, usageID).Scan(&ciphertext, &keyID, &acquiredAt); errors.Is(err, pgx.ErrNoRows) {
		return DispatchEnvelope{}, false, ErrNotFound
	} else if err != nil {
		return DispatchEnvelope{}, false, err
	}
	if acquiredAt != nil {
		return DispatchEnvelope{}, false, tx.Commit(ctx)
	}
	if keyID != r.secrets.KeyID() {
		return DispatchEnvelope{}, false, ErrInvalidSecret
	}
	plaintext, err := r.secrets.Decrypt(ciphertext, replayAAD(usageID, "dispatch", 0))
	if err != nil {
		return DispatchEnvelope{}, false, err
	}
	defer clear(plaintext)
	var envelope DispatchEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return DispatchEnvelope{}, false, err
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"DISPATCH_INTENT_PERSISTED"}, "UPSTREAM_DISPATCHING", "dispatch worker acquired immutable intent", now); err != nil {
		return DispatchEnvelope{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_dispatch_intents SET worker_lease_owner=$2,worker_lease_until=$3,worker_acquired_at=$4,encrypted_envelope=NULL,key_id=NULL,envelope_integrity=NULL WHERE usage_request_id=$1 AND worker_acquired_at IS NULL`, usageID, owner, now.Add(lease), now); err != nil {
		return DispatchEnvelope{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DispatchEnvelope{}, false, err
	}
	return envelope, true, nil
}

func (r *PostgresRepository) MarkStreaming(ctx context.Context, usageID uuid.UUID) error {
	return r.transitionExecution(ctx, usageID, []string{"UPSTREAM_DISPATCHING"}, "STREAMING", "upstream stream accepted")
}

func (r *PostgresRepository) MarkHoldReserving(ctx context.Context, usageID uuid.UUID) error {
	return r.transitionExecution(ctx, usageID, []string{"RECEIVED"}, "HOLD_RESERVING", "billing hold reservation started")
}

func (r *PostgresRepository) MarkResponseReceived(ctx context.Context, usageID uuid.UUID) error {
	if err := r.transitionExecution(ctx, usageID, []string{"UPSTREAM_DISPATCHING", "STREAMING"}, "SETTLEMENT_PENDING", "complete upstream response persisted"); err != nil {
		return err
	}
	_, err := r.pool.Exec(ctx, `UPDATE usage_requests SET settlement_status='USAGE_PENDING',provider_terminal_at=COALESCE(provider_terminal_at,$2),updated_at=$2 WHERE id=$1`, usageID, r.now())
	return err
}

func (r *PostgresRepository) AppendReplayEvent(ctx context.Context, usageID uuid.UUID, eventType string, payload []byte) (int64, error) {
	eventType = strings.ToUpper(strings.TrimSpace(eventType))
	if usageID == uuid.Nil || eventType == "" || len(payload) == 0 {
		return 0, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var status string
	if err := tx.QueryRow(ctx, `SELECT execution_status FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status); err != nil {
		return 0, err
	}
	if status == "UPSTREAM_DISPATCHING" {
		if err := transitionExecutionTx(ctx, tx, usageID, []string{"UPSTREAM_DISPATCHING"}, "STREAMING", "first normalized stream event persisted", r.now()); err != nil {
			return 0, err
		}
	} else if status != "STREAMING" {
		return 0, ErrForbidden
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0)+1 FROM usage_stream_events WHERE usage_request_id=$1`, usageID).Scan(&sequence); err != nil {
		return 0, err
	}
	ciphertext, err := r.secrets.Encrypt(payload, replayAAD(usageID, "event:"+eventType, sequence))
	if err != nil {
		return 0, err
	}
	now := r.now()
	_, err = tx.Exec(ctx, `INSERT INTO usage_stream_events(usage_request_id,sequence,event_type,encrypted_payload,key_id,payload_integrity,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULL)`, usageID, sequence, eventType, ciphertext, r.secrets.KeyID(), replayDigest(ciphertext), now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET last_persisted_sequence=$2,updated_at=$3 WHERE id=$1`, usageID, sequence, now); err != nil {
		return 0, err
	}
	return sequence, tx.Commit(ctx)
}

func (r *PostgresRepository) SaveReplayResult(ctx context.Context, usageID uuid.UUID, result ReplayResult) error {
	if usageID == uuid.Nil || (result.Kind != "JSON" && result.Kind != "STREAM_AGGREGATE") || result.HTTPStatus < 100 || result.HTTPStatus > 599 || len(result.Payload) == 0 || result.LastSequence < 0 {
		return ErrForbidden
	}
	ciphertext, err := r.secrets.Encrypt(result.Payload, replayAAD(usageID, "result:"+result.Kind, result.LastSequence))
	if err != nil {
		return err
	}
	var finalUsage any
	if result.FinalUsage != nil {
		finalUsage = result.FinalUsage
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO usage_replay_results(usage_request_id,response_kind,http_status,content_type,encrypted_payload,key_id,payload_integrity,final_usage,last_sequence,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL) ON CONFLICT(usage_request_id) DO NOTHING`, usageID, result.Kind, result.HTTPStatus, result.ContentType, ciphertext, r.secrets.KeyID(), replayDigest(ciphertext), finalUsage, result.LastSequence, now)
	if err != nil {
		return err
	}
	if result.FinalUsage != nil {
		encodedUsage, marshalErr := json.Marshal(result.FinalUsage)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err = tx.Exec(ctx, `UPDATE usage_requests SET trusted_usage_snapshot=$2,trusted_usage_source='PROVIDER_FINAL_USAGE',final_usage_observed_at=COALESCE(final_usage_observed_at,$3),provider_terminal_at=COALESCE(provider_terminal_at,$3),settlement_status='USAGE_PENDING',updated_at=$3 WHERE id=$1`, usageID, encodedUsage, now); err != nil {
			return err
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}

func (r *PostgresRepository) LoadReplay(ctx context.Context, usageID uuid.UUID, after int64) (ReplaySnapshot, error) {
	usage, err := r.GetUsage(ctx, usageID)
	if err != nil {
		return ReplaySnapshot{}, err
	}
	snapshot := ReplaySnapshot{Usage: usage}
	now := r.now()
	rows, err := r.pool.Query(ctx, `SELECT sequence,event_type,encrypted_payload,key_id,payload_integrity FROM usage_stream_events WHERE usage_request_id=$1 AND sequence>$2 AND (expires_at IS NULL OR expires_at>$3) ORDER BY sequence`, usageID, after, now)
	if err != nil {
		return ReplaySnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var event ReplayEvent
		var ciphertext []byte
		var keyID, integrity string
		if err := rows.Scan(&event.Sequence, &event.EventType, &ciphertext, &keyID, &integrity); err != nil {
			return ReplaySnapshot{}, err
		}
		if keyID != r.secrets.KeyID() || replayDigest(ciphertext) != integrity {
			return ReplaySnapshot{}, ErrInvalidSecret
		}
		event.Payload, err = r.secrets.Decrypt(ciphertext, replayAAD(usageID, "event:"+event.EventType, event.Sequence))
		if err != nil {
			return ReplaySnapshot{}, err
		}
		snapshot.Events = append(snapshot.Events, event)
	}
	if err := rows.Err(); err != nil {
		return ReplaySnapshot{}, err
	}
	var result ReplayResult
	var ciphertext []byte
	var keyID, integrity string
	var finalUsage []byte
	var resultExpiresAt *time.Time
	err = r.pool.QueryRow(ctx, `SELECT response_kind,http_status,content_type,encrypted_payload,key_id,payload_integrity,final_usage,last_sequence,expires_at FROM usage_replay_results WHERE usage_request_id=$1 AND (expires_at IS NULL OR expires_at>$2)`, usageID, now).Scan(&result.Kind, &result.HTTPStatus, &result.ContentType, &ciphertext, &keyID, &integrity, &finalUsage, &result.LastSequence, &resultExpiresAt)
	if err == nil {
		if resultExpiresAt != nil {
			result.ExpiresAt = *resultExpiresAt
		}
		if keyID != r.secrets.KeyID() || replayDigest(ciphertext) != integrity {
			return ReplaySnapshot{}, ErrInvalidSecret
		}
		result.Payload, err = r.secrets.Decrypt(ciphertext, replayAAD(usageID, "result:"+result.Kind, result.LastSequence))
		if err != nil {
			return ReplaySnapshot{}, err
		}
		if len(finalUsage) > 0 && string(finalUsage) != "null" {
			var parsed Usage
			if json.Unmarshal(finalUsage, &parsed) == nil {
				result.FinalUsage = &parsed
			}
		}
		snapshot.Result = &result
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ReplaySnapshot{}, err
	}
	var tombstoneUntil time.Time
	if err := r.pool.QueryRow(ctx, `SELECT expires_at FROM usage_idempotency_tombstones WHERE usage_request_id=$1 AND expires_at>$2`, usageID, now).Scan(&tombstoneUntil); err == nil {
		snapshot.TombstoneUntil = &tombstoneUntil
		snapshot.ResultExpired = snapshot.Result == nil && len(snapshot.Events) == 0
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ReplaySnapshot{}, err
	}
	return snapshot, nil
}

func (r *PostgresRepository) MarkDelivery(ctx context.Context, usageID uuid.UUID, status string) error {
	status = strings.ToUpper(strings.TrimSpace(status))
	switch status {
	case "NOT_STARTED", "DELIVERING", "DISCONNECTED", "DELIVERED", "USER_STOPPED":
	default:
		return ErrForbidden
	}
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET delivery_status=$2,user_stopped_at=CASE WHEN $2='USER_STOPPED' THEN COALESCE(user_stopped_at,$3) ELSE user_stopped_at END,updated_at=$3 WHERE id=$1 AND (delivery_status<>'USER_STOPPED' OR $2='USER_STOPPED')`, usageID, status, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_requests WHERE id=$1)`, usageID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func (r *PostgresRepository) RecordDeliveryConfirmation(ctx context.Context, usageID uuid.UUID, deliveryID string, sequence int64) error {
	deliveryID = strings.TrimSpace(deliveryID)
	if usageID == uuid.Nil || deliveryID == "" || sequence < 0 {
		return ErrForbidden
	}
	digest := replayDigest([]byte(deliveryID))
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_delivery_confirmations(usage_request_id,delivery_id_digest,last_confirmed_sequence,confirmed_at) VALUES($1,$2,$3,$4) ON CONFLICT(usage_request_id,delivery_id_digest) DO UPDATE SET last_confirmed_sequence=GREATEST(usage_delivery_confirmations.last_confirmed_sequence,EXCLUDED.last_confirmed_sequence),confirmed_at=EXCLUDED.confirmed_at`, usageID, digest, sequence, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET last_client_confirmed_sequence=GREATEST(last_client_confirmed_sequence,$2),updated_at=$3 WHERE id=$1`, usageID, sequence, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func addReplayTombstone(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, terminal string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE usage_stream_events SET expires_at=$2 WHERE usage_request_id=$1 AND expires_at IS NULL`, usageID, now.Add(streamRetention)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_replay_results SET expires_at=$2 WHERE usage_request_id=$1 AND expires_at IS NULL`, usageID, now.Add(resultRetention)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO usage_idempotency_tombstones(usage_request_id,user_id,idempotency_namespace,idempotency_key_digest,canonicalization_version,request_fingerprint,terminal_status,result_summary,charge_references,created_at,expires_at) SELECT id,user_id,idempotency_namespace,idempotency_key_digest,canonicalization_version,request_fingerprint,$2,jsonb_build_object('has_result',EXISTS(SELECT 1 FROM usage_replay_results result WHERE result.usage_request_id=usage_requests.id)),jsonb_build_object('usage_request_id',id,'hold_id',hold_id,'price_version_id',price_version_id,'actual_microcredits',actual_microcredits),$3,$4 FROM usage_requests WHERE id=$1 AND idempotency_key_digest IS NOT NULL ON CONFLICT(usage_request_id) DO NOTHING`, usageID, terminal, now, now.Add(tombstoneRetention))
	return err
}

func (r *PostgresRepository) CleanupReplayRetention(ctx context.Context) error {
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	caseGuard := `NOT EXISTS(SELECT 1 FROM usage_replay_retention_cases retention WHERE retention.usage_request_id=%s AND retention.released_at IS NULL AND retention.expires_at>$1)`
	if _, err := tx.Exec(ctx, `DELETE FROM usage_stream_events event WHERE expires_at<=$1 AND `+fmt.Sprintf(caseGuard, "event.usage_request_id"), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_replay_results result WHERE expires_at<=$1 AND `+fmt.Sprintf(caseGuard, "result.usage_request_id"), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests request SET idempotency_key_digest=NULL,updated_at=$1 FROM usage_idempotency_tombstones tombstone WHERE tombstone.usage_request_id=request.id AND tombstone.expires_at<=$1 AND `+fmt.Sprintf(caseGuard, "request.id"), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_idempotency_tombstones tombstone WHERE expires_at<=$1 AND `+fmt.Sprintf(caseGuard, "tombstone.usage_request_id"), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_replay_retention_cases WHERE (released_at IS NOT NULL AND clear_by<=$1) OR expires_at<=$1`, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CreateReplayRetentionCase(ctx context.Context, usageID, ownerID uuid.UUID, reason, scope string, reviewAt, expiresAt time.Time) (uuid.UUID, error) {
	reason, scope = strings.TrimSpace(reason), strings.TrimSpace(scope)
	now := r.now()
	if usageID == uuid.Nil || ownerID == uuid.Nil || reason == "" || scope == "" || !reviewAt.After(now) || !expiresAt.After(reviewAt) {
		return uuid.Nil, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	caseID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO usage_replay_retention_cases(id,usage_request_id,reason,scope,owner_id,review_at,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, caseID, usageID, reason, scope, ownerID, reviewAt.UTC(), expiresAt.UTC(), now); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_replay_case_payloads(retention_case_id,encrypted_events,encrypted_result,moved_at)
SELECT $1,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('sequence',sequence,'event_type',event_type,'ciphertext',encode(encrypted_payload,'base64'),'key_id',key_id,'integrity',payload_integrity,'created_at',created_at) ORDER BY sequence) FROM usage_stream_events WHERE usage_request_id=$2),'[]'::jsonb),
       (SELECT jsonb_build_object('response_kind',response_kind,'http_status',http_status,'content_type',content_type,'ciphertext',encode(encrypted_payload,'base64'),'key_id',key_id,'integrity',payload_integrity,'final_usage',final_usage,'last_sequence',last_sequence,'created_at',created_at) FROM usage_replay_results WHERE usage_request_id=$2),
       $3`, caseID, usageID, now); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_stream_events WHERE usage_request_id=$1`, usageID); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_replay_results WHERE usage_request_id=$1`, usageID); err != nil {
		return uuid.Nil, err
	}
	return caseID, tx.Commit(ctx)
}

func (r *PostgresRepository) ReleaseReplayRetentionCase(ctx context.Context, caseID uuid.UUID) error {
	if caseID == uuid.Nil {
		return ErrForbidden
	}
	now := r.now()
	command, err := r.pool.Exec(ctx, `UPDATE usage_replay_retention_cases SET released_at=$2,clear_by=LEAST(expires_at,$3) WHERE id=$1 AND released_at IS NULL`, caseID, now, now.Add(30*24*time.Hour))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) RecoverUncertainDispatches(ctx context.Context) error {
	now := r.now()
	rows, err := r.pool.Query(ctx, `SELECT request.id FROM usage_requests request JOIN usage_dispatch_intents intent ON intent.usage_request_id=request.id WHERE request.execution_status='UPSTREAM_DISPATCHING' AND intent.worker_lease_until<=$1 AND intent.adapter_supports_idempotency=false`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.MarkReview(ctx, id, "dispatch_result_unknown", map[string]any{"automatic_redispatch": false}); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) AnonymizeUsageReplay(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM usage_stream_events event USING usage_requests request WHERE event.usage_request_id=request.id AND request.user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_replay_results result USING usage_requests request WHERE result.usage_request_id=request.id AND request.user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_dispatch_preparations preparation USING usage_requests request WHERE preparation.usage_request_id=request.id AND request.user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_dispatch_intents intent SET encrypted_envelope=NULL,key_id=NULL,envelope_integrity=NULL FROM usage_requests request WHERE intent.usage_request_id=request.id AND request.user_id=$1`, userID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET idempotency_key_digest=NULL WHERE user_id=$1`, userID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
