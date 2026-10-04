package billing

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) RequestAdjustment(ctx context.Context, walletID uuid.UUID, amount int64, reason string, requestedBy uuid.UUID) (ManualAdjustment, error) {
	reason = strings.TrimSpace(reason)
	if walletID == uuid.Nil || requestedBy == uuid.Nil || amount == 0 || amount == math.MinInt64 || reason == "" {
		return ManualAdjustment{}, ErrConflict
	}
	wallet, err := getWallet(ctx, r.pool, walletID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualAdjustment{}, ErrNotFound
	}
	if err != nil {
		return ManualAdjustment{}, err
	}
	if wallet.Status != "ACTIVE" {
		return ManualAdjustment{}, ErrWalletUnavailable
	}
	now := r.now()
	adjustment := ManualAdjustment{
		ID:                 uuid.Must(uuid.NewV7()),
		WalletID:           walletID,
		AmountMicrocredits: amount,
		Reason:             reason,
		RequestedBy:        requestedBy,
		Status:             "PENDING_APPROVAL",
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO manual_adjustments(id,wallet_id,amount_microcredits,reason,requested_by,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'PENDING_APPROVAL',$6,$6)`, adjustment.ID, walletID, amount, reason, requestedBy, now)
	return adjustment, err
}

func (r *PostgresRepository) ApproveAdjustment(ctx context.Context, adjustmentID, approvedBy uuid.UUID, decisionReason string) (ManualAdjustment, error) {
	decisionReason = strings.TrimSpace(decisionReason)
	if adjustmentID == uuid.Nil || approvedBy == uuid.Nil || decisionReason == "" {
		return ManualAdjustment{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ManualAdjustment{}, err
	}
	defer tx.Rollback(ctx)
	adjustment, err := getAdjustment(ctx, tx, adjustmentID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualAdjustment{}, ErrNotFound
	}
	if err != nil {
		return ManualAdjustment{}, err
	}
	if adjustment.Status == "POSTED" && adjustment.ApprovedBy != nil && *adjustment.ApprovedBy == approvedBy {
		return adjustment, tx.Commit(ctx)
	}
	if err := validateAdjustmentDecision(adjustment, approvedBy, decisionReason); err != nil {
		return ManualAdjustment{}, ErrConflict
	}
	wallet, err := getWallet(ctx, tx, adjustment.WalletID, true)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if wallet.Status != "ACTIVE" {
		return ManualAdjustment{}, ErrWalletUnavailable
	}
	now := r.now()
	entryID := uuid.Must(uuid.NewV7())
	if err := insertJournal(ctx, tx, entryID, "ADJUSTMENT", "adjustment:"+adjustment.ID.String(), adjustment.Reason, now); err != nil {
		return ManualAdjustment{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, adjustment.WalletID)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if adjustment.AmountMicrocredits > 0 {
		if wallet.PostedMicrocredits > math.MaxInt64-adjustment.AmountMicrocredits {
			return ManualAdjustment{}, ErrConflict
		}
		lotID := uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,created_at) VALUES($1,$2,'ADJUSTMENT',$3,$4,$4,false,$5)`, lotID, adjustment.WalletID, "adjustment:"+adjustment.ID.String(), adjustment.AmountMicrocredits, now)
		if err == nil {
			err = insertPosting(ctx, tx, entryID, walletAccount, adjustment.AmountMicrocredits, &lotID, now)
		}
		if err == nil {
			err = insertPosting(ctx, tx, entryID, adjustmentAccount, -adjustment.AmountMicrocredits, nil, now)
		}
	} else {
		debit := -adjustment.AmountMicrocredits
		if wallet.AvailableMicrocredits < debit {
			return ManualAdjustment{}, ErrInsufficientBalance
		}
		err = consumeAdjustmentLots(ctx, tx, entryID, walletAccount, adjustment.WalletID, debit, now)
		if err == nil {
			err = insertPosting(ctx, tx, entryID, adjustmentAccount, debit, nil, now)
		}
	}
	if err != nil {
		return ManualAdjustment{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits+$2 >= 0`, adjustment.WalletID, adjustment.AmountMicrocredits, now)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() != 1 {
		return ManualAdjustment{}, ErrInsufficientBalance
	}
	command, err = tx.Exec(ctx, `UPDATE manual_adjustments SET approved_by=$2,status='POSTED',journal_entry_id=$3,decision_reason=$4,decided_at=$5,updated_at=$5 WHERE id=$1 AND status='PENDING_APPROVAL'`, adjustment.ID, approvedBy, entryID, decisionReason, now)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() != 1 {
		return ManualAdjustment{}, ErrConflict
	}
	adjustment.ApprovedBy = &approvedBy
	adjustment.Status = "POSTED"
	adjustment.JournalEntryID = &entryID
	adjustment.DecisionReason = &decisionReason
	adjustment.DecidedAt = &now
	adjustment.UpdatedAt = now
	return adjustment, tx.Commit(ctx)
}

func (r *PostgresRepository) RejectAdjustment(ctx context.Context, adjustmentID, rejectedBy uuid.UUID, decisionReason string) (ManualAdjustment, error) {
	decisionReason = strings.TrimSpace(decisionReason)
	if adjustmentID == uuid.Nil || rejectedBy == uuid.Nil || decisionReason == "" {
		return ManualAdjustment{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ManualAdjustment{}, err
	}
	defer tx.Rollback(ctx)
	adjustment, err := getAdjustment(ctx, tx, adjustmentID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualAdjustment{}, ErrNotFound
	}
	if err != nil {
		return ManualAdjustment{}, err
	}
	if adjustment.Status == "REJECTED" && adjustment.RejectedBy != nil && *adjustment.RejectedBy == rejectedBy {
		return adjustment, tx.Commit(ctx)
	}
	if err := validateAdjustmentDecision(adjustment, rejectedBy, decisionReason); err != nil {
		return ManualAdjustment{}, ErrConflict
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE manual_adjustments SET rejected_by=$2,status='REJECTED',decision_reason=$3,decided_at=$4,updated_at=$4 WHERE id=$1 AND status='PENDING_APPROVAL'`, adjustment.ID, rejectedBy, decisionReason, now)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() != 1 {
		return ManualAdjustment{}, ErrConflict
	}
	adjustment.RejectedBy = &rejectedBy
	adjustment.Status = "REJECTED"
	adjustment.DecisionReason = &decisionReason
	adjustment.DecidedAt = &now
	adjustment.UpdatedAt = now
	return adjustment, tx.Commit(ctx)
}

func validateAdjustmentDecision(adjustment ManualAdjustment, actor uuid.UUID, reason string) error {
	if adjustment.Status != "PENDING_APPROVAL" || actor == uuid.Nil || actor == adjustment.RequestedBy || strings.TrimSpace(reason) == "" {
		return ErrConflict
	}
	return nil
}

func consumeAdjustmentLots(ctx context.Context, tx pgx.Tx, entryID, walletAccount, walletID uuid.UUID, amount int64, now time.Time) error {
	rows, err := tx.Query(ctx, availableLotsQuery, walletID, now)
	if err != nil {
		return err
	}
	type lot struct {
		id        uuid.UUID
		remaining int64
	}
	lots := make([]lot, 0)
	for rows.Next() {
		var item lot
		if err := rows.Scan(&item.id, &item.remaining); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	remaining := amount
	for _, item := range lots {
		if remaining == 0 {
			break
		}
		consume := min(remaining, item.remaining)
		if consume == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1`, item.id, consume); err != nil {
			return err
		}
		if err := insertPosting(ctx, tx, entryID, walletAccount, -consume, &item.id, now); err != nil {
			return err
		}
		remaining -= consume
	}
	if remaining != 0 {
		return errors.New("wallet balance is not backed by credit lots")
	}
	return nil
}

func getAdjustment(ctx context.Context, database queryable, adjustmentID uuid.UUID, lock bool) (ManualAdjustment, error) {
	query := `SELECT id,wallet_id,amount_microcredits,reason,requested_by,approved_by,rejected_by,status,journal_entry_id,decision_reason,created_at,updated_at,decided_at FROM manual_adjustments WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var adjustment ManualAdjustment
	err := database.QueryRow(ctx, query, adjustmentID).Scan(&adjustment.ID, &adjustment.WalletID, &adjustment.AmountMicrocredits, &adjustment.Reason, &adjustment.RequestedBy, &adjustment.ApprovedBy, &adjustment.RejectedBy, &adjustment.Status, &adjustment.JournalEntryID, &adjustment.DecisionReason, &adjustment.CreatedAt, &adjustment.UpdatedAt, &adjustment.DecidedAt)
	return adjustment, err
}
