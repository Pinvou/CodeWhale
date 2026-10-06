package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ForcedRefundCase records a provider-initiated refund as financial evidence.
// It intentionally does not reuse the ordinary refund lifecycle: the payment
// remains a successful money movement until an audited review resolves the case.
type ForcedRefundCase struct {
	ID                   uuid.UUID       `json:"forced_refund_case_id"`
	PaymentEvidenceID    uuid.UUID       `json:"payment_evidence_id"`
	PaymentOrderID       uuid.UUID       `json:"payment_order_id"`
	PaymentTransactionID uuid.UUID       `json:"payment_transaction_id"`
	EntitlementPackageID uuid.UUID       `json:"entitlement_package_id"`
	WalletID             uuid.UUID       `json:"wallet_id"`
	ProviderRefundID     string          `json:"provider_refund_id"`
	AmountFen            int64           `json:"amount_fen,string"`
	Status               string          `json:"status"`
	EntitlementSnapshot  json.RawMessage `json:"entitlement_snapshot"`
	CreatedAt            time.Time       `json:"created_at"`
	ResolvedAt           *time.Time      `json:"resolved_at,omitempty"`
	ManualReviewCaseID   *uuid.UUID      `json:"manual_review_case_id,omitempty"`
	ResolutionCode       *string         `json:"resolution_code,omitempty"`
	ResolutionReason     *string         `json:"resolution_reason,omitempty"`
	ReceivableID         *uuid.UUID      `json:"receivable_id,omitempty"`
	RevokedBase          int64           `json:"revoked_base_microcredits,string"`
	RevokedBonus         int64           `json:"revoked_bonus_microcredits,string"`
	Version              int64           `json:"version"`
}

func (r *PostgresRepository) RecordAndFreezeForcedRefund(ctx context.Context, transactionID uuid.UUID, providerRefundID string, amountFen int64, payload json.RawMessage) (ForcedRefundCase, error) {
	providerRefundID = strings.TrimSpace(providerRefundID)
	if transactionID == uuid.Nil || providerRefundID == "" || len(providerRefundID) > 200 || amountFen <= 0 {
		return ForcedRefundCase{}, ErrConflict
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return ForcedRefundCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ForcedRefundCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "forced-refund.evidence", transactionID.String()+":"+providerRefundID); err != nil {
		return ForcedRefundCase{}, err
	}
	if existing, found, err := getForcedRefundCase(ctx, tx, transactionID, providerRefundID); err != nil {
		return ForcedRefundCase{}, err
	} else if found {
		if existing.AmountFen != amountFen {
			return ForcedRefundCase{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}

	var result ForcedRefundCase
	var providerID, providerVersion, merchantOrderNo, providerTransactionID, currency string
	var transactionAmountFen int64
	err = tx.QueryRow(ctx, `SELECT transaction.payment_order_id,transaction.provider_id,transaction.provider_version,payment.merchant_order_no,transaction.provider_transaction_id,transaction.amount_fen,transaction.currency,package.id,package.wallet_id
		FROM payment_transactions transaction
		JOIN payment_orders payment ON payment.id=transaction.payment_order_id
		JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id AND package.payment_order_id=transaction.payment_order_id
		WHERE transaction.id=$1 AND transaction.status IN ('PAID','LATE_PAID')`, transactionID).Scan(&result.PaymentOrderID, &providerID, &providerVersion, &merchantOrderNo, &providerTransactionID, &transactionAmountFen, &currency, &result.EntitlementPackageID, &result.WalletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ForcedRefundCase{}, ErrNotFound
	}
	if err != nil {
		return ForcedRefundCase{}, err
	}
	if amountFen > transactionAmountFen {
		return ForcedRefundCase{}, ErrConflict
	}
	if err := lockPaymentSourcePackage(ctx, tx, result.EntitlementPackageID, transactionID, result.PaymentOrderID, result.WalletID); err != nil {
		return ForcedRefundCase{}, err
	}
	result.ID = uuid.Must(uuid.NewV7())
	result.PaymentTransactionID = transactionID
	result.ProviderRefundID = providerRefundID
	result.AmountFen = amountFen
	result.Status = "REVIEW_REQUIRED"
	result.Version = 1
	result.CreatedAt = r.now()
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
		'package',to_jsonb(package),
		'components',COALESCE((SELECT jsonb_agg(to_jsonb(component) ORDER BY component.id) FROM entitlement_components component WHERE component.package_id=package.id),'[]'::jsonb),
		'grants',COALESCE((SELECT jsonb_agg(to_jsonb(member_grant) ORDER BY member_grant.id) FROM member_quota_grants member_grant WHERE member_grant.package_id=package.id),'[]'::jsonb)
	) FROM entitlement_packages package WHERE package.id=$1`, result.EntitlementPackageID).Scan(&result.EntitlementSnapshot); err != nil {
		return ForcedRefundCase{}, err
	}

	digest := sha256.Sum256(payload)
	result.PaymentEvidenceID = uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result) VALUES($1,$2,$3,'FORCED_REFUND',$4,$5,$6,$7,$8,$6,$9,$10,'FORCED_REFUND_CONFIRMED',$11,$11,$12,$13,'REVIEW_REQUIRED')`, result.PaymentEvidenceID, result.PaymentOrderID, transactionID, providerID, providerVersion, providerRefundID, merchantOrderNo, providerTransactionID, amountFen, currency, result.CreatedAt, hex.EncodeToString(digest[:]), payload)
	if err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW',failure_reason='provider initiated a separate forced refund',intent_version=intent_version+1,updated_at=$2 WHERE payment_transaction_id=$1 AND status='REQUESTED'`, transactionID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN',updated_at=$2 WHERE id=$1 AND status NOT IN ('REVOKED','CREDIT_FAILED')`, result.EntitlementPackageID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN',updated_at=$2 WHERE package_id=$1 AND status<>'REVOKED'`, result.EntitlementPackageID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason='FORCED_REFUND_REVIEW',allocation_version=allocation_version+1,updated_at=$2 WHERE package_id=$1 AND status='ACTIVE'`, result.EntitlementPackageID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits+active_grant_microcredits,active_grant_microcredits=0,updated_at=$2 WHERE package_id=$1 AND active_grant_microcredits>0`, result.EntitlementPackageID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO forced_refund_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_refund_id,amount_fen,status,entitlement_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'REVIEW_REQUIRED',$9,$10)`, result.ID, result.PaymentEvidenceID, result.PaymentOrderID, transactionID, result.EntitlementPackageID, result.WalletID, providerRefundID, amountFen, result.EntitlementSnapshot, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	anomaly, err := createPaymentAnomalyWithinTransaction(ctx, tx, "FORCED_REFUND", "P1", "forced-refund:"+result.ID.String(), paymentAnomalyReference{PaymentOrderID: &result.PaymentOrderID, PaymentTransactionID: &transactionID, PaymentEvidenceID: &result.PaymentEvidenceID, ForcedRefundCaseID: &result.ID}, map[string]any{"provider_refund_id": providerRefundID, "amount_fen": amountFen, "entitlement_snapshot": result.EntitlementSnapshot}, result.CreatedAt)
	if err != nil {
		return ForcedRefundCase{}, err
	}
	result.ManualReviewCaseID = &anomaly.ID
	if _, err := tx.Exec(ctx, `UPDATE forced_refund_cases SET manual_review_case_id=$2 WHERE id=$1`, result.ID, anomaly.ID); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET refund_summary=refund_summary||jsonb_build_object('forced_review_count',(SELECT count(*) FROM forced_refund_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),attention_summary='REVIEW_REQUIRED',summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, result.PaymentOrderID, result.CreatedAt); err != nil {
		return ForcedRefundCase{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.forced-refund.review-required.v1", result.ID, 1, map[string]any{"forced_refund_case_id": result.ID, "payment_order_id": result.PaymentOrderID, "payment_transaction_id": transactionID, "entitlement_package_id": result.EntitlementPackageID, "provider_refund_id": providerRefundID, "amount_fen": amountFen}); err != nil {
		return ForcedRefundCase{}, err
	}
	return result, tx.Commit(ctx)
}

func getForcedRefundCase(ctx context.Context, tx pgx.Tx, transactionID uuid.UUID, providerRefundID string) (ForcedRefundCase, bool, error) {
	var item ForcedRefundCase
	err := tx.QueryRow(ctx, `SELECT id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_refund_id,amount_fen,status,entitlement_snapshot,created_at,resolved_at,manual_review_case_id,resolution_code,resolution_reason,receivable_id,revoked_base_microcredits,revoked_bonus_microcredits,version FROM forced_refund_cases WHERE payment_transaction_id=$1 AND provider_refund_id=$2`, transactionID, providerRefundID).Scan(&item.ID, &item.PaymentEvidenceID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.WalletID, &item.ProviderRefundID, &item.AmountFen, &item.Status, &item.EntitlementSnapshot, &item.CreatedAt, &item.ResolvedAt, &item.ManualReviewCaseID, &item.ResolutionCode, &item.ResolutionReason, &item.ReceivableID, &item.RevokedBase, &item.RevokedBonus, &item.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

func proportionalCeiling(value, numerator, denominator int64) (int64, error) {
	if value < 0 || numerator < 0 || denominator <= 0 {
		return 0, ErrConflict
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	product.Add(product, big.NewInt(denominator-1))
	product.Div(product, big.NewInt(denominator))
	if !product.IsInt64() {
		return 0, ErrConflict
	}
	return product.Int64(), nil
}

// ApplyReviewedForcedRefund deterministically accounts a provider-initiated
// partial refund after an operator has verified the immutable provider evidence.
// It never uses the ordinary refund state machine and only touches the source
// transaction's package. Any unavailable cash-backed BASE becomes a receivable.
func (r *PostgresRepository) ApplyReviewedForcedRefund(ctx context.Context, caseID uuid.UUID) (ForcedRefundCase, error) {
	if caseID == uuid.Nil {
		return ForcedRefundCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ForcedRefundCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "forced-refund.apply", caseID.String()); err != nil {
		return ForcedRefundCase{}, err
	}
	var source, packageID, orderID, walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT payment_transaction_id,entitlement_package_id,payment_order_id,wallet_id FROM forced_refund_cases WHERE id=$1`, caseID).Scan(&source, &packageID, &orderID, &walletID); errors.Is(err, pgx.ErrNoRows) {
		return ForcedRefundCase{}, ErrNotFound
	} else if err != nil {
		return ForcedRefundCase{}, err
	}
	if err := lockPaymentSourcePackage(ctx, tx, packageID, source, orderID, walletID); err != nil {
		return ForcedRefundCase{}, err
	}
	var item ForcedRefundCase
	var transactionAmountFen int64
	err = tx.QueryRow(ctx, `SELECT forced.id,forced.payment_evidence_id,forced.payment_order_id,forced.payment_transaction_id,forced.entitlement_package_id,forced.wallet_id,forced.provider_refund_id,forced.amount_fen,forced.status,forced.entitlement_snapshot,forced.created_at,forced.resolved_at,forced.manual_review_case_id,forced.resolution_code,forced.resolution_reason,forced.receivable_id,forced.revoked_base_microcredits,forced.revoked_bonus_microcredits,forced.version,transaction.amount_fen FROM forced_refund_cases forced JOIN payment_transactions transaction ON transaction.id=forced.payment_transaction_id WHERE forced.id=$1 FOR UPDATE OF forced FOR NO KEY UPDATE OF transaction`, caseID).Scan(&item.ID, &item.PaymentEvidenceID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.WalletID, &item.ProviderRefundID, &item.AmountFen, &item.Status, &item.EntitlementSnapshot, &item.CreatedAt, &item.ResolvedAt, &item.ManualReviewCaseID, &item.ResolutionCode, &item.ResolutionReason, &item.ReceivableID, &item.RevokedBase, &item.RevokedBonus, &item.Version, &transactionAmountFen)
	if errors.Is(err, pgx.ErrNoRows) {
		return ForcedRefundCase{}, ErrNotFound
	}
	if err != nil {
		return ForcedRefundCase{}, err
	}
	if item.PaymentTransactionID != source || item.EntitlementPackageID != packageID || item.PaymentOrderID != orderID || item.WalletID != walletID {
		return ForcedRefundCase{}, ErrConflict
	}
	if item.Version > 1 {
		if item.Status != "REVIEW_REQUIRED" && !(item.Status == "CONFIRMED" && item.ResolutionCode != nil && *item.ResolutionCode == "CONFIRM_FORCED_REFUND") {
			return ForcedRefundCase{}, ErrConflict
		}
		return item, tx.Commit(ctx)
	}
	if item.Status != "REVIEW_REQUIRED" {
		return ForcedRefundCase{}, ErrConflict
	}
	type componentState struct {
		id, lotID                              uuid.UUID
		kind                                   string
		original, available, active, suspended int64
		target, toRevoke                       int64
	}
	rows, err := tx.Query(ctx, `SELECT id,credit_lot_id,component_type,original_microcredits,available_microcredits,active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE package_id=$1 ORDER BY component_type,id FOR UPDATE`, item.EntitlementPackageID)
	if err != nil {
		return ForcedRefundCase{}, err
	}
	components := []componentState{}
	for rows.Next() {
		var component componentState
		if err := rows.Scan(&component.id, &component.lotID, &component.kind, &component.original, &component.available, &component.active, &component.suspended); err != nil {
			rows.Close()
			return ForcedRefundCase{}, err
		}
		components = append(components, component)
	}
	rows.Close()
	if len(components) == 0 {
		return ForcedRefundCase{}, ErrConflict
	}
	var plannedRevoked, plannedBaseShortfall int64
	for index := range components {
		component := &components[index]
		component.target, err = proportionalCeiling(component.original, item.AmountFen, transactionAmountFen)
		if err != nil {
			return ForcedRefundCase{}, err
		}
		component.toRevoke = component.target
		if revocable := component.available + component.active + component.suspended; component.toRevoke > revocable {
			component.toRevoke = revocable
		}
		plannedRevoked += component.toRevoke
		if component.kind == "BASE" {
			plannedBaseShortfall += component.target - component.toRevoke
		}
	}
	now := r.now()
	entryID := uuid.Must(uuid.NewV7())
	reference := "forced-refund:" + item.ID.String()
	if plannedRevoked+plannedBaseShortfall > 0 {
		if err := insertJournal(ctx, tx, entryID, "FORCED_REFUND", reference, "reviewed provider forced refund", now); err != nil {
			return ForcedRefundCase{}, err
		}
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, item.WalletID)
	if err != nil {
		return ForcedRefundCase{}, err
	}
	var revokedBase, revokedBonus, baseShortfall int64
	for _, component := range components {
		target := component.target
		toRevoke := component.toRevoke
		fromAvailable := toRevoke
		if fromAvailable > component.available {
			fromAvailable = component.available
		}
		fromActive := toRevoke - fromAvailable
		if fromActive > component.active {
			fromActive = component.active
		}
		fromSuspended := toRevoke - fromAvailable - fromActive
		remainingGrantRevoke := fromActive + fromSuspended
		if remainingGrantRevoke > 0 {
			grantRows, err := tx.Query(ctx, `SELECT id,available_microcredits FROM member_quota_grants WHERE component_id=$1 AND available_microcredits>0 ORDER BY id FOR UPDATE`, component.id)
			if err != nil {
				return ForcedRefundCase{}, err
			}
			type revocableGrant struct {
				id        uuid.UUID
				available int64
			}
			grants := []revocableGrant{}
			for grantRows.Next() {
				var grantID uuid.UUID
				var available int64
				if err := grantRows.Scan(&grantID, &available); err != nil {
					grantRows.Close()
					return ForcedRefundCase{}, err
				}
				grants = append(grants, revocableGrant{id: grantID, available: available})
			}
			if err := grantRows.Err(); err != nil {
				grantRows.Close()
				return ForcedRefundCase{}, err
			}
			grantRows.Close()
			for _, grant := range grants {
				if remainingGrantRevoke == 0 {
					break
				}
				amount := remainingGrantRevoke
				if amount > grant.available {
					amount = grant.available
				}
				if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=available_microcredits-$2,revoked_microcredits=revoked_microcredits+$2,status=CASE WHEN available_microcredits=$2 THEN 'REVOKED' ELSE status END,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.id, amount, now); err != nil {
					return ForcedRefundCase{}, err
				}
				remainingGrantRevoke -= amount
			}
			if remainingGrantRevoke != 0 {
				return ForcedRefundCase{}, ErrConflict
			}
		}
		if toRevoke > 0 {
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,active_grant_microcredits=active_grant_microcredits-$3,suspended_grant_microcredits=suspended_grant_microcredits-$4,revoked_microcredits=revoked_microcredits+$5,updated_at=$6 WHERE id=$1 AND available_microcredits>=$2 AND active_grant_microcredits>=$3 AND suspended_grant_microcredits>=$4`, component.id, fromAvailable, fromActive, fromSuspended, toRevoke, now)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return ForcedRefundCase{}, err
			}
			command, err = tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, component.lotID, toRevoke)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return ForcedRefundCase{}, err
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -toRevoke, &component.lotID, now); err != nil {
				return ForcedRefundCase{}, err
			}
		}
		if component.kind == "BASE" {
			revokedBase += toRevoke
			baseShortfall += target - toRevoke
		} else {
			revokedBonus += toRevoke
		}
	}
	totalRevoked := revokedBase + revokedBonus
	if baseShortfall > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -baseShortfall, nil, now); err != nil {
			return ForcedRefundCase{}, err
		}
	}
	if totalRevoked+baseShortfall > 0 {
		if err := insertPosting(ctx, tx, entryID, refundClearingAccount, totalRevoked+baseShortfall, nil, now); err != nil {
			return ForcedRefundCase{}, err
		}
	}
	if totalRevoked > 0 {
		command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits>=$2`, item.WalletID, totalRevoked, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return ForcedRefundCase{}, err
		}
	}
	if baseShortfall > 0 {
		receivable, err := createReceivableWithinTransaction(ctx, tx, item.WalletID, "FORCED_REFUND", reference, baseShortfall, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, nil, now)
		if err != nil {
			return ForcedRefundCase{}, err
		}
		item.ReceivableID = &receivable.ID
	}
	item.RevokedBase, item.RevokedBonus = revokedBase, revokedBonus
	item.Version++
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET status=CASE WHEN revoked_microcredits=original_microcredits THEN 'REVOKED' ELSE 'FROZEN' END,updated_at=$2 WHERE package_id=$1`, item.EntitlementPackageID, now); err != nil {
		return ForcedRefundCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN NOT EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND component.status<>'REVOKED') THEN 'REVOKED' ELSE 'FROZEN' END,updated_at=$2 WHERE id=$1`, item.EntitlementPackageID, now); err != nil {
		return ForcedRefundCase{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE forced_refund_cases SET revoked_base_microcredits=$2,revoked_bonus_microcredits=$3,receivable_id=$4,version=version+1 WHERE id=$1 AND status='REVIEW_REQUIRED'`, item.ID, revokedBase, revokedBonus, item.ReceivableID)
	if err != nil {
		return ForcedRefundCase{}, err
	}
	return item, tx.Commit(ctx)
}
