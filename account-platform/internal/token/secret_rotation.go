package token

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SecretRotationResult struct {
	ActiveKeyID string
	Rotated     int
	Remaining   int64
}

const secretRotationRemainingSQL = `SELECT
  (SELECT count(*) FROM provider_credentials WHERE key_id<>$1) +
  (SELECT count(*) FROM usage_dispatch_preparations WHERE key_id<>$1) +
  (SELECT count(*) FROM usage_dispatch_intents WHERE key_id IS NOT NULL AND key_id<>$1) +
  (SELECT count(*) FROM usage_stream_events WHERE key_id<>$1) +
  (SELECT count(*) FROM usage_replay_results WHERE key_id<>$1)`

type secretRotationRepository interface {
	RotateSecretKey(context.Context, int) (SecretRotationResult, error)
}

type SecretRotationWorker struct {
	repository secretRotationRepository
	interval   time.Duration
	batchSize  int
}

func NewSecretRotationWorker(repository secretRotationRepository, interval time.Duration, batchSize int) (*SecretRotationWorker, error) {
	if repository == nil || interval < time.Minute || interval > 24*time.Hour || batchSize < 1 || batchSize > 1000 {
		return nil, ErrForbidden
	}
	return &SecretRotationWorker{repository: repository, interval: interval, batchSize: batchSize}, nil
}

func (w *SecretRotationWorker) Run(ctx context.Context) {
	w.rotate(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.rotate(ctx)
		}
	}
}

func (w *SecretRotationWorker) rotate(ctx context.Context) {
	for {
		result, err := w.repository.RotateSecretKey(ctx, w.batchSize)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Error("secret key rotation failed", "error_code", "SECRET_ROTATION_FAILED")
			}
			return
		}
		slog.Info("secret key rotation batch completed", "active_key_id", result.ActiveKeyID, "rotated", result.Rotated, "remaining", result.Remaining)
		if result.Rotated == 0 || result.Remaining == 0 {
			return
		}
	}
}

func (r *PostgresRepository) RotateSecretKey(ctx context.Context, batchSize int) (SecretRotationResult, error) {
	if batchSize < 1 || batchSize > 1000 {
		return SecretRotationResult{}, ErrForbidden
	}
	activeKeyID := r.secrets.KeyID()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SecretRotationResult{}, err
	}
	defer tx.Rollback(ctx)

	remainingBatch := batchSize
	rotated := 0
	steps := []func(context.Context, pgx.Tx, string, int) (int, error){
		r.rotateProviderCredentials,
		r.rotateDispatchPreparations,
		r.rotateDispatchIntents,
		r.rotateStreamEvents,
		r.rotateReplayResults,
	}
	for _, step := range steps {
		if remainingBatch == 0 {
			break
		}
		count, rotateErr := step(ctx, tx, activeKeyID, remainingBatch)
		if rotateErr != nil {
			return SecretRotationResult{}, rotateErr
		}
		rotated += count
		remainingBatch -= count
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretRotationResult{}, err
	}
	remaining, err := r.SecretRotationRemaining(ctx)
	if err != nil {
		return SecretRotationResult{}, err
	}
	return SecretRotationResult{ActiveKeyID: activeKeyID, Rotated: rotated, Remaining: remaining}, nil
}

func (r *PostgresRepository) SecretRotationRemaining(ctx context.Context) (int64, error) {
	var remaining int64
	err := r.pool.QueryRow(ctx, secretRotationRemainingSQL, r.secrets.KeyID()).Scan(&remaining)
	return remaining, err
}

func (r *PostgresRepository) rotateProviderCredentials(ctx context.Context, tx pgx.Tx, activeKeyID string, limit int) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id,token_account_id,ciphertext,key_id FROM provider_credentials WHERE key_id<>$1 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2`, activeKeyID, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		id, accountID uuid.UUID
		ciphertext    []byte
		keyID         string
	}
	items := make([]item, 0, limit)
	for rows.Next() {
		var current item
		if err := rows.Scan(&current.id, &current.accountID, &current.ciphertext, &current.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, current := range items {
		aad := []byte(current.accountID.String() + ":" + current.id.String())
		plaintext, err := r.secrets.DecryptWithKeyID(current.keyID, current.ciphertext, aad)
		if err != nil {
			return 0, err
		}
		ciphertext, err := r.secrets.Encrypt(plaintext, aad)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET ciphertext=$2,key_id=$3 WHERE id=$1 AND key_id=$4`, current.id, ciphertext, activeKeyID, current.keyID); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func (r *PostgresRepository) rotateDispatchPreparations(ctx context.Context, tx pgx.Tx, activeKeyID string, limit int) (int, error) {
	return r.rotateDispatchTable(ctx, tx, "usage_dispatch_preparations", activeKeyID, limit)
}

func (r *PostgresRepository) rotateDispatchIntents(ctx context.Context, tx pgx.Tx, activeKeyID string, limit int) (int, error) {
	return r.rotateDispatchTable(ctx, tx, "usage_dispatch_intents", activeKeyID, limit)
}

func (r *PostgresRepository) rotateDispatchTable(ctx context.Context, tx pgx.Tx, table, activeKeyID string, limit int) (int, error) {
	query := `SELECT usage_request_id,encrypted_envelope,key_id FROM ` + table + ` WHERE encrypted_envelope IS NOT NULL AND key_id<>$1 ORDER BY usage_request_id FOR UPDATE SKIP LOCKED LIMIT $2`
	rows, err := tx.Query(ctx, query, activeKeyID, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		usageID    uuid.UUID
		ciphertext []byte
		keyID      string
	}
	items := make([]item, 0, limit)
	for rows.Next() {
		var current item
		if err := rows.Scan(&current.usageID, &current.ciphertext, &current.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, current := range items {
		aad := replayAAD(current.usageID, "dispatch", 0)
		plaintext, err := r.secrets.DecryptWithKeyID(current.keyID, current.ciphertext, aad)
		if err != nil {
			return 0, err
		}
		ciphertext, err := r.secrets.Encrypt(plaintext, aad)
		if err != nil {
			return 0, err
		}
		update := `UPDATE ` + table + ` SET encrypted_envelope=$2,key_id=$3,envelope_integrity=$4 WHERE usage_request_id=$1 AND key_id=$5`
		if _, err := tx.Exec(ctx, update, current.usageID, ciphertext, activeKeyID, replayDigest(ciphertext), current.keyID); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func (r *PostgresRepository) rotateStreamEvents(ctx context.Context, tx pgx.Tx, activeKeyID string, limit int) (int, error) {
	rows, err := tx.Query(ctx, `SELECT usage_request_id,sequence,event_type,encrypted_payload,key_id FROM usage_stream_events WHERE key_id<>$1 ORDER BY usage_request_id,sequence FOR UPDATE SKIP LOCKED LIMIT $2`, activeKeyID, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		usageID    uuid.UUID
		sequence   int64
		eventType  string
		ciphertext []byte
		keyID      string
	}
	items := make([]item, 0, limit)
	for rows.Next() {
		var current item
		if err := rows.Scan(&current.usageID, &current.sequence, &current.eventType, &current.ciphertext, &current.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, current := range items {
		aad := replayAAD(current.usageID, "event:"+current.eventType, current.sequence)
		plaintext, err := r.secrets.DecryptWithKeyID(current.keyID, current.ciphertext, aad)
		if err != nil {
			return 0, err
		}
		ciphertext, err := r.secrets.Encrypt(plaintext, aad)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE usage_stream_events SET encrypted_payload=$3,key_id=$4,payload_integrity=$5 WHERE usage_request_id=$1 AND sequence=$2 AND key_id=$6`, current.usageID, current.sequence, ciphertext, activeKeyID, replayDigest(ciphertext), current.keyID); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func (r *PostgresRepository) rotateReplayResults(ctx context.Context, tx pgx.Tx, activeKeyID string, limit int) (int, error) {
	rows, err := tx.Query(ctx, `SELECT usage_request_id,response_kind,last_sequence,encrypted_payload,key_id FROM usage_replay_results WHERE key_id<>$1 ORDER BY usage_request_id FOR UPDATE SKIP LOCKED LIMIT $2`, activeKeyID, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		usageID    uuid.UUID
		kind       string
		sequence   int64
		ciphertext []byte
		keyID      string
	}
	items := make([]item, 0, limit)
	for rows.Next() {
		var current item
		if err := rows.Scan(&current.usageID, &current.kind, &current.sequence, &current.ciphertext, &current.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, current := range items {
		aad := replayAAD(current.usageID, "result:"+current.kind, current.sequence)
		plaintext, err := r.secrets.DecryptWithKeyID(current.keyID, current.ciphertext, aad)
		if err != nil {
			return 0, err
		}
		ciphertext, err := r.secrets.Encrypt(plaintext, aad)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE usage_replay_results SET encrypted_payload=$2,key_id=$3,payload_integrity=$4 WHERE usage_request_id=$1 AND key_id=$5`, current.usageID, ciphertext, activeKeyID, replayDigest(ciphertext), current.keyID); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}
