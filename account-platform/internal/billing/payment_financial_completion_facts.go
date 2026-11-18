package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A completion acknowledges an existing owner receipt. It never executes a
// repair or issues money/rights; callers can name a receipt but cannot supply
// its financial facts.
type PaymentFinancialCompletionInput struct {
	ProofKind   string    `json:"proof_kind"`
	ReferenceID uuid.UUID `json:"reference_id"`
}

type PaymentFinancialCompletionFacts struct {
	ProofKind             string          `json:"proof_kind"`
	ReferenceID           uuid.UUID       `json:"reference_id"`
	OrderID               uuid.UUID       `json:"payment_order_id"`
	EvidenceID            uuid.UUID       `json:"payment_evidence_id"`
	TransactionID         *uuid.UUID      `json:"payment_transaction_id,omitempty"`
	ProviderID            string          `json:"provider_id"`
	ProviderVersion       string          `json:"provider_version"`
	ProviderTransactionID string          `json:"provider_transaction_id,omitempty"`
	OrderAmountFen        int64           `json:"order_amount_fen,string"`
	AmountFen             int64           `json:"amount_fen,string"`
	Currency              string          `json:"currency"`
	Receipt               json.RawMessage `json:"receipt"`
	Digest                string          `json:"facts_digest"`
}

type paymentCompletionQuery struct {
	StartedAt time.Time
	Order     ProviderOrder
}

func paymentCompletionEligible(item PaymentAnomalyCase) bool {
	switch item.AnomalyType {
	case "DUPLICATE_PAYMENT", "LATE_PAYMENT_CONFLICT", "PAYMENT_FACT_CONFLICT", "ILLEGAL_TRANSITION":
		return item.Status == "RESOLVED" && item.ResolutionCode != nil && *item.ResolutionCode == "ESCALATE_FINANCIAL_REPAIR" && item.FinancialActionStatus == "PENDING" && item.PaymentOrderID != nil
	default:
		return false
	}
}

func paymentCompletionDigest(value any) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// Read-only discovery exposes usable owner receipts through the existing
// detail permission. It is a snapshot for operators, never an authorization to
// confirm: proposal and confirmation each recompute under the financial gate.
func (r *PostgresRepository) paymentCompletionCandidates(ctx context.Context, caseID uuid.UUID) ([]PaymentFinancialCompletionFacts, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1`, caseID))
	if err != nil {
		return nil, err
	}
	if !found || !paymentCompletionEligible(item) || item.PaymentEvidenceID == nil {
		return nil, nil
	}
	order, err := getPaymentOrderByID(ctx, tx, *item.PaymentOrderID, false)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH original_transaction AS (
SELECT transaction.id FROM payment_transactions transaction JOIN payment_evidence evidence ON evidence.payment_order_id=transaction.payment_order_id
 AND evidence.provider_id=transaction.provider_id AND evidence.provider_version=transaction.provider_version AND evidence.provider_transaction_id=transaction.provider_transaction_id
WHERE evidence.id=$1 AND transaction.payment_order_id=$2
) SELECT 'PAYMENT_REPAIR',repair.id FROM payment_order_repairs repair JOIN original_transaction ON original_transaction.id=repair.payment_transaction_id
UNION ALL SELECT 'REFUND',refund.id FROM refunds refund JOIN original_transaction ON original_transaction.id=refund.payment_transaction_id WHERE refund.status='SUCCEEDED' AND refund.amount_fen=$3
ORDER BY 1,2 LIMIT 20`, *item.PaymentEvidenceID, order.ID, order.AmountFen)
	if err != nil {
		return nil, err
	}
	var inputs []PaymentFinancialCompletionInput
	for rows.Next() {
		var input PaymentFinancialCompletionInput
		if err := rows.Scan(&input.ProofKind, &input.ReferenceID); err != nil {
			rows.Close()
			return nil, err
		}
		inputs = append(inputs, input)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []PaymentFinancialCompletionFacts
	for _, input := range inputs {
		facts, err := paymentCompletionFacts(ctx, tx, item, order, input, nil)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, facts)
	}
	return result, tx.Commit(ctx)
}

func paymentCompletionFacts(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, order PaymentOrder, input PaymentFinancialCompletionInput, query *paymentCompletionQuery) (PaymentFinancialCompletionFacts, error) {
	if !paymentCompletionEligible(item) || *item.PaymentOrderID != order.ID {
		return PaymentFinancialCompletionFacts{}, ErrConflict
	}
	facts := PaymentFinancialCompletionFacts{ProofKind: input.ProofKind, ReferenceID: input.ReferenceID, OrderID: order.ID, ProviderID: order.ProviderID, ProviderVersion: order.ProviderVersion, OrderAmountFen: order.AmountFen}
	if item.PaymentEvidenceID == nil {
		if input.ProofKind != "NO_FINANCIAL_EFFECT" {
			return facts, ErrConflict
		}
		return paymentCompletionNoEffect(ctx, tx, item, order, input, query, facts, nil, nil, "")
	}
	facts.EvidenceID = *item.PaymentEvidenceID
	var evidenceTransaction *uuid.UUID
	var providerTransaction *string
	var amount *int64
	var currency, status, merchantOrder, providerID, providerVersion string
	var evidencePayload json.RawMessage
	err := tx.QueryRow(ctx, `SELECT payment_transaction_id,provider_transaction_id,amount_fen,COALESCE(currency,''),COALESCE(provider_status,''),merchant_order_no,provider_id,provider_version,payload_snapshot FROM payment_evidence WHERE id=$1 AND payment_order_id=$2`, facts.EvidenceID, order.ID).Scan(&evidenceTransaction, &providerTransaction, &amount, &currency, &status, &merchantOrder, &providerID, &providerVersion, &evidencePayload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return facts, ErrConflict
		}
		return facts, err
	}
	if providerID != order.ProviderID || providerVersion != order.ProviderVersion || merchantOrder != order.MerchantOrderNo || order.ProviderID == "" || order.ProviderVersion == "" {
		return facts, ErrConflict
	}
	var providerFacts struct {
		Customer *string `json:"external_customer_key"`
		SKU      *string `json:"external_sku"`
		Channel  *string `json:"channel"`
	}
	if amount == nil || *amount != order.AmountFen || currency != order.Currency || json.Unmarshal(evidencePayload, &providerFacts) != nil || providerFacts.Customer == nil || *providerFacts.Customer != order.RequesterID.String() || providerFacts.Channel == nil || *providerFacts.Channel != order.Channel || (providerFacts.SKU == nil && order.ProviderSKU != "") || (providerFacts.SKU != nil && *providerFacts.SKU != order.ProviderSKU) {
		return facts, ErrConflict
	}
	if input.ProofKind == "NO_FINANCIAL_EFFECT" {
		return paymentCompletionNoEffect(ctx, tx, item, order, input, query, facts, evidenceTransaction, providerTransaction, status)
	}
	if input.ReferenceID == uuid.Nil || amount == nil || *amount <= 0 || currency != order.Currency || *amount != order.AmountFen || providerTransaction == nil || strings.TrimSpace(*providerTransaction) == "" || !paymentFinancialRepairSuccessStatuses[strings.ToUpper(strings.TrimSpace(status))] {
		return facts, ErrConflict
	}
	facts.AmountFen, facts.Currency, facts.ProviderTransactionID = *amount, currency, *providerTransaction
	var transactionID uuid.UUID
	var transactionStatus, fulfillment string
	err = tx.QueryRow(ctx, `SELECT id,status,fulfillment_status FROM payment_transactions WHERE payment_order_id=$1 AND provider_id=$2 AND provider_version=$3 AND provider_transaction_id=$4 AND amount_fen=$5 AND currency=$6 AND channel=$7`, order.ID, order.ProviderID, order.ProviderVersion, *providerTransaction, *amount, currency, order.Channel).Scan(&transactionID, &transactionStatus, &fulfillment)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return facts, ErrConflict
		}
		return facts, err
	}
	if (item.PaymentTransactionID != nil && *item.PaymentTransactionID != transactionID) || (evidenceTransaction != nil && *evidenceTransaction != transactionID) || (transactionStatus != "PAID" && transactionStatus != "LATE_PAID") || fulfillment != "CREDITED" {
		return facts, ErrConflict
	}
	facts.TransactionID = &transactionID
	switch input.ProofKind {
	case "PAYMENT_REPAIR":
		// Both the existing reviewed-order writer and approval-driven writer
		// commit the repair with its actual journal/package provenance. Their
		// source evidence can differ from the anomaly's original observation,
		// but must identify exactly the same original money movement.
		err = tx.QueryRow(ctx, `SELECT jsonb_build_object('repair_id',repair.id,'approval_request_id',repair.approval_request_id,'authorization_event_id',repair.authorization_event_id,'approval_epoch',repair.approval_epoch,'execution_key_digest',repair.execution_key_digest,'facts_digest',repair.facts_digest,'requester_id',repair.requester_user_id,'approver_id',repair.actor_id,'package_id',package.id,'journal_id',journal.id,'points_awarded',repair.points_awarded::text,'balance_before',repair.wallet_balance_before::text,'balance_after',repair.wallet_balance_after::text,'created_at',repair.created_at)
FROM payment_order_repairs repair
JOIN entitlement_packages package ON package.id=repair.entitlement_package_id AND package.source_payment_transaction_id=repair.payment_transaction_id AND package.payment_order_id=repair.payment_order_id AND package.wallet_id=$5
JOIN payment_evidence repair_evidence ON repair_evidence.id=repair.payment_evidence_id AND repair_evidence.payment_order_id=repair.payment_order_id
 AND repair_evidence.provider_id=$4 AND repair_evidence.provider_version=$6 AND repair_evidence.provider_transaction_id=$7
 AND repair_evidence.amount_fen=$8 AND repair_evidence.currency=$9 AND UPPER(COALESCE(repair_evidence.provider_status,'')) IN ('PAID','PAY_SUCCESS','SUCCESS')
JOIN journal_entries journal ON journal.entry_type='REPAIR_CREDIT' AND journal.business_reference='repair-payment-transaction:'||repair.payment_transaction_id::text
WHERE repair.id=$1 AND repair.payment_order_id=$2 AND repair.payment_transaction_id=$3
AND repair.points_awarded>0 AND package.points_total=repair.points_awarded`, input.ReferenceID, order.ID, transactionID, order.ProviderID, order.WalletID, order.ProviderVersion, facts.ProviderTransactionID, facts.AmountFen, facts.Currency).Scan(&facts.Receipt)
	case "REFUND":
		// A refund for another transaction of the same order is not evidence
		// that this anomalous payment was returned.
		err = tx.QueryRow(ctx, `SELECT jsonb_build_object('refund_id',refund.id,'merchant_refund_no',refund.merchant_refund_no,'provider_refund_id',refund.channel_refund_id,'amount_fen',refund.amount_fen::text,'package_id',package.id,'credit_microcredits',refund.credit_microcredits::text,'journal_id',journal.id,'created_at',refund.created_at,'completed_at',refund.updated_at)
FROM refunds refund
JOIN entitlement_packages package ON package.id=refund.entitlement_package_id AND package.source_payment_transaction_id=refund.payment_transaction_id AND package.payment_order_id=refund.payment_order_id AND package.wallet_id=$4
JOIN journal_entries journal ON journal.entry_type='REFUND' AND journal.business_reference='refund:'||refund.id::text
WHERE refund.id=$1 AND refund.payment_order_id=$2 AND refund.payment_transaction_id=$3 AND refund.amount_fen=$5
AND refund.status='SUCCEEDED' AND length(trim(refund.channel_refund_id))>0
AND package.status='REVOKED' AND package.points_revoked=refund.credit_microcredits
AND package.points_remaining=0 AND package.points_reserved=0 AND package.points_used=0 AND package.points_expired=0`, input.ReferenceID, order.ID, transactionID, order.WalletID, facts.AmountFen).Scan(&facts.Receipt)
	default:
		return facts, ErrConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return facts, ErrConflict
	}
	if err != nil {
		return facts, err
	}
	// Include the original immutable evidence, so reinterpreting or replacing
	// its owner facts cannot silently approve a stale proposal.
	facts.Digest = paymentCompletionDigest(struct {
		Facts    PaymentFinancialCompletionFacts
		Evidence json.RawMessage
	}{facts, evidencePayload})
	return facts, nil
}

func paymentCompletionNoEffect(ctx context.Context, tx pgx.Tx, item PaymentAnomalyCase, order PaymentOrder, input PaymentFinancialCompletionInput, query *paymentCompletionQuery, facts PaymentFinancialCompletionFacts, evidenceTransaction *uuid.UUID, providerTransaction *string, evidenceStatus string) (PaymentFinancialCompletionFacts, error) {
	if input.ReferenceID != uuid.Nil || query == nil || query.StartedAt.IsZero() || !providerFailureMatchesOrder(query.Order, order) || (item.PaymentEvidenceID != nil && !strings.EqualFold(strings.TrimSpace(evidenceStatus), "FAILED")) || evidenceTransaction != nil || providerTransaction != nil || order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		return facts, ErrConflict
	}
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT $2>clock_timestamp() OR $2<clock_timestamp()-interval '30 seconds'
OR EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1)
OR EXISTS(SELECT 1 FROM entitlement_packages WHERE payment_order_id=$1)
OR EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND (received_at>$2 OR payment_transaction_id IS NOT NULL OR provider_transaction_id IS NOT NULL OR UPPER(COALESCE(provider_status,'')) IN ('PAID','PAY_SUCCESS','SUCCESS')))
OR EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$1 AND id<>$3 AND (status='OPEN' OR financial_action_status='PENDING'))
OR EXISTS(SELECT 1 FROM late_payment_review_cases review JOIN late_payment_review_evidence evidence ON evidence.case_id=review.id WHERE review.payment_order_id=$1 AND evidence.evidence_type='USER_PAYMENT_CLAIM')`, order.ID, query.StartedAt, item.ID).Scan(&blocked)
	if err != nil {
		return facts, err
	}
	if blocked {
		return facts, ErrConflict
	}
	facts.AmountFen, facts.Currency = order.AmountFen, order.Currency
	// Volatile trace/time fields are recorded separately by the proposal and
	// confirmation; semantic FAILED facts must agree across both live queries.
	facts.Receipt, _ = json.Marshal(map[string]any{"merchant_order_no": order.MerchantOrderNo, "provider_status": "FAILED", "external_customer_key": query.Order.ExternalCustomerKey, "external_sku": query.Order.ExternalSKU, "channel": order.Channel})
	facts.Digest = paymentCompletionDigest(facts)
	return facts, nil
}
