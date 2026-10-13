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

func (r *PostgresRepository) MarkHoldReserving(ctx context.Context, usageID uuid.UUID) error {
	return r.transitionExecution(ctx, usageID, []string{"RECEIVED"}, "HOLD_RESERVING", "billing hold reservation started")
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

func (r *PostgresRepository) CompleteCancellationHold(ctx context.Context, usageID, holdID, priceVersionID uuid.UUID) error {
	if usageID == uuid.Nil || holdID == uuid.Nil || priceVersionID == uuid.Nil {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockReplayUsageTx(ctx, tx, usageID); err != nil {
		return err
	}
	now := r.now()
	var status, cancelStatus string
	if err := tx.QueryRow(ctx, `SELECT execution_status,cancel_status FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status, &cancelStatus); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status == "DISPATCH_INTENT_PERSISTED" || status == "UPSTREAM_DISPATCHING" || status == "STREAMING" || status == "SETTLEMENT_PENDING" || status == "SUCCEEDED" {
		return tx.Commit(ctx)
	}
	if (status != "HOLD_RESERVING" && status != "REVIEW") || cancelStatus != "CANCEL_REQUESTED" {
		return ErrForbidden
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',settlement_status='HOLD_ACTIVE',hold_id=$2,price_version_id=$3,updated_at=$4 WHERE id=$1 AND (status='CREATED' OR (status='REVIEW' AND error_class='cancel_hold_recovery_unconfirmed' AND cancel_status='CANCEL_REQUESTED' AND hold_id IS NULL AND NOT EXISTS(SELECT 1 FROM usage_dispatch_intents intent WHERE intent.usage_request_id=usage_requests.id) AND EXISTS(SELECT 1 FROM usage_cancellation_evidence evidence WHERE evidence.usage_request_id=usage_requests.id AND evidence.evidence_type IN ('CANCEL_REQUESTED','MODEL_EMERGENCY_REQUESTED') AND evidence.sanitized_evidence->>'pre_dispatch'='true')))`, usageID, holdID, priceVersionID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"HOLD_RESERVING", "REVIEW"}, "HOLD_RESERVED", "billing ensured one hold", now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrCancellationRequested
}

func lockReplayUsageTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID) error {
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM usage_requests WHERE id=$1`, usageID).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return lockReplayUserTx(ctx, tx, userID)
}

func replayAAD(usageID uuid.UUID, kind string, sequence int64) []byte {
	return []byte(fmt.Sprintf("usage-replay:%s:%s:%d", usageID, kind, sequence))
}

func replayDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
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

const (
	streamRetention    = 24 * time.Hour
	resultRetention    = 30 * 24 * time.Hour
	tombstoneRetention = 90 * 24 * time.Hour
)

func lockReplayUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, "token.replay.user:"+userID.String()); err != nil {
		return err
	}
	// F04 has no anonymization producer or marker storage. The public
	// stage verifier rejects later histories; F08 restores its marker fence
	// together with the actual cleanup producer and immutable migration.
	return nil
}
