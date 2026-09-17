package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) RecordConfirmedChargebackForReview(ctx context.Context, transactionID uuid.UUID, providerChargebackID string, payload json.RawMessage) (ChargebackCase, error) {
	providerChargebackID = strings.TrimSpace(providerChargebackID)
	if transactionID == uuid.Nil || providerChargebackID == "" || len(providerChargebackID) > 200 {
		return ChargebackCase{}, ErrConflict
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return ChargebackCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ChargebackCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "chargeback.evidence", transactionID.String()+":"+providerChargebackID); err != nil {
		return ChargebackCase{}, err
	}
	var orderID uuid.UUID
	var providerID, providerVersion, merchantOrderNo, providerTransactionID, currency string
	var amountFen int64
	err = tx.QueryRow(ctx, `SELECT transaction.payment_order_id,transaction.provider_id,transaction.provider_version,payment.merchant_order_no,transaction.provider_transaction_id,transaction.amount_fen,transaction.currency FROM payment_transactions transaction JOIN payment_orders payment ON payment.id=transaction.payment_order_id WHERE transaction.id=$1 AND transaction.status IN ('PAID','LATE_PAID') FOR UPDATE OF transaction`, transactionID).Scan(&orderID, &providerID, &providerVersion, &merchantOrderNo, &providerTransactionID, &amountFen, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargebackCase{}, ErrNotFound
	}
	if err != nil {
		return ChargebackCase{}, err
	}
	var existing ChargebackCase
	err = tx.QueryRow(ctx, chargebackCaseSelect+` WHERE payment_transaction_id=$1 AND provider_chargeback_id=$2`, transactionID, providerChargebackID).Scan(chargebackCaseScanTargets(&existing)...)
	if err == nil {
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChargebackCase{}, err
	}
	digest := sha256.Sum256(payload)
	evidenceID := uuid.Must(uuid.NewV7())
	command, err := tx.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result) VALUES($1,$2,$3,'CHARGEBACK',$4,$5,$6,$7,$8,$9,$10,'CHARGEBACK_CONFIRMED',$11,$11,$12,$13,'RECORDED') ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING`, evidenceID, orderID, transactionID, providerID, providerVersion, providerChargebackID, merchantOrderNo, providerTransactionID, amountFen, currency, r.now(), hex.EncodeToString(digest[:]), payload)
	if err != nil {
		return ChargebackCase{}, err
	}
	if command.RowsAffected() == 0 {
		if err := tx.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id=$1 AND provider_version=$2 AND evidence_type='CHARGEBACK' AND evidence_key=$3 AND payload_digest=$4`, providerID, providerVersion, providerChargebackID, hex.EncodeToString(digest[:])).Scan(&evidenceID); err != nil {
			return ChargebackCase{}, err
		}
	}
	var packageID, walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id,wallet_id FROM entitlement_packages WHERE source_payment_transaction_id=$1 AND payment_order_id=$2 FOR UPDATE`, transactionID, orderID).Scan(&packageID, &walletID); err != nil {
		return ChargebackCase{}, err
	}
	now := r.now()
	result := ChargebackCase{ID: uuid.Must(uuid.NewV7()), PaymentEvidenceID: evidenceID, PaymentOrderID: orderID, PaymentTransactionID: transactionID, EntitlementPackageID: packageID, WalletID: walletID, ProviderChargebackID: providerChargebackID, Status: "REVIEW_REQUIRED", CreatedAt: now, Version: 1}
	_, err = tx.Exec(ctx, `INSERT INTO chargeback_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_chargeback_id,status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'REVIEW_REQUIRED',$8)`, result.ID, evidenceID, orderID, transactionID, packageID, walletID, providerChargebackID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN',updated_at=$2 WHERE id=$1 AND status NOT IN ('REVOKED','CREDIT_FAILED')`, packageID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN',updated_at=$2 WHERE package_id=$1 AND status<>'REVOKED'`, packageID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason='CHARGEBACK_REVIEW',allocation_version=allocation_version+1,updated_at=$2 WHERE package_id=$1 AND status='ACTIVE'`, packageID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits+active_grant_microcredits,active_grant_microcredits=0,updated_at=$2 WHERE package_id=$1 AND active_grant_microcredits>0`, packageID, now)
	}
	if err != nil {
		return ChargebackCase{}, err
	}
	anomaly, err := createPaymentAnomalyWithinTransaction(ctx, tx, "CHARGEBACK", "P1", "chargeback:"+result.ID.String(), paymentAnomalyReference{PaymentOrderID: &orderID, PaymentTransactionID: &transactionID, PaymentEvidenceID: &evidenceID, ChargebackCaseID: &result.ID}, map[string]any{"provider_chargeback_id": providerChargebackID, "provider_transaction_id": providerTransactionID, "amount_fen": amountFen, "payload_digest": hex.EncodeToString(digest[:])}, now)
	if err != nil {
		return ChargebackCase{}, err
	}
	result.ManualReviewCaseID = &anomaly.ID
	if _, err := tx.Exec(ctx, `UPDATE chargeback_cases SET manual_review_case_id=$2 WHERE id=$1`, result.ID, anomaly.ID); err != nil {
		return ChargebackCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET chargeback_summary=chargeback_summary||jsonb_build_object('review_count',(SELECT count(*) FROM chargeback_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),attention_summary='REVIEW_REQUIRED',summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, orderID, now); err != nil {
		return ChargebackCase{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.chargeback.review-required.v1", result.ID, 1, map[string]any{"chargeback_case_id": result.ID, "manual_review_case_id": anomaly.ID, "payment_order_id": orderID, "payment_transaction_id": transactionID}); err != nil {
		return ChargebackCase{}, err
	}
	return result, tx.Commit(ctx)
}

type ChargebackCase struct {
	ID                       uuid.UUID  `json:"chargeback_case_id"`
	PaymentEvidenceID        uuid.UUID  `json:"payment_evidence_id"`
	PaymentOrderID           uuid.UUID  `json:"payment_order_id"`
	PaymentTransactionID     uuid.UUID  `json:"payment_transaction_id"`
	EntitlementPackageID     uuid.UUID  `json:"entitlement_package_id"`
	WalletID                 uuid.UUID  `json:"wallet_id"`
	ProviderChargebackID     string     `json:"provider_chargeback_id"`
	Status                   string     `json:"status"`
	RevokedBaseMicrocredits  int64      `json:"revoked_base_microcredits"`
	RevokedBonusMicrocredits int64      `json:"revoked_bonus_microcredits"`
	ReceivableID             *uuid.UUID `json:"receivable_id,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	ResolvedAt               *time.Time `json:"resolved_at,omitempty"`
	ManualReviewCaseID       *uuid.UUID `json:"manual_review_case_id,omitempty"`
	ResolutionCode           *string    `json:"resolution_code,omitempty"`
	ResolutionReason         *string    `json:"resolution_reason,omitempty"`
	Version                  int64      `json:"version"`
}

func (r *PostgresRepository) ApplyConfirmedChargeback(ctx context.Context, evidenceID uuid.UUID, providerChargebackID string) (ChargebackCase, error) {
	providerChargebackID = strings.TrimSpace(providerChargebackID)
	if evidenceID == uuid.Nil || providerChargebackID == "" || len(providerChargebackID) > 200 {
		return ChargebackCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ChargebackCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "chargeback.apply", evidenceID.String()); err != nil {
		return ChargebackCase{}, err
	}
	existing, found, err := getChargebackCase(ctx, tx, evidenceID)
	if err != nil {
		return ChargebackCase{}, err
	}
	if !found || existing.ProviderChargebackID != providerChargebackID || existing.Status == "DISMISSED" {
		return ChargebackCase{}, ErrConflict
	}
	if existing.Status == "CONFIRMED" {
		return existing, tx.Commit(ctx)
	}

	var orderID, transactionID, packageID, walletID uuid.UUID
	var providerStatus, transactionStatus string
	err = tx.QueryRow(ctx, `SELECT evidence.payment_order_id,evidence.payment_transaction_id,COALESCE(evidence.provider_status,''),transaction.status,package.id,package.wallet_id
		FROM payment_evidence evidence
		JOIN payment_transactions transaction ON transaction.id=evidence.payment_transaction_id AND transaction.payment_order_id=evidence.payment_order_id
		JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id AND package.payment_order_id=evidence.payment_order_id
		WHERE evidence.id=$1 FOR UPDATE OF transaction,package`, evidenceID).Scan(&orderID, &transactionID, &providerStatus, &transactionStatus, &packageID, &walletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargebackCase{}, ErrNotFound
	}
	if err != nil {
		return ChargebackCase{}, err
	}
	if providerStatus != "CHARGEBACK_CONFIRMED" || (transactionStatus != "PAID" && transactionStatus != "LATE_PAID") {
		return ChargebackCase{}, ErrConflict
	}
	if _, err := getWallet(ctx, tx, walletID, true); err != nil {
		return ChargebackCase{}, err
	}

	type componentState struct {
		id, lotID                              uuid.UUID
		kind                                   string
		original, available, active, suspended int64
	}
	rows, err := tx.Query(ctx, `SELECT id,credit_lot_id,component_type,original_microcredits,reclassified_out_microcredits,available_microcredits,active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE package_id=$1 ORDER BY id FOR UPDATE`, packageID)
	if err != nil {
		return ChargebackCase{}, err
	}
	components := []componentState{}
	for rows.Next() {
		var item componentState
		var historicalOriginal, reclassifiedOut int64
		if err := rows.Scan(&item.id, &item.lotID, &item.kind, &historicalOriginal, &reclassifiedOut, &item.available, &item.active, &item.suspended); err != nil {
			rows.Close()
			return ChargebackCase{}, err
		}
		item.original, err = effectiveComponentOriginal(historicalOriginal, reclassifiedOut)
		if err != nil {
			rows.Close()
			return ChargebackCase{}, err
		}
		components = append(components, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ChargebackCase{}, err
	}
	rows.Close()
	if len(components) == 0 {
		return ChargebackCase{}, ErrConflict
	}

	now := r.now()
	entryID := uuid.Must(uuid.NewV7())
	chargebackReference := "payment-transaction:" + transactionID.String() + ":chargeback:" + providerChargebackID
	if err := insertJournal(ctx, tx, entryID, "CHARGEBACK", chargebackReference, "confirmed provider chargeback", now); err != nil {
		return ChargebackCase{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, walletID)
	if err != nil {
		return ChargebackCase{}, err
	}
	var revokedBase, revokedBonus, baseShortfall int64
	for _, component := range components {
		revocable := component.available + component.active + component.suspended
		if revocable > 0 {
			if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET revoked_microcredits=revoked_microcredits+available_microcredits,available_microcredits=0,status='REVOKED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$2 WHERE component_id=$1 AND available_microcredits>0`, component.id, now); err != nil {
				return ChargebackCase{}, err
			}
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=0,active_grant_microcredits=0,suspended_grant_microcredits=0,revoked_microcredits=revoked_microcredits+$2,updated_at=$3 WHERE id=$1 AND available_microcredits=$4 AND active_grant_microcredits=$5 AND suspended_grant_microcredits=$6`, component.id, revocable, now, component.available, component.active, component.suspended)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return ChargebackCase{}, err
			}
			command, err = tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, component.lotID, revocable)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return ChargebackCase{}, err
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -revocable, &component.lotID, now); err != nil {
				return ChargebackCase{}, err
			}
		}
		if component.kind == "BASE" {
			revokedBase += revocable
			baseShortfall += component.original - revocable
		} else {
			revokedBonus += revocable
		}
	}
	totalRevoked := revokedBase + revokedBonus
	if baseShortfall > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -baseShortfall, nil, now); err != nil {
			return ChargebackCase{}, err
		}
	}
	if totalRevoked+baseShortfall > 0 {
		if err := insertPosting(ctx, tx, entryID, chargebackClearingAccount, totalRevoked+baseShortfall, nil, now); err != nil {
			return ChargebackCase{}, err
		}
	}
	if totalRevoked > 0 {
		command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits>=$2`, walletID, totalRevoked, now)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return ChargebackCase{}, err
		}
	}

	var receivableID *uuid.UUID
	if baseShortfall > 0 {
		receivable, err := createReceivableWithinTransaction(ctx, tx, walletID, "CHARGEBACK", chargebackReference, baseShortfall, &orderID, &transactionID, &packageID, nil, now)
		if err != nil {
			return ChargebackCase{}, err
		}
		receivableID = &receivable.ID
	}
	result := existing
	result.Status, result.RevokedBaseMicrocredits, result.RevokedBonusMicrocredits, result.ReceivableID, result.ResolvedAt = "CONFIRMED", revokedBase, revokedBonus, receivableID, &now
	result.Version++
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET status=CASE WHEN revoked_microcredits=original_microcredits-reclassified_out_microcredits THEN 'REVOKED' ELSE 'FROZEN' END,updated_at=$2 WHERE package_id=$1`, packageID, now); err != nil {
		return ChargebackCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN NOT EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND component.status<>'REVOKED') THEN 'REVOKED' ELSE 'FROZEN' END,updated_at=$2 WHERE id=$1`, packageID, now); err != nil {
		return ChargebackCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE chargeback_cases SET status='CONFIRMED',revoked_base_microcredits=$2,revoked_bonus_microcredits=$3,receivable_id=$4,resolved_at=$5,version=version+1 WHERE id=$1 AND status='REVIEW_REQUIRED'`, result.ID, revokedBase, revokedBonus, receivableID, now); err != nil {
		return ChargebackCase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders payment_order SET chargeback_summary=(SELECT jsonb_build_object('open_count',count(*) FILTER(WHERE chargeback.status='CONFIRMED'),'lost_amount_fen',COALESCE(sum(transaction.amount_fen) FILTER(WHERE chargeback.status='CONFIRMED'),0)) FROM chargeback_cases chargeback JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id WHERE chargeback.payment_order_id=$1),attention_summary=CASE WHEN EXISTS(SELECT 1 FROM receivables receivable WHERE receivable.payment_order_id=$1 AND receivable.outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN' WHEN payment_order.attention_summary='RECEIVABLE_OPEN' THEN 'NORMAL' ELSE payment_order.attention_summary END,summary_version=summary_version+1,updated_at=$2 WHERE payment_order.id=$1`, orderID, now); err != nil {
		return ChargebackCase{}, err
	}
	return result, tx.Commit(ctx)
}

func getChargebackCase(ctx context.Context, tx pgx.Tx, evidenceID uuid.UUID) (ChargebackCase, bool, error) {
	var item ChargebackCase
	err := tx.QueryRow(ctx, chargebackCaseSelect+` WHERE payment_evidence_id=$1`, evidenceID).Scan(chargebackCaseScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

const chargebackCaseSelect = `SELECT id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_chargeback_id,status,revoked_base_microcredits,revoked_bonus_microcredits,receivable_id,created_at,resolved_at,manual_review_case_id,resolution_code,resolution_reason,version FROM chargeback_cases`

func chargebackCaseScanTargets(item *ChargebackCase) []any {
	return []any{&item.ID, &item.PaymentEvidenceID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.WalletID, &item.ProviderChargebackID, &item.Status, &item.RevokedBaseMicrocredits, &item.RevokedBonusMicrocredits, &item.ReceivableID, &item.CreatedAt, &item.ResolvedAt, &item.ManualReviewCaseID, &item.ResolutionCode, &item.ResolutionReason, &item.Version}
}
