package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// paymentFinancialRepairCommand is the closed command name shared by the proof
// authority, the activation projection and the consumer mapping.
const paymentFinancialRepairCommand = "PAYMENT_FINANCIAL_REPAIR"

// paymentFinancialRepairReason is the only reason code the first release admits.
const paymentFinancialRepairReason = "CONFIRMED_PAYMENT_NOT_CREDITED"

// paymentFinancialRepairSuccessStatuses is the closed set of raw provider
// statuses that the installed adapters persist for a successful payment. A new
// adapter must extend this set explicitly; an unknown status fails closed
// instead of being treated as success.
var paymentFinancialRepairSuccessStatuses = map[string]bool{
	"PAY_SUCCESS": true, "SUCCESS": true, "PAID": true, "TRADE_SUCCESS": true,
}

type PaymentFinancialRepairResult struct {
	RepairID              uuid.UUID `json:"repair_id"`
	PaymentOrderID        uuid.UUID `json:"payment_order_id"`
	PaymentTransactionID  uuid.UUID `json:"payment_transaction_id"`
	EntitlementPackageID  uuid.UUID `json:"entitlement_package_id"`
	JournalEntryID        uuid.UUID `json:"journal_entry_id"`
	ProviderTransactionID string    `json:"provider_transaction_id"`
	CreditMicrocredits    int64     `json:"credit_microcredits"`
	AppliedToReceivables  int64     `json:"applied_to_receivables"`
	WalletVersionAfter    int64     `json:"wallet_version_after"`
	FactsDigest           string    `json:"facts_digest"`
	Status                string    `json:"status"`
}

// PaymentFinancialRepairCommandInput is the closed command the approval froze.
// The caller only names the order and the persisted success evidence; every
// amount, transaction reference, component and ledger direction is derived by
// the owner from those two rows.
type PaymentFinancialRepairCommandInput struct {
	PaymentOrderID    uuid.UUID
	PaymentEvidenceID uuid.UUID
}

// PaymentFinancialRepairPlan is the closed, recomputable owner projection of one
// approved repair. It also carries the reconstructed provider fact the existing
// merge writer consumes, so execution never calls the provider.
type PaymentFinancialRepairPlan struct {
	OrderID                  uuid.UUID
	WalletID                 uuid.UUID
	EvidenceID               uuid.UUID
	MerchantOrderNo          string
	Channel                  string
	Currency                 string
	ProductID                string
	ProviderID               string
	ProviderVersion          string
	ProviderSKU              string
	ProviderTransactionID    string
	AmountFen                int64
	CreditMicrocredits       int64
	BaseAmount               int64
	BonusAmount              int64
	PaidAt                   time.Time
	OrderUpdatedAt           time.Time
	OrderSummaryVersion      int64
	WalletPostedMicrocredits int64
	WalletVersion            int64
	AppliedToReceivables     int64
	ReviewTransactionID      uuid.UUID
	FactsDigest              string
	Provider                 ProviderOrder
}

// PaymentFinancialRepairFrozenFacts is the wire-visible owner projection an
// approval froze. FactsDigest is an owner-only recomputation input; a consumed
// authorization leaves it empty and the execution transaction recomputes it,
// while a presented digest must match exactly.
type PaymentFinancialRepairFrozenFacts struct {
	OrderUpdatedAt        time.Time
	AmountFen             int64
	CreditMicrocredits    int64
	ProviderTransactionID string
	WalletID              uuid.UUID
	WalletVersion         int64
	AppliedToReceivables  int64
	FactsDigest           string
}

type PaymentFinancialRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	// ApproverUserID is the approving human identity the approval froze. The
	// reused audit table requires a non-zero actor (design §4 item 5:
	// "actor_id=审批执行身份"), so the execution refuses a nil approver instead
	// of writing an unattributable row.
	ApproverUserID uuid.UUID
	// RequesterUserID is the requesting identity the same approval froze. The
	// user decision of 2026-09-21 requires the audit row to record BOTH sides of
	// the independent approval (migration 000077 adds the column), so the
	// execution refuses a nil requester or a requester equal to the approver.
	RequesterUserID   uuid.UUID
	ExecutionKey      string
	PolicyVersion     int64
	SchemaVersion     int64
	ActivationVersion int64
	Command           PaymentFinancialRepairCommandInput
	Frozen            PaymentFinancialRepairFrozenFacts
}

type paymentRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// paymentRepairReceivableApplication mirrors the wallet-side receivable
// settlement order the existing writer uses (created_at then id) so the frozen
// projection can state the exact amount a later execution may apply.
func paymentRepairReceivableApplication(ctx context.Context, source paymentRepairReader, walletID uuid.UUID, maximum int64) (int64, error) {
	if maximum <= 0 {
		return 0, nil
	}
	rows, err := source.Query(ctx, `SELECT outstanding_microcredits FROM receivables
WHERE wallet_id=$1 AND outstanding_microcredits>0 ORDER BY created_at,id`, walletID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	remaining, applied := maximum, int64(0)
	for rows.Next() {
		var outstanding int64
		if err := rows.Scan(&outstanding); err != nil {
			return 0, err
		}
		if outstanding <= 0 {
			continue
		}
		amount := min(remaining, outstanding)
		remaining, applied = remaining-amount, applied+amount
		if remaining == 0 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return applied, nil
}

// paymentFinancialRepairPlanFrom derives the only permitted repair from
// already-open owner reads. Every rejected contract returns
// ErrApprovalSnapshotMismatch so the caller classifies it as a definite,
// zero-write rejection.
func paymentFinancialRepairPlanFrom(ctx context.Context, source paymentRepairReader, input PaymentFinancialRepairCommandInput,
	policyVersion, schemaVersion, activationVersion int64) (PaymentFinancialRepairPlan, error) {
	if input.PaymentOrderID == uuid.Nil || input.PaymentEvidenceID == uuid.Nil ||
		policyVersion < 1 || schemaVersion < 1 || activationVersion < 1 {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := PaymentFinancialRepairPlan{OrderID: input.PaymentOrderID, EvidenceID: input.PaymentEvidenceID}
	var orderStatus, fulfillmentStatus string
	var rawProduct []byte
	var requesterID uuid.UUID
	if err := source.QueryRow(ctx, `SELECT merchant_order_no,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,
status,fulfillment_status,COALESCE(provider_sku,''),provider_id,provider_version,updated_at,summary_version,product_snapshot,requester_id
FROM payment_orders WHERE id=$1`, input.PaymentOrderID).
		Scan(&plan.MerchantOrderNo, &plan.WalletID, &plan.ProductID, &plan.Channel, &plan.AmountFen, &plan.Currency,
			&plan.CreditMicrocredits, &orderStatus, &fulfillmentStatus, &plan.ProviderSKU, &plan.ProviderID, &plan.ProviderVersion,
			&plan.OrderUpdatedAt, &plan.OrderSummaryVersion, &rawProduct, &requesterID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairPlan{}, ErrNotFound
		}
		return PaymentFinancialRepairPlan{}, err
	}
	if orderStatus != "MANUAL_REVIEW" || fulfillmentStatus != "MANUAL_REVIEW" || plan.WalletID == uuid.Nil ||
		plan.AmountFen <= 0 || plan.CreditMicrocredits <= 0 || plan.Currency == "" || plan.ProviderID == "" || plan.ProviderVersion == "" {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var product Product
	if json.Unmarshal(rawProduct, &product) != nil || product.ProductID != plan.ProductID ||
		product.AmountFen != plan.AmountFen || product.CreditMicrocredits != plan.CreditMicrocredits || product.Currency != plan.Currency {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	base, bonus, err := productComponentAmounts(plan.CreditMicrocredits, product.BasePoints, product.BonusPoints)
	if err != nil {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.BaseAmount, plan.BonusAmount = base, bonus
	// The frozen evidence must be a persisted success fact for this order.
	var evidenceOrderID uuid.UUID
	var evidenceProviderID, evidenceProviderVersion, evidenceMerchantOrderNo, evidenceStatus, mergeResult, payloadDigest string
	var evidenceType, providerTransactionID, evidenceCurrency string
	var evidenceAmountFen *int64
	var providerEventAt *time.Time
	var rawEvidence []byte
	if err := source.QueryRow(ctx, `SELECT payment_order_id,evidence_type,provider_id,provider_version,merchant_order_no,
COALESCE(provider_transaction_id,''),amount_fen,COALESCE(currency,''),COALESCE(provider_status,''),provider_event_at,
payload_digest,merge_result,payload_snapshot FROM payment_evidence WHERE id=$1`, input.PaymentEvidenceID).
		Scan(&evidenceOrderID, &evidenceType, &evidenceProviderID, &evidenceProviderVersion, &evidenceMerchantOrderNo,
			&providerTransactionID, &evidenceAmountFen, &evidenceCurrency, &evidenceStatus, &providerEventAt,
			&payloadDigest, &mergeResult, &rawEvidence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairPlan{}, ErrNotFound
		}
		return PaymentFinancialRepairPlan{}, err
	}
	if evidenceOrderID != plan.OrderID || !validHexDigest(payloadDigest) ||
		(mergeResult != "RECORDED" && mergeResult != "DUPLICATE" && mergeResult != "MERGED") ||
		!paymentFinancialRepairSuccessStatuses[strings.ToUpper(strings.TrimSpace(evidenceStatus))] ||
		strings.TrimSpace(providerTransactionID) != providerTransactionID || providerTransactionID == "" ||
		providerEventAt == nil || providerEventAt.IsZero() ||
		evidenceProviderID != plan.ProviderID || evidenceProviderVersion != plan.ProviderVersion ||
		evidenceMerchantOrderNo != plan.MerchantOrderNo || evidenceAmountFen == nil || *evidenceAmountFen != plan.AmountFen ||
		evidenceCurrency != plan.Currency {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Only the immutable provider evidence can supply customer and SKU facts.
	// An order snapshot is an expected value, never a missing provider fact.
	var providerFacts struct {
		Customer *string `json:"external_customer_key"`
		SKU      *string `json:"external_sku"`
	}
	if json.Unmarshal(rawEvidence, &providerFacts) != nil || providerFacts.Customer == nil ||
		*providerFacts.Customer != requesterID.String() ||
		(providerFacts.SKU == nil && plan.ProviderSKU != "") ||
		(providerFacts.SKU != nil && *providerFacts.SKU != plan.ProviderSKU) {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	evidenceSKU := ""
	if providerFacts.SKU != nil {
		evidenceSKU = *providerFacts.SKU
	}
	plan.ProviderTransactionID, plan.PaidAt = providerTransactionID, providerEventAt.UTC()
	// Internal absence (design §5 as amended): the order must not be credited,
	// and the frozen provider reference must correspond to exactly one
	// review-required transaction. That transaction is what put the order into
	// MANUAL_REVIEW, and the repair reuses the existing repair branch so the
	// ledger entry stays REPAIR_CREDIT.
	var creditedTransaction, packaged, repaired, refunded, chargedBack bool
	var reviewTransactions, frozenReviewTransactions int64
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1 AND (status IN ('PAID','LATE_PAID') OR fulfillment_status='CREDITED')),
 EXISTS(SELECT 1 FROM entitlement_packages WHERE payment_order_id=$1),
 EXISTS(SELECT 1 FROM payment_order_repairs WHERE payment_order_id=$1),
 EXISTS(SELECT 1 FROM refunds WHERE payment_order_id=$1),
 EXISTS(SELECT 1 FROM chargeback_cases WHERE payment_order_id=$1),
 (SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1 AND fulfillment_status='REVIEW_REQUIRED'),
 (SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1 AND fulfillment_status='REVIEW_REQUIRED' AND provider_transaction_id=$2)`,
		plan.OrderID, providerTransactionID).
		Scan(&creditedTransaction, &packaged, &repaired, &refunded, &chargedBack, &reviewTransactions, &frozenReviewTransactions); err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	if creditedTransaction || packaged || repaired || refunded || chargedBack || reviewTransactions != 1 || frozenReviewTransactions != 1 {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if err := source.QueryRow(ctx, `SELECT id FROM payment_transactions
WHERE payment_order_id=$1 AND fulfillment_status='REVIEW_REQUIRED' AND provider_transaction_id=$2`,
		plan.OrderID, providerTransactionID).Scan(&plan.ReviewTransactionID); err != nil {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var walletStatus string
	if err := source.QueryRow(ctx, `SELECT status,posted_microcredits,version FROM wallets WHERE id=$1`, plan.WalletID).
		Scan(&walletStatus, &plan.WalletPostedMicrocredits, &plan.WalletVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return PaymentFinancialRepairPlan{}, err
	}
	if walletStatus != "ACTIVE" || plan.WalletPostedMicrocredits < 0 || plan.WalletVersion < 1 {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	applied, err := paymentRepairReceivableApplication(ctx, source, plan.WalletID, plan.BaseAmount)
	if err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	plan.AppliedToReceivables = applied
	var outstandingTotal, receivableCount int64
	if err := source.QueryRow(ctx, `SELECT COALESCE(sum(outstanding_microcredits),0),count(*) FROM receivables
WHERE wallet_id=$1 AND outstanding_microcredits>0`, plan.WalletID).Scan(&outstandingTotal, &receivableCount); err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	encoded, err := json.Marshal(map[string]any{
		"command_type":     paymentFinancialRepairCommand,
		"payment_order_id": plan.OrderID.String(), "wallet_id": plan.WalletID.String(),
		"merchant_order_no": plan.MerchantOrderNo, "channel": plan.Channel, "currency": plan.Currency,
		"amount_fen": plan.AmountFen, "credit_microcredits": plan.CreditMicrocredits,
		"base_microcredits": plan.BaseAmount, "bonus_microcredits": plan.BonusAmount,
		"provider_id": plan.ProviderID, "provider_version": plan.ProviderVersion,
		"provider_transaction_id": plan.ProviderTransactionID,
		"order_status":            orderStatus, "fulfillment_status": fulfillmentStatus,
		"order_updated_at": plan.OrderUpdatedAt.UTC().Format(time.RFC3339Nano), "order_summary_version": plan.OrderSummaryVersion,
		"evidence_id": plan.EvidenceID.String(), "evidence_type": evidenceType,
		"evidence_external_customer_key": *providerFacts.Customer, "evidence_external_sku": evidenceSKU,
		"evidence_status": evidenceStatus, "evidence_payload_digest": payloadDigest,
		"evidence_paid_at":      plan.PaidAt.Format(time.RFC3339Nano),
		"review_transaction_id": plan.ReviewTransactionID.String(),
		"wallet_status":         walletStatus, "wallet_posted_microcredits": plan.WalletPostedMicrocredits,
		"wallet_version":               plan.WalletVersion,
		"receivable_outstanding_total": outstandingTotal, "receivable_open_count": receivableCount,
		"applied_to_receivables": plan.AppliedToReceivables,
		"policy_version":         policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	plan.Provider = ProviderOrder{
		MerchantOrderNo: plan.MerchantOrderNo, Status: "SUCCESS", Channel: plan.Channel,
		AmountFen: plan.AmountFen, Currency: plan.Currency,
		ChannelTransactionID: &plan.ProviderTransactionID, PaidAt: &plan.PaidAt,
		ProviderStatus: evidenceStatus, ExternalSKU: evidenceSKU, ExternalCustomerKey: *providerFacts.Customer,
	}
	return plan, nil
}

// paymentFinancialRepairProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof
// recomputation.
func paymentFinancialRepairProofProjection(plan PaymentFinancialRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"payment_order_updated_at": plan.OrderUpdatedAt.UTC().Format(time.RFC3339Nano),
		"facts_digest":             plan.FactsDigest, "policy_version": 1,
	})
	financial, _ := json.Marshal(map[string]any{
		"amount_fen": plan.AmountFen, "credit_microcredits": plan.CreditMicrocredits,
		"provider_transaction_id": plan.ProviderTransactionID,
		"wallet_id":               plan.WalletID.String(), "wallet_version": plan.WalletVersion,
		"applied_to_receivables": plan.AppliedToReceivables,
	})
	return critical, financial
}

// PaymentFinancialRepairProofInput derives the closed proof-era projection of
// one confirmed-payment repair for a request that is still being assembled. It
// is deliberately not activation-gated: only proof issuance and execution
// require the live ACTIVE row.
func (r *PostgresRepository) PaymentFinancialRepairProofInput(ctx context.Context, target PaymentFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == paymentFinancialRepairCommand {
			activation = item
			break
		}
	}
	if activation.CommandType == "" {
		return approvalencoding.LegacyProofInput{}, ErrConflict
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	defer tx.Rollback(bounded)
	plan, err := paymentFinancialRepairPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := paymentFinancialRepairProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: paymentFinancialRepairCommand, PolicyVersion: activation.PolicyVersion,
		SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType:    "PAYMENT_ORDER", TargetResourceID: plan.OrderID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"payment_order_id": plan.OrderID, "payment_evidence_id": plan.EvidenceID,
			"reason_code": paymentFinancialRepairReason,
		}),
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

// ApplyPaymentFinancialRepair commits exactly one confirmed-payment repair by
// reusing the existing audited merge writer with the frozen provider fact. It
// never calls the provider and never rewrites an existing transaction, package,
// lot, journal or repair row.
func (p *PaymentCoordinator) ApplyPaymentFinancialRepair(ctx context.Context, input PaymentFinancialRepairExecutionInput) (PaymentFinancialRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if p == nil || p.repository == nil || p.repository.pool == nil || input.ApprovalRequestID == uuid.Nil ||
		input.AuthorizationEventID == uuid.Nil || input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.ApproverUserID == uuid.Nil || input.RequesterUserID == uuid.Nil || input.RequesterUserID == input.ApproverUserID ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	stored, found, err := paymentFinancialRepairCaseMatches(ctx, tx, input, executionKey)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if found {
		return stored, tx.Commit(ctx)
	}
	plan, err := paymentFinancialRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if mismatch := paymentFinancialRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch(mismatch)
	}
	order, err := getPaymentOrderByID(ctx, tx, plan.OrderID, true)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairResult{}, ErrNotFound
		}
		return PaymentFinancialRepairResult{}, err
	}
	if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch("order_status_changed")
	}
	now := p.now()
	var balanceBefore int64
	if err := tx.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1 FOR UPDATE`, plan.WalletID).Scan(&balanceBefore); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if balanceBefore != plan.WalletPostedMicrocredits {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch("wallet_version_changed")
	}
	snapshot, err := json.Marshal(map[string]any{
		"status": plan.Provider.Status, "provider_status": plan.Provider.ProviderStatus,
		"provider_transaction_id": plan.ProviderTransactionID, "paid_at": plan.PaidAt,
		"external_customer_key": plan.Provider.ExternalCustomerKey, "external_sku": plan.Provider.ExternalSKU,
	})
	if err != nil {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	evidenceKey := "repair:" + plan.OrderID.String() + ":" + plan.ProviderTransactionID
	merged, evidenceID, mergeErr := p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "MANUAL_REPAIR", evidenceKey, snapshot, plan.Provider, true, now)
	if mergeErr != nil {
		return PaymentFinancialRepairResult{}, mergeErr
	}
	if evidenceID == uuid.Nil {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	var transactionID, packageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT transaction.id,package.id FROM payment_transactions transaction
JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id
WHERE transaction.payment_order_id=$1 AND transaction.provider_transaction_id=$2`, merged.ID, plan.ProviderTransactionID).
		Scan(&transactionID, &packageID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	repairID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payment_order_repairs(id,payment_order_id,payment_transaction_id,payment_evidence_id,actor_id,reason,
provider_snapshot,entitlement_package_id,points_awarded,wallet_balance_before,wallet_balance_after,created_at,
approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,facts_digest,requester_user_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		// The audit row references the frozen evidence the approval froze (not the
		// writer's own MANUAL_REPAIR provenance row), records the approving
		// identity as its actor and the requesting identity it approved for, per
		// design §4 item 5 and the 2026-09-21 decision that both sides of the
		// independent approval must be attributable.
		repairID, plan.OrderID, transactionID, plan.EvidenceID, input.ApproverUserID, paymentFinancialRepairReason,
		snapshot, packageID, plan.CreditMicrocredits, balanceBefore, balanceBefore+plan.CreditMicrocredits-plan.AppliedToReceivables,
		now, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		ledgerRepairExecutionKeyDigest(executionKey), plan.FactsDigest, input.RequesterUserID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	var journalID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM journal_entries WHERE business_reference=$1`, "repair-payment-transaction:"+transactionID.String()).Scan(&journalID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	var walletVersionAfter int64
	if err := tx.QueryRow(ctx, `SELECT version FROM wallets WHERE id=$1`, plan.WalletID).Scan(&walletVersionAfter); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	committed, err := p.repository.PaymentFinancialRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if committed.PaymentTransactionID != transactionID || committed.EntitlementPackageID != packageID || committed.JournalEntryID != journalID ||
		committed.RepairID != repairID || committed.Status != "SUCCEEDED" ||
		walletVersionAfter < 1 || committed.FactsDigest != plan.FactsDigest {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	return committed, nil
}

// PaymentFinancialRepairForRequest reads the committed repair of one approval
// request without creating any new fact (result-first).
func (r *PostgresRepository) PaymentFinancialRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (PaymentFinancialRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readPaymentFinancialRepair(ctx, tx, approvalRequestID)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	return result, nil
}

func readPaymentFinancialRepair(ctx context.Context, source paymentRepairReader, approvalRequestID uuid.UUID) (PaymentFinancialRepairResult, error) {
	var result PaymentFinancialRepairResult
	var pointsAwarded, balanceBefore, balanceAfter int64
	var walletID uuid.UUID
	err := source.QueryRow(ctx, `SELECT repair.id,repair.payment_order_id,repair.payment_transaction_id,repair.entitlement_package_id,
transaction.provider_transaction_id,repair.points_awarded,repair.wallet_balance_before,repair.wallet_balance_after,repair.facts_digest,
payment_order.wallet_id
FROM payment_order_repairs repair
JOIN payment_transactions transaction ON transaction.id=repair.payment_transaction_id
JOIN payment_orders payment_order ON payment_order.id=repair.payment_order_id
WHERE repair.approval_request_id=$1`, approvalRequestID).
		Scan(&result.RepairID, &result.PaymentOrderID, &result.PaymentTransactionID, &result.EntitlementPackageID,
			&result.ProviderTransactionID, &pointsAwarded, &balanceBefore, &balanceAfter, &result.FactsDigest, &walletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentFinancialRepairResult{}, ErrNotFound
	}
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	result.CreditMicrocredits = pointsAwarded
	// The audited row records both wallet balances, so the amount that settled
	// open receivables is derived, never re-read from the mutable wallet.
	result.AppliedToReceivables = pointsAwarded - (balanceAfter - balanceBefore)
	if result.AppliedToReceivables < 0 || result.AppliedToReceivables > pointsAwarded {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT id FROM journal_entries WHERE business_reference=$1`,
		"repair-payment-transaction:"+result.PaymentTransactionID.String()).Scan(&result.JournalEntryID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairResult{}, ErrNotFound
		}
		return PaymentFinancialRepairResult{}, err
	}
	if err := source.QueryRow(ctx, `SELECT version FROM wallets WHERE id=$1`, walletID).Scan(&result.WalletVersionAfter); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	result.Status = "SUCCEEDED"
	return result, nil
}

// paymentFinancialRepairCaseMatches is the idempotent-hit check: the stored
// repair row must carry the same approval identity, epoch and execution key as
// the delivery being consumed.
func paymentFinancialRepairCaseMatches(ctx context.Context, source paymentRepairReader, input PaymentFinancialRepairExecutionInput, executionKey string) (PaymentFinancialRepairResult, bool, error) {
	var storedOrderID uuid.UUID
	var authorizationEventID uuid.UUID
	var approvalEpoch int64
	var executionKeyDigest string
	err := source.QueryRow(ctx, `SELECT payment_order_id,authorization_event_id,approval_epoch,execution_key_digest
FROM payment_order_repairs WHERE approval_request_id=$1`, input.ApprovalRequestID).
		Scan(&storedOrderID, &authorizationEventID, &approvalEpoch, &executionKeyDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentFinancialRepairResult{}, false, nil
	}
	if err != nil {
		return PaymentFinancialRepairResult{}, false, err
	}
	matches := authorizationEventID == input.AuthorizationEventID && approvalEpoch == input.ApprovalEpoch &&
		executionKeyDigest == ledgerRepairExecutionKeyDigest(executionKey) && storedOrderID == input.Command.PaymentOrderID
	if !matches {
		return PaymentFinancialRepairResult{}, false, nil
	}
	stored, err := readPaymentFinancialRepair(ctx, source, input.ApprovalRequestID)
	if err != nil {
		return PaymentFinancialRepairResult{}, false, err
	}
	return stored, true, nil
}

func paymentFinancialRepairFrozenMismatch(plan PaymentFinancialRepairPlan, frozen PaymentFinancialRepairFrozenFacts) string {
	switch {
	case !plan.OrderUpdatedAt.UTC().Equal(frozen.OrderUpdatedAt.UTC()):
		return "payment_order_updated_at"
	case plan.AmountFen != frozen.AmountFen:
		return "amount_fen"
	case plan.CreditMicrocredits != frozen.CreditMicrocredits:
		return "credit_microcredits"
	case plan.ProviderTransactionID != frozen.ProviderTransactionID:
		return "provider_transaction_id"
	case plan.WalletID != frozen.WalletID:
		return "wallet_id"
	case plan.WalletVersion != frozen.WalletVersion:
		return "wallet_version"
	case plan.AppliedToReceivables != frozen.AppliedToReceivables:
		return "applied_to_receivables"
	// The consumed authorization freezes only the wire-visible owner
	// projection; the owner-only facts digest is recomputed inside the
	// execution transaction, so an empty value is not a caller claim.
	case frozen.FactsDigest == "":
		return ""
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}
