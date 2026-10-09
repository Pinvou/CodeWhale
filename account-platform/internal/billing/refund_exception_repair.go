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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// refundExceptionRepairCommand is the closed command name shared by the proof
// authority, the activation projection and the consumer mapping.
const refundExceptionRepairCommand = "REFUND_EXCEPTION_REPAIR"

// refundExceptionRepairReason is the only reason code the first release admits.
const refundExceptionRepairReason = "PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"

// refundExceptionProviderFailureStatuses is the closed set of provider statuses
// that mean the provider refund did not succeed. An empty status and every
// status in this set fail closed; any other persisted status is accepted exactly
// as design §5 requires ("provider_status 非失败").
var refundExceptionProviderFailureStatuses = map[string]bool{
	"FAILED": true, "FAIL": true, "REJECTED": true, "CLOSED": true, "PAY_CLOSED": true,
	"CANCELLED": true, "CANCELED": true, "REFUND_FAILED": true, "REFUND_CLOSED": true,
}

type RefundExceptionRepairResult struct {
	CaseID                uuid.UUID  `json:"case_id"`
	RefundID              uuid.UUID  `json:"refund_id"`
	WalletID              uuid.UUID  `json:"wallet_id"`
	PackageID             uuid.UUID  `json:"package_id"`
	RevokedMicrocredits   int64      `json:"revoked_microcredits"`
	ShortfallMicrocredits int64      `json:"shortfall_microcredits"`
	JournalEntryID        uuid.UUID  `json:"journal_entry_id"`
	ReceivableID          *uuid.UUID `json:"receivable_id,omitempty"`
	ProviderRefundID      string     `json:"provider_refund_id"`
	FactsDigest           string     `json:"facts_digest"`
	Status                string     `json:"status"`
}

// RefundExceptionRepairCommandInput is the closed command the approval froze.
// The caller only names the refund, the persisted provider evidence and the
// anomaly case; every amount and the revoked/shortfall split are owner-derived.
type RefundExceptionRepairCommandInput struct {
	RefundID          uuid.UUID
	PaymentEvidenceID uuid.UUID
	AnomalyCaseID     uuid.UUID
}

// refundExceptionReclaim is one reserved component whose frozen amount can still
// be reclaimed in full exactly like the existing success path.
type refundExceptionReclaim struct {
	ComponentID     uuid.UUID
	CreditLotID     uuid.UUID
	ComponentType   string
	Available       int64
	Active          int64
	Suspended       int64
	Reclaimable     bool
	ReclaimableSize int64
}

// RefundExceptionRepairPlan is the closed, recomputable owner projection of one
// approved refund exception repair.
type RefundExceptionRepairPlan struct {
	RefundID              uuid.UUID
	PaymentOrderID        uuid.UUID
	PaymentTransactionID  uuid.UUID
	PackageID             uuid.UUID
	WalletID              uuid.UUID
	EvidenceID            uuid.UUID
	AnomalyCaseID         uuid.UUID
	AnomalyVersion        int64
	AmountFen             int64
	CreditMicrocredits    int64
	RevokedMicrocredits   int64
	ShortfallMicrocredits int64
	WalletPosted          int64
	WalletVersion         int64
	PackageStatus         string
	ProviderRefundID      string
	RefundUpdatedAt       time.Time
	Reclaims              []refundExceptionReclaim
	ExpectedGrants        int64
	FactsDigest           string
}

// RefundExceptionRepairFrozenFacts is the wire-visible owner projection an
// approval froze. FactsDigest is an owner-only recomputation input; a consumed
// authorization leaves it empty and the execution transaction recomputes it,
// while a presented digest must match exactly.
type RefundExceptionRepairFrozenFacts struct {
	RefundUpdatedAt       time.Time
	AnomalyVersion        int64
	AmountFen             int64
	CreditMicrocredits    int64
	ProviderRefundID      string
	RevokedMicrocredits   int64
	ShortfallMicrocredits int64
	WalletID              uuid.UUID
	WalletVersion         int64
	PackageID             uuid.UUID
	FactsDigest           string
}

type RefundExceptionRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              RefundExceptionRepairCommandInput
	Frozen               RefundExceptionRepairFrozenFacts
}

type refundExceptionRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func refundExceptionProviderStatusAccepted(status string) bool {
	trimmed := strings.ToUpper(strings.TrimSpace(status))
	return trimmed != "" && !refundExceptionProviderFailureStatuses[trimmed]
}

// refundExceptionConflict keeps the stable ErrConflict identity while naming the
// owner CAS that failed, which is what the operator reads in the audit trail.
func refundExceptionConflict(stage string, cause error) error {
	if cause == nil {
		return errors.Join(ErrConflict, errors.New(stage))
	}
	return errors.Join(ErrConflict, fmt.Errorf("%s: %w", stage, cause))
}

// refundExceptionRepairPlanFrom derives the only permitted repair from
// already-open owner reads. Every rejected contract returns
// ErrApprovalSnapshotMismatch (or ErrNotFound) so the caller classifies it as a
// definite, zero-write rejection.
func refundExceptionRepairPlanFrom(ctx context.Context, source refundExceptionRepairReader, input RefundExceptionRepairCommandInput,
	policyVersion, schemaVersion, activationVersion int64) (RefundExceptionRepairPlan, error) {
	if input.RefundID == uuid.Nil || input.PaymentEvidenceID == uuid.Nil || input.AnomalyCaseID == uuid.Nil ||
		policyVersion < 1 || schemaVersion < 1 || activationVersion < 1 {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := RefundExceptionRepairPlan{RefundID: input.RefundID, EvidenceID: input.PaymentEvidenceID, AnomalyCaseID: input.AnomalyCaseID}
	var refundStatus, merchantRefundNo, currency string
	var channelRefundID *string
	var intentVersion int64
	if err := source.QueryRow(ctx, `SELECT payment_order_id,payment_transaction_id,entitlement_package_id,merchant_refund_no,
channel_refund_id,amount_fen,credit_microcredits,status,intent_version,updated_at
FROM refunds WHERE id=$1`, input.RefundID).
		Scan(&plan.PaymentOrderID, &plan.PaymentTransactionID, &plan.PackageID, &merchantRefundNo,
			&channelRefundID, &plan.AmountFen, &plan.CreditMicrocredits, &refundStatus, &intentVersion, &plan.RefundUpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrNotFound
		}
		return RefundExceptionRepairPlan{}, err
	}
	// Refunds carry no currency of their own; the frozen provider evidence must
	// match the order's frozen currency.
	if err := source.QueryRow(ctx, `SELECT currency FROM payment_orders WHERE id=$1`, plan.PaymentOrderID).Scan(&currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return RefundExceptionRepairPlan{}, err
	}
	if refundStatus != "MANUAL_REVIEW" || plan.PackageID == uuid.Nil || plan.PaymentOrderID == uuid.Nil ||
		plan.PaymentTransactionID == uuid.Nil || plan.AmountFen <= 0 || plan.CreditMicrocredits <= 0 ||
		channelRefundID == nil || strings.TrimSpace(*channelRefundID) == "" || currency == "" || strings.TrimSpace(merchantRefundNo) == "" {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.ProviderRefundID = strings.TrimSpace(*channelRefundID)
	// The frozen evidence must be the persisted provider refund success fact.
	var evidenceOrderID, evidenceTransactionID uuid.UUID
	var evidenceRefundID, evidenceStatus, payloadDigest string
	var evidenceAmountFen *int64
	var evidenceCurrency *string
	if err := source.QueryRow(ctx, `SELECT payment_order_id,payment_transaction_id,provider_refund_id,amount_fen,currency,
provider_status,payload_digest FROM payment_evidence WHERE id=$1`, input.PaymentEvidenceID).
		Scan(&evidenceOrderID, &evidenceTransactionID, &evidenceRefundID, &evidenceAmountFen, &evidenceCurrency,
			&evidenceStatus, &payloadDigest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrNotFound
		}
		return RefundExceptionRepairPlan{}, err
	}
	if evidenceOrderID != plan.PaymentOrderID || evidenceTransactionID != plan.PaymentTransactionID ||
		evidenceRefundID != plan.ProviderRefundID || evidenceAmountFen == nil || *evidenceAmountFen != plan.AmountFen ||
		evidenceCurrency == nil || *evidenceCurrency != currency || !refundExceptionProviderStatusAccepted(evidenceStatus) ||
		!validHexDigest(payloadDigest) {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// The anomaly must be the reviewed escalation of this exact refund.
	var anomalyType, anomalyStatus, resolutionCode, financialAction, dedupeKey string
	if err := source.QueryRow(ctx, `SELECT anomaly_type,status,COALESCE(resolution_code,''),financial_action_status,version,dedupe_key
FROM payment_anomaly_cases WHERE id=$1`, input.AnomalyCaseID).
		Scan(&anomalyType, &anomalyStatus, &resolutionCode, &financialAction, &plan.AnomalyVersion, &dedupeKey); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrNotFound
		}
		return RefundExceptionRepairPlan{}, err
	}
	if anomalyType != "REFUND_FAILURE" || anomalyStatus != "RESOLVED" || resolutionCode != "ESCALATE_FINANCIAL_REPAIR" ||
		financialAction != "PENDING" || plan.AnomalyVersion < 1 ||
		dedupeKey != "refund:"+plan.RefundID.String()+":local-conflict" {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if err := source.QueryRow(ctx, `SELECT wallet_id,status FROM entitlement_packages WHERE id=$1`, plan.PackageID).
		Scan(&plan.WalletID, &plan.PackageStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return RefundExceptionRepairPlan{}, err
	}
	var walletStatus string
	if err := source.QueryRow(ctx, `SELECT status,posted_microcredits,version FROM wallets WHERE id=$1`, plan.WalletID).
		Scan(&walletStatus, &plan.WalletPosted, &plan.WalletVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return RefundExceptionRepairPlan{}, err
	}
	if plan.PackageStatus != "FROZEN" || walletStatus != "ACTIVE" || plan.WalletVersion < 1 || plan.WalletPosted < 0 {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Zero facts: no previous repair journal, receivable or case for this refund.
	var journalExists, receivableExists, caseExists, conflictCase bool
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM journal_entries WHERE business_reference=$2),
 EXISTS(SELECT 1 FROM receivables WHERE source_type='FORCED_REFUND' AND source_reference=$2),
 EXISTS(SELECT 1 FROM refund_exception_repair_cases WHERE refund_id=$1),
 EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$3) OR
 EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$3)`,
		plan.RefundID, "refund-repair:"+plan.RefundID.String(), plan.PackageID).Scan(&journalExists, &receivableExists, &caseExists, &conflictCase); err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	if journalExists || receivableExists || caseExists || conflictCase {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// Recompute which reserved components can still be reclaimed in full, using
	// exactly the frozen-value CAS the existing success path applies.
	rows, err := source.Query(ctx, `SELECT reservation.component_id,reservation.credit_lot_id,reservation.component_type,
reservation.available_microcredits,reservation.active_grant_microcredits,reservation.suspended_grant_microcredits,
component.status,component.available_microcredits,component.active_grant_microcredits,component.suspended_grant_microcredits,
component.used_microcredits,component.reserved_microcredits,component.expired_microcredits,
lot.remaining_microcredits
FROM refund_component_reservations reservation
JOIN entitlement_components component ON component.id=reservation.component_id
JOIN credit_lots lot ON lot.id=reservation.credit_lot_id
WHERE reservation.refund_id=$1 ORDER BY reservation.component_id`, plan.RefundID)
	if err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	reclaims := []refundExceptionReclaim{}
	var reservationTotal int64
	for rows.Next() {
		var item refundExceptionReclaim
		var componentStatus string
		var currentAvailable, currentActive, currentSuspended, used, reserved, expired, lotRemaining int64
		if err := rows.Scan(&item.ComponentID, &item.CreditLotID, &item.ComponentType,
			&item.Available, &item.Active, &item.Suspended,
			&componentStatus, &currentAvailable, &currentActive, &currentSuspended, &used, &reserved, &expired, &lotRemaining); err != nil {
			rows.Close()
			return RefundExceptionRepairPlan{}, err
		}
		item.ReclaimableSize = item.Available + item.Active + item.Suspended
		reservationTotal += item.ReclaimableSize
		item.Reclaimable = componentStatus == "FROZEN" && used == 0 && reserved == 0 && expired == 0 &&
			currentAvailable == item.Available && currentActive == item.Active && currentSuspended == item.Suspended &&
			lotRemaining >= item.ReclaimableSize
		reclaims = append(reclaims, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RefundExceptionRepairPlan{}, err
	}
	rows.Close()
	if len(reclaims) == 0 || reservationTotal != plan.CreditMicrocredits {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var expectedGrants int64
	if err := source.QueryRow(ctx, `SELECT count(*) FROM refund_grant_reservations WHERE refund_id=$1`, plan.RefundID).Scan(&expectedGrants); err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	plan.ExpectedGrants = expectedGrants
	for _, item := range reclaims {
		if item.Reclaimable {
			plan.RevokedMicrocredits += item.ReclaimableSize
		}
	}
	plan.ShortfallMicrocredits = plan.CreditMicrocredits - plan.RevokedMicrocredits
	if plan.RevokedMicrocredits < 0 || plan.ShortfallMicrocredits < 0 || plan.RevokedMicrocredits > plan.WalletPosted ||
		plan.RevokedMicrocredits+plan.ShortfallMicrocredits != plan.CreditMicrocredits {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	sort.Slice(reclaims, func(i, j int) bool { return reclaims[i].ComponentID.String() < reclaims[j].ComponentID.String() })
	type reclaimFacts struct {
		ComponentID     string `json:"component_id"`
		CreditLotID     string `json:"credit_lot_id"`
		ComponentType   string `json:"component_type"`
		Available       int64  `json:"available_microcredits"`
		Active          int64  `json:"active_grant_microcredits"`
		Suspended       int64  `json:"suspended_grant_microcredits"`
		Reclaimable     bool   `json:"reclaimable"`
		ReclaimableSize int64  `json:"reclaimable_microcredits"`
	}
	facts := make([]reclaimFacts, 0, len(reclaims))
	for _, item := range reclaims {
		facts = append(facts, reclaimFacts{ComponentID: item.ComponentID.String(), CreditLotID: item.CreditLotID.String(),
			ComponentType: item.ComponentType, Available: item.Available, Active: item.Active, Suspended: item.Suspended,
			Reclaimable: item.Reclaimable, ReclaimableSize: item.ReclaimableSize})
	}
	encoded, err := json.Marshal(map[string]any{
		"command_type": refundExceptionRepairCommand,
		"refund_id":    plan.RefundID.String(), "payment_order_id": plan.PaymentOrderID.String(),
		"payment_transaction_id": plan.PaymentTransactionID.String(), "package_id": plan.PackageID.String(),
		"wallet_id": plan.WalletID.String(), "wallet_posted_microcredits": plan.WalletPosted, "wallet_version": plan.WalletVersion,
		"amount_fen": plan.AmountFen, "credit_microcredits": plan.CreditMicrocredits,
		"revoked_microcredits": plan.RevokedMicrocredits, "shortfall_microcredits": plan.ShortfallMicrocredits,
		"provider_refund_id": plan.ProviderRefundID, "merchant_refund_no": merchantRefundNo,
		"refund_status": refundStatus, "refund_intent_version": intentVersion,
		"refund_updated_at": plan.RefundUpdatedAt.UTC().Format(time.RFC3339Nano),
		"evidence_id":       plan.EvidenceID.String(), "evidence_status": evidenceStatus, "evidence_payload_digest": payloadDigest,
		"anomaly_case_id": plan.AnomalyCaseID.String(), "anomaly_version": plan.AnomalyVersion,
		"package_status": plan.PackageStatus, "expected_grant_reservations": plan.ExpectedGrants,
		"component_reservations": facts,
		"policy_version":         policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	// The reclaim set is part of the plan: the execution loop is driven by it,
	// while the digest above only covers the frozen owner facts.
	plan.Reclaims = reclaims
	return plan, nil
}

// refundExceptionRepairProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof
// recomputation.
func refundExceptionRepairProofProjection(plan RefundExceptionRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"refund_updated_at": plan.RefundUpdatedAt.UTC().Format(time.RFC3339Nano),
		"anomaly_version":   plan.AnomalyVersion, "facts_digest": plan.FactsDigest, "policy_version": 1,
	})
	financial, _ := json.Marshal(map[string]any{
		"amount_fen": plan.AmountFen, "credit_microcredits": plan.CreditMicrocredits,
		"provider_refund_id":   plan.ProviderRefundID,
		"revoked_microcredits": plan.RevokedMicrocredits, "shortfall_microcredits": plan.ShortfallMicrocredits,
		"wallet_id": plan.WalletID.String(), "wallet_version": plan.WalletVersion, "package_id": plan.PackageID.String(),
	})
	return critical, financial
}

// RefundExceptionRepairProofInput derives the closed proof-era projection of one
// provider-refunded / locally-unreversed exception for a request that is still
// being assembled. It is deliberately not activation-gated.
func (r *PostgresRepository) RefundExceptionRepairProofInput(ctx context.Context, target RefundExceptionRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == refundExceptionRepairCommand {
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
	plan, err := refundExceptionRepairPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := refundExceptionRepairProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: refundExceptionRepairCommand, PolicyVersion: activation.PolicyVersion,
		SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType:    "REFUND", TargetResourceID: plan.RefundID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"refund_id": plan.RefundID, "payment_evidence_id": plan.EvidenceID, "anomaly_case_id": plan.AnomalyCaseID,
			"reason_code": refundExceptionRepairReason,
		}),
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

// ApplyRefundExceptionRepair commits exactly one refund exception repair:
// reclaim what the frozen reservations still allow, convert the remainder into a
// FORCED_REFUND receivable, write the repair REFUND journal, advance the refund
// and the escalated anomaly, and record the immutable case row.
func (r *PostgresRepository) ApplyRefundExceptionRepair(ctx context.Context, input RefundExceptionRepairExecutionInput) (RefundExceptionRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return RefundExceptionRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RefundExceptionRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, found, err := refundExceptionRepairCaseMatches(ctx, tx, input, executionKey); err != nil {
		return RefundExceptionRepairResult{}, err
	} else if found {
		return stored, tx.Commit(ctx)
	}
	plan, err := refundExceptionRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return RefundExceptionRepairResult{}, err
	}
	if mismatch := refundExceptionRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return RefundExceptionRepairResult{}, wrapSnapshotMismatch(mismatch)
	}
	now := r.now()
	var lockedStatus string
	var lockedUpdatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT status,updated_at FROM refunds WHERE id=$1 FOR UPDATE`, plan.RefundID).
		Scan(&lockedStatus, &lockedUpdatedAt); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	if lockedStatus != "MANUAL_REVIEW" || !lockedUpdatedAt.UTC().Equal(plan.RefundUpdatedAt.UTC()) {
		return RefundExceptionRepairResult{}, wrapSnapshotMismatch("refund_updated_at")
	}
	caseID, entryID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	reference := "refund-repair:" + plan.RefundID.String()
	if err := insertJournal(ctx, tx, entryID, "REFUND", reference, "refund exception repair", now); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, plan.WalletID)
	if err != nil {
		return RefundExceptionRepairResult{}, err
	}
	for _, item := range plan.Reclaims {
		if !item.Reclaimable {
			continue
		}
		amount := item.ReclaimableSize
		if err := insertPosting(ctx, tx, entryID, walletAccount, -amount, &item.CreditLotID, now); err != nil {
			return RefundExceptionRepairResult{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, item.CreditLotID, amount)
		if err != nil || tag.RowsAffected() != 1 {
			return RefundExceptionRepairResult{}, refundExceptionConflict("credit_lot_cas", err)
		}
		tag, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=0,active_grant_microcredits=0,
suspended_grant_microcredits=0,revoked_microcredits=revoked_microcredits+$2,status='REVOKED',updated_at=$3
WHERE id=$1 AND status='FROZEN' AND available_microcredits=$4 AND active_grant_microcredits=$5 AND suspended_grant_microcredits=$6
AND used_microcredits=0 AND reserved_microcredits=0 AND expired_microcredits=0`,
			item.ComponentID, amount, now, item.Available, item.Active, item.Suspended)
		if err != nil || tag.RowsAffected() != 1 {
			return RefundExceptionRepairResult{}, refundExceptionConflict("component_cas", err)
		}
	}
	if plan.ExpectedGrants > 0 {
		tag, err := tx.Exec(ctx, `UPDATE member_quota_grants AS quota_grant SET revoked_microcredits=revoked_microcredits+reservation.available_microcredits,
available_microcredits=0,status='REVOKED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$2
FROM refund_grant_reservations reservation
WHERE reservation.refund_id=$1 AND reservation.grant_id=quota_grant.id AND quota_grant.available_microcredits=reservation.available_microcredits
AND quota_grant.used_microcredits=0 AND quota_grant.reserved_microcredits=0 AND quota_grant.expired_microcredits=0 AND quota_grant.revoked_microcredits=0
AND quota_grant.status='SUSPENDED' AND quota_grant.suspension_reason='REFUND_PENDING'`, plan.RefundID, now)
		if err != nil || tag.RowsAffected() != plan.ExpectedGrants {
			return RefundExceptionRepairResult{}, refundExceptionConflict("grant_cas", err)
		}
		// Preserve the ordinary refund grant audit and event contract. Read and
		// close the result set before using the same pgx transaction to write.
		grantRows, err := tx.Query(ctx, `SELECT quota_grant.id,quota_grant.organization_id,quota_grant.member_user_id,
quota_grant.package_id,quota_grant.component_id,quota_grant.component_type,quota_grant.original_microcredits,
quota_grant.available_microcredits,quota_grant.reserved_microcredits,quota_grant.used_microcredits,
quota_grant.expired_microcredits,quota_grant.revoked_microcredits,quota_grant.status,quota_grant.suspension_reason,
quota_grant.membership_version,quota_grant.allocation_version,quota_grant.created_by,quota_grant.expires_at,
quota_grant.created_at,quota_grant.updated_at,reservation.available_microcredits
FROM refund_grant_reservations reservation JOIN member_quota_grants AS quota_grant ON quota_grant.id=reservation.grant_id
WHERE reservation.refund_id=$1 ORDER BY quota_grant.id`, plan.RefundID)
		if err != nil {
			return RefundExceptionRepairResult{}, err
		}
		type revokedGrant struct {
			grant  MemberQuotaGrant
			amount int64
		}
		grants := make([]revokedGrant, 0)
		for grantRows.Next() {
			var item revokedGrant
			if err := scanMemberQuotaGrantWithAmount(grantRows, &item.grant, &item.amount); err != nil {
				grantRows.Close()
				return RefundExceptionRepairResult{}, err
			}
			grants = append(grants, item)
		}
		if err := grantRows.Err(); err != nil {
			grantRows.Close()
			return RefundExceptionRepairResult{}, err
		}
		grantRows.Close()
		for _, item := range grants {
			if err := appendMemberQuotaAudit(ctx, tx, item.grant, "REVOKED", nil, item.amount, now); err != nil {
				return RefundExceptionRepairResult{}, err
			}
			if err := appendMemberQuotaOutbox(ctx, tx, item.grant, "billing.member-quota.revoked.v1", now); err != nil {
				return RefundExceptionRepairResult{}, err
			}
		}
	}
	total := plan.RevokedMicrocredits + plan.ShortfallMicrocredits
	if err := insertPosting(ctx, tx, entryID, refundClearingAccount, total, nil, now); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	var receivableID *uuid.UUID
	if plan.ShortfallMicrocredits > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -plan.ShortfallMicrocredits, nil, now); err != nil {
			return RefundExceptionRepairResult{}, err
		}
		receivable, err := createReceivableWithinTransaction(ctx, tx, plan.WalletID, "FORCED_REFUND", reference,
			plan.ShortfallMicrocredits, &plan.PaymentOrderID, &plan.PaymentTransactionID, &plan.PackageID, nil, now)
		if err != nil {
			return RefundExceptionRepairResult{}, err
		}
		receivableID = &receivable.ID
	}
	if plan.RevokedMicrocredits > 0 {
		tag, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3
WHERE id=$1 AND posted_microcredits>=$2`, plan.WalletID, plan.RevokedMicrocredits, now)
		if err != nil || tag.RowsAffected() != 1 {
			return RefundExceptionRepairResult{}, refundExceptionConflict("wallet_cas", err)
		}
	}
	// The component updates above already project the package counters through
	// project_entitlement_package_components(). When the whole refund was
	// reclaimed the package closes with REVOKED; a partial reclaim keeps the
	// package FROZEN (its remaining counters describe what is left) while the
	// shortfall is carried by the FORCED_REFUND receivable.
	if plan.ShortfallMicrocredits == 0 {
		tag, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status='REVOKED',updated_at=$2
WHERE id=$1 AND status='FROZEN' AND points_remaining=0 AND points_used=0 AND points_reserved=0 AND points_expired=0`,
			plan.PackageID, now)
		if err != nil || tag.RowsAffected() != 1 {
			return RefundExceptionRepairResult{}, refundExceptionConflict("package_cas", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status='SUCCEEDED',intent_version=intent_version+1,updated_at=$2 WHERE id=$1`, plan.RefundID, now); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.refund.succeeded.v1", plan.RefundID, 2, map[string]any{
		"refund_id": plan.RefundID, "payment_order_id": plan.PaymentOrderID, "payment_transaction_id": plan.PaymentTransactionID,
		"entitlement_package_id": plan.PackageID, "amount_fen": plan.AmountFen,
	}); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='APPLIED',version=version+1,updated_at=$3
WHERE id=$1 AND status='RESOLVED' AND resolution_code='ESCALATE_FINANCIAL_REPAIR' AND financial_action_status='PENDING' AND version=$2`,
		plan.AnomalyCaseID, plan.AnomalyVersion, now)
	if err != nil || tag.RowsAffected() != 1 {
		return RefundExceptionRepairResult{}, refundExceptionConflict("anomaly_cas", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO refund_exception_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,
execution_key_digest,refund_id,anomaly_case_id,wallet_id,package_id,revoked_microcredits,shortfall_microcredits,payment_evidence_id,
facts_digest,journal_entry_id,receivable_id,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'EXECUTED',$16)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		ledgerRepairExecutionKeyDigest(executionKey), plan.RefundID, plan.AnomalyCaseID, plan.WalletID, plan.PackageID,
		plan.RevokedMicrocredits, plan.ShortfallMicrocredits, plan.EvidenceID, plan.FactsDigest, entryID, receivableID, now); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	stored, err := r.RefundExceptionRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored.CaseID != caseID || stored.JournalEntryID != entryID || stored.Status != "SUCCEEDED" ||
		stored.RevokedMicrocredits != plan.RevokedMicrocredits || stored.ShortfallMicrocredits != plan.ShortfallMicrocredits {
		if err == nil {
			err = ErrConflict
		}
		return RefundExceptionRepairResult{}, err
	}
	return stored, nil
}

// RefundExceptionRepairForRequest reads the committed repair of one approval
// request without creating any new fact (result-first).
func (r *PostgresRepository) RefundExceptionRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (RefundExceptionRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return RefundExceptionRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RefundExceptionRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readRefundExceptionRepair(ctx, tx, approvalRequestID)
	if err != nil {
		return RefundExceptionRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	return result, nil
}

func readRefundExceptionRepair(ctx context.Context, source refundExceptionRepairReader, approvalRequestID uuid.UUID) (RefundExceptionRepairResult, error) {
	var result RefundExceptionRepairResult
	if err := source.QueryRow(ctx, `SELECT id,refund_id,wallet_id,package_id,revoked_microcredits,shortfall_microcredits,
journal_entry_id,receivable_id,facts_digest FROM refund_exception_repair_cases WHERE approval_request_id=$1`, approvalRequestID).
		Scan(&result.CaseID, &result.RefundID, &result.WalletID, &result.PackageID, &result.RevokedMicrocredits,
			&result.ShortfallMicrocredits, &result.JournalEntryID, &result.ReceivableID, &result.FactsDigest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefundExceptionRepairResult{}, ErrNotFound
		}
		return RefundExceptionRepairResult{}, err
	}
	if err := source.QueryRow(ctx, `SELECT COALESCE(channel_refund_id,'') FROM refunds WHERE id=$1`, result.RefundID).Scan(&result.ProviderRefundID); err != nil {
		return RefundExceptionRepairResult{}, err
	}
	result.Status = "SUCCEEDED"
	return result, nil
}

// refundExceptionRepairCaseMatches is the idempotent-hit check: the stored case
// must carry the same approval identity, epoch and execution key.
func refundExceptionRepairCaseMatches(ctx context.Context, source refundExceptionRepairReader, input RefundExceptionRepairExecutionInput, executionKey string) (RefundExceptionRepairResult, bool, error) {
	var storedRefundID, authorizationEventID uuid.UUID
	var approvalEpoch int64
	var executionKeyDigest string
	err := source.QueryRow(ctx, `SELECT refund_id,authorization_event_id,approval_epoch,execution_key_digest
FROM refund_exception_repair_cases WHERE approval_request_id=$1`, input.ApprovalRequestID).
		Scan(&storedRefundID, &authorizationEventID, &approvalEpoch, &executionKeyDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundExceptionRepairResult{}, false, nil
	}
	if err != nil {
		return RefundExceptionRepairResult{}, false, err
	}
	if authorizationEventID != input.AuthorizationEventID || approvalEpoch != input.ApprovalEpoch ||
		executionKeyDigest != ledgerRepairExecutionKeyDigest(executionKey) || storedRefundID != input.Command.RefundID {
		return RefundExceptionRepairResult{}, false, nil
	}
	stored, err := readRefundExceptionRepair(ctx, source, input.ApprovalRequestID)
	if err != nil {
		return RefundExceptionRepairResult{}, false, err
	}
	return stored, true, nil
}

func refundExceptionRepairFrozenMismatch(plan RefundExceptionRepairPlan, frozen RefundExceptionRepairFrozenFacts) string {
	switch {
	case !plan.RefundUpdatedAt.UTC().Equal(frozen.RefundUpdatedAt.UTC()):
		return "refund_updated_at"
	case plan.AnomalyVersion != frozen.AnomalyVersion:
		return "anomaly_version"
	case plan.AmountFen != frozen.AmountFen:
		return "amount_fen"
	case plan.CreditMicrocredits != frozen.CreditMicrocredits:
		return "credit_microcredits"
	case plan.ProviderRefundID != frozen.ProviderRefundID:
		return "provider_refund_id"
	case plan.RevokedMicrocredits != frozen.RevokedMicrocredits:
		return "revoked_microcredits"
	case plan.ShortfallMicrocredits != frozen.ShortfallMicrocredits:
		return "shortfall_microcredits"
	case plan.WalletID != frozen.WalletID:
		return "wallet_id"
	case plan.WalletVersion != frozen.WalletVersion:
		return "wallet_version"
	case plan.PackageID != frozen.PackageID:
		return "package_id"
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
