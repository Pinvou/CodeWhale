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

// usageSettlementRepairCommand is the closed command name shared by the proof
// authority, the activation projection and the consumer mapping.
const usageSettlementRepairCommand = "USAGE_SETTLEMENT_REPAIR"

// usageSettlementRepairReason is the only reason code the first release admits.
const usageSettlementRepairReason = "MISSING_USAGE_SETTLEMENT"

// usageSettlementRepairEvidenceKind is the only evidence kind the first release
// implements: a usage loss review that was never settled against the wallet.
// The COMMITTED-shortfall case needs a persisted corrected-usage fact that
// Billing does not hold yet, so it stays closed until a later policy/schema
// version (recorded in docs/admin-governance-security-tasks.md).
const usageSettlementRepairEvidenceKind = "LOSS_REVIEW"

// usageSettlementRepairReferencePrefix is the closed business reference of the
// repair journal and of the overage receivable of one repaired usage request.
const usageSettlementRepairReferencePrefix = "usage-settlement-repair:"

// usageSettlementRepairLockScope serializes the repair with itself per hold.
const usageSettlementRepairLockScope = "usage.settlement.repair"

type UsageSettlementRepairResult struct {
	CaseID               uuid.UUID  `json:"case_id"`
	UsageRequestID       uuid.UUID  `json:"usage_request_id"`
	HoldID               uuid.UUID  `json:"hold_id"`
	WalletID             uuid.UUID  `json:"wallet_id"`
	LossReviewID         *uuid.UUID `json:"loss_review_id,omitempty"`
	EvidenceKind         string     `json:"evidence_kind"`
	ExpectedMicrocredits int64      `json:"expected_microcredits"`
	SettledMicrocredits  int64      `json:"settled_microcredits"`
	OverageMicrocredits  int64      `json:"overage_microcredits"`
	AllocationsDigest    string     `json:"allocations_digest"`
	FactsDigest          string     `json:"facts_digest"`
	JournalEntryID       uuid.UUID  `json:"journal_entry_id"`
	ReceivableID         *uuid.UUID `json:"receivable_id,omitempty"`
	Status               string     `json:"status"`
}

// UsageSettlementRepairCommandInput is the closed command the approval froze.
// The caller names the usage request, its hold and the persisted evidence; every
// amount is owner-derived.
type UsageSettlementRepairCommandInput struct {
	UsageRequestID uuid.UUID
	HoldID         uuid.UUID
	EvidenceKind   string
	LossReviewID   uuid.UUID
}

// usageRepairAllocation is one frozen hold allocation with the capacity the
// closed plan measured for it.
type usageRepairAllocation struct {
	PackageID   uuid.UUID
	ComponentID uuid.UUID
	LotID       uuid.UUID
	Reserved    int64
	Settleable  int64
}

// UsageSettlementRepairPlan is the closed, recomputable owner projection of one
// approved usage settlement repair.
type UsageSettlementRepairPlan struct {
	PolicyVersion        int64
	UsageRequestID       uuid.UUID
	HoldID               uuid.UUID
	LossReviewID         uuid.UUID
	WalletID             uuid.UUID
	WalletVersion        int64
	WalletPosted         int64
	ActualBefore         int64
	HoldUpdatedAt        time.Time
	LossReviewUpdatedAt  time.Time
	EvidenceKind         string
	ExpectedMicrocredits int64
	SettledMicrocredits  int64
	OverageMicrocredits  int64
	AllocationsDigest    string
	FactsDigest          string
	Allocations          []usageRepairAllocation
}

// UsageSettlementRepairFrozenFacts is the wire-visible owner projection an
// approval froze. Fresh execution requires the exact nonempty digest; committed
// matching identity replays before these fresh checks.
type UsageSettlementRepairFrozenFacts struct {
	HoldUpdatedAt        time.Time
	LossReviewUpdatedAt  time.Time
	ExpectedMicrocredits int64
	SettledMicrocredits  int64
	WalletID             uuid.UUID
	WalletVersion        int64
	AllocationsDigest    string
	FactsDigest          string
}

type UsageSettlementRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              UsageSettlementRepairCommandInput
	Frozen               UsageSettlementRepairFrozenFacts
}

// usageRepairReader is the read surface the closed plan needs.
type usageRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func usageSettlementRepairConflict(stage string, cause error) error {
	if cause == nil {
		return errors.Join(ErrConflict, errors.New(stage))
	}
	return errors.Join(ErrConflict, fmt.Errorf("%s: %w", stage, cause))
}

// usageSettlementRepairPlanFrom derives the only permitted repair from
// already-open owner reads. Every rejected contract returns
// ErrApprovalSnapshotMismatch (or ErrNotFound) so the caller classifies it as a
// definite, zero-write rejection.
func usageSettlementRepairPlanFrom(ctx context.Context, source usageRepairReader, input UsageSettlementRepairCommandInput,
	policyVersion, schemaVersion, activationVersion int64) (UsageSettlementRepairPlan, error) {
	if input.UsageRequestID == uuid.Nil || input.HoldID == uuid.Nil || input.LossReviewID == uuid.Nil ||
		strings.TrimSpace(input.EvidenceKind) != usageSettlementRepairEvidenceKind ||
		policyVersion != 1 || schemaVersion != 1 || activationVersion < 1 {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := UsageSettlementRepairPlan{UsageRequestID: input.UsageRequestID, HoldID: input.HoldID,
		LossReviewID: input.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind, PolicyVersion: policyVersion}
	var holdStatus, releaseReason string
	var maxMicrocredits int64
	var actual *int64
	if err := source.QueryRow(ctx, `SELECT wallet_id,status,max_microcredits,actual_microcredits,
COALESCE(usage_snapshot->>'release_reason',''),updated_at FROM usage_holds WHERE id=$1 AND usage_request_id=$2`,
		input.HoldID, input.UsageRequestID).
		Scan(&plan.WalletID, &holdStatus, &maxMicrocredits, &actual, &releaseReason, &plan.HoldUpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UsageSettlementRepairPlan{}, ErrNotFound
		}
		return UsageSettlementRepairPlan{}, err
	}
	// The first release only repairs a hold that was released because the
	// entitlement could not be settled, and whose loss review is still open.
	if holdStatus != "RELEASED" || releaseReason != "ENTITLEMENT_EXPIRED_UNCERTAIN" || maxMicrocredits < 0 {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if actual != nil {
		plan.ActualBefore = *actual
	}
	var reviewHoldID, reviewUsageRequestID uuid.UUID
	var reviewReason, reviewStatus string
	var claimed *int64
	if err := source.QueryRow(ctx, `SELECT hold_id,usage_request_id,reason_code,status,claimed_actual_microcredits,updated_at
FROM usage_loss_reviews WHERE id=$1`, input.LossReviewID).
		Scan(&reviewHoldID, &reviewUsageRequestID, &reviewReason, &reviewStatus, &claimed, &plan.LossReviewUpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UsageSettlementRepairPlan{}, ErrNotFound
		}
		return UsageSettlementRepairPlan{}, err
	}
	if reviewHoldID != plan.HoldID || reviewUsageRequestID != plan.UsageRequestID ||
		reviewReason != "ENTITLEMENT_EXPIRED_UNCERTAIN" || reviewStatus != "REVIEW_REQUIRED" ||
		claimed == nil || *claimed <= 0 {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}

	var priceID *uuid.UUID
	var model, protocol string
	var formulaID *uuid.UUID
	var pricingJSON, usageJSON []byte
	if err := source.QueryRow(ctx, `SELECT price_version_id,COALESCE(model_id,''),COALESCE(protocol_code,''),formula_version_id,pricing_snapshot FROM usage_holds WHERE id=$1`, plan.HoldID).Scan(&priceID, &model, &protocol, &formulaID, &pricingJSON); err != nil {
		return UsageSettlementRepairPlan{}, err
	}
	if err := source.QueryRow(ctx, `SELECT usage_snapshot FROM usage_loss_reviews WHERE id=$1`, plan.LossReviewID).Scan(&usageJSON); err != nil {
		return UsageSettlementRepairPlan{}, err
	}
	recomputed, err := usageRepairFrozenPricing(priceID, model, protocol, formulaID, pricingJSON, usageJSON)
	if err != nil || recomputed != *claimed {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.ExpectedMicrocredits = recomputed
	var walletStatus string
	if err := source.QueryRow(ctx, `SELECT status,posted_microcredits,version FROM wallets WHERE id=$1`, plan.WalletID).
		Scan(&walletStatus, &plan.WalletPosted, &plan.WalletVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		return UsageSettlementRepairPlan{}, err
	}
	if walletStatus != "ACTIVE" || plan.WalletVersion < 1 || plan.WalletPosted < 0 {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// No previous repair may exist for this usage request, and any receivable the
	// original settlement already created must not be re-created.
	var repairExists, repairJournal, repairReceivable bool
	reference := usageSettlementRepairReferencePrefix + plan.UsageRequestID.String()
	if err := source.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM usage_settlement_repair_cases WHERE usage_request_id=$1),
 EXISTS(SELECT 1 FROM journal_entries WHERE business_reference=$2),
 EXISTS(SELECT 1 FROM receivables WHERE source_type='USAGE_OVERAGE' AND source_reference=$2)`,
		plan.UsageRequestID, reference).Scan(&repairExists, &repairJournal, &repairReceivable); err != nil {
		return UsageSettlementRepairPlan{}, err
	}
	if repairExists || repairJournal || repairReceivable {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	// The settleable capacity of each frozen allocation is what the release path
	// gave back to the component: the component's current available balance,
	// capped by the allocation's original reservation.
	rows, err := source.Query(ctx, `SELECT allocation.package_id,allocation.component_id,component.credit_lot_id,
allocation.reserved_microcredits,component.available_microcredits
FROM usage_hold_allocations allocation
JOIN entitlement_components component ON component.id=allocation.component_id
WHERE allocation.hold_id=$1
ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id`,
		plan.HoldID)
	if err != nil {
		return UsageSettlementRepairPlan{}, err
	}
	allocations := []usageRepairAllocation{}
	remaining := plan.ExpectedMicrocredits
	for rows.Next() {
		item := usageRepairAllocation{}
		var available int64
		if err := rows.Scan(&item.PackageID, &item.ComponentID, &item.LotID, &item.Reserved, &available); err != nil {
			rows.Close()
			return UsageSettlementRepairPlan{}, err
		}
		if item.Reserved <= 0 || available < 0 || item.LotID == uuid.Nil {
			rows.Close()
			return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		item.Settleable = min(remaining, min(item.Reserved, available))
		if item.Settleable < 0 {
			rows.Close()
			return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
		}
		remaining -= item.Settleable
		allocations = append(allocations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return UsageSettlementRepairPlan{}, err
	}
	rows.Close()
	if len(allocations) == 0 {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.SettledMicrocredits = plan.ExpectedMicrocredits - remaining
	plan.OverageMicrocredits = remaining
	if plan.SettledMicrocredits < 0 || plan.OverageMicrocredits < 0 ||
		plan.SettledMicrocredits+plan.OverageMicrocredits != plan.ExpectedMicrocredits {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if plan.SettledMicrocredits > plan.WalletPosted {
		// Insufficient wallet backing is a definite zero-effect refusal; a plan
		// cannot shrink totals while leaving its per-lot debits unchanged.
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	sort.Slice(allocations, func(i, j int) bool { return allocations[i].ComponentID.String() < allocations[j].ComponentID.String() })
	type allocationFacts struct {
		ComponentID string `json:"component_id"`
		LotID       string `json:"credit_lot_id"`
		Reserved    int64  `json:"reserved_microcredits"`
		Settleable  int64  `json:"settleable_microcredits"`
	}
	facts := make([]allocationFacts, 0, len(allocations))
	for _, item := range allocations {
		facts = append(facts, allocationFacts{ComponentID: item.ComponentID.String(), LotID: item.LotID.String(),
			Reserved: item.Reserved, Settleable: item.Settleable})
	}
	digest, err := json.Marshal(map[string]any{
		"hold_id": plan.HoldID.String(), "usage_request_id": plan.UsageRequestID.String(),
		"wallet_id": plan.WalletID.String(), "wallet_version": plan.WalletVersion,
		"expected_microcredits": plan.ExpectedMicrocredits, "settled_microcredits": plan.SettledMicrocredits,
		"overage_microcredits": plan.OverageMicrocredits, "allocations": facts,
	})
	if err != nil {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.AllocationsDigest = digestHex(digest)
	plan.Allocations = allocations
	var ownerRaw []byte
	if err := source.QueryRow(ctx, `SELECT jsonb_build_object('hold',to_jsonb(h),'loss_review',to_jsonb(l),'wallet',to_jsonb(w),'allocations',(SELECT jsonb_agg(jsonb_build_object('allocation',to_jsonb(a),'component',to_jsonb(c),'package',to_jsonb(p),'lot',to_jsonb(cl)) ORDER BY a.component_id) FROM usage_hold_allocations a JOIN entitlement_components c ON c.id=a.component_id JOIN entitlement_packages p ON p.id=a.package_id JOIN credit_lots cl ON cl.id=c.credit_lot_id WHERE a.hold_id=h.id)) FROM usage_holds h JOIN usage_loss_reviews l ON l.id=$2 JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, plan.HoldID, plan.LossReviewID).Scan(&ownerRaw); err != nil {
		return UsageSettlementRepairPlan{}, err
	}
	ownerSnapshot, err := paymentRepairJSONSnapshot(ownerRaw)
	if err != nil {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	encoded, err := json.Marshal(map[string]any{
		"owner_snapshot": ownerSnapshot, "command_type": usageSettlementRepairCommand, "usage_request_id": plan.UsageRequestID.String(),
		"hold_id": plan.HoldID.String(), "hold_status": holdStatus, "release_reason": releaseReason,
		"hold_updated_at": plan.HoldUpdatedAt.UTC().Format(time.RFC3339Nano), "max_microcredits": maxMicrocredits,
		"actual_microcredits": actual, "loss_review_id": plan.LossReviewID.String(),
		"loss_review_status": reviewStatus, "loss_review_updated_at": plan.LossReviewUpdatedAt.UTC().Format(time.RFC3339Nano),
		"claimed_actual_microcredits": plan.ExpectedMicrocredits, "wallet_id": plan.WalletID.String(),
		"wallet_posted_microcredits": plan.WalletPosted, "wallet_version": plan.WalletVersion,
		"expected_microcredits": plan.ExpectedMicrocredits, "settled_microcredits": plan.SettledMicrocredits,
		"overage_microcredits": plan.OverageMicrocredits, "allocations_digest": plan.AllocationsDigest,
		"policy_version": policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return UsageSettlementRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	return plan, nil
}

// usageSettlementRepairProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof recomputation.
func usageSettlementRepairProofProjection(plan UsageSettlementRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"hold_updated_at":        plan.HoldUpdatedAt.UTC().Format(time.RFC3339Nano),
		"loss_review_updated_at": plan.LossReviewUpdatedAt.UTC().Format(time.RFC3339Nano),
		"facts_digest":           plan.FactsDigest, "policy_version": plan.PolicyVersion,
	})
	financial, _ := json.Marshal(map[string]any{
		"expected_microcredits": plan.ExpectedMicrocredits, "settled_microcredits": plan.SettledMicrocredits,
		"settlement_microcredits": plan.SettledMicrocredits, "overage_microcredits": plan.OverageMicrocredits,
		"wallet_id": plan.WalletID.String(), "wallet_version": plan.WalletVersion,
		"allocations_digest": plan.AllocationsDigest,
	})
	return critical, financial
}

// UsageSettlementRepairProofInput derives the closed proof-era projection of one
// unsettled usage loss for a request that is still being assembled. It is
// deliberately not activation-gated.
func (r *PostgresRepository) UsageSettlementRepairProofInput(ctx context.Context, target UsageSettlementRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == usageSettlementRepairCommand {
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
	plan, err := usageSettlementRepairPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := usageSettlementRepairProofProjection(plan)
	normalized := map[string]any{
		"usage_request_id": plan.UsageRequestID.String(), "hold_id": plan.HoldID.String(),
		"evidence_kind": plan.EvidenceKind, "reason_code": usageSettlementRepairReason,
	}
	if plan.LossReviewID != uuid.Nil {
		normalized["loss_review_id"] = plan.LossReviewID.String()
	}
	return approvalencoding.LegacyProofInput{
		CommandType: usageSettlementRepairCommand, PolicyVersion: activation.PolicyVersion,
		SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType:    "USAGE_HOLD", TargetResourceID: plan.HoldID.String(),
		NormalizedCommand:        encodeEntitlementCorrectionProjection(normalized),
		CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

// ApplyUsageSettlementRepair commits exactly one补结算 of an unsettled usage loss:
// the USAGE journal with its per-lot wallet legs, the revenue leg, the overage
// receivable, the entitlement deductions, the allocation backfill, the hold
// snapshot and the loss review transition. It never re-sends a model request and
// never touches the original settlement journal.
func (r *PostgresRepository) ApplyUsageSettlementRepair(ctx context.Context, input UsageSettlementRepairExecutionInput) (UsageSettlementRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return UsageSettlementRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, found, err := usageSettlementRepairCaseMatches(ctx, tx, input, executionKey); err != nil {
		return UsageSettlementRepairResult{}, err
	} else if found {
		return stored, tx.Commit(ctx)
	}
	if err := lockTransactionKey(ctx, tx, usageSettlementRepairLockScope, input.Command.HoldID.String()); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	var walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT wallet_id FROM usage_holds WHERE id=$1 AND usage_request_id=$2`, input.Command.HoldID, input.Command.UsageRequestID).Scan(&walletID); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	if _, err := getWallet(ctx, tx, walletID, true); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM usage_holds WHERE id=$1 FOR UPDATE`, input.Command.HoldID).Scan(&lockedID); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM usage_loss_reviews WHERE id=$1 FOR UPDATE`, input.Command.LossReviewID).Scan(&lockedID); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	lockRows, err := tx.Query(ctx, `SELECT a.component_id FROM usage_hold_allocations a JOIN entitlement_components c ON c.id=a.component_id JOIN entitlement_packages p ON p.id=a.package_id JOIN credit_lots l ON l.id=c.credit_lot_id WHERE a.hold_id=$1 ORDER BY a.component_id FOR UPDATE OF a,c,p,l`, input.Command.HoldID)
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	for lockRows.Next() {
		if err := lockRows.Scan(&lockedID); err != nil {
			lockRows.Close()
			return UsageSettlementRepairResult{}, err
		}
	}
	err = lockRows.Err()
	lockRows.Close()
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	now := r.now()
	if err := usageSettlementRepairExecutionActivation(ctx, tx, input, now); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	plan, err := usageSettlementRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	if mismatch := usageSettlementRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return UsageSettlementRepairResult{}, wrapSnapshotMismatch(mismatch)
	}

	caseID, entryID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	reference := usageSettlementRepairReferencePrefix + plan.UsageRequestID.String()
	if err := insertJournal(ctx, tx, entryID, "USAGE", reference, "usage settlement repair", now); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, plan.WalletID)
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	for _, item := range plan.Allocations {
		if item.Settleable == 0 {
			// A zero-delta posting is forbidden by ledger_postings, so the
			// allocation is backfilled without a wallet leg.
			tag, err := tx.Exec(ctx, `UPDATE usage_hold_allocations SET settled_microcredits=$3,released_microcredits=$4,updated_at=$5
WHERE hold_id=$1 AND component_id=$2`, plan.HoldID, item.ComponentID, int64(0), item.Reserved, now)
			if err != nil || tag.RowsAffected() != 1 {
				return UsageSettlementRepairResult{}, usageSettlementRepairConflict("allocation_backfill", err)
			}
			continue
		}
		tag, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2
WHERE id=$1 AND remaining_microcredits>=$2`, item.LotID, item.Settleable)
		if err != nil || tag.RowsAffected() != 1 {
			return UsageSettlementRepairResult{}, usageSettlementRepairConflict("credit_lot_cas", err)
		}
		if err := insertPosting(ctx, tx, entryID, walletAccount, -item.Settleable, &item.LotID, now); err != nil {
			return UsageSettlementRepairResult{}, err
		}
		memberBacked, memberErr := hasMemberQuotaAllocations(ctx, tx, plan.HoldID, item.ComponentID)
		if memberErr != nil {
			return UsageSettlementRepairResult{}, memberErr
		}
		if memberBacked {
			if err := settleMemberQuotaComponent(ctx, tx, plan.HoldID, item.ComponentID, item.Settleable, now); err != nil {
				return UsageSettlementRepairResult{}, err
			}
		} else {
			tag, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,
used_microcredits=used_microcredits+$2,status=CASE WHEN status IN ('FROZEN','REVOKED','EXPIRED') THEN status WHEN available_microcredits-$2=0 AND reserved_microcredits+active_grant_microcredits+suspended_grant_microcredits=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,updated_at=$3
WHERE id=$1 AND available_microcredits>=$2`, item.ComponentID, item.Settleable, now)
			if err != nil || tag.RowsAffected() != 1 {
				return UsageSettlementRepairResult{}, usageSettlementRepairConflict("component_cas", err)
			}
		}
		tag, err = tx.Exec(ctx, `UPDATE usage_hold_allocations SET settled_microcredits=$3,released_microcredits=$4,updated_at=$5
WHERE hold_id=$1 AND component_id=$2`, plan.HoldID, item.ComponentID, item.Settleable, item.Reserved-item.Settleable, now)
		if err != nil || tag.RowsAffected() != 1 {
			return UsageSettlementRepairResult{}, usageSettlementRepairConflict("allocation_backfill", err)
		}
	}
	var receivableID *uuid.UUID
	if plan.OverageMicrocredits > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -plan.OverageMicrocredits, nil, now); err != nil {
			return UsageSettlementRepairResult{}, err
		}
		receivable, err := createReceivableWithinTransaction(ctx, tx, plan.WalletID, "USAGE_OVERAGE", reference,
			plan.OverageMicrocredits, nil, nil, nil, &plan.UsageRequestID, now)
		if err != nil {
			return UsageSettlementRepairResult{}, err
		}
		receivableID = &receivable.ID
	}
	if err := insertPosting(ctx, tx, entryID, usageRevenueAccount, plan.ExpectedMicrocredits, nil, now); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3
WHERE id=$1 AND posted_microcredits>=$2`, plan.WalletID, plan.SettledMicrocredits, now)
	if err != nil || tag.RowsAffected() != 1 {
		return UsageSettlementRepairResult{}, usageSettlementRepairConflict("wallet_cas", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package
SET status=CASE WHEN package.status IN ('FROZEN','REVOKED','EXPIRED') THEN package.status WHEN EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id
        AND (component.available_microcredits>0 OR component.reserved_microcredits>0 OR component.active_grant_microcredits>0 OR component.suspended_grant_microcredits>0)) THEN 'ACTIVE' ELSE 'CONSUMED' END,
    updated_at=$2
WHERE package.id IN (SELECT package_id FROM usage_hold_allocations WHERE hold_id=$1)`, plan.HoldID, now); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	repairSnapshot, err := json.Marshal(map[string]any{
		"approval_request_id": input.ApprovalRequestID.String(), "case_id": caseID.String(),
		"expected_microcredits": plan.ExpectedMicrocredits, "settled_microcredits": plan.SettledMicrocredits,
		"overage_microcredits": plan.OverageMicrocredits, "allocations_digest": plan.AllocationsDigest,
		"actual_before": plan.ActualBefore, "actual_after": max(plan.ActualBefore, plan.ExpectedMicrocredits),
	})
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	tag, err = tx.Exec(ctx, `UPDATE usage_holds SET usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||jsonb_build_object('settlement_repair',$2::jsonb),
actual_microcredits=GREATEST(COALESCE(actual_microcredits,0),$3),terminal_at=$4,updated_at=$4
WHERE id=$1 AND status='RELEASED' AND updated_at=$5`, plan.HoldID, repairSnapshot, plan.ExpectedMicrocredits, now, plan.HoldUpdatedAt)
	if err != nil || tag.RowsAffected() != 1 {
		return UsageSettlementRepairResult{}, usageSettlementRepairConflict("hold_cas", err)
	}
	tag, err = tx.Exec(ctx, `UPDATE usage_loss_reviews SET status='RESOLVED_SETTLED',updated_at=$3
WHERE id=$1 AND status='REVIEW_REQUIRED' AND updated_at=$2`, plan.LossReviewID, plan.LossReviewUpdatedAt, now)
	if err != nil || tag.RowsAffected() != 1 {
		return UsageSettlementRepairResult{}, usageSettlementRepairConflict("loss_review_cas", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_settlement_repair_cases(id,approval_request_id,authorization_event_id,
approval_epoch,execution_key_digest,usage_request_id,hold_id,loss_review_id,wallet_id,evidence_kind,
expected_microcredits,settled_microcredits,overage_microcredits,allocations_digest,facts_digest,journal_entry_id,
receivable_id,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,'EXECUTED',$18)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		legacyFinancialExecutionKeyDigestV1(executionKey), plan.UsageRequestID, plan.HoldID, plan.LossReviewID,
		plan.WalletID, plan.EvidenceKind, plan.ExpectedMicrocredits, plan.SettledMicrocredits,
		plan.OverageMicrocredits, plan.AllocationsDigest, plan.FactsDigest, entryID, receivableID, now); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	stored, err := r.UsageSettlementRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored.CaseID != caseID || stored.JournalEntryID != entryID || stored.Status != "SUCCEEDED" ||
		stored.ExpectedMicrocredits != plan.ExpectedMicrocredits ||
		stored.SettledMicrocredits != plan.SettledMicrocredits {
		if err == nil {
			err = ErrConflict
		}
		return UsageSettlementRepairResult{}, err
	}
	return stored, nil
}

// UsageSettlementRepairForRequest reads the committed repair of one approval
// request without creating any new fact (result-first).
func (r *PostgresRepository) UsageSettlementRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (UsageSettlementRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return UsageSettlementRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readUsageSettlementRepair(ctx, tx, approvalRequestID)
	if err != nil {
		return UsageSettlementRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UsageSettlementRepairResult{}, err
	}
	return result, nil
}

func readUsageSettlementRepair(ctx context.Context, source usageRepairReader, approvalRequestID uuid.UUID) (UsageSettlementRepairResult, error) {
	var result UsageSettlementRepairResult
	if err := source.QueryRow(ctx, `SELECT id,usage_request_id,hold_id,wallet_id,loss_review_id,evidence_kind,
expected_microcredits,settled_microcredits,overage_microcredits,allocations_digest,facts_digest,journal_entry_id,receivable_id
FROM usage_settlement_repair_cases WHERE approval_request_id=$1`, approvalRequestID).
		Scan(&result.CaseID, &result.UsageRequestID, &result.HoldID, &result.WalletID, &result.LossReviewID,
			&result.EvidenceKind, &result.ExpectedMicrocredits, &result.SettledMicrocredits,
			&result.OverageMicrocredits, &result.AllocationsDigest, &result.FactsDigest,
			&result.JournalEntryID, &result.ReceivableID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UsageSettlementRepairResult{}, ErrNotFound
		}
		return UsageSettlementRepairResult{}, err
	}
	result.Status = "SUCCEEDED"
	return result, nil
}

// usageSettlementRepairCaseMatches is the idempotent-hit check: the stored case
// must belong to the same usage request, approval request and execution key.
func usageSettlementRepairCaseMatches(ctx context.Context, source usageRepairReader,
	input UsageSettlementRepairExecutionInput, executionKey string) (UsageSettlementRepairResult, bool, error) {
	var storedID, storedRequestID, storedEventID uuid.UUID
	var storedEpoch int64
	var storedDigest string
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest
FROM usage_settlement_repair_cases WHERE usage_request_id=$1`, input.Command.UsageRequestID).
		Scan(&storedID, &storedRequestID, &storedEventID, &storedEpoch, &storedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageSettlementRepairResult{}, false, nil
	}
	if err != nil {
		return UsageSettlementRepairResult{}, false, err
	}
	if storedRequestID != input.ApprovalRequestID || storedEventID != input.AuthorizationEventID ||
		storedEpoch != input.ApprovalEpoch || storedDigest != legacyFinancialExecutionKeyDigestV1(executionKey) {
		return UsageSettlementRepairResult{}, false, ErrConflict
	}
	stored, err := readUsageSettlementRepair(ctx, source, input.ApprovalRequestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return UsageSettlementRepairResult{}, false, ErrConflict
		}
		return UsageSettlementRepairResult{}, false, err
	}
	return stored, stored.CaseID == storedID, nil
}

// usageSettlementRepairFrozenMismatch compares the frozen wire projection with
// the owner-recomputed plan, including a required nonempty facts digest.
func usageSettlementRepairFrozenMismatch(plan UsageSettlementRepairPlan, frozen UsageSettlementRepairFrozenFacts) string {
	switch {
	case !plan.HoldUpdatedAt.UTC().Equal(frozen.HoldUpdatedAt.UTC()):
		return "hold_updated_at"
	case !plan.LossReviewUpdatedAt.UTC().Equal(frozen.LossReviewUpdatedAt.UTC()):
		return "loss_review_updated_at"
	case plan.ExpectedMicrocredits != frozen.ExpectedMicrocredits:
		return "expected_microcredits"
	case plan.SettledMicrocredits != frozen.SettledMicrocredits:
		return "settled_microcredits"
	case plan.WalletID != frozen.WalletID:
		return "wallet_id"
	case plan.WalletVersion != frozen.WalletVersion:
		return "wallet_version"
	case plan.AllocationsDigest != frozen.AllocationsDigest:
		return "allocations_digest"
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}

func usageSettlementRepairExecutionActivation(ctx context.Context, source usageRepairReader, input UsageSettlementRepairExecutionInput, now time.Time) error {
	active := ApprovalCommandActivation{CommandType: usageSettlementRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE"}
	var enabled bool
	err := source.QueryRow(ctx, `SELECT active,activation_version,updated_at FROM billing_approval_execution.lock_usage_activation()`).Scan(&enabled, &active.Version, &active.UpdatedAt)
	if enabled {
		active.State = "ACTIVE"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalSnapshotMismatch
	}
	if err != nil {
		return err
	}
	if !validActivationItem(active, usageSettlementRepairCommand, billingApprovalActivationSpecs[usageSettlementRepairCommand], now) ||
		active.State != "ACTIVE" || active.PolicyVersion != input.PolicyVersion || int64(active.SnapshotSchemaVersion) != input.SchemaVersion || active.Version != input.ActivationVersion {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}
