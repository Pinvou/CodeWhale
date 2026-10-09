package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// receivableWriteOffCommand is the closed command name shared by the proof
// authority, the activation projection and the consumer mapping.
const receivableWriteOffCommand = "RECEIVABLE_WRITE_OFF"

// receivableWriteOffReason is the only reason code the first release admits;
// free text is audited separately and never enters the numeric facts.
const receivableWriteOffReason = "APPROVED_RECEIVABLE_WRITE_OFF"

type ReceivableWriteOffResult struct {
	CaseID                  uuid.UUID `json:"case_id"`
	ReceivableID            uuid.UUID `json:"receivable_id"`
	WalletID                uuid.UUID `json:"wallet_id"`
	AmountMicrocredits      int64     `json:"amount_microcredits"`
	ReceivableVersionBefore int64     `json:"receivable_version_before"`
	ReceivableVersionAfter  int64     `json:"receivable_version_after"`
	StatusAfter             string    `json:"status_after"`
	FactsDigest             string    `json:"facts_digest"`
	JournalEntryID          uuid.UUID `json:"journal_entry_id"`
	AllocationID            uuid.UUID `json:"allocation_id"`
	Status                  string    `json:"status"`
}

// ReceivableWriteOffCommandInput is the closed command the approval froze. The
// wallet, the platform accounts and every amount other than the requested
// write-off come from the owner read path.
type ReceivableWriteOffCommandInput struct {
	ReceivableID              uuid.UUID
	WriteOffMicrocredits      int64
	ExpectedReceivableVersion int64
}

// ReceivableWriteOffPlan is the closed, recomputable owner projection of one
// approved write-off.
type ReceivableWriteOffPlan struct {
	ReceivableID                 uuid.UUID
	WalletID                     uuid.UUID
	SourceType                   string
	SourceReference              string
	OriginalMicrocredits         int64
	SettledMicrocredits          int64
	OutstandingMicrocredits      int64
	ReceivableStatus             string
	ReceivableVersion            int64
	WalletReceivableMicrocredits int64
	WriteOffMicrocredits         int64
	FactsDigest                  string
}

// ReceivableWriteOffFrozenFacts is the wire-visible owner projection an
// approval froze. Fresh execution recomputes FactsDigest inside the owner
// transaction and requires it to match the frozen approval digest exactly.
type ReceivableWriteOffFrozenFacts struct {
	ReceivableVersion            int64
	WriteOffMicrocredits         int64
	OutstandingMicrocredits      int64
	SettledMicrocredits          int64
	OriginalMicrocredits         int64
	SourceType                   string
	SourceReference              string
	WalletReceivableMicrocredits int64
	FactsDigest                  string
}

type ReceivableWriteOffExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              ReceivableWriteOffCommandInput
	Frozen               ReceivableWriteOffFrozenFacts
}

type receivableWriteOffReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type receivableWriteOffAllocationSummary struct {
	SourceType       string `json:"source_type"`
	Count            int64  `json:"count"`
	TotalMicrocredit int64  `json:"total_microcredits"`
}

// receivableWriteOffPlanFrom derives the only permitted write-off from
// already-open owner reads. Every failing contract returns
// ErrApprovalSnapshotMismatch so the caller classifies it as a definite,
// zero-write rejection.
func receivableWriteOffPlanFrom(ctx context.Context, source receivableWriteOffReader, input ReceivableWriteOffCommandInput,
	policyVersion, schemaVersion, activationVersion int64) (ReceivableWriteOffPlan, error) {
	if input.ReceivableID == uuid.Nil || input.WriteOffMicrocredits < 1 || input.ExpectedReceivableVersion < 1 ||
		policyVersion < 1 || schemaVersion < 1 || activationVersion < 1 {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	plan := ReceivableWriteOffPlan{ReceivableID: input.ReceivableID, WriteOffMicrocredits: input.WriteOffMicrocredits}
	var settledAt *time.Time
	err := source.QueryRow(ctx, `SELECT id,wallet_id,source_type,source_reference,original_microcredits,settled_microcredits,
	outstanding_microcredits,status,version,settled_at FROM receivables WHERE id=$1`, input.ReceivableID).
		Scan(&plan.ReceivableID, &plan.WalletID, &plan.SourceType, &plan.SourceReference, &plan.OriginalMicrocredits,
			&plan.SettledMicrocredits, &plan.OutstandingMicrocredits, &plan.ReceivableStatus, &plan.ReceivableVersion, &settledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReceivableWriteOffPlan{}, ErrNotFound
	}
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	if plan.SourceType != "USAGE_OVERAGE" || plan.WalletID == uuid.Nil || plan.ReceivableVersion != input.ExpectedReceivableVersion ||
		(plan.ReceivableStatus != "OPEN" && plan.ReceivableStatus != "PARTIALLY_SETTLED") ||
		plan.OriginalMicrocredits <= 0 || plan.SettledMicrocredits < 0 || plan.OutstandingMicrocredits <= 0 ||
		plan.SettledMicrocredits+plan.OutstandingMicrocredits != plan.OriginalMicrocredits ||
		input.WriteOffMicrocredits > plan.OutstandingMicrocredits || settledAt != nil || strings.TrimSpace(plan.SourceReference) == "" {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	var financialStatus, freezeReason string
	var walletVersion int64
	err = source.QueryRow(ctx, `SELECT receivable_microcredits,financial_status,COALESCE(financial_freeze_reason,''),version
FROM wallets WHERE id=$1`, plan.WalletID).
		Scan(&plan.WalletReceivableMicrocredits, &financialStatus, &freezeReason, &walletVersion)
	if err != nil || plan.WalletReceivableMicrocredits < input.WriteOffMicrocredits || walletVersion < 1 ||
		(financialStatus != "RECEIVABLE_OPEN" && financialStatus != "CLEAR") {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ReceivableWriteOffPlan{}, err
		}
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	var allocationCount int64
	if err := source.QueryRow(ctx, `SELECT count(*) FROM receivable_allocations WHERE receivable_id=$1`, plan.ReceivableID).Scan(&allocationCount); err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	rows, err := source.Query(ctx, `SELECT source_type,count(*),COALESCE(sum(amount_microcredits),0) FROM receivable_allocations
WHERE receivable_id=$1 GROUP BY source_type ORDER BY source_type`, plan.ReceivableID)
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	summaries := []receivableWriteOffAllocationSummary{}
	var allocatedTotal, countedAllocations int64
	for rows.Next() {
		var summary receivableWriteOffAllocationSummary
		if err := rows.Scan(&summary.SourceType, &summary.Count, &summary.TotalMicrocredit); err != nil {
			rows.Close()
			return ReceivableWriteOffPlan{}, err
		}
		summaries = append(summaries, summary)
		allocatedTotal += summary.TotalMicrocredit
		countedAllocations += summary.Count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ReceivableWriteOffPlan{}, err
	}
	rows.Close()
	if countedAllocations != allocationCount || allocatedTotal != plan.SettledMicrocredits {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	var usageRequestID, packageID, orderID, transactionID *uuid.UUID
	if err := source.QueryRow(ctx, `SELECT usage_request_id,entitlement_package_id,payment_order_id,payment_transaction_id
FROM receivables WHERE id=$1`, plan.ReceivableID).Scan(&usageRequestID, &packageID, &orderID, &transactionID); err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].SourceType < summaries[j].SourceType })
	encoded, err := json.Marshal(map[string]any{
		"command_type":  receivableWriteOffCommand,
		"receivable_id": plan.ReceivableID.String(), "wallet_id": plan.WalletID.String(),
		"source_type": plan.SourceType, "source_reference": plan.SourceReference,
		"original_microcredits": plan.OriginalMicrocredits, "settled_microcredits": plan.SettledMicrocredits,
		"outstanding_microcredits": plan.OutstandingMicrocredits, "receivable_status": plan.ReceivableStatus,
		"receivable_version": plan.ReceivableVersion, "settled_at": nil,
		"wallet_receivable_microcredits": plan.WalletReceivableMicrocredits, "wallet_financial_status": financialStatus,
		"wallet_financial_freeze_reason": freezeReason, "wallet_version": walletVersion,
		"usage_request_id": nullableReceivableID(usageRequestID), "entitlement_package_id": nullableReceivableID(packageID),
		"payment_order_id": nullableReceivableID(orderID), "payment_transaction_id": nullableReceivableID(transactionID),
		"allocation_count": allocationCount, "allocations": summaries,
		"write_off_microcredits": plan.WriteOffMicrocredits,
		"policy_version":         policyVersion, "snapshot_schema_version": schemaVersion, "activation_version": activationVersion,
	})
	if err != nil {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	plan.FactsDigest = digestHex(encoded)
	return plan, nil
}

func nullableReceivableID(value *uuid.UUID) any {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	return value.String()
}

// receivableWriteOffProofProjection is the single closed owner projection
// shared by the request assembly path and the authoritative proof
// recomputation.
func receivableWriteOffProofProjection(plan ReceivableWriteOffPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"receivable_version": plan.ReceivableVersion, "facts_digest": plan.FactsDigest, "policy_version": 1,
	})
	financial, _ := json.Marshal(map[string]any{
		"write_off_microcredits": plan.WriteOffMicrocredits, "outstanding_microcredits": plan.OutstandingMicrocredits,
		"settled_microcredits": plan.SettledMicrocredits, "original_microcredits": plan.OriginalMicrocredits,
		"source_type": plan.SourceType, "source_reference": plan.SourceReference,
		"wallet_receivable_microcredits": plan.WalletReceivableMicrocredits,
	})
	return critical, financial
}

// ReceivableWriteOffProofInput derives the closed proof-era projection of one
// approved write-off for a request that is still being assembled. It is
// deliberately not activation-gated: only proof issuance and execution require
// the live ACTIVE row.
func (r *PostgresRepository) ReceivableWriteOffProofInput(ctx context.Context, target ReceivableWriteOffCommandInput) (approvalencoding.LegacyProofInput, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == receivableWriteOffCommand {
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
	plan, err := receivableWriteOffPlanFrom(bounded, tx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	return approvalencoding.LegacyProofInput{
		CommandType: receivableWriteOffCommand, PolicyVersion: activation.PolicyVersion, SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType: "RECEIVABLE", TargetResourceID: plan.ReceivableID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"receivable_id": plan.ReceivableID, "write_off_microcredits": plan.WriteOffMicrocredits,
			"expected_receivable_version": plan.ReceivableVersion, "reason_code": receivableWriteOffReason,
		}),
		CriticalResourceVersions: func() []byte { critical, _ := receivableWriteOffProofProjection(plan); return critical }(),
		FinancialSnapshot:        func() []byte { _, financial := receivableWriteOffProofProjection(plan); return financial }(),
	}, nil
}

// ApplyReceivableWriteOff commits exactly one approved write-off: one
// APPROVED_WRITE_OFF allocation, one balanced ADJUSTMENT journal, the receivable
// and wallet projections and the immutable case row. It never touches
// available balances, credit lots or entitlement facts.
func (r *PostgresRepository) ApplyReceivableWriteOff(ctx context.Context, input ReceivableWriteOffExecutionInput) (ReceivableWriteOffResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return ReceivableWriteOffResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ReceivableWriteOffResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, err := readReceivableWriteOffCase(ctx, tx, input.ApprovalRequestID); err == nil {
		if !stored.matchesFrozen(input, executionKey) {
			return ReceivableWriteOffResult{}, ErrConflict
		}
		return stored.ReceivableWriteOffResult, tx.Commit(ctx)
	} else if !errors.Is(err, ErrNotFound) {
		return ReceivableWriteOffResult{}, err
	}
	if err := receivableWriteOffExecutionActivation(ctx, tx, input, r.now()); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	plan, err := receivableWriteOffPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return ReceivableWriteOffResult{}, err
	}
	if mismatch := receivableWriteOffFrozenMismatch(plan, input.Command, input.Frozen); mismatch != "" {
		return ReceivableWriteOffResult{}, wrapSnapshotMismatch(mismatch)
	}
	caseID, journalID, allocationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	reference := "write-off:" + input.ApprovalRequestID.String()
	now := r.now()
	statusAfter := "PARTIALLY_SETTLED"
	if plan.WriteOffMicrocredits == plan.OutstandingMicrocredits {
		statusAfter = "WRITTEN_OFF"
	}
	if err := insertJournal(ctx, tx, journalID, "ADJUSTMENT", reference, "receivable write-off", now); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	if err := insertPosting(ctx, tx, journalID, receivableAccount, plan.WriteOffMicrocredits, nil, now); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	if err := insertPosting(ctx, tx, journalID, adjustmentAccount, -plan.WriteOffMicrocredits, nil, now); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE receivables SET settled_microcredits=settled_microcredits+$3,outstanding_microcredits=outstanding_microcredits-$3,
status=CASE WHEN outstanding_microcredits=$3 THEN 'WRITTEN_OFF' ELSE 'PARTIALLY_SETTLED' END,
settled_at=CASE WHEN outstanding_microcredits=$3 THEN $4 ELSE settled_at END,version=version+1,updated_at=$4
WHERE id=$1 AND version=$2 AND outstanding_microcredits>=$3 AND source_type='USAGE_OVERAGE'`,
		plan.ReceivableID, plan.ReceivableVersion, plan.WriteOffMicrocredits, now)
	if err != nil || tag.RowsAffected() != 1 {
		return ReceivableWriteOffResult{}, ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE wallets SET receivable_microcredits=receivable_microcredits-$2,
financial_status=CASE WHEN receivable_microcredits=$2 THEN 'CLEAR' ELSE 'RECEIVABLE_OPEN' END,
financial_freeze_reason=CASE WHEN receivable_microcredits=$2 THEN NULL ELSE financial_freeze_reason END,
version=version+1,updated_at=$3 WHERE id=$1 AND receivable_microcredits>=$2`, plan.WalletID, plan.WriteOffMicrocredits, now)
	if err != nil || tag.RowsAffected() != 1 {
		return ReceivableWriteOffResult{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivable_allocations(id,receivable_id,wallet_id,source_type,source_reference,amount_microcredits,journal_entry_id,created_at)
VALUES($1,$2,$3,'APPROVED_WRITE_OFF',$4,$5,$6,$7)`, allocationID, plan.ReceivableID, plan.WalletID, reference, plan.WriteOffMicrocredits, journalID, now); err != nil {
		if receivableWriteOffUniqueViolation(err) {
			return ReceivableWriteOffResult{}, ErrConflict
		}
		return ReceivableWriteOffResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders payment_order SET attention_summary=CASE
WHEN EXISTS(SELECT 1 FROM receivables receivable WHERE receivable.payment_order_id=payment_order.id AND receivable.outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN'
WHEN payment_order.attention_summary='RECEIVABLE_OPEN' THEN 'NORMAL' ELSE payment_order.attention_summary END,
summary_version=summary_version+1,updated_at=$2 WHERE payment_order.id IN (
SELECT DISTINCT receivable.payment_order_id FROM receivables receivable WHERE receivable.wallet_id=$1 AND receivable.payment_order_id IS NOT NULL)`, plan.WalletID, now); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivable_write_off_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
receivable_id,wallet_id,amount_microcredits,receivable_version_before,receivable_version_after,status_after,facts_digest,journal_entry_id,allocation_id,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'EXECUTED',$15)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch, ledgerRepairExecutionKeyDigest(executionKey),
		plan.ReceivableID, plan.WalletID, plan.WriteOffMicrocredits, plan.ReceivableVersion, plan.ReceivableVersion+1,
		statusAfter, plan.FactsDigest, journalID, allocationID, now); err != nil {
		if receivableWriteOffUniqueViolation(err) {
			if stored, readErr := readReceivableWriteOffCaseByReceivable(ctx, tx, plan.ReceivableID); readErr == nil && stored.matchesFrozen(input, executionKey) {
				return stored.ReceivableWriteOffResult, tx.Commit(ctx)
			}
			return ReceivableWriteOffResult{}, ErrConflict
		}
		return ReceivableWriteOffResult{}, err
	}
	result := ReceivableWriteOffResult{
		CaseID: caseID, ReceivableID: plan.ReceivableID, WalletID: plan.WalletID, AmountMicrocredits: plan.WriteOffMicrocredits,
		ReceivableVersionBefore: plan.ReceivableVersion, ReceivableVersionAfter: plan.ReceivableVersion + 1,
		StatusAfter: statusAfter, FactsDigest: plan.FactsDigest, JournalEntryID: journalID, AllocationID: allocationID, Status: "SUCCEEDED",
	}
	if err := tx.Commit(ctx); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	stored, err := r.ReceivableWriteOffForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored != result {
		if err == nil {
			err = ErrConflict
		}
		return ReceivableWriteOffResult{}, err
	}
	return stored, nil
}

// ReceivableWriteOffForRequest reads the committed write-off of one approval
// request without creating any new fact (result-first).
func (r *PostgresRepository) ReceivableWriteOffForRequest(ctx context.Context, approvalRequestID uuid.UUID) (ReceivableWriteOffResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return ReceivableWriteOffResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReceivableWriteOffResult{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := readReceivableWriteOffCase(ctx, tx, approvalRequestID)
	if err != nil {
		return ReceivableWriteOffResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReceivableWriteOffResult{}, err
	}
	return stored.ReceivableWriteOffResult, nil
}

type receivableWriteOffCaseRecord struct {
	ReceivableWriteOffResult
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKeyDigest   string
}

func (record receivableWriteOffCaseRecord) matchesFrozen(input ReceivableWriteOffExecutionInput, executionKey string) bool {
	return record.ApprovalRequestID == input.ApprovalRequestID && record.AuthorizationEventID == input.AuthorizationEventID &&
		record.ApprovalEpoch == input.ApprovalEpoch && record.ExecutionKeyDigest == ledgerRepairExecutionKeyDigest(executionKey) &&
		record.ReceivableID == input.Command.ReceivableID && record.AmountMicrocredits == input.Command.WriteOffMicrocredits &&
		record.ReceivableVersionBefore == input.Command.ExpectedReceivableVersion &&
		(input.Frozen.FactsDigest == "" || record.FactsDigest == input.Frozen.FactsDigest)
}

const receivableWriteOffCaseColumns = `id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
receivable_id,wallet_id,amount_microcredits,receivable_version_before,receivable_version_after,status_after,
facts_digest,journal_entry_id,allocation_id`

func scanReceivableWriteOffCase(row pgx.Row) (receivableWriteOffCaseRecord, error) {
	var record receivableWriteOffCaseRecord
	err := row.Scan(&record.CaseID, &record.ApprovalRequestID, &record.AuthorizationEventID, &record.ApprovalEpoch, &record.ExecutionKeyDigest,
		&record.ReceivableID, &record.WalletID, &record.AmountMicrocredits, &record.ReceivableVersionBefore, &record.ReceivableVersionAfter,
		&record.StatusAfter, &record.FactsDigest, &record.JournalEntryID, &record.AllocationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return receivableWriteOffCaseRecord{}, ErrNotFound
	}
	if err != nil {
		return receivableWriteOffCaseRecord{}, err
	}
	record.Status = "SUCCEEDED"
	return record, nil
}

func readReceivableWriteOffCase(ctx context.Context, source receivableWriteOffReader, approvalRequestID uuid.UUID) (receivableWriteOffCaseRecord, error) {
	return scanReceivableWriteOffCase(source.QueryRow(ctx, `SELECT `+receivableWriteOffCaseColumns+` FROM receivable_write_off_cases WHERE approval_request_id=$1`, approvalRequestID))
}

func readReceivableWriteOffCaseByReceivable(ctx context.Context, source receivableWriteOffReader, receivableID uuid.UUID) (receivableWriteOffCaseRecord, error) {
	return scanReceivableWriteOffCase(source.QueryRow(ctx, `SELECT `+receivableWriteOffCaseColumns+` FROM receivable_write_off_cases WHERE receivable_id=$1 ORDER BY created_at,id LIMIT 1`, receivableID))
}

func receivableWriteOffUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func wrapSnapshotMismatch(field string) error {
	return errors.Join(ErrApprovalSnapshotMismatch, errors.New(field))
}

func receivableWriteOffFrozenMismatch(plan ReceivableWriteOffPlan, command ReceivableWriteOffCommandInput, frozen ReceivableWriteOffFrozenFacts) string {
	switch {
	case plan.ReceivableID != command.ReceivableID:
		return "receivable_id"
	case plan.ReceivableVersion != command.ExpectedReceivableVersion:
		return "expected_receivable_version"
	case plan.WriteOffMicrocredits != command.WriteOffMicrocredits:
		return "write_off_microcredits"
	case plan.ReceivableVersion != frozen.ReceivableVersion:
		return "receivable_version"
	case plan.WriteOffMicrocredits != frozen.WriteOffMicrocredits:
		return "write_off_microcredits"
	case plan.OutstandingMicrocredits != frozen.OutstandingMicrocredits:
		return "outstanding_microcredits"
	case plan.SettledMicrocredits != frozen.SettledMicrocredits:
		return "settled_microcredits"
	case plan.OriginalMicrocredits != frozen.OriginalMicrocredits:
		return "original_microcredits"
	case plan.SourceType != frozen.SourceType:
		return "source_type"
	case plan.SourceReference != frozen.SourceReference:
		return "source_reference"
	case plan.WalletReceivableMicrocredits != frozen.WalletReceivableMicrocredits:
		return "wallet_receivable_microcredits"
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}

// Only a fresh effect needs current activation. Committed results return before
// this check so a later deactivation cannot turn a durable success into a retry.
func receivableWriteOffExecutionActivation(ctx context.Context, source receivableWriteOffReader, input ReceivableWriteOffExecutionInput, now time.Time) error {
	var active ApprovalCommandActivation
	err := source.QueryRow(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activations WHERE command_type=$1 AND policy_version=$2`, receivableWriteOffCommand, input.PolicyVersion).
		Scan(&active.CommandType, &active.PolicyVersion, &active.SnapshotSchemaVersion, &active.State, &active.Version, &active.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalSnapshotMismatch
	}
	if err != nil {
		return err
	}
	if !validActivationItem(active, receivableWriteOffCommand, billingApprovalActivationSpecs[receivableWriteOffCommand], now) ||
		active.State != "ACTIVE" || active.PolicyVersion != input.PolicyVersion || int64(active.SnapshotSchemaVersion) != input.SchemaVersion || active.Version != input.ActivationVersion {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}
