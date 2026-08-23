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
	AmountFen            int64           `json:"amount_fen"`
	Status               string          `json:"status"`
	EntitlementSnapshot  json.RawMessage `json:"entitlement_snapshot"`
	CreatedAt            time.Time       `json:"created_at"`
	ResolvedAt           *time.Time      `json:"resolved_at,omitempty"`
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
		return existing, tx.Commit(ctx)
	}

	var result ForcedRefundCase
	var providerID, providerVersion, merchantOrderNo, providerTransactionID, currency string
	var transactionAmountFen int64
	err = tx.QueryRow(ctx, `SELECT transaction.payment_order_id,transaction.provider_id,transaction.provider_version,payment.merchant_order_no,transaction.provider_transaction_id,transaction.amount_fen,transaction.currency,package.id,package.wallet_id
		FROM payment_transactions transaction
		JOIN payment_orders payment ON payment.id=transaction.payment_order_id
		JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id AND package.payment_order_id=transaction.payment_order_id
		WHERE transaction.id=$1 AND transaction.status IN ('PAID','LATE_PAID')
		FOR UPDATE OF transaction,package`, transactionID).Scan(&result.PaymentOrderID, &providerID, &providerVersion, &merchantOrderNo, &providerTransactionID, &transactionAmountFen, &currency, &result.EntitlementPackageID, &result.WalletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ForcedRefundCase{}, ErrNotFound
	}
	if err != nil {
		return ForcedRefundCase{}, err
	}
	if amountFen > transactionAmountFen {
		return ForcedRefundCase{}, ErrConflict
	}
	result.ID = uuid.Must(uuid.NewV7())
	result.PaymentTransactionID = transactionID
	result.ProviderRefundID = providerRefundID
	result.AmountFen = amountFen
	result.Status = "REVIEW_REQUIRED"
	result.CreatedAt = r.now()
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
		'package',to_jsonb(package),
		'components',COALESCE((SELECT jsonb_agg(to_jsonb(component) ORDER BY component.id) FROM entitlement_components component WHERE component.package_id=package.id),'[]'::jsonb),
		'grants',COALESCE((SELECT jsonb_agg(to_jsonb(grant) ORDER BY grant.id) FROM member_quota_grants grant WHERE grant.package_id=package.id),'[]'::jsonb)
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
	if _, err := tx.Exec(ctx, `INSERT INTO forced_refund_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_refund_id,amount_fen,status,entitlement_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'REVIEW_REQUIRED',$9,$10)`, result.ID, result.PaymentEvidenceID, result.PaymentOrderID, transactionID, result.EntitlementPackageID, result.WalletID, providerRefundID, amountFen, result.EntitlementSnapshot, result.CreatedAt); err != nil {
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
	err := tx.QueryRow(ctx, `SELECT id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,provider_refund_id,amount_fen,status,entitlement_snapshot,created_at,resolved_at FROM forced_refund_cases WHERE payment_transaction_id=$1 AND provider_refund_id=$2`, transactionID, providerRefundID).Scan(&item.ID, &item.PaymentEvidenceID, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.WalletID, &item.ProviderRefundID, &item.AmountFen, &item.Status, &item.EntitlementSnapshot, &item.CreatedAt, &item.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}
