package billing

// Historical fixture producer, frozen from main 98b6c1324a4a0781577a4e266866478820f61019.
// Only the exact public 900031/900032 schema can execute this model of the
// pre-admission writer. It is not a compatibility path in a shipped binary.
// Current-schema fixtures still use the real admission receipt and SQL guard.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

var errPreAdmissionFixtureSchema = errors.New("exact pre-admission public fixture prefix required")

type preAdmissionFixtureQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func requirePreAdmissionFixtureSchema(ctx context.Context, q preAdmissionFixtureQuerier) error {
	var version int64
	var absent bool
	if err := q.QueryRow(ctx, `SELECT COALESCE(max(version),0),to_regclass('public.payment_credit_attempts') IS NULL FROM public.schema_migrations`).Scan(&version, &absent); err != nil {
		return err
	}
	if !absent || (version != 900031 && version != 900032) {
		return errPreAdmissionFixtureSchema
	}
	return nil
}

func mergeRetainedFixturePayment(ctx context.Context, tx pgx.Tx, p *PaymentCoordinator, order PaymentOrder, evidenceType, evidenceKey string, payload []byte, provider ProviderOrder, repair bool, now time.Time) (PaymentOrder, uuid.UUID, error) {
	var current bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.payment_credit_attempts') IS NOT NULL`).Scan(&current); err != nil {
		return order, uuid.Nil, err
	}
	if current {
		return p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, evidenceType, evidenceKey, payload, provider, repair, now)
	}
	return p.mergePreAdmissionFixturePayment(ctx, tx, order, evidenceType, evidenceKey, payload, provider, repair, now)
}

// Frozen original method SHA256: 6ab107c69302ccd18c37e5c62ccf5b67b4d9a11c856a9cf09adc86a789ecf98b
func (p *PaymentCoordinator) mergePreAdmissionFixturePayment(ctx context.Context, tx pgx.Tx, order PaymentOrder, evidenceType, evidenceKey string, payload []byte, provider ProviderOrder, repair bool, now time.Time) (PaymentOrder, uuid.UUID, error) {
	if err := requirePreAdmissionFixtureSchema(ctx, tx); err != nil {
		return order, uuid.Nil, err
	}
	if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil {
		return order, uuid.Nil, ErrConflict
	}
	valid := providerMatchesOrder(provider, order)
	if !valid {
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, nil, evidenceType, evidenceKey, "REVIEW_REQUIRED", provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary='REVIEW_REQUIRED',status=CASE WHEN status IN ('CREATED','PENDING','EXPIRED') THEN 'MANUAL_REVIEW' ELSE status END,fulfillment_status=CASE WHEN fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE fulfillment_status END,payment_url=NULL,qr_data=NULL,summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return order, uuid.Nil, err
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "order channel amount or currency mismatch"}, now); err != nil {
			return order, uuid.Nil, err
		}
		return order, evidenceID, ErrConflict
	}

	var transaction PaymentTransaction
	err := tx.QueryRow(ctx, `SELECT id,payment_order_id,provider_id,provider_version,provider_transaction_id,channel,status,fulfillment_status,amount_fen,currency,COALESCE(provider_status,''),provider_paid_at,verified_at,fulfilled_at
FROM payment_transactions WHERE provider_transaction_id=$1 FOR NO KEY UPDATE`, *provider.ChannelTransactionID).Scan(
		&transaction.ID, &transaction.PaymentOrderID, &transaction.ProviderID, &transaction.ProviderVersion,
		&transaction.ProviderTransactionID, &transaction.Channel, &transaction.Status, &transaction.FulfillmentStatus,
		&transaction.AmountFen, &transaction.Currency, &transaction.ProviderStatus, &transaction.PaidAt,
		&transaction.VerifiedAt, &transaction.FulfilledAt)
	if err == nil {
		mergeResult := "DUPLICATE"
		if transaction.PaymentOrderID != order.ID || transaction.ProviderID != order.ProviderID || transaction.ProviderVersion != order.ProviderVersion || transaction.AmountFen != order.AmountFen || transaction.Currency != order.Currency {
			mergeResult = "REVIEW_REQUIRED"
		}
		var evidenceTransactionID *uuid.UUID
		if transaction.PaymentOrderID == order.ID {
			evidenceTransactionID = &transaction.ID
		}
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, evidenceTransactionID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if mergeResult == "REVIEW_REQUIRED" {
			if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P0", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "provider transaction conflicts with an existing owner or immutable snapshot"}, now); err != nil {
				return order, uuid.Nil, err
			}
			return order, evidenceID, ErrConflict
		}
		if repair && transaction.FulfillmentStatus == "REVIEW_REQUIRED" {
			if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
				return order, evidenceID, err
			}
			packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, true)
			if err != nil {
				return order, evidenceID, err
			}
			resolvedStatus := "PAID"
			if transaction.PaidAt != nil && (!order.ExpiresAt.After(*transaction.PaidAt) || (order.CloseStatus != "" && order.CloseStatus != "OPEN")) {
				resolvedStatus = "LATE_PAID"
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET status=$2,fulfillment_status='CREDITED',fulfilled_at=$3,updated_at=$3,evidence_version=evidence_version+1 WHERE id=$1 AND fulfillment_status='REVIEW_REQUIRED'`, transaction.ID, resolvedStatus, now); err != nil {
				return order, evidenceID, err
			}
			if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
				"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
				"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
				"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
				"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
			}); err != nil {
				return order, evidenceID, err
			}
		}
		order, err = projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now)
		return order, evidenceID, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return order, uuid.Nil, err
	}

	if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
		return order, uuid.Nil, err
	}
	var priorTransactions int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&priorTransactions); err != nil {
		return order, uuid.Nil, err
	}
	transaction.ID = uuid.Must(uuid.NewV7())
	transactionStatus, fulfillmentStatus, requiresReview := classifyPaymentTransactionAfterClose(order.ExpiresAt, *provider.PaidAt, order.CloseStatus, p.Policy(), priorTransactions, repair)
	_, err = tx.Exec(ctx, `INSERT INTO payment_transactions(
id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,
amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$14,$14)`,
		transaction.ID, order.ID, order.ProviderID, order.ProviderVersion, order.Channel, *provider.ChannelTransactionID,
		transactionStatus, fulfillmentStatus, order.AmountFen, order.Currency, evidenceType, provider.ProviderStatus,
		provider.PaidAt, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	mergeResult := "MERGED"
	if requiresReview {
		mergeResult = "REVIEW_REQUIRED"
	}
	evidenceID, err := p.insertPaymentEvidence(ctx, tx, order, &transaction.ID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	if requiresReview {
		anomalyType := "LATE_PAYMENT_CONFLICT"
		if priorTransactions > 0 {
			anomalyType = "DUPLICATE_PAYMENT"
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, anomalyType, "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentTransactionID: &transaction.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "provider_paid_at": provider.PaidAt, "prior_transaction_count": priorTransactions}, now); err != nil {
			return order, uuid.Nil, err
		}
	}
	if !requiresReview {
		packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, false)
		if err != nil {
			return order, evidenceID, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET fulfillment_status='CREDITED',fulfilled_at=$2,updated_at=$2 WHERE id=$1`, transaction.ID, now); err != nil {
			return order, evidenceID, err
		}
		if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
			"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
			"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
			"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
			"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
		}); err != nil {
			return order, evidenceID, err
		}
	}
	if err := reopenArchivedLatePaymentCase(ctx, tx, order.ID, now); err != nil {
		return order, evidenceID, err
	}
	order, err = projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now)
	return order, evidenceID, err
}

// Frozen original method SHA256: b3ecfddcbc14b9ad54a90cb610c5589fe0ad9933f511c12494ce9cfae7f077d1
func (p *PaymentCoordinator) repairPreAdmissionFixtureOrder(ctx context.Context, orderID, actorID uuid.UUID, reason, commandKey string) (PaymentOrder, error) {
	if err := requirePreAdmissionFixtureSchema(ctx, p.repository.pool); err != nil {
		return PaymentOrder{}, err
	}
	commandKey = strings.TrimSpace(commandKey)
	if len(commandKey) < 16 || len(commandKey) > 128 {
		return PaymentOrder{}, ErrConflict
	}
	return p.repairPreAdmissionFixtureOrderInternal(ctx, orderID, actorID, reason, uuid.Nil, commandKey)
}

// Frozen original method SHA256: fed3f999e670015a3f43685ea822b392cd0532103cd278b87ae24a39bca3071b
func (p *PaymentCoordinator) repairPreAdmissionFixtureOrderInternal(ctx context.Context, orderID, actorID uuid.UUID, reason string, caseID uuid.UUID, commandKey string) (PaymentOrder, error) {
	if err := requirePreAdmissionFixtureSchema(ctx, p.repository.pool); err != nil {
		return PaymentOrder{}, err
	}
	reason = strings.TrimSpace(reason)
	if actorID == uuid.Nil || reason == "" || len(reason) > 500 {
		return PaymentOrder{}, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	directCommand := caseID == uuid.Nil && commandKey != ""
	if directCommand {
		if saved, found, err := readDirectPaymentRepair(ctx, p.repository.pool, orderID, actorID, commandKey, reason); err != nil || found {
			return saved, err
		}
	}
	// Bind the positive repair receipt to this immutable anomaly command. An
	// unrelated historical repair of the same order cannot authorize completion.
	receiptKey := caseID.String() + ":" + commandKey
	digest := sha256.Sum256([]byte(orderID.String() + "|" + actorID.String() + "|" + reason))
	requestHash := hex.EncodeToString(digest[:])
	if caseID != uuid.Nil {
		var savedHash string
		var body []byte
		err := p.repository.pool.QueryRow(ctx, `SELECT request_hash,response_body FROM idempotency_records WHERE scope='payment.anomaly-repair' AND idempotency_key=$1`, receiptKey).Scan(&savedHash, &body)
		if err == nil {
			var result PaymentOrder
			if savedHash != requestHash || json.Unmarshal(body, &result) != nil || result.ID != orderID {
				return PaymentOrder{}, ErrConflict
			}
			return result, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return PaymentOrder{}, err
		}
	}
	if !p.Capabilities().ManualRepairSupported || !p.Capabilities().OrderQuerySupported {
		return PaymentOrder{}, payment.ErrUnsupported
	}
	if p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return PaymentOrder{}, ErrConflict
	}
	if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		return PaymentOrder{}, ErrConflict
	}
	provider, providerErr := providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	// The provider read remains outside the financial transaction. For a direct
	// command, a concurrent commit's receipt has priority over this late read.
	if !directCommand {
		if providerErr != nil {
			return PaymentOrder{}, ErrPaymentProvider
		}
		if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil || provider.MerchantOrderNo != order.MerchantOrderNo || provider.Channel != order.Channel || provider.AmountFen != order.AmountFen || provider.Currency != order.Currency {
			return PaymentOrder{}, ErrConflict
		}
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err = getPaymentOrderByID(ctx, tx, orderID, true)
	if err == nil && directCommand {
		if saved, found, receiptErr := readDirectPaymentRepair(ctx, tx, orderID, actorID, commandKey, reason); receiptErr != nil || found {
			if receiptErr != nil {
				return PaymentOrder{}, receiptErr
			}
			return saved, tx.Commit(ctx)
		}
		if providerErr != nil {
			return PaymentOrder{}, ErrPaymentProvider
		}
		if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil || provider.MerchantOrderNo != order.MerchantOrderNo || provider.Channel != order.Channel || provider.AmountFen != order.AmountFen || provider.Currency != order.Currency {
			return PaymentOrder{}, ErrConflict
		}
	}
	if err != nil || order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		if err != nil {
			return PaymentOrder{}, err
		}
		return PaymentOrder{}, ErrConflict
	}
	now := p.now()
	snapshot, _ := json.Marshal(map[string]any{
		"status": provider.Status, "provider_status": provider.ProviderStatus, "provider_trace_id": provider.ProviderTraceID,
		"provider_transaction_id": *provider.ChannelTransactionID, "paid_at": *provider.PaidAt,
	})
	var balanceBefore int64
	if err := tx.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1 FOR UPDATE`, order.WalletID).Scan(&balanceBefore); err != nil {
		return PaymentOrder{}, err
	}
	order, evidenceID, mergeErr := p.mergePreAdmissionFixturePayment(ctx, tx, order, "MANUAL_REPAIR", "repair:"+order.ID.String()+":"+*provider.ChannelTransactionID, snapshot, provider, true, now)
	if mergeErr != nil {
		if !errors.Is(mergeErr, ErrConflict) {
			return PaymentOrder{}, mergeErr
		}
		if err := tx.Commit(ctx); err != nil {
			return PaymentOrder{}, err
		}
		return order, mergeErr
	}
	var transactionID, packageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT transaction.id,package.id FROM payment_transactions transaction JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id WHERE transaction.payment_order_id=$1 AND transaction.provider_transaction_id=$2`, order.ID, *provider.ChannelTransactionID).Scan(&transactionID, &packageID); err != nil {
		return PaymentOrder{}, err
	}
	var balanceAfter int64
	if err := tx.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, order.WalletID).Scan(&balanceAfter); err != nil {
		return PaymentOrder{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_order_repairs(id,payment_order_id,payment_transaction_id,payment_evidence_id,actor_id,reason,provider_snapshot,entitlement_package_id,points_awarded,wallet_balance_before,wallet_balance_after,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (payment_transaction_id) DO NOTHING`, uuid.Must(uuid.NewV7()), order.ID, transactionID, evidenceID, actorID, reason, snapshot, packageID, order.CreditMicrocredits, balanceBefore, balanceAfter, now); err != nil {
		return PaymentOrder{}, err
	}
	if caseID != uuid.Nil {
		body, err := json.Marshal(order)
		if err != nil {
			return PaymentOrder{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,request_hash,response_status,response_body,created_at,expires_at) VALUES('payment.anomaly-repair',$1,$2,200,$3,$4,$4::timestamptz+interval '100 years')`, receiptKey, requestHash, body, now); err != nil {
			return PaymentOrder{}, err
		}
	}
	if directCommand {
		// Use the actual database projection, including timestamp precision.
		order, err = getPaymentOrderByID(ctx, tx, orderID, false)
		if err != nil {
			return PaymentOrder{}, err
		}
		body, err := json.Marshal(order)
		if err != nil {
			return PaymentOrder{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO payment_order_repair_commands(id,payment_order_id,actor_id,idempotency_key,command_protocol,reason,request_hash,response_body,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), orderID, actorID, commandKey, reason, directPaymentRepairHash(orderID, actorID, reason), body, now); err != nil {
			return PaymentOrder{}, directPaymentRepairWriteError(err)
		}
		var found bool
		order, found, err = readDirectPaymentRepair(ctx, tx, orderID, actorID, commandKey, reason)
		if err != nil || !found {
			if err == nil {
				err = ErrConflict
			}
			return PaymentOrder{}, err
		}
	}
	return order, tx.Commit(ctx)
}
