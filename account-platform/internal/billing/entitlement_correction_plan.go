package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func encodeEntitlementCorrectionProjection(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

// EntitlementCorrectionProofInput derives the closed proof-era projection of a
// complete, unused entitlement component reclassification for a request that is
// still being assembled. Every amount, component and digest comes from the
// owner domain read path; the caller may only choose the non-empty reason code,
// and the reason code never grants execution. Billing proof and the eventual
// Billing write must still independently re-read the same facts, so this method
// holds no authority of its own.
func (r *PostgresRepository) EntitlementCorrectionProofInput(ctx context.Context, packageID, orderID uuid.UUID, reasonCode string) (approvalencoding.LegacyProofInput, error) {
	if strings.TrimSpace(reasonCode) == "" {
		return approvalencoding.LegacyProofInput{}, ErrConflict
	}
	plan, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := entitlementCorrectionProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: entitlementComponentAdjustmentCommand, PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: plan.PackageID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"package_id": plan.PackageID, "payment_order_id": plan.PaymentOrderID, "reason_code": reasonCode,
		}),
		CriticalResourceVersions: critical,
		FinancialSnapshot:        financial,
	}, nil
}

// entitlementCorrectionProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof recomputation.
// A target component does not exist before the first execution, so both target
// identifiers stay explicitly null instead of becoming zero UUIDs.
func entitlementCorrectionProofProjection(plan entitlementCorrectionReadPlan) (json.RawMessage, json.RawMessage) {
	nullableID := func(value uuid.UUID) any {
		if value == uuid.Nil {
			return nil
		}
		return value
	}
	critical := encodeEntitlementCorrectionProjection(map[string]any{
		"package_id": plan.PackageID, "wallet_id": plan.WalletID, "wallet_version": plan.WalletVersion,
		"payment_order_id": plan.PaymentOrderID, "payment_transaction_id": plan.PaymentTransactionID,
		"source_component_id": plan.SourceComponentID, "source_lot_id": plan.SourceLotID,
		"target_component_id": nullableID(plan.TargetComponentID), "target_lot_id": nullableID(plan.TargetLotID),
		"source_type": plan.Amounts.SourceType, "target_type": plan.Amounts.TargetType,
		"amount_microcredits": plan.Amounts.Amount, "source_evidence_digest": plan.SourceEvidenceDigest,
		"facts_digest": plan.FactsDigest, "policy_version": 2,
	})
	financial := encodeEntitlementCorrectionProjection(map[string]any{
		"package_total_microcredits": plan.PackageTotal, "source_original_microcredits": plan.SourceOriginal,
		"target_original_microcredits": plan.TargetOriginal,
		"effective_total_microcredits": plan.SourceOriginal + plan.TargetOriginal,
	})
	return critical, financial
}

type entitlementCorrectionReadPlan struct {
	PackageID            uuid.UUID
	WalletID             uuid.UUID
	PaymentOrderID       uuid.UUID
	PaymentTransactionID uuid.UUID
	SourceComponentID    uuid.UUID
	SourceLotID          uuid.UUID
	ExistingTargetID     uuid.UUID
	TargetComponentID    uuid.UUID
	TargetLotID          uuid.UUID
	WalletVersion        int64
	PackageTotal         int64
	SourceOriginal       int64
	TargetOriginal       int64
	SourceCreditedAt     time.Time
	SourceExpiresAt      time.Time
	SourceEvidenceRef    string
	SourceEvidenceDigest string
	FactsDigest          string
	Amounts              entitlementCorrectionAmounts
}

// planEntitlementComponentCorrection is a read-only, unexposed precheck. It
// cannot grant execution: approval proof and the eventual Billing write must
// independently re-read every fact under their own deadlines and locks.
func (r *PostgresRepository) planEntitlementComponentCorrection(ctx context.Context, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	if r == nil || r.pool == nil {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	defer tx.Rollback(bounded)
	plan, err := entitlementCorrectionPlanFrom(bounded, tx, r.now(), packageID, orderID)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	return plan, nil
}

// entitlementCorrectionQuerier is satisfied by the pool-backed read-only
// transaction and by the serializable transaction that claims a proof.
type entitlementCorrectionQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// entitlementCorrectionPlanFrom derives the only permitted complete
// reclassification from already-open owner-domain reads.
func entitlementCorrectionPlanFrom(ctx context.Context, source entitlementCorrectionQuerier, now time.Time, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	if packageID == uuid.Nil || orderID == uuid.Nil {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var err error
	var walletID, transactionID uuid.UUID
	var packageStatus, sourceType, walletStatus, orderStatus, fulfillmentStatus, transactionStatus string
	var packageTotal, remaining, reserved, used, expired, revoked, orderCredits, orderAmount, transactionAmount, walletVersion int64
	var packageUntil time.Time
	var orderCurrency, transactionCurrency, productID, sourceReference string
	var rawProduct []byte
	err = source.QueryRow(ctx, `SELECT p.wallet_id,p.source_payment_transaction_id,p.status,p.source_type,
 p.points_total,p.points_remaining,p.points_reserved,p.points_used,p.points_expired,p.points_revoked,p.valid_until,
 w.status,w.version,o.status,o.fulfillment_status,o.credit_microcredits,o.amount_fen,o.currency,o.product_id,o.product_snapshot,
 t.status,t.amount_fen,t.currency,p.source_reference
 FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id
 JOIN payment_orders o ON o.id=p.payment_order_id AND o.wallet_id=p.wallet_id
 JOIN payment_transactions t ON t.id=p.source_payment_transaction_id AND t.payment_order_id=o.id
 WHERE p.id=$1 AND p.payment_order_id=$2`, packageID, orderID).
		Scan(&walletID, &transactionID, &packageStatus, &sourceType,
			&packageTotal, &remaining, &reserved, &used, &expired, &revoked, &packageUntil,
			&walletStatus, &walletVersion, &orderStatus, &fulfillmentStatus, &orderCredits, &orderAmount,
			&orderCurrency, &productID, &rawProduct, &transactionStatus, &transactionAmount, &transactionCurrency, &sourceReference)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if packageStatus != "ACTIVE" || sourceType != "PAYMENT" || walletStatus != "ACTIVE" || walletVersion < 1 ||
		fulfillmentStatus != "CREDITED" ||
		!((orderStatus == "PAID" && transactionStatus == "PAID") || (orderStatus == "PAID_LATE" && transactionStatus == "LATE_PAID")) ||
		packageTotal <= 0 || remaining != packageTotal || reserved != 0 || used != 0 || expired != 0 || revoked != 0 ||
		orderCredits != packageTotal || orderAmount <= 0 || transactionAmount != orderAmount ||
		orderCurrency != "CNY" || transactionCurrency != orderCurrency || !packageUntil.After(now) ||
		sourceReference != "payment-transaction:"+transactionID.String() {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var product Product
	if json.Unmarshal(rawProduct, &product) != nil || product.ProductID != productID ||
		product.CreditMicrocredits != orderCredits || product.AmountFen != orderAmount || product.Currency != orderCurrency ||
		product.BasePoints < 0 || product.BonusPoints < 0 {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var hasRelated bool
	err = source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM member_quota_grants WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM usage_hold_allocations WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM receivables WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM entitlement_component_corrections WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM payment_order_repairs WHERE payment_order_id=$2) OR
 EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$2 OR payment_transaction_id=$3)`, packageID, orderID, transactionID).Scan(&hasRelated)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if hasRelated {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	var journalID uuid.UUID
	err = source.QueryRow(ctx, `SELECT id FROM journal_entries WHERE business_reference=$1 AND entry_type='RECHARGE'`, sourceReference).Scan(&journalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	rows, err := source.Query(ctx, `SELECT c.id,c.component_type,c.credit_lot_id,c.original_microcredits,c.available_microcredits,
 c.reserved_microcredits,c.used_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,
 c.expired_microcredits,c.revoked_microcredits,c.reclassified_out_microcredits,c.status,c.expires_at,c.credited_at,
 l.wallet_id,l.original_microcredits,l.remaining_microcredits,l.refundable,l.expires_at,l.source_type,l.source_reference
 FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id WHERE c.package_id=$1 ORDER BY c.id`, packageID)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	var base, bonus int64
	componentIDs := map[string]uuid.UUID{}
	lotIDs := map[string]uuid.UUID{}
	lotAmounts := map[uuid.UUID]int64{}
	creditedAtByKind := map[string]time.Time{}
	expiresAtByKind := map[string]time.Time{}
	count := 0
	for rows.Next() {
		var id, lotID, lotWalletID uuid.UUID
		var kind, status string
		var original, available, reservedComponent, usedComponent, activeGrant, suspendedGrant, expiredComponent, revokedComponent, reclassified, lotOriginal, lotRemaining int64
		var lotSourceType, lotSourceReference string
		var creditedAt, expiresAt, lotExpiresAt time.Time
		var refundable bool
		if err = rows.Scan(&id, &kind, &lotID, &original, &available, &reservedComponent, &usedComponent,
			&activeGrant, &suspendedGrant, &expiredComponent, &revokedComponent, &reclassified, &status, &expiresAt,
			&creditedAt, &lotWalletID, &lotOriginal, &lotRemaining, &refundable, &lotExpiresAt, &lotSourceType, &lotSourceReference); err != nil {
			break
		}
		count++
		if count > 2 || (kind != "BASE" && kind != "BONUS") || componentIDs[kind] != uuid.Nil ||
			status != "ACTIVE" || original <= 0 || available != original ||
			reservedComponent != 0 || usedComponent != 0 || activeGrant != 0 || suspendedGrant != 0 ||
			expiredComponent != 0 || revokedComponent != 0 || reclassified != 0 ||
			lotWalletID != walletID || lotOriginal != original || lotRemaining != original ||
			lotSourceType != "RECHARGE" ||
			(lotSourceReference != sourceReference && (kind != "BONUS" || lotSourceReference != sourceReference+":bonus")) ||
			refundable != (kind == "BASE") || !expiresAt.Equal(lotExpiresAt) ||
			!expiresAt.Equal(packageUntil) || !expiresAt.After(now) ||
			creditedAt.IsZero() || creditedAt.After(now) || !expiresAt.After(creditedAt) ||
			original > math.MaxInt64-base-bonus {
			err = ErrConflict
			break
		}
		componentIDs[kind], lotIDs[kind] = id, lotID
		creditedAtByKind[kind], expiresAtByKind[kind] = creditedAt, expiresAt
		lotAmounts[lotID] = original
		if kind == "BASE" {
			base += original
		} else {
			bonus += original
		}
	}
	if closeErr := rows.Err(); err == nil {
		err = closeErr
	}
	rows.Close()
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if count == 0 {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}
	amounts, err := calculateEntitlementCorrectionAmounts(packageTotal, base, bonus, product.BasePoints, product.BonusPoints)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	postingRows, err := source.Query(ctx, `SELECT posting.credit_lot_id,posting.delta_microcredits
 FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id
	 WHERE posting.journal_entry_id=$1 AND account.wallet_id=$2 AND account.account_type='WALLET_LIABILITY'`, journalID, walletID)
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	seenLots := map[uuid.UUID]bool{}
	for postingRows.Next() {
		var postedLot uuid.UUID
		var delta int64
		if err = postingRows.Scan(&postedLot, &delta); err != nil {
			break
		}
		if postedLot == uuid.Nil || seenLots[postedLot] || lotAmounts[postedLot] != delta {
			err = ErrConflict
			break
		}
		seenLots[postedLot] = true
	}
	if closeErr := postingRows.Err(); err == nil {
		err = closeErr
	}
	postingRows.Close()
	if err != nil {
		return entitlementCorrectionReadPlan{}, err
	}
	if len(seenLots) != len(lotAmounts) {
		return entitlementCorrectionReadPlan{}, ErrConflict
	}

	sourceOriginal := base
	if amounts.SourceType == "BONUS" {
		sourceOriginal = bonus
	}
	targetOriginal := int64(0)
	if lotID := lotIDs[amounts.TargetType]; lotID != uuid.Nil {
		targetOriginal = lotAmounts[lotID]
	}
	evidenceDigest, factsDigest := entitlementCorrectionDigests(entitlementCorrectionDigestInput{
		PackageID: packageID, WalletID: walletID, WalletVersion: walletVersion,
		PaymentOrderID: orderID, PaymentTransactionID: transactionID, JournalEntryID: journalID,
		SourceReference: sourceReference, PackageTotal: packageTotal, PackageExpiresAt: packageUntil,
		OrderCreditMicrocredits: orderCredits, OrderAmountFen: orderAmount, Currency: orderCurrency,
		ProductBasePoints: product.BasePoints, ProductBonusPoints: product.BonusPoints,
		CurrentBase: base, CurrentBonus: bonus, SourceOriginal: sourceOriginal, TargetOriginal: targetOriginal,
		SourceComponentID: componentIDs[amounts.SourceType], SourceLotID: lotIDs[amounts.SourceType],
		TargetComponentID: componentIDs[amounts.TargetType], TargetLotID: lotIDs[amounts.TargetType],
		Amounts: amounts,
	})
	return entitlementCorrectionReadPlan{
		PackageID: packageID, WalletID: walletID, PaymentOrderID: orderID, PaymentTransactionID: transactionID,
		SourceComponentID: componentIDs[amounts.SourceType], SourceLotID: lotIDs[amounts.SourceType],
		ExistingTargetID: componentIDs[amounts.TargetType], TargetComponentID: componentIDs[amounts.TargetType],
		TargetLotID: lotIDs[amounts.TargetType], WalletVersion: walletVersion, PackageTotal: packageTotal,
		SourceOriginal: sourceOriginal, TargetOriginal: targetOriginal,
		SourceCreditedAt: creditedAtByKind[amounts.SourceType], SourceExpiresAt: expiresAtByKind[amounts.SourceType],
		SourceEvidenceRef:    sourceReference,
		SourceEvidenceDigest: evidenceDigest, FactsDigest: factsDigest, Amounts: amounts,
	}, nil
}

type entitlementCorrectionDigestInput struct {
	PackageID               uuid.UUID
	WalletID                uuid.UUID
	WalletVersion           int64
	PaymentOrderID          uuid.UUID
	PaymentTransactionID    uuid.UUID
	JournalEntryID          uuid.UUID
	SourceReference         string
	PackageTotal            int64
	PackageExpiresAt        time.Time
	OrderCreditMicrocredits int64
	OrderAmountFen          int64
	Currency                string
	ProductBasePoints       int64
	ProductBonusPoints      int64
	CurrentBase             int64
	CurrentBonus            int64
	SourceOriginal          int64
	TargetOriginal          int64
	SourceComponentID       uuid.UUID
	SourceLotID             uuid.UUID
	TargetComponentID       uuid.UUID
	TargetLotID             uuid.UUID
	Amounts                 entitlementCorrectionAmounts
}

// entitlementCorrectionDigests binds the original payment evidence and the
// recomputed correction facts with the shared typed closed encoding. Both
// digests are derived only from owner-domain persisted rows.
func entitlementCorrectionDigests(input entitlementCorrectionDigestInput) (string, string) {
	id := func(value uuid.UUID) string {
		if value == uuid.Nil {
			return ""
		}
		return value.String()
	}
	evidence, err := json.Marshal(map[string]any{
		"command_type": "ENTITLEMENT_COMPONENT_ADJUSTMENT", "package_id": id(input.PackageID),
		"payment_order_id": id(input.PaymentOrderID), "payment_transaction_id": id(input.PaymentTransactionID),
		"journal_entry_id": id(input.JournalEntryID), "source_reference": input.SourceReference,
		"order_credit_microcredits": input.OrderCreditMicrocredits, "order_amount_fen": input.OrderAmountFen,
		"currency": input.Currency, "product_base_points": input.ProductBasePoints, "product_bonus_points": input.ProductBonusPoints,
	})
	facts, err2 := json.Marshal(map[string]any{
		"command_type": "ENTITLEMENT_COMPONENT_ADJUSTMENT", "package_id": id(input.PackageID),
		"wallet_id": id(input.WalletID), "wallet_version": input.WalletVersion,
		"payment_order_id": id(input.PaymentOrderID), "payment_transaction_id": id(input.PaymentTransactionID),
		"package_total_microcredits": input.PackageTotal, "package_expires_at": input.PackageExpiresAt.UTC().Format(time.RFC3339Nano),
		"source_component_id": id(input.SourceComponentID), "source_lot_id": id(input.SourceLotID),
		"target_component_id": id(input.TargetComponentID), "target_lot_id": id(input.TargetLotID),
		"source_type": input.Amounts.SourceType, "target_type": input.Amounts.TargetType,
		"amount_microcredits": input.Amounts.Amount, "source_original_microcredits": input.SourceOriginal,
		"target_original_microcredits": input.TargetOriginal, "current_base_microcredits": input.CurrentBase,
		"current_bonus_microcredits": input.CurrentBonus, "policy_version": 2,
	})
	if err != nil || err2 != nil {
		return "", ""
	}
	return digestHex(evidence), digestHex(facts)
}

func digestHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
