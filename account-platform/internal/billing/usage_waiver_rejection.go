package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A definitive, persisted Token rejection retires only this original pending
// claim. HTTP failures and unknown replies are never rejection authority.
func (r *PostgresRepository) RecordUsageWaiverRejection(ctx context.Context, c usagewaiver.Command, decision usagewaiver.Decision) error {
	if !decision.ValidFor(c) || decision.Admission != usagewaiver.Rejected {
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	original, err := readWaiverCommand(ctx, tx, c.Reference)
	if err != nil {
		return err
	}
	if original.Digest != c.Digest {
		return ErrConflict
	}
	if _, err := getWallet(ctx, tx, c.WalletID, true); err != nil {
		return err
	}
	h, err := getHoldByUsage(ctx, tx, c.UsageID, true)
	if err != nil {
		return err
	}
	if h.ID != c.HoldID {
		return ErrConflict
	}
	raw, _ := json.Marshal(decision)
	var prior []byte
	err = tx.QueryRow(ctx, `SELECT decision_snapshot FROM usage_waiver_rejections WHERE command_reference=$1`, c.Reference).Scan(&prior)
	if err == nil {
		var equal bool
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb=$2::jsonb`, prior, raw).Scan(&equal); err != nil {
			return err
		}
		if !equal {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var claim uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT command_reference FROM usage_waiver_hold_claims WHERE hold_id=$1 FOR UPDATE`, c.HoldID).Scan(&claim); err != nil {
		return err
	}
	if claim != c.Reference {
		return ErrConflict
	}
	var finished bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_waiver_results WHERE command_reference=$1)`, c.Reference).Scan(&finished); err != nil {
		return err
	}
	if finished {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_rejections(command_reference,decision_snapshot,created_at) VALUES($1,$2,$3)`, c.Reference, raw, r.now()); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM usage_waiver_hold_claims WHERE hold_id=$1 AND command_reference=$2`, c.HoldID, c.Reference)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}
