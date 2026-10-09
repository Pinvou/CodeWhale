package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// chargebackFinancialRepairCommand is the closed command name shared by the
// proof authority, the activation projection and the consumer mapping.
const chargebackFinancialRepairCommand = "CHARGEBACK_FINANCIAL_REPAIR"

// chargebackFinancialRepairReason is the only reason code the first release
// admits: the BASE shortfall of a confirmed provider chargeback.
const chargebackFinancialRepairReason = "BASE_SHORTFALL_COMPENSATION"

// chargebackRepairReferencePrefix is the closed business reference of the
// compensation journal and receivable of one repaired chargeback case.
const chargebackRepairReferencePrefix = "chargeback-repair:"

// chargebackRepairResolutionReason is the fixed audit reason recorded on the
// chargeback case; the operator-facing approval reason stays on the Account
// approval request and is never copied into Billing facts.
const chargebackRepairResolutionReason = "approved chargeback BASE shortfall compensation"

// chargebackRepairLockScope is the advisory lock the existing confirmation path
// takes for one payment evidence row; the repair takes the same lock so the two
// writers can never interleave for the same chargeback.
const chargebackRepairLockScope = "chargeback.apply"

type ChargebackFinancialRepairResult struct {
	CaseID                    uuid.UUID `json:"case_id"`
	ChargebackCaseID          uuid.UUID `json:"chargeback_case_id"`
	WalletID                  uuid.UUID `json:"wallet_id"`
	PackageID                 uuid.UUID `json:"package_id"`
	PaymentEvidenceID         uuid.UUID `json:"payment_evidence_id"`
	BaseShortfallMicrocredits int64     `json:"base_shortfall_microcredits"`
	ComponentDigest           string    `json:"component_digest"`
	FactsDigest               string    `json:"facts_digest"`
	JournalEntryID            uuid.UUID `json:"journal_entry_id"`
	ReceivableID              uuid.UUID `json:"receivable_id"`
	ProviderChargebackID      string    `json:"provider_chargeback_id"`
	Status                    string    `json:"status"`
}

// ChargebackFinancialRepairCommandInput is the closed command the approval
// froze. The caller only names the chargeback case and the persisted provider
// evidence; the shortfall, the wallet/package projection and every amount are
// owner-derived.
type ChargebackFinancialRepairCommandInput struct {
	ChargebackCaseID  uuid.UUID
	PaymentEvidenceID uuid.UUID
}

// chargebackRepairComponent is one package component as the closed plan sees it.
// Revocable must be zero for every component: any remaining availability means
// the case belongs to the existing confirmation path, not to this repair.
type chargebackRepairComponent struct {
	ID                                                      uuid.UUID
	ComponentType                                           string
	EffectiveOriginal                                       int64
	Revocable                                               int64
	Original, ReclassifiedOut, Available, Active, Suspended int64
}

// ChargebackFinancialRepairPlan is the closed, recomputable owner projection of
// one approved chargeback shortfall compensation.
type ChargebackFinancialRepairPlan struct {
	PolicyVersion             int64
	ChargebackCaseID          uuid.UUID
	CaseVersion               int64
	EvidenceID                uuid.UUID
	AnomalyCaseID             uuid.UUID
	AnomalyVersion            int64
	AnomalyStatus             string
	PaymentOrderID            uuid.UUID
	PaymentTransactionID      uuid.UUID
	PackageID                 uuid.UUID
	WalletID                  uuid.UUID
	WalletVersion             int64
	ProviderChargebackID      string
	BaseShortfallMicrocredits int64
	ComponentDigest           string
	FactsDigest               string
}

// ChargebackFinancialRepairFrozenFacts is the wire-visible owner projection an
// approval froze. Fresh execution requires the exact owner digest. A matching
// immutable committed result is returned before these fresh snapshot gates.
type ChargebackFinancialRepairFrozenFacts struct {
	CaseVersion               int64
	BaseShortfallMicrocredits int64
	ComponentDigest           string
	WalletID                  uuid.UUID
	WalletVersion             int64
	PackageID                 uuid.UUID
	FactsDigest               string
}

type ChargebackFinancialRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              ChargebackFinancialRepairCommandInput
	Frozen               ChargebackFinancialRepairFrozenFacts
}

// chargebackRepairReader is the read surface the closed plan needs; it is
// satisfied by both the transaction and the pool.
type chargebackRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// chargebackRepairConflict keeps the stable ErrConflict identity while naming
// the owner CAS that failed, which is what the operator reads in the audit
// trail.
func chargebackRepairConflict(stage string, cause error) error {
	if cause == nil {
		return errors.Join(ErrConflict, errors.New(stage))
	}
	return errors.Join(ErrConflict, fmt.Errorf("%s: %w", stage, cause))
}

// chargebackFinancialRepairPlanFrom derives the only permitted compensation from
// already-open owner reads. Every rejected contract returns
// ErrApprovalSnapshotMismatch (or ErrNotFound) so the caller classifies it as a
// definite, zero-write rejection.
func chargebackFinancialRepairPlanFrom(ctx context.Context, source chargebackRepairReader, input ChargebackFinancialRepairCommandInput,
	policyVersion, schemaVersion, activationVersion int64) (ChargebackFinancialRepairPlan, error) {
	if input.ChargebackCaseID == uuid.Nil || input.PaymentEvidenceID == uuid.Nil ||
		policyVersion != 1 || schemaVersion != 1 || activationVersion < 1 {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := ChargebackFinancialRepairPlan{ChargebackCaseID: input.ChargebackCaseID, EvidenceID: input.PaymentEvidenceID, PolicyVersion: policyVersion}
	var caseStatus string
	var revokedBase, revokedBonus int64
	var caseReceivableID *uuid.UUID
	if err := source.QueryRow(ctx, `SELECT payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,
wallet_id,provider_chargeback_id,status,revoked_base_microcredits,revoked_bonus_microcredits,receivable_id,version
FROM chargeback_cases WHERE id=$1`, input.ChargebackCaseID).
		Scan(&plan.EvidenceID, &plan.PaymentOrderID, &plan.PaymentTransactionID, &plan.PackageID, &plan.WalletID,
			&plan.ProviderChargebackID, &caseStatus, &revokedBase, &revokedBonus, &caseReceivableID, &plan.CaseVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrNotFound
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	// The frozen evidence must be the case's own evidence, the case must still be
	// untouched, and the provider reference must be a closed non-empty token.
	if plan.EvidenceID != input.PaymentEvidenceID || caseStatus != "REVIEW_REQUIRED" || revokedBase != 0 || revokedBonus != 0 ||
		caseReceivableID != nil || plan.CaseVersion < 1 || plan.PaymentOrderID == uuid.Nil ||
		plan.PaymentTransactionID == uuid.Nil || plan.PackageID == uuid.Nil || plan.WalletID == uuid.Nil ||
		strings.TrimSpace(plan.ProviderChargebackID) != plan.ProviderChargebackID || plan.ProviderChargebackID == "" {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var evidenceType, evidenceStatus, evidenceKey, payloadDigest string
	var evidenceOrderID, evidenceTransactionID uuid.UUID
	if err := source.QueryRow(ctx, `SELECT payment_order_id,payment_transaction_id,evidence_type,evidence_key,
COALESCE(provider_status,''),payload_digest FROM payment_evidence WHERE id=$1`, input.PaymentEvidenceID).
		Scan(&evidenceOrderID, &evidenceTransactionID, &evidenceType, &evidenceKey, &evidenceStatus, &payloadDigest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrNotFound
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	if evidenceOrderID != plan.PaymentOrderID || evidenceTransactionID != plan.PaymentTransactionID ||
		evidenceType != "CHARGEBACK" || evidenceStatus != "CHARGEBACK_CONFIRMED" ||
		evidenceKey != plan.ProviderChargebackID || !validHexDigest(payloadDigest) {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var transactionStatus string
	if err := source.QueryRow(ctx, `SELECT status FROM payment_transactions WHERE id=$1`, plan.PaymentTransactionID).
		Scan(&transactionStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	if transactionStatus != "PAID" && transactionStatus != "LATE_PAID" {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var packageWalletID uuid.UUID
	if err := source.QueryRow(ctx, `SELECT wallet_id FROM entitlement_packages WHERE id=$1`, plan.PackageID).Scan(&packageWalletID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	if packageWalletID != plan.WalletID {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var walletStatus string
	var walletPosted int64
	if err := source.QueryRow(ctx, `SELECT status,posted_microcredits,version FROM wallets WHERE id=$1`, plan.WalletID).
		Scan(&walletStatus, &walletPosted, &plan.WalletVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	if walletStatus != "ACTIVE" || plan.WalletVersion < 1 || walletPosted < 0 {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Every component must be fully consumed: anything still revocable belongs to
	// the existing CONFIRM_CHARGEBACK path, and this command must not overlap it.
	rows, err := source.Query(ctx, `SELECT id,component_type,original_microcredits,reclassified_out_microcredits,
available_microcredits,active_grant_microcredits,suspended_grant_microcredits
FROM entitlement_components WHERE package_id=$1 ORDER BY id`, plan.PackageID)
	if err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	components := []chargebackRepairComponent{}
	var baseShortfall int64
	for rows.Next() {
		var item chargebackRepairComponent
		var historicalOriginal, reclassifiedOut int64
		var available, active, suspended int64
		if err := rows.Scan(&item.ID, &item.ComponentType, &historicalOriginal, &reclassifiedOut,
			&available, &active, &suspended); err != nil {
			rows.Close()
			return ChargebackFinancialRepairPlan{}, err
		}
		effective, err := effectiveComponentOriginal(historicalOriginal, reclassifiedOut)
		if err != nil {
			rows.Close()
			return ChargebackFinancialRepairPlan{}, err
		}
		item.Original, item.ReclassifiedOut = historicalOriginal, reclassifiedOut
		item.Available, item.Active, item.Suspended = available, active, suspended
		item.EffectiveOriginal = effective
		item.Revocable = available + active + suspended
		if item.Revocable != 0 {
			// Anything still revocable belongs to the existing CONFIRM_CHARGEBACK
			// path; the closed plan reports a definite zero-write denial instead of
			// partially compensating the same case.
			rows.Close()
			return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		if item.ComponentType == "BASE" {
			if effective < 0 {
				rows.Close()
				return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
			}
			baseShortfall += effective
		}
		components = append(components, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ChargebackFinancialRepairPlan{}, err
	}
	rows.Close()
	if len(components) == 0 || baseShortfall <= 0 {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.BaseShortfallMicrocredits = baseShortfall
	sort.Slice(components, func(i, j int) bool { return components[i].ID.String() < components[j].ID.String() })
	type componentFacts struct {
		ComponentID           string `json:"component_id"`
		ComponentType         string `json:"component_type"`
		EffectiveOriginal     int64  `json:"effective_original_microcredits"`
		RevocableMicrocredits int64  `json:"revocable_microcredits"`
		Original              int64  `json:"original_microcredits"`
		ReclassifiedOut       int64  `json:"reclassified_out_microcredits"`
		Available             int64  `json:"available_microcredits"`
		Active                int64  `json:"active_grant_microcredits"`
		Suspended             int64  `json:"suspended_grant_microcredits"`
	}
	facts := make([]componentFacts, 0, len(components))
	for _, item := range components {
		facts = append(facts, componentFacts{ComponentID: item.ID.String(), ComponentType: item.ComponentType,
			EffectiveOriginal: item.EffectiveOriginal, RevocableMicrocredits: item.Revocable, Original: item.Original, ReclassifiedOut: item.ReclassifiedOut, Available: item.Available, Active: item.Active, Suspended: item.Suspended})
	}
	digest, err := json.Marshal(map[string]any{
		"command_type": chargebackFinancialRepairCommand, "policy_version": policyVersion,
		"snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
		"chargeback_case_id": plan.ChargebackCaseID.String(), "chargeback_case_version": plan.CaseVersion,
		"payment_evidence_id": plan.EvidenceID.String(), "provider_chargeback_id": plan.ProviderChargebackID,
		"payment_order_id": plan.PaymentOrderID.String(), "payment_transaction_id": plan.PaymentTransactionID.String(),
		"package_id": plan.PackageID.String(), "wallet_id": plan.WalletID.String(), "wallet_version": plan.WalletVersion,
		"base_shortfall_microcredits": plan.BaseShortfallMicrocredits, "components": facts,
	})
	if err != nil {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.ComponentDigest = digestHex(digest)
	// Zero facts: no previous compensation journal, receivable or case row, and no
	// historical forced refund or refund row for the same package. The existing
	// predicate deliberately remains unchanged while eligibility is reviewed.
	var journalExists, receivableExists, caseExists, conflictCase bool
	reference := chargebackRepairReferencePrefix + plan.ChargebackCaseID.String()
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM journal_entries WHERE business_reference=$2),
 EXISTS(SELECT 1 FROM receivables WHERE source_type='CHARGEBACK' AND source_reference=$2),
 EXISTS(SELECT 1 FROM chargeback_financial_repair_cases WHERE chargeback_case_id=$1),
 EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$3) OR
 EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=$3)`,
		plan.ChargebackCaseID, reference, plan.PackageID).Scan(&journalExists, &receivableExists, &caseExists, &conflictCase); err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	if journalExists || receivableExists || caseExists || conflictCase {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if err := source.QueryRow(ctx, `SELECT id,financial_action_status,version FROM payment_anomaly_cases
WHERE chargeback_case_id=$1 AND anomaly_type='CHARGEBACK'`, plan.ChargebackCaseID).
		Scan(&plan.AnomalyCaseID, &plan.AnomalyStatus, &plan.AnomalyVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return ChargebackFinancialRepairPlan{}, err
	}
	if plan.AnomalyVersion < 1 || (plan.AnomalyStatus != "PENDING" && plan.AnomalyStatus != "APPLIED") {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Persisted source tuples are part of the frozen owner facts. Decode JSONB
	// with Number before hashing so wallet and financial bigint tokens stay exact.
	var ownerRaw []byte
	if err := source.QueryRow(ctx, `SELECT jsonb_build_object(
 'case',(SELECT to_jsonb(c) FROM chargeback_cases c WHERE c.id=$1),
 'evidence',(SELECT to_jsonb(e) FROM payment_evidence e WHERE e.id=$2),
 'anomaly',(SELECT to_jsonb(a) FROM payment_anomaly_cases a WHERE a.id=$3),
 'order',(SELECT to_jsonb(o) FROM payment_orders o WHERE o.id=$4),
 'transaction',(SELECT to_jsonb(t) FROM payment_transactions t WHERE t.id=$5),
 'package',(SELECT to_jsonb(p) FROM entitlement_packages p WHERE p.id=$6),
 'components',COALESCE((SELECT jsonb_agg(jsonb_build_object('component',to_jsonb(c),'lot',to_jsonb(l)) ORDER BY c.id) FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id WHERE c.package_id=$6),'[]'::jsonb),
 'wallet',(SELECT jsonb_build_object('id',w.id,'status',w.status,'version',w.version,'posted_microcredits',w.posted_microcredits,'receivable_microcredits',w.receivable_microcredits,'financial_status',w.financial_status) FROM wallets w WHERE w.id=$7))`, plan.ChargebackCaseID, plan.EvidenceID, plan.AnomalyCaseID, plan.PaymentOrderID, plan.PaymentTransactionID, plan.PackageID, plan.WalletID).Scan(&ownerRaw); err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	ownerSnapshot, err := paymentRepairJSONSnapshot(ownerRaw)
	if err != nil {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	encoded, err := json.Marshal(map[string]any{
		"command_type": chargebackFinancialRepairCommand, "owner_snapshot": ownerSnapshot, "zero_facts": []bool{journalExists, receivableExists, caseExists, conflictCase}, "chargeback_case_id": plan.ChargebackCaseID.String(),
		"case_version": plan.CaseVersion, "case_status": caseStatus,
		"provider_chargeback_id": plan.ProviderChargebackID, "payment_evidence_id": plan.EvidenceID.String(),
		"evidence_status": evidenceStatus, "evidence_payload_digest": payloadDigest,
		"payment_order_id": plan.PaymentOrderID.String(), "payment_transaction_id": plan.PaymentTransactionID.String(),
		"transaction_status": transactionStatus, "package_id": plan.PackageID.String(),
		"wallet_id": plan.WalletID.String(), "wallet_version": plan.WalletVersion,
		"base_shortfall_microcredits": plan.BaseShortfallMicrocredits, "component_digest": plan.ComponentDigest,
		"anomaly_case_id": plan.AnomalyCaseID.String(), "anomaly_version": plan.AnomalyVersion,
		"anomaly_financial_action_status": plan.AnomalyStatus,
		"policy_version":                  policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	return plan, nil
}

// chargebackFinancialRepairProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof
// recomputation.
func chargebackFinancialRepairProofProjection(plan ChargebackFinancialRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"chargeback_case_version": plan.CaseVersion, "facts_digest": plan.FactsDigest, "policy_version": plan.PolicyVersion,
	})
	financial, _ := json.Marshal(map[string]any{
		"base_shortfall_microcredits": plan.BaseShortfallMicrocredits,
		"wallet_id":                   plan.WalletID.String(),
		"wallet_version":              plan.WalletVersion,
		"package_id":                  plan.PackageID.String(),
		"component_digest":            plan.ComponentDigest,
		"provider_chargeback_id":      plan.ProviderChargebackID,
	})
	return critical, financial
}

// ChargebackFinancialRepairProofInput derives the closed proof-era projection of
// one confirmed chargeback shortfall for a request that is still being
// assembled. It is deliberately not activation-gated.
func (r *PostgresRepository) ChargebackFinancialRepairProofInput(ctx context.Context, target ChargebackFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == chargebackFinancialRepairCommand {
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
	plan, err := chargebackFinancialRepairPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := chargebackFinancialRepairProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: chargebackFinancialRepairCommand, PolicyVersion: activation.PolicyVersion,
		SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType:    "CHARGEBACK_CASE", TargetResourceID: plan.ChargebackCaseID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"chargeback_case_id": plan.ChargebackCaseID.String(), "payment_evidence_id": plan.EvidenceID.String(),
			"expected_case_version": plan.CaseVersion, "reason_code": chargebackFinancialRepairReason,
		}),
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

// ApplyChargebackFinancialRepair commits exactly one BASE shortfall
// compensation: the receivable with its wallet projection, the compensation
// CHARGEBACK journal, the chargeback case transition, the escalated anomaly and
// the immutable case row. No component, lot, wallet balance or BONUS fact moves.
func (r *PostgresRepository) ApplyChargebackFinancialRepair(ctx context.Context, input ChargebackFinancialRepairExecutionInput) (ChargebackFinancialRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return ChargebackFinancialRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, found, err := chargebackFinancialRepairCaseMatches(ctx, tx, input, executionKey); err != nil {
		return ChargebackFinancialRepairResult{}, err
	} else if found {
		return stored, tx.Commit(ctx)
	}
	// Match the ordinary confirmation lock order before deriving fresh facts.
	if err := lockTransactionKey(ctx, tx, chargebackRepairLockScope, input.Command.PaymentEvidenceID.String()); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	var orderID, transactionID, packageID, walletID, evidenceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT payment_order_id,payment_transaction_id,entitlement_package_id,wallet_id,payment_evidence_id FROM chargeback_cases WHERE id=$1`, input.Command.ChargebackCaseID).Scan(&orderID, &transactionID, &packageID, &walletID, &evidenceID); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if evidenceID != input.Command.PaymentEvidenceID {
		return ChargebackFinancialRepairResult{}, ErrApprovalSnapshotMismatch
	}
	if err := lockPaymentSourcePackage(ctx, tx, packageID, transactionID, orderID, walletID); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM chargeback_cases WHERE id=$1 FOR UPDATE`, input.Command.ChargebackCaseID).Scan(&lockedID); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 ORDER BY id FOR UPDATE`, packageID)
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	for rows.Next() {
		if err := rows.Scan(&lockedID); err != nil {
			rows.Close()
			return ChargebackFinancialRepairResult{}, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ChargebackFinancialRepairResult{}, err
	}
	rows.Close()
	now := r.now()
	if err := chargebackFinancialRepairExecutionActivation(ctx, tx, input, now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	plan, err := chargebackFinancialRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if mismatch := chargebackFinancialRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return ChargebackFinancialRepairResult{}, wrapSnapshotMismatch(mismatch)
	}

	tag, err := tx.Exec(ctx, `UPDATE chargeback_cases SET status='CONFIRMED',resolution_code='CONFIRM_CHARGEBACK_SHORTFALL',
resolution_reason=$3,resolved_at=$4,version=version+1
WHERE id=$1 AND version=$2 AND status='REVIEW_REQUIRED' AND revoked_base_microcredits=0
AND revoked_bonus_microcredits=0 AND receivable_id IS NULL`,
		plan.ChargebackCaseID, plan.CaseVersion, chargebackRepairResolutionReason, now)
	if err != nil || tag.RowsAffected() != 1 {
		return ChargebackFinancialRepairResult{}, chargebackRepairConflict("chargeback_case_cas", err)
	}
	receivable, err := createReceivableWithinTransaction(ctx, tx, plan.WalletID, "CHARGEBACK",
		chargebackRepairReferencePrefix+plan.ChargebackCaseID.String(), plan.BaseShortfallMicrocredits,
		&plan.PaymentOrderID, &plan.PaymentTransactionID, &plan.PackageID, nil, now)
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	caseID, entryID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := insertJournal(ctx, tx, entryID, "CHARGEBACK",
		chargebackRepairReferencePrefix+plan.ChargebackCaseID.String(), "chargeback shortfall compensation", now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if err := insertPosting(ctx, tx, entryID, receivableAccount, -plan.BaseShortfallMicrocredits, nil, now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if err := insertPosting(ctx, tx, entryID, chargebackClearingAccount, plan.BaseShortfallMicrocredits, nil, now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	tag, err = tx.Exec(ctx, `UPDATE chargeback_cases SET receivable_id=$3 WHERE id=$1 AND version=$2 AND status='CONFIRMED'`,
		plan.ChargebackCaseID, plan.CaseVersion+1, receivable.ID)
	if err != nil || tag.RowsAffected() != 1 {
		return ChargebackFinancialRepairResult{}, chargebackRepairConflict("chargeback_case_receivable", err)
	}
	if plan.AnomalyStatus == "PENDING" {
		tag, err = tx.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='APPLIED',version=version+1,updated_at=$3
WHERE id=$1 AND anomaly_type='CHARGEBACK' AND financial_action_status='PENDING' AND version=$2`,
			plan.AnomalyCaseID, plan.AnomalyVersion, now)
		if err != nil || tag.RowsAffected() != 1 {
			return ChargebackFinancialRepairResult{}, chargebackRepairConflict("anomaly_cas", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders payment_order SET chargeback_summary=(SELECT jsonb_build_object('open_count',count(*) FILTER(WHERE chargeback.status='CONFIRMED'),'lost_amount_fen',COALESCE(sum(transaction.amount_fen) FILTER(WHERE chargeback.status='CONFIRMED'),0)::text) FROM chargeback_cases chargeback JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id WHERE chargeback.payment_order_id=$1),attention_summary=CASE WHEN EXISTS(SELECT 1 FROM receivables receivable WHERE receivable.payment_order_id=$1 AND receivable.outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN' WHEN payment_order.attention_summary='RECEIVABLE_OPEN' THEN 'NORMAL' ELSE payment_order.attention_summary END,summary_version=summary_version+1,updated_at=$2 WHERE payment_order.id=$1`, plan.PaymentOrderID, now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chargeback_financial_repair_cases(id,approval_request_id,authorization_event_id,
approval_epoch,execution_key_digest,chargeback_case_id,anomaly_case_id,wallet_id,package_id,payment_evidence_id,
base_shortfall_microcredits,component_digest,facts_digest,journal_entry_id,receivable_id,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'EXECUTED',$16)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		legacyFinancialExecutionKeyDigestV1(executionKey), plan.ChargebackCaseID, plan.AnomalyCaseID, plan.WalletID,
		plan.PackageID, plan.EvidenceID, plan.BaseShortfallMicrocredits, plan.ComponentDigest, plan.FactsDigest,
		entryID, receivable.ID, now); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	stored, err := r.ChargebackFinancialRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored.CaseID != caseID || stored.JournalEntryID != entryID ||
		stored.ReceivableID != receivable.ID || stored.Status != "SUCCEEDED" ||
		stored.BaseShortfallMicrocredits != plan.BaseShortfallMicrocredits {
		if err == nil {
			err = ErrConflict
		}
		return ChargebackFinancialRepairResult{}, err
	}
	return stored, nil
}

// ChargebackFinancialRepairForRequest reads the committed compensation of one
// approval request without creating any new fact (result-first).
func (r *PostgresRepository) ChargebackFinancialRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (ChargebackFinancialRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return ChargebackFinancialRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readChargebackFinancialRepair(ctx, tx, approvalRequestID)
	if err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	return result, nil
}

func readChargebackFinancialRepair(ctx context.Context, source chargebackRepairReader, approvalRequestID uuid.UUID) (ChargebackFinancialRepairResult, error) {
	var result ChargebackFinancialRepairResult
	if err := source.QueryRow(ctx, `SELECT id,chargeback_case_id,wallet_id,package_id,payment_evidence_id,
base_shortfall_microcredits,component_digest,facts_digest,journal_entry_id,receivable_id
FROM chargeback_financial_repair_cases WHERE approval_request_id=$1`, approvalRequestID).
		Scan(&result.CaseID, &result.ChargebackCaseID, &result.WalletID, &result.PackageID, &result.PaymentEvidenceID,
			&result.BaseShortfallMicrocredits, &result.ComponentDigest, &result.FactsDigest,
			&result.JournalEntryID, &result.ReceivableID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChargebackFinancialRepairResult{}, ErrNotFound
		}
		return ChargebackFinancialRepairResult{}, err
	}
	if err := source.QueryRow(ctx, `SELECT provider_chargeback_id FROM chargeback_cases WHERE id=$1`, result.ChargebackCaseID).
		Scan(&result.ProviderChargebackID); err != nil {
		return ChargebackFinancialRepairResult{}, err
	}
	result.Status = "SUCCEEDED"
	return result, nil
}

// chargebackFinancialRepairCaseMatches is the idempotent-hit check: the stored
// case must belong to the same chargeback, approval request and execution key.
// A case for the same chargeback under a different approval is a conflict, not
// a second compensation.
func chargebackFinancialRepairCaseMatches(ctx context.Context, source chargebackRepairReader,
	input ChargebackFinancialRepairExecutionInput, executionKey string) (ChargebackFinancialRepairResult, bool, error) {
	var storedID, storedRequestID, storedEventID uuid.UUID
	var storedEpoch int64
	var storedDigest string
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest
FROM chargeback_financial_repair_cases WHERE chargeback_case_id=$1`, input.Command.ChargebackCaseID).
		Scan(&storedID, &storedRequestID, &storedEventID, &storedEpoch, &storedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargebackFinancialRepairResult{}, false, nil
	}
	if err != nil {
		return ChargebackFinancialRepairResult{}, false, err
	}
	if storedRequestID != input.ApprovalRequestID || storedEventID != input.AuthorizationEventID ||
		storedEpoch != input.ApprovalEpoch || storedDigest != legacyFinancialExecutionKeyDigestV1(executionKey) {
		return ChargebackFinancialRepairResult{}, false, ErrConflict
	}
	stored, err := readChargebackFinancialRepair(ctx, source, input.ApprovalRequestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ChargebackFinancialRepairResult{}, false, ErrConflict
		}
		return ChargebackFinancialRepairResult{}, false, err
	}
	return stored, stored.CaseID == storedID, nil
}

// chargebackFinancialRepairFrozenMismatch compares the frozen wire projection
// with the owner-recomputed plan. Fresh execution never accepts an empty digest.
func chargebackFinancialRepairFrozenMismatch(plan ChargebackFinancialRepairPlan, frozen ChargebackFinancialRepairFrozenFacts) string {
	switch {
	case plan.CaseVersion != frozen.CaseVersion:
		return "chargeback_case_version"
	case plan.BaseShortfallMicrocredits != frozen.BaseShortfallMicrocredits:
		return "base_shortfall_microcredits"
	case plan.ComponentDigest != frozen.ComponentDigest:
		return "component_digest"
	case plan.WalletID != frozen.WalletID:
		return "wallet_id"
	case plan.WalletVersion != frozen.WalletVersion:
		return "wallet_version"
	case plan.PackageID != frozen.PackageID:
		return "package_id"
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}

func chargebackFinancialRepairExecutionActivation(ctx context.Context, source chargebackRepairReader, input ChargebackFinancialRepairExecutionInput, now time.Time) error {
	active := ApprovalCommandActivation{CommandType: chargebackFinancialRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE"}
	var enabled bool
	err := source.QueryRow(ctx, `SELECT active,activation_version,updated_at FROM billing_approval_execution.lock_chargeback_activation()`).
		Scan(&enabled, &active.Version, &active.UpdatedAt)
	if enabled {
		active.State = "ACTIVE"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalSnapshotMismatch
	}
	if err != nil {
		return err
	}
	if !validActivationItem(active, chargebackFinancialRepairCommand, billingApprovalActivationSpecs[chargebackFinancialRepairCommand], now) ||
		active.State != "ACTIVE" || active.PolicyVersion != input.PolicyVersion || int64(active.SnapshotSchemaVersion) != input.SchemaVersion || active.Version != input.ActivationVersion {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}
