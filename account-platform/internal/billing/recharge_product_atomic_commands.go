package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These fields describe existing PAY-53 human commands. They are deliberately
// separate from provider/workload admission callbacks and financial commands.
type rechargeProductAtomicCommand struct {
	CatalogItemID                                    uuid.UUID
	ExpectedVersion                                  int64
	Kind, IdempotencyKey, RequestHash, Actor, Reason string
	Status                                           int
}

// The command key is acquired before any catalogue locks. Replay is read-only
// and precedes current version/admission checks, exactly as the HTTP receipt
// path. New state, the complete response and its audit share one commit.
func executeRechargeProductAtomicCommand[T any](ctx context.Context, r *PostgresRepository, command rechargeProductAtomicCommand, mutate func(pgx.Tx) (T, int64, error)) (T, error) {
	var empty T
	command.IdempotencyKey, command.RequestHash, command.Actor, command.Reason = strings.TrimSpace(command.IdempotencyKey), strings.TrimSpace(command.RequestHash), strings.TrimSpace(command.Actor), strings.TrimSpace(command.Reason)
	if command.CatalogItemID == uuid.Nil || command.ExpectedVersion < 1 || len(command.IdempotencyKey) < 16 || len(command.IdempotencyKey) > 128 || command.RequestHash == "" || command.Actor == "" || command.Reason == "" {
		return empty, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockTransactionKey(ctx, tx, "catalog.command", command.IdempotencyKey); err != nil {
		return empty, err
	}
	var hash string
	var body []byte
	var unexpired bool
	err = tx.QueryRow(ctx, `SELECT request_hash,response_body,expires_at>now() FROM recharge_product_command_records WHERE idempotency_key=$1`, command.IdempotencyKey).Scan(&hash, &body, &unexpired)
	if err == nil {
		if !unexpired || hash != command.RequestHash {
			return empty, ErrConflict
		}
		var replay T
		if err = json.Unmarshal(body, &replay); err != nil {
			return empty, err
		}
		return replay, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	item, after, err := mutate(tx)
	if err != nil {
		return empty, err
	}
	body, err = json.Marshal(item)
	if err != nil {
		return empty, err
	}
	now := r.now()
	if _, err = tx.Exec(ctx, `INSERT INTO recharge_product_command_records(idempotency_key,command_type,request_hash,catalog_item_id,actor_subject,reason,expected_version,response_status,response_body,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, command.IdempotencyKey, command.Kind, command.RequestHash, command.CatalogItemID, command.Actor, command.Reason, command.ExpectedVersion, command.Status, body, now, now.Add(24*time.Hour)); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO recharge_product_audits(audit_id,command_type,catalog_item_id,actor_subject,reason,idempotency_key,request_hash,expected_version,before_version,after_version,result,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,'SUCCEEDED',$10)`, uuid.Must(uuid.NewV7()), command.Kind, command.CatalogItemID, command.Actor, command.Reason, command.IdempotencyKey, command.RequestHash, command.ExpectedVersion, after, now); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return item, nil
}
