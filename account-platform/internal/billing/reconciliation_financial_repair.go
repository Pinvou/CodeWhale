package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reconciliationFinancialRepairCommand is the closed command name shared by the
// proof authority, the activation projection and the consumer mapping.
const reconciliationFinancialRepairCommand = "RECONCILIATION_FINANCIAL_REPAIR"

// reconciliationRepairResolutionCode is the only resolution the first release
// admits: the provider side already refunded or reversed the difference.
const reconciliationRepairResolutionCode = "PROVIDER_REFUND_CONFIRMED"

// reconciliationRepairLedgerEffect is the explicit zero-accounting-effect
// declaration every projection must carry. Any other value is a denial.
const reconciliationRepairLedgerEffect = "NONE"

// reconciliationRepairLockScope serializes this command with itself for one
// reconciliation case.
const reconciliationRepairLockScope = "reconciliation.repair"

type ReconciliationFinancialRepairResult struct {
	CaseID                uuid.UUID `json:"case_id"`
	RunID                 uuid.UUID `json:"run_id"`
	Channel               string    `json:"channel"`
	BusinessDate          string    `json:"business_date"`
	MerchantOrderNo       string    `json:"merchant_order_no"`
	AmountFen             int64     `json:"amount_fen"`
	Currency              string    `json:"currency"`
	ProviderTransactionID string    `json:"provider_transaction_id"`
	PaymentEvidenceID     uuid.UUID `json:"payment_evidence_id"`
	ResolutionCode        string    `json:"resolution_code"`
	RecheckCaseCount      int64     `json:"recheck_case_count"`
	FactsDigest           string    `json:"facts_digest"`
	LedgerEffect          string    `json:"ledger_effect"`
	Status                string    `json:"status"`
}

// ReconciliationFinancialRepairCommandInput is the closed command the approval
// froze. The caller names the reconciliation case, the persisted provider
// disposal evidence and the frozen business date; it can never name an internal
// order, wallet, user or amount.
type ReconciliationFinancialRepairCommandInput struct {
	ReconciliationCaseID uuid.UUID
	PaymentEvidenceID    uuid.UUID
	BusinessDate         string
}

// ReconciliationFinancialRepairPlan is the closed, recomputable owner
// projection of one approved reconciliation closure.
type ReconciliationFinancialRepairPlan struct {
	PolicyVersion, CaseVersion int64
	ManualReviewCaseID         *uuid.UUID
	CaseID                     uuid.UUID
	RunID                      uuid.UUID
	Channel                    string
	BusinessDate               string
	MerchantOrderNo            string
	AmountFen                  int64
	Currency                   string
	ProviderTransactionID      string
	PaymentEvidenceID          uuid.UUID
	ProviderSnapshotDigest     string
	RecheckCaseCount           int64
	FactsDigest                string
}

// ReconciliationFinancialRepairFrozenFacts is the wire-visible owner projection
// an approval froze. Fresh execution requires the exact nonempty owner digest;
// matching committed identity is returned before fresh gates.
type ReconciliationFinancialRepairFrozenFacts struct {
	RunID                  uuid.UUID
	AmountFen              int64
	Currency               string
	ProviderTransactionID  string
	ProviderSnapshotDigest string
	FactsDigest            string
}

type ReconciliationFinancialRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              ReconciliationFinancialRepairCommandInput
	Frozen               ReconciliationFinancialRepairFrozenFacts
}

// reconciliationRepairReader is the read surface the closed plan needs.
type reconciliationRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func reconciliationRepairConflict(stage string, cause error) error {
	if cause == nil {
		return errors.Join(ErrConflict, errors.New(stage))
	}
	return errors.Join(ErrConflict, fmt.Errorf("%s: %w", stage, cause))
}

// reconciliationProviderSnapshot is the closed view of the frozen provider bill
// row the case recorded. Only these fields are read; provider text never leaves
// the owner domain.
type reconciliationProviderSnapshot struct {
	EntryType            string    `json:"entry_type"`
	ProviderReferenceID  string    `json:"provider_reference_id"`
	MerchantOrderNo      string    `json:"merchant_order_no"`
	ChannelTransactionID string    `json:"channel_transaction_id"`
	Channel              string    `json:"channel"`
	AmountFen            int64     `json:"amount_fen"`
	Currency             string    `json:"currency"`
	Status               string    `json:"status"`
	PaidAt               time.Time `json:"paid_at"`
}

// reconciliationFinancialRepairPlanFrom derives the only permitted closure from
// already-open owner reads. Every rejected contract returns
// ErrApprovalSnapshotMismatch (or ErrNotFound) so the caller classifies it as a
// definite, zero-write rejection.
func reconciliationFinancialRepairPlanFrom(ctx context.Context, source reconciliationRepairReader,
	input ReconciliationFinancialRepairCommandInput, policyVersion, schemaVersion, activationVersion int64) (ReconciliationFinancialRepairPlan, error) {
	businessDate, err := time.Parse("2006-01-02", strings.TrimSpace(input.BusinessDate))
	if input.ReconciliationCaseID == uuid.Nil || input.PaymentEvidenceID == uuid.Nil || err != nil ||
		policyVersion != 1 || schemaVersion != 1 || activationVersion < 1 {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := ReconciliationFinancialRepairPlan{CaseID: input.ReconciliationCaseID,
		PaymentEvidenceID: input.PaymentEvidenceID, PolicyVersion: policyVersion, BusinessDate: businessDate.UTC().Format("2006-01-02")}
	var snapshotRaw []byte
	var caseType, caseStatus, merchantOrderNo string
	var caseOrderID *uuid.UUID
	var resolvedAt *time.Time
	if err := source.QueryRow(ctx, `SELECT reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,
provider_snapshot,resolved_at,version,manual_review_case_id FROM reconciliation_cases WHERE id=$1`, input.ReconciliationCaseID).
		Scan(&plan.RunID, &caseOrderID, &merchantOrderNo, &caseType, &caseStatus, &snapshotRaw, &resolvedAt, &plan.CaseVersion, &plan.ManualReviewCaseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationFinancialRepairPlan{}, ErrNotFound
		}
		return ReconciliationFinancialRepairPlan{}, err
	}
	if caseType != "PROVIDER_PAID_INTERNAL_MISSING" || caseStatus != "OPEN" || resolvedAt != nil || caseOrderID != nil ||
		strings.TrimSpace(merchantOrderNo) == "" || len(snapshotRaw) == 0 {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.MerchantOrderNo = merchantOrderNo
	plan.ProviderSnapshotDigest = digestHex(snapshotRaw)
	var snapshot reconciliationProviderSnapshot
	if err := json.Unmarshal(snapshotRaw, &snapshot); err != nil {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	entryType := snapshot.EntryType
	if strings.TrimSpace(entryType) == "" {
		entryType = "PAYMENT"
	}
	if entryType != "PAYMENT" || snapshot.MerchantOrderNo != merchantOrderNo ||
		snapshot.ChannelTransactionID == "" || snapshot.ChannelTransactionID != strings.TrimSpace(snapshot.ChannelTransactionID) ||
		snapshot.AmountFen <= 0 || strings.TrimSpace(snapshot.Currency) == "" ||
		!successfulBillEntryStatus(ProviderBillEntry{EntryType: "PAYMENT", Status: snapshot.Status}) {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.AmountFen, plan.Currency, plan.ProviderTransactionID = snapshot.AmountFen, snapshot.Currency, snapshot.ChannelTransactionID
	var runChannel, runStatus, providerID, providerVersion string
	var runDate time.Time
	if err := source.QueryRow(ctx, `SELECT channel,status,business_date,provider_id,provider_version FROM reconciliation_runs WHERE id=$1`, plan.RunID).
		Scan(&runChannel, &runStatus, &runDate, &providerID, &providerVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationFinancialRepairPlan{}, ErrNotFound
		}
		return ReconciliationFinancialRepairPlan{}, err
	}
	if providerID == "" || providerVersion == "" || plan.CaseVersion < 1 || runStatus != "COMPLETED" || runChannel != snapshot.Channel || runDate.UTC().Format("2006-01-02") != plan.BusinessDate {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.Channel = runChannel
	// The frozen evidence must be the provider-side refund or reversal of this
	// exact bill row: same merchant order, same channel transaction, same amount
	// and currency, and it must carry a provider refund reference.
	var evidenceStatus string
	var providerRefundID *string
	var evidenceMerchant, evidenceTransaction, evidenceCurrency, evidenceDigest string
	var evidenceAmount int64
	var evidenceProvider, evidenceVersion, evidenceType string
	var evidenceRaw []byte
	if err := source.QueryRow(ctx, `SELECT merchant_order_no,COALESCE(provider_transaction_id,''),provider_refund_id,amount_fen,currency,
COALESCE(provider_status,''),payload_digest,provider_id,provider_version,evidence_type,payload_snapshot FROM payment_evidence WHERE id=$1`, input.PaymentEvidenceID).
		Scan(&evidenceMerchant, &evidenceTransaction, &providerRefundID, &evidenceAmount, &evidenceCurrency,
			&evidenceStatus, &evidenceDigest, &evidenceProvider, &evidenceVersion, &evidenceType, &evidenceRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationFinancialRepairPlan{}, ErrNotFound
		}
		return ReconciliationFinancialRepairPlan{}, err
	}
	if evidenceMerchant != merchantOrderNo || evidenceTransaction != plan.ProviderTransactionID ||
		providerRefundID == nil || strings.TrimSpace(*providerRefundID) == "" || *providerRefundID != strings.TrimSpace(*providerRefundID) ||
		evidenceAmount != plan.AmountFen || evidenceCurrency != plan.Currency ||
		!successfulBillEntryStatus(ProviderBillEntry{EntryType: "REFUND", Status: evidenceStatus}) || !validHexDigest(evidenceDigest) || evidenceProvider != providerID || evidenceVersion != providerVersion || evidenceType != "BILL" {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var refundBill ProviderBillEntry
	if json.Unmarshal(evidenceRaw, &refundBill) != nil || normalizedBillEntryType(refundBill) != "REFUND" || !successfulBillEntryStatus(refundBill) || billReference(refundBill) != *providerRefundID || refundBill.Channel != runChannel || refundBill.MerchantOrderNo != merchantOrderNo || strings.TrimSpace(refundBill.ChannelTransactionID) != plan.ProviderTransactionID || refundBill.AmountFen != plan.AmountFen || refundBill.Currency != plan.Currency || refundBill.Status != evidenceStatus {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if plan.ManualReviewCaseID != nil {
		var domain, resourceType, status string
		var resourceID uuid.UUID
		if err := source.QueryRow(ctx, `SELECT domain,resource_type,resource_id,status FROM manual_review_cases WHERE id=$1`, *plan.ManualReviewCaseID).Scan(&domain, &resourceType, &resourceID, &status); err != nil {
			return ReconciliationFinancialRepairPlan{}, err
		}
		if domain != "BILLING" || resourceType != "RECONCILIATION_CASE" || resourceID != plan.CaseID || (status != "OPEN" && status != "IN_REVIEW") {
			return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
	}
	// The difference must still have no internal attribution at all: no order and
	// no transaction may claim this merchant order or channel transaction.
	var internalOrder, internalTransaction bool
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM payment_orders WHERE merchant_order_no=$1),
 EXISTS(SELECT 1 FROM payment_transactions WHERE provider_transaction_id=$2)`,
		merchantOrderNo, plan.ProviderTransactionID).Scan(&internalOrder, &internalTransaction); err != nil {
		return ReconciliationFinancialRepairPlan{}, err
	}
	if internalOrder || internalTransaction {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var caseExists bool
	if err := source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_repair_cases WHERE reconciliation_case_id=$1)`,
		plan.CaseID).Scan(&caseExists); err != nil {
		return ReconciliationFinancialRepairPlan{}, err
	}
	if caseExists {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Repair-then-recheck: closing one case must not leave another OPEN
	// difference for the same merchant order/channel/business date across runs.
	if err := source.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases c JOIN reconciliation_runs r ON r.id=c.reconciliation_run_id
WHERE r.channel=$1 AND r.business_date=$2::date AND c.merchant_order_no=$3 AND c.case_type='PROVIDER_PAID_INTERNAL_MISSING'
AND c.status IN ('OPEN','IN_REVIEW') AND c.id<>$4`, plan.Channel, plan.BusinessDate, merchantOrderNo, plan.CaseID).Scan(&plan.RecheckCaseCount); err != nil {
		return ReconciliationFinancialRepairPlan{}, err
	}

	if plan.RecheckCaseCount != 0 {
		return ReconciliationFinancialRepairPlan{}, ErrConflict
	}
	var ownerRaw []byte
	if err := source.QueryRow(ctx, `SELECT jsonb_build_object('case',(SELECT to_jsonb(c) FROM reconciliation_cases c WHERE id=$1),'run',(SELECT to_jsonb(r) FROM reconciliation_runs r WHERE id=$2),'evidence',(SELECT to_jsonb(e) FROM payment_evidence e WHERE id=$3),'manual_review',(SELECT to_jsonb(m) FROM manual_review_cases m WHERE id=$4))`, plan.CaseID, plan.RunID, plan.PaymentEvidenceID, plan.ManualReviewCaseID).Scan(&ownerRaw); err != nil {
		return ReconciliationFinancialRepairPlan{}, err
	}
	ownerSnapshot, err := paymentRepairJSONSnapshot(ownerRaw)
	if err != nil {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Freeze the existing zero-accounting baseline without accepting caller amounts.
	var zeroRaw []byte
	if err := source.QueryRow(ctx, `SELECT jsonb_build_object(
 'journal_entries',(SELECT count(*) FROM journal_entries),'ledger_postings',(SELECT count(*) FROM ledger_postings),
 'wallets',(SELECT count(*) FROM wallets),'wallet_versions',(SELECT COALESCE(string_agg(id::text||':'||version::text,',' ORDER BY id),'') FROM wallets),
 'receivables',(SELECT count(*) FROM receivables),'receivable_balances',(SELECT COALESCE(string_agg(id::text||':'||original_microcredits::text||':'||settled_microcredits::text||':'||outstanding_microcredits::text,',' ORDER BY id),'') FROM receivables),
 'credit_lots',(SELECT count(*) FROM credit_lots),'entitlement_packages',(SELECT count(*) FROM entitlement_packages),'entitlement_components',(SELECT count(*) FROM entitlement_components),
 'payment_orders',(SELECT count(*) FROM payment_orders),'payment_transactions',(SELECT count(*) FROM payment_transactions),'payment_evidence',(SELECT count(*) FROM payment_evidence),
 'refunds',(SELECT count(*) FROM refunds),'chargeback_cases',(SELECT count(*) FROM chargeback_cases),'forced_refund_cases',(SELECT count(*) FROM forced_refund_cases),'usage_holds',(SELECT count(*) FROM usage_holds))`).Scan(&zeroRaw); err != nil {
		return ReconciliationFinancialRepairPlan{}, err
	}
	zeroSnapshot, err := paymentRepairJSONSnapshot(zeroRaw)
	if err != nil {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}

	encoded, err := json.Marshal(map[string]any{
		"command_type": reconciliationFinancialRepairCommand, "owner_snapshot": ownerSnapshot, "zero_accounting_baseline": zeroSnapshot, "repair_case_exists": caseExists, "case_id": plan.CaseID.String(),
		"run_id": plan.RunID.String(), "channel": plan.Channel, "business_date": plan.BusinessDate,
		"merchant_order_no": plan.MerchantOrderNo, "case_type": caseType, "case_status": caseStatus,
		"amount_fen": plan.AmountFen, "currency": plan.Currency,
		"provider_transaction_id":  plan.ProviderTransactionID,
		"provider_snapshot_digest": plan.ProviderSnapshotDigest,
		"payment_evidence_id":      plan.PaymentEvidenceID.String(), "evidence_status": evidenceStatus,
		"evidence_payload_digest": evidenceDigest, "internal_order_exists": internalOrder,
		"internal_transaction_exists": internalTransaction, "recheck_case_count": plan.RecheckCaseCount,
		"ledger_effect":  reconciliationRepairLedgerEffect,
		"policy_version": policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return ReconciliationFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	return plan, nil
}

// reconciliationFinancialRepairProofProjection is the single closed owner
// projection shared by the request assembly path and the authoritative proof
// recomputation.
func reconciliationFinancialRepairProofProjection(plan ReconciliationFinancialRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"run_id": plan.RunID.String(), "case_id": plan.CaseID.String(),
		"provider_snapshot_digest": plan.ProviderSnapshotDigest, "facts_digest": plan.FactsDigest, "policy_version": plan.PolicyVersion,
	})
	financial, _ := json.Marshal(map[string]any{
		"amount_fen": plan.AmountFen, "currency": plan.Currency,
		"provider_transaction_id": plan.ProviderTransactionID, "ledger_effect": reconciliationRepairLedgerEffect,
	})
	return critical, financial
}

// ReconciliationFinancialRepairProofInput derives the closed proof-era
// projection of one provider-paid/internal-missing difference for a request that
// is still being assembled. It is deliberately not activation-gated.
func (r *PostgresRepository) ReconciliationFinancialRepairProofInput(ctx context.Context, target ReconciliationFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == reconciliationFinancialRepairCommand {
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
	plan, err := reconciliationFinancialRepairPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := reconciliationFinancialRepairProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: reconciliationFinancialRepairCommand, PolicyVersion: activation.PolicyVersion,
		SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType:    "RECONCILIATION_CASE", TargetResourceID: plan.CaseID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"reconciliation_case_id": plan.CaseID.String(), "payment_evidence_id": plan.PaymentEvidenceID.String(),
			"business_date": plan.BusinessDate, "resolution_code": reconciliationRepairResolutionCode,
		}),
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

// ApplyReconciliationFinancialRepair closes exactly one reconciliation
// difference with zero accounting effect. The case is resolved because the
// provider side already refunded or reversed the amount, the repair-then-recheck
// step proves no other OPEN difference remains for the merchant order, and the
// immutable case row records the closure. No ledger, wallet, receivable,
// package, order, transaction or evidence fact is written or changed.
func (r *PostgresRepository) ApplyReconciliationFinancialRepair(ctx context.Context, input ReconciliationFinancialRepairExecutionInput) (ReconciliationFinancialRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion != 1 || input.SchemaVersion != 1 || input.ActivationVersion < 1 {
		return ReconciliationFinancialRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, found, err := reconciliationFinancialRepairCaseMatches(ctx, tx, input, executionKey); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	} else if found {
		return stored, tx.Commit(ctx)
	}
	var channel, businessDate, merchant string
	if err := tx.QueryRow(ctx, `SELECT r.channel,r.business_date::text,c.merchant_order_no FROM reconciliation_cases c JOIN reconciliation_runs r ON r.id=c.reconciliation_run_id WHERE c.id=$1`, input.Command.ReconciliationCaseID).Scan(&channel, &businessDate, &merchant); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	if err := lockTransactionKey(ctx, tx, "reconciliation-repair-source", channel+":"+businessDate+":"+merchant); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	if err := lockTransactionKey(ctx, tx, reconciliationRepairLockScope, input.Command.ReconciliationCaseID.String()); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM reconciliation_cases WHERE id=$1 FOR UPDATE`, input.Command.ReconciliationCaseID).Scan(&lockedID); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT m.id FROM manual_review_cases m JOIN reconciliation_cases c ON c.manual_review_case_id=m.id WHERE c.id=$1 FOR UPDATE OF m`, input.Command.ReconciliationCaseID)
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	for rows.Next() {
		if err := rows.Scan(&lockedID); err != nil {
			rows.Close()
			return ReconciliationFinancialRepairResult{}, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	now := r.now()
	if err := reconciliationFinancialRepairExecutionActivation(ctx, tx, input, now); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	plan, err := reconciliationFinancialRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	if mismatch := reconciliationFinancialRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return ReconciliationFinancialRepairResult{}, wrapSnapshotMismatch(mismatch)
	}

	tag, err := tx.Exec(ctx, `UPDATE reconciliation_cases SET status='RESOLVED',resolution=$2,resolved_at=$3,updated_at=$3,version=version+1
WHERE id=$1 AND status='OPEN' AND resolved_at IS NULL AND case_type='PROVIDER_PAID_INTERNAL_MISSING' AND version=$4`,
		plan.CaseID, reconciliationRepairResolutionCode, now, plan.CaseVersion)
	if err != nil || tag.RowsAffected() != 1 {
		return ReconciliationFinancialRepairResult{}, reconciliationRepairConflict("reconciliation_case_cas", err)
	}
	if plan.ManualReviewCaseID != nil {
		tag, err := tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1 AND status IN ('OPEN','IN_REVIEW')`, *plan.ManualReviewCaseID, now)
		if err != nil || tag.RowsAffected() != 1 {
			return ReconciliationFinancialRepairResult{}, reconciliationRepairConflict("manual_review_cas", err)
		}
		if err := appendManualReviewEvent(ctx, tx, *plan.ManualReviewCaseID, "RESOLVED", nil, map[string]any{"resolution_code": reconciliationRepairResolutionCode, "approval_request_id": input.ApprovalRequestID}, now); err != nil {
			return ReconciliationFinancialRepairResult{}, err
		}
	}
	caseID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_repair_cases(id,approval_request_id,authorization_event_id,
approval_epoch,execution_key_digest,reconciliation_run_id,reconciliation_case_id,channel,business_date,merchant_order_no,
amount_fen,currency,payment_evidence_id,provider_snapshot_digest,facts_digest,resolution_code,recheck_case_count,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,'EXECUTED',$18)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		legacyFinancialExecutionKeyDigestV1(executionKey), plan.RunID, plan.CaseID, plan.Channel, plan.BusinessDate,
		plan.MerchantOrderNo, plan.AmountFen, plan.Currency, plan.PaymentEvidenceID, plan.ProviderSnapshotDigest,
		plan.FactsDigest, reconciliationRepairResolutionCode, plan.RecheckCaseCount, now); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	stored, err := r.ReconciliationFinancialRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored.CaseID != plan.CaseID || stored.RunID != plan.RunID || stored.Status != "SUCCEEDED" ||
		stored.RecheckCaseCount != 0 || stored.LedgerEffect != reconciliationRepairLedgerEffect {
		if err == nil {
			err = ErrConflict
		}
		return ReconciliationFinancialRepairResult{}, err
	}
	return stored, nil
}

// ReconciliationFinancialRepairForRequest reads the committed closure of one
// approval request without creating any new fact (result-first).
func (r *PostgresRepository) ReconciliationFinancialRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (ReconciliationFinancialRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return ReconciliationFinancialRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readReconciliationFinancialRepair(ctx, tx, approvalRequestID)
	if err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReconciliationFinancialRepairResult{}, err
	}
	return result, nil
}

func readReconciliationFinancialRepair(ctx context.Context, source reconciliationRepairReader, approvalRequestID uuid.UUID) (ReconciliationFinancialRepairResult, error) {
	var result ReconciliationFinancialRepairResult
	var providerTransactionID *string
	var businessDate time.Time
	if err := source.QueryRow(ctx, `SELECT repair.reconciliation_case_id,repair.reconciliation_run_id,repair.channel,
repair.business_date,repair.merchant_order_no,repair.amount_fen,repair.currency,repair.facts_digest,
repair.payment_evidence_id,repair.resolution_code,repair.recheck_case_count,
reconciliation_case.provider_snapshot->>'channel_transaction_id'
FROM reconciliation_repair_cases repair
JOIN reconciliation_cases reconciliation_case ON reconciliation_case.id=repair.reconciliation_case_id
WHERE repair.approval_request_id=$1`, approvalRequestID).
		Scan(&result.CaseID, &result.RunID, &result.Channel, &businessDate, &result.MerchantOrderNo,
			&result.AmountFen, &result.Currency, &result.FactsDigest, &result.PaymentEvidenceID,
			&result.ResolutionCode, &result.RecheckCaseCount, &providerTransactionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReconciliationFinancialRepairResult{}, ErrNotFound
		}
		return ReconciliationFinancialRepairResult{}, err
	}
	if providerTransactionID != nil {
		result.ProviderTransactionID = *providerTransactionID
	}
	result.BusinessDate = businessDate.UTC().Format("2006-01-02")
	result.LedgerEffect = reconciliationRepairLedgerEffect
	result.Status = "SUCCEEDED"
	return result, nil
}

// reconciliationFinancialRepairCaseMatches is the idempotent-hit check: the
// stored case must belong to the same reconciliation case, approval request and
// execution key. A case for the same reconciliation case under a different
// approval is a conflict, not a second closure.
func reconciliationFinancialRepairCaseMatches(ctx context.Context, source reconciliationRepairReader,
	input ReconciliationFinancialRepairExecutionInput, executionKey string) (ReconciliationFinancialRepairResult, bool, error) {
	var storedID, storedRequestID, storedEventID uuid.UUID
	var storedEpoch int64
	var storedDigest string
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest
FROM reconciliation_repair_cases WHERE reconciliation_case_id=$1`, input.Command.ReconciliationCaseID).
		Scan(&storedID, &storedRequestID, &storedEventID, &storedEpoch, &storedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReconciliationFinancialRepairResult{}, false, nil
	}
	if err != nil {
		return ReconciliationFinancialRepairResult{}, false, err
	}
	if storedRequestID != input.ApprovalRequestID || storedEventID != input.AuthorizationEventID ||
		storedEpoch != input.ApprovalEpoch || storedDigest != legacyFinancialExecutionKeyDigestV1(executionKey) {
		return ReconciliationFinancialRepairResult{}, false, ErrConflict
	}
	stored, err := readReconciliationFinancialRepair(ctx, source, input.ApprovalRequestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ReconciliationFinancialRepairResult{}, false, ErrConflict
		}
		return ReconciliationFinancialRepairResult{}, false, err
	}
	return stored, stored.CaseID == input.Command.ReconciliationCaseID, nil
}

// reconciliationFinancialRepairFrozenMismatch compares the frozen wire
// projection with the owner-recomputed plan; fresh execution rejects empty facts.
func reconciliationFinancialRepairFrozenMismatch(plan ReconciliationFinancialRepairPlan, frozen ReconciliationFinancialRepairFrozenFacts) string {
	switch {
	case plan.RunID != frozen.RunID:
		return "run_id"
	case plan.AmountFen != frozen.AmountFen:
		return "amount_fen"
	case plan.Currency != frozen.Currency:
		return "currency"
	case plan.ProviderTransactionID != frozen.ProviderTransactionID:
		return "provider_transaction_id"
	case plan.ProviderSnapshotDigest != frozen.ProviderSnapshotDigest:
		return "provider_snapshot_digest"
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}

func reconciliationFinancialRepairExecutionActivation(ctx context.Context, source reconciliationRepairReader, input ReconciliationFinancialRepairExecutionInput, now time.Time) error {
	active := ApprovalCommandActivation{CommandType: reconciliationFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE"}
	var enabled bool
	err := source.QueryRow(ctx, `SELECT active,activation_version,updated_at FROM billing_approval_execution.lock_reconciliation_activation()`).Scan(&enabled, &active.Version, &active.UpdatedAt)
	if enabled {
		active.State = "ACTIVE"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalSnapshotMismatch
	}
	if err != nil {
		return err
	}
	if !validActivationItem(active, reconciliationFinancialRepairCommand, billingApprovalActivationSpecs[reconciliationFinancialRepairCommand], now) ||
		active.State != "ACTIVE" || active.PolicyVersion != input.PolicyVersion || int64(active.SnapshotSchemaVersion) != input.SchemaVersion || active.Version != input.ActivationVersion {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}
