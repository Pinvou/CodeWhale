package billing

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (r *PostgresRepository) RequestAdjustment(ctx context.Context, walletID uuid.UUID, amount int64, reason string, requestedBy uuid.UUID) (ManualAdjustment, error) {
	return r.requestAdjustment(ctx, walletID, amount, reason, requestedBy, "")
}

func (r *PostgresRepository) RequestAdjustmentWithIdempotency(ctx context.Context, walletID uuid.UUID, amount int64, reason string, requestedBy uuid.UUID, idempotencyKey string) (ManualAdjustment, error) {
	return r.requestAdjustment(ctx, walletID, amount, reason, requestedBy, strings.TrimSpace(idempotencyKey))
}

func (r *PostgresRepository) requestAdjustment(ctx context.Context, walletID uuid.UUID, amount int64, reason string, requestedBy uuid.UUID, idempotencyKey string) (ManualAdjustment, error) {
	reason = strings.TrimSpace(reason)
	if walletID == uuid.Nil || requestedBy == uuid.Nil || amount == 0 || amount == math.MinInt64 || reason == "" || (idempotencyKey != "" && (len(idempotencyKey) < 8 || len(idempotencyKey) > 120)) {
		return ManualAdjustment{}, ErrConflict
	}
	if idempotencyKey != "" {
		existing, err := getAdjustmentByRequestKey(ctx, r.pool, requestedBy, idempotencyKey)
		if err == nil {
			if existing.WalletID != walletID || existing.AmountMicrocredits != amount || existing.Reason != reason {
				return ManualAdjustment{}, ErrConflict
			}
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ManualAdjustment{}, err
		}
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
		Version:            1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if idempotencyKey != "" {
		adjustment.RequestIdempotencyKey = &idempotencyKey
	}
	command, err := r.pool.Exec(ctx, `INSERT INTO manual_adjustments(id,wallet_id,amount_microcredits,reason,requested_by,status,request_idempotency_key,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'PENDING_APPROVAL',NULLIF($6,''),$7,$7) ON CONFLICT (requested_by,request_idempotency_key) WHERE request_idempotency_key IS NOT NULL DO NOTHING`, adjustment.ID, walletID, amount, reason, requestedBy, idempotencyKey, now)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() == 1 {
		return adjustment, nil
	}
	existing, err := getAdjustmentByRequestKey(ctx, r.pool, requestedBy, idempotencyKey)
	if err != nil || existing.WalletID != walletID || existing.AmountMicrocredits != amount || existing.Reason != reason {
		return ManualAdjustment{}, ErrConflict
	}
	return existing, nil
}

func (r *PostgresRepository) ExecuteApprovedAdjustment(ctx context.Context, adjustmentID, approvalRequestID uuid.UUID, executionIdempotencyKey string, approvedBy uuid.UUID, decisionReason string, expectedVersion int64) (ManualAdjustment, error) {
	decisionReason = strings.TrimSpace(decisionReason)
	executionIdempotencyKey = strings.TrimSpace(executionIdempotencyKey)
	if adjustmentID == uuid.Nil || approvalRequestID == uuid.Nil || approvedBy == uuid.Nil || decisionReason == "" || executionIdempotencyKey == "" || expectedVersion < 1 {
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
	if adjustment.Status == "POSTED" && adjustment.ApprovedBy != nil && *adjustment.ApprovedBy == approvedBy && adjustment.ApprovalRequestID != nil && *adjustment.ApprovalRequestID == approvalRequestID && adjustment.ExecutionIdempotencyKey != nil && *adjustment.ExecutionIdempotencyKey == executionIdempotencyKey && adjustment.Version == expectedVersion+1 && adjustment.DecisionReason != nil && *adjustment.DecisionReason == decisionReason {
		return adjustment, tx.Commit(ctx)
	}
	if adjustment.Version != expectedVersion {
		return ManualAdjustment{}, ErrConflict
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
		reference := "adjustment:" + adjustment.ID.String()
		var expiresAt *time.Time
		_, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'ADJUSTMENT',$3,$4,$4,false,$5,$6)`, lotID, adjustment.WalletID, reference, adjustment.AmountMicrocredits, expiresAt, now)
		packageID := uuid.Must(uuid.NewV7())
		if err == nil {
			var command pgconn.CommandTag
			command, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) SELECT $1,CASE WHEN owner_type='PERSONAL' THEN owner_id ELSE NULL END,id,NULL,$2,'MANUAL_BONUS','Manual bonus','',0,$3,$3,$4,$4,$5,'ACTIVE',$4,$4,'MANUAL_ADJUSTMENT',$6 FROM wallets WHERE id=$7`, packageID, lotID, adjustment.AmountMicrocredits, now, expiresAt, reference, adjustment.WalletID)
			if err == nil && command.RowsAffected() != 1 {
				err = ErrConflict
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BONUS',$1,$3,$3,$4,$5,'ACTIVE',$4,$4)`, lotID, packageID, adjustment.AmountMicrocredits, now, expiresAt)
		}
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
	command, err = tx.Exec(ctx, `UPDATE manual_adjustments SET approved_by=$2,status='POSTED',journal_entry_id=$3,decision_reason=$4,decided_at=$5,updated_at=$5,version=version+1,approval_request_id=$6,execution_idempotency_key=$7 WHERE id=$1 AND status='PENDING_APPROVAL'`, adjustment.ID, approvedBy, entryID, decisionReason, now, approvalRequestID, executionIdempotencyKey)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() != 1 {
		return ManualAdjustment{}, ErrConflict
	}
	adjustment.ApprovedBy = &approvedBy
	adjustment.Status = "POSTED"
	adjustment.Version++
	adjustment.ApprovalRequestID = &approvalRequestID
	adjustment.ExecutionIdempotencyKey = &executionIdempotencyKey
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
	command, err := tx.Exec(ctx, `UPDATE manual_adjustments SET rejected_by=$2,status='REJECTED',decision_reason=$3,decided_at=$4,updated_at=$4,version=version+1 WHERE id=$1 AND status='PENDING_APPROVAL'`, adjustment.ID, rejectedBy, decisionReason, now)
	if err != nil {
		return ManualAdjustment{}, err
	}
	if command.RowsAffected() != 1 {
		return ManualAdjustment{}, ErrConflict
	}
	adjustment.RejectedBy = &rejectedBy
	adjustment.Status = "REJECTED"
	adjustment.Version++
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
	rows, err := tx.Query(ctx, `SELECT component.package_id,component.id,component.credit_lot_id,GREATEST(component.available_microcredits-COALESCE((SELECT sum(refund.credit_microcredits) FROM refunds refund JOIN credit_lots lot ON lot.source_reference='recharge:'||refund.payment_order_id::text WHERE lot.id=component.credit_lot_id AND refund.status IN ('REQUESTED','MANUAL_REVIEW')),0),0)::bigint FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1 AND component.available_microcredits>0 AND (component.expires_at IS NULL OR component.expires_at>$2) AND component.status='ACTIVE' ORDER BY component.expires_at NULLS LAST,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF component`, walletID, now)
	if err != nil {
		return err
	}
	type lot struct {
		packageID   uuid.UUID
		componentID uuid.UUID
		lotID       uuid.UUID
		remaining   int64
	}
	lots := make([]lot, 0)
	for rows.Next() {
		var item lot
		if err := rows.Scan(&item.packageID, &item.componentID, &item.lotID, &item.remaining); err != nil {
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
		command, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, item.lotID, consume)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrConflict
		}
		command, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,revoked_microcredits=revoked_microcredits+$2,status=CASE WHEN available_microcredits=$2 AND reserved_microcredits=0 THEN 'REVOKED' ELSE status END,updated_at=$3 WHERE id=$1 AND available_microcredits>=$2`, item.componentID, consume, now)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrConflict
		}
		if err := insertPosting(ctx, tx, entryID, walletAccount, -consume, &item.lotID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND (component.available_microcredits>0 OR component.reserved_microcredits>0)) THEN 'ACTIVE' ELSE 'CONSUMED' END,updated_at=$2 WHERE id=$1`, item.packageID, now); err != nil {
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
	query := `SELECT id,wallet_id,amount_microcredits,reason,requested_by,approved_by,rejected_by,status,version,request_idempotency_key,approval_request_id,execution_idempotency_key,journal_entry_id,decision_reason,created_at,updated_at,decided_at FROM manual_adjustments WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var adjustment ManualAdjustment
	err := database.QueryRow(ctx, query, adjustmentID).Scan(&adjustment.ID, &adjustment.WalletID, &adjustment.AmountMicrocredits, &adjustment.Reason, &adjustment.RequestedBy, &adjustment.ApprovedBy, &adjustment.RejectedBy, &adjustment.Status, &adjustment.Version, &adjustment.RequestIdempotencyKey, &adjustment.ApprovalRequestID, &adjustment.ExecutionIdempotencyKey, &adjustment.JournalEntryID, &adjustment.DecisionReason, &adjustment.CreatedAt, &adjustment.UpdatedAt, &adjustment.DecidedAt)
	return adjustment, err
}

func getAdjustmentByRequestKey(ctx context.Context, database queryable, requestedBy uuid.UUID, idempotencyKey string) (ManualAdjustment, error) {
	var adjustmentID uuid.UUID
	if err := database.QueryRow(ctx, `SELECT id FROM manual_adjustments WHERE requested_by=$1 AND request_idempotency_key=$2`, requestedBy, idempotencyKey).Scan(&adjustmentID); err != nil {
		return ManualAdjustment{}, err
	}
	return getAdjustment(ctx, database, adjustmentID, false)
}
