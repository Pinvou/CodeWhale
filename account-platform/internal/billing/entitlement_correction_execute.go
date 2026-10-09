package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EntitlementCorrectionResult is the closed domain result of one committed
// entitlement component correction.
type EntitlementCorrectionResult struct {
	CorrectionID       uuid.UUID `json:"correction_id"`
	PackageID          uuid.UUID `json:"package_id"`
	WalletID           uuid.UUID `json:"wallet_id"`
	SourceComponentID  uuid.UUID `json:"source_component_id"`
	SourceLotID        uuid.UUID `json:"source_lot_id"`
	TargetComponentID  uuid.UUID `json:"target_component_id"`
	TargetLotID        uuid.UUID `json:"target_lot_id"`
	SourceType         string    `json:"source_type"`
	TargetType         string    `json:"target_type"`
	AmountMicrocredits int64     `json:"amount_microcredits"`
	JournalEntryID     uuid.UUID `json:"journal_entry_id"`
	Status             string    `json:"status"`
}

// entitlementCorrectionFrozenFacts is the owner revision an approval froze.
type entitlementCorrectionFrozenFacts struct {
	WalletID             uuid.UUID
	WalletVersion        int64
	PaymentTransactionID uuid.UUID
	SourceComponentID    uuid.UUID
	SourceLotID          uuid.UUID
	TargetComponentID    uuid.UUID
	TargetLotID          uuid.UUID
	SourceType           string
	TargetType           string
	AmountMicrocredits   int64
	SourceEvidenceDigest string
	FactsDigest          string
}

type entitlementCorrectionRecord struct {
	EntitlementCorrectionResult
	ApprovalRequestID    uuid.UUID
	ExecutionKeyDigest   string
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	PaymentOrderID       uuid.UUID
	PaymentTransactionID uuid.UUID
	ReasonCode           string
	SourceEvidenceDigest string
	FactsDigest          string
}

func (f entitlementCorrectionFrozenFacts) matches(plan entitlementCorrectionReadPlan) bool {
	return f.WalletID == plan.WalletID && f.WalletVersion == plan.WalletVersion &&
		f.PaymentTransactionID == plan.PaymentTransactionID &&
		f.SourceComponentID == plan.SourceComponentID && f.SourceLotID == plan.SourceLotID &&
		f.TargetComponentID == plan.TargetComponentID && f.TargetLotID == plan.TargetLotID &&
		f.SourceType == plan.Amounts.SourceType && f.TargetType == plan.Amounts.TargetType &&
		f.AmountMicrocredits == plan.Amounts.Amount &&
		f.SourceEvidenceDigest == plan.SourceEvidenceDigest && f.FactsDigest == plan.FactsDigest
}

func (record entitlementCorrectionRecord) matchesFrozen(approvalRequestID, authorizationEventID uuid.UUID, approvalEpoch int64,
	packageID, paymentOrderID uuid.UUID, executionKey string, reasonCode string, frozen entitlementCorrectionFrozenFacts) bool {
	digest := correctionExecutionKeyDigest(executionKey)
	return record.ApprovalRequestID == approvalRequestID && record.AuthorizationEventID == authorizationEventID &&
		record.ApprovalEpoch == approvalEpoch && record.PackageID == packageID && record.PaymentOrderID == paymentOrderID &&
		record.WalletID == frozen.WalletID && record.PaymentTransactionID == frozen.PaymentTransactionID &&
		record.SourceComponentID == frozen.SourceComponentID && record.SourceLotID == frozen.SourceLotID &&
		record.SourceType == frozen.SourceType && record.TargetType == frozen.TargetType &&
		record.AmountMicrocredits == frozen.AmountMicrocredits && record.ReasonCode == reasonCode &&
		record.SourceEvidenceDigest == frozen.SourceEvidenceDigest && record.FactsDigest == frozen.FactsDigest &&
		record.ExecutionKeyDigest == digest
}

func correctionExecutionKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// EntitlementCorrectionForRequest reads the committed correction of one
// approval request without creating any new fact.
func (r *PostgresRepository) EntitlementCorrectionForRequest(ctx context.Context, approvalRequestID uuid.UUID) (EntitlementCorrectionResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return EntitlementCorrectionResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := readEntitlementCorrection(ctx, tx, approvalRequestID)
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	return stored.EntitlementCorrectionResult, nil
}

type entitlementCorrectionReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readEntitlementCorrection(ctx context.Context, source entitlementCorrectionReader, approvalRequestID uuid.UUID) (entitlementCorrectionRecord, error) {
	var record entitlementCorrectionRecord
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
package_id,wallet_id,payment_order_id,payment_transaction_id,source_component_id,source_lot_id,target_component_id,target_lot_id,
source_type,target_type,amount_microcredits,reason_code,source_evidence_digest,facts_digest,journal_entry_id
FROM entitlement_component_corrections WHERE approval_request_id=$1`, approvalRequestID).Scan(
		&record.CorrectionID, &record.ApprovalRequestID, &record.AuthorizationEventID, &record.ApprovalEpoch, &record.ExecutionKeyDigest,
		&record.PackageID, &record.WalletID, &record.PaymentOrderID, &record.PaymentTransactionID, &record.SourceComponentID, &record.SourceLotID,
		&record.TargetComponentID, &record.TargetLotID, &record.SourceType, &record.TargetType,
		&record.AmountMicrocredits, &record.ReasonCode, &record.SourceEvidenceDigest, &record.FactsDigest, &record.JournalEntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementCorrectionRecord{}, ErrNotFound
	}
	if err != nil {
		return entitlementCorrectionRecord{}, err
	}
	if err := verifyEntitlementCorrectionConservation(ctx, source, record.PackageID, record.SourceComponentID,
		record.SourceLotID, record.TargetComponentID, record.TargetLotID, record.SourceType, record.TargetType,
		record.JournalEntryID, record.AmountMicrocredits); err != nil {
		return entitlementCorrectionRecord{}, err
	}
	record.Status = "SUCCEEDED"
	return record, nil
}

// ExecuteEntitlementComponentCorrection is the single domain effect of the
// correction command. One serializable transaction re-reads every frozen owner
// fact, locks wallet -> package -> components/lots, writes the paired
// postings, and only then persists the immutable correction case. Any drift
// returns ErrApprovalSnapshotMismatch with zero writes; a repeated approval
// request can only read the already committed case.
func (r *PostgresRepository) ExecuteEntitlementComponentCorrection(ctx context.Context,
	approvalRequestID, authorizationEventID uuid.UUID, approvalEpoch int64,
	packageID, paymentOrderID, requesterID, approverID uuid.UUID,
	executionIdempotencyKey, decisionReason, reasonCode string,
	frozen entitlementCorrectionFrozenFacts) (EntitlementCorrectionResult, error) {
	executionIdempotencyKey = strings.TrimSpace(executionIdempotencyKey)
	decisionReason = strings.TrimSpace(decisionReason)
	reasonCode = strings.TrimSpace(reasonCode)
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil || authorizationEventID == uuid.Nil || approvalEpoch < 1 ||
		packageID == uuid.Nil || paymentOrderID == uuid.Nil || requesterID == uuid.Nil || approverID == uuid.Nil ||
		requesterID == approverID || executionIdempotencyKey == "" || decisionReason == "" ||
		len(reasonCode) == 0 || len([]rune(reasonCode)) > 80 || frozen.AmountMicrocredits < 1 || frozen.SourceType == frozen.TargetType {
		return EntitlementCorrectionResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, err := readEntitlementCorrection(ctx, tx, approvalRequestID); err == nil {
		if !stored.matchesFrozen(approvalRequestID, authorizationEventID, approvalEpoch, packageID, paymentOrderID,
			executionIdempotencyKey, reasonCode, frozen) {
			return EntitlementCorrectionResult{}, ErrConflict
		}
		return stored.EntitlementCorrectionResult, tx.Commit(ctx)
	} else if !errors.Is(err, ErrNotFound) {
		return EntitlementCorrectionResult{}, err
	}
	now := r.now()
	plan, err := entitlementCorrectionPlanFrom(ctx, tx, now, packageID, paymentOrderID)
	if err != nil {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	if !frozen.matches(plan) {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	var walletStatus string
	var walletVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,version FROM wallets WHERE id=$1 FOR UPDATE`, plan.WalletID).Scan(&walletStatus, &walletVersion); err != nil {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	var packageStatus string
	var packageUntil time.Time
	if err := tx.QueryRow(ctx, `SELECT status,valid_until FROM entitlement_packages WHERE id=$1 FOR UPDATE`, plan.PackageID).Scan(&packageStatus, &packageUntil); err != nil {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	if walletStatus != "ACTIVE" || walletVersion != plan.WalletVersion || packageStatus != "ACTIVE" || !packageUntil.After(now) {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	if err := lockEntitlementCorrectionComponents(ctx, tx, plan); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	now = r.now()
	if !packageUntil.After(now) || !plan.SourceExpiresAt.After(now) {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	correctionID := uuid.Must(uuid.NewV7())
	journalID := uuid.Must(uuid.NewV7())
	targetComponentID := uuid.Must(uuid.NewV7())
	targetLotID := uuid.Must(uuid.NewV7())
	reference := "approval-execution:" + approvalRequestID.String()
	if err := insertJournal(ctx, tx, journalID, "ADJUSTMENT", reference, "entitlement component correction", now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, plan.WalletID)
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	amount := plan.Amounts.Amount
	refundable := plan.Amounts.TargetType == "BASE"
	lotReference := plan.SourceEvidenceRef + ":correction:" + correctionID.String()
	if _, err := tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,$4,$5,$6,$7)`, targetLotID, plan.WalletID, lotReference, amount, refundable, plan.SourceExpiresAt, now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$5,$6,$7,'ACTIVE',$8,$8)`, targetComponentID, plan.PackageID, plan.Amounts.TargetType, targetLotID, amount, plan.SourceCreditedAt, plan.SourceExpiresAt, now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,
	reclassified_out_microcredits=reclassified_out_microcredits+$2,updated_at=$3
	WHERE id=$1 AND status='ACTIVE' AND available_microcredits=original_microcredits AND reclassified_out_microcredits=0`, plan.SourceComponentID, amount, now)
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if command.RowsAffected() != 1 {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	command, err = tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2
	WHERE id=$1 AND remaining_microcredits=original_microcredits`, plan.SourceLotID, amount)
	if err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if command.RowsAffected() != 1 {
		return EntitlementCorrectionResult{}, ErrApprovalSnapshotMismatch
	}
	if err := insertPosting(ctx, tx, journalID, walletAccount, -amount, &plan.SourceLotID, now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if err := insertPosting(ctx, tx, journalID, walletAccount, amount, &targetLotID, now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO entitlement_component_corrections(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
package_id,wallet_id,payment_order_id,payment_transaction_id,source_component_id,source_lot_id,target_component_id,target_lot_id,
source_type,target_type,amount_microcredits,reason_code,source_evidence_digest,facts_digest,journal_entry_id,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		correctionID, approvalRequestID, authorizationEventID, approvalEpoch, correctionExecutionKeyDigest(executionIdempotencyKey),
		plan.PackageID, plan.WalletID, plan.PaymentOrderID, plan.PaymentTransactionID, plan.SourceComponentID, plan.SourceLotID,
		targetComponentID, targetLotID, plan.Amounts.SourceType, plan.Amounts.TargetType, amount, reasonCode,
		plan.SourceEvidenceDigest, plan.FactsDigest, journalID, now); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	if err := verifyEntitlementCorrectionComponents(ctx, tx, plan.PackageID, plan.SourceComponentID, plan.SourceLotID,
		targetComponentID, targetLotID, plan.Amounts.SourceType, plan.Amounts.TargetType, amount); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	result := EntitlementCorrectionResult{
		CorrectionID: correctionID, PackageID: plan.PackageID, WalletID: plan.WalletID,
		SourceComponentID: plan.SourceComponentID, SourceLotID: plan.SourceLotID,
		TargetComponentID: targetComponentID, TargetLotID: targetLotID,
		SourceType: plan.Amounts.SourceType, TargetType: plan.Amounts.TargetType,
		AmountMicrocredits: amount, JournalEntryID: journalID, Status: "SUCCEEDED",
	}
	if err := tx.Commit(ctx); err != nil {
		return EntitlementCorrectionResult{}, err
	}
	confirmed, err := r.EntitlementCorrectionForRequest(ctx, approvalRequestID)
	if err != nil || confirmed != result {
		if err == nil {
			err = ErrConflict
		}
		return EntitlementCorrectionResult{}, err
	}
	return confirmed, nil
}

// lockEntitlementCorrectionComponents locks the source component and lot and
// re-verifies the untouched-availability invariant the plan already proved.
func lockEntitlementCorrectionComponents(ctx context.Context, tx pgx.Tx, plan entitlementCorrectionReadPlan) error {
	var available, original, reclassified, reserved, used, granted, suspended, expired, revoked int64
	var status string
	var expiresAt, creditedAt time.Time
	var lotID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT credit_lot_id,original_microcredits,available_microcredits,reclassified_out_microcredits,
reserved_microcredits,used_microcredits,active_grant_microcredits,suspended_grant_microcredits,expired_microcredits,revoked_microcredits,
status,credited_at,expires_at FROM entitlement_components WHERE id=$1 FOR UPDATE`, plan.SourceComponentID).
		Scan(&lotID, &original, &available, &reclassified, &reserved, &used, &granted, &suspended, &expired, &revoked, &status, &creditedAt, &expiresAt)
	if err != nil {
		return ErrApprovalSnapshotMismatch
	}
	if lotID != plan.SourceLotID || original != plan.SourceOriginal || available != original || reclassified != 0 ||
		reserved != 0 || used != 0 || granted != 0 || suspended != 0 || expired != 0 || revoked != 0 ||
		status != "ACTIVE" || !expiresAt.Equal(plan.SourceExpiresAt) || !creditedAt.Equal(plan.SourceCreditedAt) {
		return ErrApprovalSnapshotMismatch
	}
	var lotOriginal, lotRemaining int64
	if err := tx.QueryRow(ctx, `SELECT original_microcredits,remaining_microcredits FROM credit_lots WHERE id=$1 FOR UPDATE`, plan.SourceLotID).Scan(&lotOriginal, &lotRemaining); err != nil {
		return ErrApprovalSnapshotMismatch
	}
	if lotOriginal != original || lotRemaining != original {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}

// verifyEntitlementCorrectionConservation proves the three domain invariants
// before the transaction may commit.
func verifyEntitlementCorrectionConservation(ctx context.Context, source entitlementCorrectionReader, packageID, sourceComponentID, sourceLotID, targetComponentID, targetLotID uuid.UUID,
	sourceType, targetType string, journalID uuid.UUID, amount int64) error {
	if err := verifyEntitlementCorrectionComponents(ctx, source, packageID, sourceComponentID, sourceLotID,
		targetComponentID, targetLotID, sourceType, targetType, amount); err != nil {
		return err
	}
	if err := verifyEntitlementCorrectionPostings(ctx, source, journalID, sourceLotID, targetLotID, amount); err != nil {
		return err
	}
	return nil
}

func verifyEntitlementCorrectionComponents(ctx context.Context, source entitlementCorrectionReader, packageID, sourceComponentID, sourceLotID, targetComponentID, targetLotID uuid.UUID,
	sourceType, targetType string, amount int64) error {
	var sourceConserved, targetConserved bool
	if err := source.QueryRow(ctx, `SELECT available_microcredits+reserved_microcredits+used_microcredits+active_grant_microcredits+
suspended_grant_microcredits+expired_microcredits+revoked_microcredits+reclassified_out_microcredits=original_microcredits
FROM entitlement_components WHERE id=$1`, sourceComponentID).Scan(&sourceConserved); err != nil || !sourceConserved {
		return ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT available_microcredits+reserved_microcredits+used_microcredits+active_grant_microcredits+
suspended_grant_microcredits+expired_microcredits+revoked_microcredits+reclassified_out_microcredits=original_microcredits
FROM entitlement_components WHERE id=$1`, targetComponentID).Scan(&targetConserved); err != nil || !targetConserved {
		return ErrConflict
	}
	var effectiveTotal, pointsTotal int64
	if err := source.QueryRow(ctx, `SELECT COALESCE(sum(original_microcredits-reclassified_out_microcredits),0) FROM entitlement_components WHERE package_id=$1`, packageID).Scan(&effectiveTotal); err != nil {
		return ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT points_total FROM entitlement_packages WHERE id=$1`, packageID).Scan(&pointsTotal); err != nil {
		return ErrConflict
	}
	if effectiveTotal != pointsTotal {
		return ErrConflict
	}
	var componentIdentity bool
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM entitlement_components WHERE id=$1 AND package_id=$5 AND credit_lot_id=$2 AND component_type=$6 AND reclassified_out_microcredits=$8)
 AND EXISTS(SELECT 1 FROM entitlement_components WHERE id=$3 AND package_id=$5 AND credit_lot_id=$4 AND component_type=$7 AND reclassified_out_microcredits=0 AND original_microcredits=$8)`,
		sourceComponentID, sourceLotID, targetComponentID, targetLotID, packageID, sourceType, targetType, amount).
		Scan(&componentIdentity); err != nil || !componentIdentity {
		return ErrConflict
	}
	return nil
}

func verifyEntitlementCorrectionPostings(ctx context.Context, source entitlementCorrectionReader, journalID, sourceLotID, targetLotID uuid.UUID, amount int64) error {
	var postingCount int
	var postingTotal int64
	if err := source.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, journalID).
		Scan(&postingCount, &postingTotal); err != nil || postingCount != 2 || postingTotal != 0 {
		return ErrConflict
	}
	var sourceDelta, targetDelta int64
	if err := source.QueryRow(ctx, `SELECT delta_microcredits FROM ledger_postings WHERE journal_entry_id=$1 AND credit_lot_id=$2`,
		journalID, sourceLotID).Scan(&sourceDelta); err != nil || sourceDelta != -amount {
		return ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT delta_microcredits FROM ledger_postings WHERE journal_entry_id=$1 AND credit_lot_id=$2`,
		journalID, targetLotID).Scan(&targetDelta); err != nil || targetDelta != amount {
		return ErrConflict
	}
	return nil
}
