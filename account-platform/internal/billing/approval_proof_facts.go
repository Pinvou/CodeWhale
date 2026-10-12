package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// billingProofFacts reads only owner-domain persisted facts. In particular,
// this path must never call the payment provider or create an observation.
type billingProofFacts interface {
	AdjustmentForExecution(context.Context, uuid.UUID) (ManualAdjustment, error)
	GetWallet(context.Context, uuid.UUID) (Wallet, error)
	providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error)
	productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error)
	confirmedResumeProbe(context.Context, string, string, string, uuid.UUID) (bool, error)
}

func (r *PostgresRepository) confirmedResumeProbe(ctx context.Context, providerID, providerVersion, productID string, orderID uuid.UUID) (bool, error) {
	return r.confirmedResumeProbeFrom(ctx, r.pool, providerID, providerVersion, productID, orderID)
}

func (r *PostgresRepository) confirmedResumeProbeFrom(ctx context.Context, source recoveryQuerier, providerID, providerVersion, productID string, orderID uuid.UUID) (bool, error) {
	var confirmed bool
	err := source.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM payment_provider_failure_confirmations confirmation
		JOIN payment_orders payment ON payment.id=confirmation.payment_order_id
		WHERE confirmation.payment_order_id=$1 AND payment.provider_id=$2 AND payment.provider_version=$3
		AND confirmation.provider_id=payment.provider_id AND confirmation.provider_version=payment.provider_version
		AND ($4='' OR payment.product_id=$4)
	)`, orderID, providerID, providerVersion, productID).Scan(&confirmed)
	return confirmed, err
}

// currentBillingProofInput reconstructs the closed legacy projection from the
// current rows, retaining only the already-frozen normalized command. A
// state/invariant failure is a DENY only when all revision facts are readable.
func currentBillingProofInput(ctx context.Context, repository billingProofFacts, frozen approvalencoding.LegacyProofInput) (approvalencoding.LegacyProofInput, string, string, error) {
	current := frozen
	ownerState := ""
	marshal := func(value any) json.RawMessage {
		encoded, _ := json.Marshal(value)
		return encoded
	}
	switch frozen.CommandType {
	case entitlementComponentAdjustmentCommand:
		owner, ok := repository.(interface {
			EntitlementCorrectionProofInput(context.Context, uuid.UUID, uuid.UUID, string) (approvalencoding.LegacyProofInput, error)
		})
		var command struct {
			PackageID      uuid.UUID `json:"package_id"`
			PaymentOrderID uuid.UUID `json:"payment_order_id"`
			ReasonCode     string    `json:"reason_code"`
		}
		if !ok || json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.PackageID == uuid.Nil || command.PackageID.String() != frozen.TargetResourceID {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		fresh, err := owner.EntitlementCorrectionProofInput(ctx, command.PackageID, command.PaymentOrderID, command.ReasonCode)
		if err != nil {
			return current, "", "", err
		}
		current = fresh
		ownerState = "ENTITLEMENT_PACKAGE_ACTIVE"
	case "WALLET_ADJUSTMENT":
		var command struct {
			AdjustmentID       uuid.UUID `json:"adjustment_id"`
			WalletID           uuid.UUID `json:"wallet_id"`
			AmountMicrocredits int64     `json:"amount_microcredits"`
			RequestedBy        uuid.UUID `json:"requested_by"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.AdjustmentID == uuid.Nil {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		adjustment, err := repository.AdjustmentForExecution(ctx, command.AdjustmentID)
		if err != nil {
			return current, "", "", err
		}
		wallet, err := repository.GetWallet(ctx, adjustment.WalletID)
		if err != nil {
			return current, "", "", err
		}
		ownerState = adjustment.Status + ":" + wallet.Status
		current.CriticalResourceVersions = marshal(map[string]any{"adjustment_version": adjustment.Version})
		current.FinancialSnapshot = marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": adjustment.AmountMicrocredits})
		if adjustment.ID != command.AdjustmentID || adjustment.ID.String() != frozen.TargetResourceID || adjustment.WalletID != command.WalletID || adjustment.AmountMicrocredits != command.AmountMicrocredits || adjustment.RequestedBy != command.RequestedBy {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
		if adjustment.Status != "PENDING_APPROVAL" || wallet.Status != "ACTIVE" {
			return current, "RESOURCE_STATE_CHANGED", ownerState, nil
		}
	case "PAYMENT_PROVIDER_RESUME":
		var command ProviderResumeVersions
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.ProbeOrderID == uuid.Nil {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		actual, err := repository.providerResumeVersions(ctx, command)
		if err != nil {
			return current, "", "", err
		}
		confirmed, err := repository.confirmedResumeProbe(ctx, command.ProviderID, command.ProviderVersion, "", command.ProbeOrderID)
		if err != nil {
			return current, "", "", err
		}
		ownerState = "confirmed:" + strconv.FormatBool(confirmed)
		current.CriticalResourceVersions = marshal(actual)
		current.FinancialSnapshot = marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": actual.ProviderControlVersion, "product_control_version": nil})
		if !confirmed {
			return current, "EVIDENCE_MISSING", ownerState, nil
		}
		if actual.ProviderID != frozen.TargetResourceID || !sameProviderResumeVersions(actual, command) {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	case "PAYMENT_PRODUCT_RESUME":
		var command ProductResumeVersions
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.ProbeOrderID == uuid.Nil {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		actual, err := repository.productResumeVersions(ctx, command)
		if err != nil {
			return current, "", "", err
		}
		confirmed, err := repository.confirmedResumeProbe(ctx, command.ProviderID, command.ProviderVersion, command.ProductID, command.ProbeOrderID)
		if err != nil {
			return current, "", "", err
		}
		ownerState = "confirmed:" + strconv.FormatBool(confirmed)
		current.CriticalResourceVersions = marshal(actual)
		current.FinancialSnapshot = marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": actual.ProviderControlVersion, "product_control_version": actual.ProductControlVersion})
		if !confirmed {
			return current, "EVIDENCE_MISSING", ownerState, nil
		}
		if actual.ProductID != frozen.TargetResourceID || !sameProductResumeVersions(actual, command) {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	case ledgerRepairCommand:
		var command struct {
			TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
			TargetPostingID        uuid.UUID `json:"target_posting_id"`
			WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
			CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
			AmountMicrocredits     int64     `json:"amount_microcredits"`
			ReasonCode             string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.TargetJournalEntryID == uuid.Nil ||
			command.TargetPostingID == uuid.Nil || command.ReasonCode != "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT" {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		owner, ok := repository.(interface {
			ledgerRepairPlan(context.Context, LedgerRepairCommandInput) (LedgerRepairPlan, error)
		})
		if !ok {
			return current, "", "", errBillingApprovalProofUnavailable
		}
		plan, err := owner.ledgerRepairPlan(ctx, LedgerRepairCommandInput{
			TargetJournalEntryID: command.TargetJournalEntryID, TargetPostingID: command.TargetPostingID,
			WrongLedgerAccountID: command.WrongLedgerAccountID, CorrectLedgerAccountID: command.CorrectLedgerAccountID,
			AmountMicrocredits: command.AmountMicrocredits,
		})
		if errors.Is(err, ErrApprovalSnapshotMismatch) {
			return current, "DOMAIN_INVARIANT_FAILED", ownerState, nil
		}
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.EntryType + ":" + plan.WrongLedgerAccountType + "->" + plan.CorrectLedgerAccountType
		critical, financial := ledgerRepairProofProjection(plan)
		current.CriticalResourceVersions, current.FinancialSnapshot = critical, financial
		if plan.TargetJournalEntryID != command.TargetJournalEntryID || plan.TargetPostingID != command.TargetPostingID ||
			plan.WrongLedgerAccountID != command.WrongLedgerAccountID || plan.CorrectLedgerAccountID != command.CorrectLedgerAccountID ||
			plan.AmountMicrocredits != command.AmountMicrocredits {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	case receivableWriteOffCommand:
		var command struct {
			ReceivableID              uuid.UUID `json:"receivable_id"`
			WriteOffMicrocredits      int64     `json:"write_off_microcredits"`
			ExpectedReceivableVersion int64     `json:"expected_receivable_version"`
			ReasonCode                string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.ReceivableID == uuid.Nil ||
			command.WriteOffMicrocredits < 1 || command.ExpectedReceivableVersion < 1 ||
			command.ReasonCode != receivableWriteOffReason {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		owner, ok := repository.(interface {
			receivableWriteOffPlan(context.Context, ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error)
		})
		if !ok {
			return current, "", "", errBillingApprovalProofUnavailable
		}
		plan, err := owner.receivableWriteOffPlan(ctx, ReceivableWriteOffCommandInput{
			ReceivableID: command.ReceivableID, WriteOffMicrocredits: command.WriteOffMicrocredits,
			ExpectedReceivableVersion: command.ExpectedReceivableVersion,
		})
		if errors.Is(err, ErrApprovalSnapshotMismatch) || errors.Is(err, ErrNotFound) {
			return current, "DOMAIN_INVARIANT_FAILED", ownerState, nil
		}
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.SourceType + ":" + plan.ReceivableStatus
		critical, financial := receivableWriteOffProofProjection(plan)
		current.CriticalResourceVersions, current.FinancialSnapshot = critical, financial
		if plan.ReceivableID != command.ReceivableID || plan.WriteOffMicrocredits != command.WriteOffMicrocredits ||
			plan.ReceivableVersion != command.ExpectedReceivableVersion {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	default:
		return current, "", "", errors.New("unsupported Billing proof command")
	}
	return current, "", ownerState, nil
}

func (r *PostgresRepository) entitlementCorrectionPlan(ctx context.Context, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	return r.planEntitlementComponentCorrection(ctx, packageID, orderID)
}

func (r *PostgresRepository) ledgerRepairPlan(ctx context.Context, input LedgerRepairCommandInput) (LedgerRepairPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return LedgerRepairPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, ledgerRepairCommand, r.now())
	if err != nil {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	return r.LedgerRepairPlanForCommand(ctx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
}

func (r *PostgresRepository) receivableWriteOffPlan(ctx context.Context, input ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, receivableWriteOffCommand, r.now())
	if err != nil {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := receivableWriteOffPlanFrom(ctx, tx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	return plan, nil
}
