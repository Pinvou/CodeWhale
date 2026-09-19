package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

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
	entitlementCorrectionPlan(context.Context, uuid.UUID, uuid.UUID) (entitlementCorrectionReadPlan, error)
	ledgerRepairPlan(context.Context, LedgerRepairCommandInput) (LedgerRepairPlan, error)
	receivableWriteOffPlan(context.Context, ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error)
	paymentFinancialRepairPlan(context.Context, PaymentFinancialRepairCommandInput) (PaymentFinancialRepairPlan, error)
	refundExceptionRepairPlan(context.Context, RefundExceptionRepairCommandInput) (RefundExceptionRepairPlan, error)
	chargebackFinancialRepairPlan(context.Context, ChargebackFinancialRepairCommandInput) (ChargebackFinancialRepairPlan, error)
}

// chargebackFinancialRepairPlan re-reads the owner activation row and recomputes
// the closed chargeback shortfall projection. The activation row must be ACTIVE
// with the current version, and neither this path nor execution calls the
// provider.
func (r *PostgresRepository) chargebackFinancialRepairPlan(ctx context.Context, input ChargebackFinancialRepairCommandInput) (ChargebackFinancialRepairPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, chargebackFinancialRepairCommand, r.now())
	if err != nil {
		return ChargebackFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := chargebackFinancialRepairPlanFrom(ctx, tx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
	if err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ChargebackFinancialRepairPlan{}, err
	}
	return plan, nil
}

// refundExceptionRepairPlan re-reads the owner activation row and recomputes the
// closed refund repair projection. The activation row must be ACTIVE with the
// current version, and neither this path nor execution calls the provider.
func (r *PostgresRepository) refundExceptionRepairPlan(ctx context.Context, input RefundExceptionRepairCommandInput) (RefundExceptionRepairPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, refundExceptionRepairCommand, r.now())
	if err != nil {
		return RefundExceptionRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := refundExceptionRepairPlanFrom(ctx, tx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
	if err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundExceptionRepairPlan{}, err
	}
	return plan, nil
}

// paymentFinancialRepairPlan re-reads the owner activation row and recomputes
// the closed payment-repair projection. The activation row must be ACTIVE with
// the current version, and this path never calls the provider.
func (r *PostgresRepository) paymentFinancialRepairPlan(ctx context.Context, input PaymentFinancialRepairCommandInput) (PaymentFinancialRepairPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, paymentFinancialRepairCommand, r.now())
	if err != nil {
		return PaymentFinancialRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := paymentFinancialRepairPlanFrom(ctx, tx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
	if err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentFinancialRepairPlan{}, err
	}
	return plan, nil
}

// receivableWriteOffPlan re-reads the owner activation row and recomputes the
// closed write-off projection. The activation row must be ACTIVE with the
// current version, so a proof can never be issued while the command is
// INACTIVE.
func (r *PostgresRepository) receivableWriteOffPlan(ctx context.Context, input ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error) {
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	active, err := activeBillingProofCommand(snapshot, receivableWriteOffCommand, r.now())
	if err != nil {
		return ReceivableWriteOffPlan{}, ErrApprovalSnapshotMismatch
	}
	return r.receivableWriteOffPlanFrom(ctx, input, active.PolicyVersion, int64(active.SnapshotSchemaVersion), active.Version)
}

func (r *PostgresRepository) receivableWriteOffPlanFrom(ctx context.Context, input ReceivableWriteOffCommandInput, policyVersion, schemaVersion, activationVersion int64) (ReceivableWriteOffPlan, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := receivableWriteOffPlanFrom(ctx, tx, input, policyVersion, schemaVersion, activationVersion)
	if err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReceivableWriteOffPlan{}, err
	}
	return plan, nil
}

// ledgerRepairPlan re-reads the owner activation row and recomputes the closed
// ledger repair projection. The activation row must be ACTIVE with the current
// version, so a proof can never be issued while the command is INACTIVE.
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

func (r *PostgresRepository) entitlementCorrectionPlan(ctx context.Context, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	return r.planEntitlementComponentCorrection(ctx, packageID, orderID)
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
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		var command struct {
			PackageID      uuid.UUID `json:"package_id"`
			PaymentOrderID uuid.UUID `json:"payment_order_id"`
			ReasonCode     string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.PackageID == uuid.Nil ||
			command.PaymentOrderID == uuid.Nil || strings.TrimSpace(command.ReasonCode) == "" {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.entitlementCorrectionPlan(ctx, command.PackageID, command.PaymentOrderID)
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.Amounts.SourceType + "->" + plan.Amounts.TargetType
		current.CriticalResourceVersions, current.FinancialSnapshot = entitlementCorrectionProofProjection(plan)
		if plan.PackageID != command.PackageID || plan.PaymentOrderID != command.PaymentOrderID {
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
			command.TargetPostingID == uuid.Nil || strings.TrimSpace(command.ReasonCode) == "" {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.ledgerRepairPlan(ctx, LedgerRepairCommandInput{
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
			strings.TrimSpace(command.ReasonCode) != receivableWriteOffReason {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.receivableWriteOffPlan(ctx, ReceivableWriteOffCommandInput{
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
	case paymentFinancialRepairCommand:
		var command struct {
			PaymentOrderID    uuid.UUID `json:"payment_order_id"`
			PaymentEvidenceID uuid.UUID `json:"payment_evidence_id"`
			ReasonCode        string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.PaymentOrderID == uuid.Nil ||
			command.PaymentEvidenceID == uuid.Nil || strings.TrimSpace(command.ReasonCode) != paymentFinancialRepairReason {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.paymentFinancialRepairPlan(ctx, PaymentFinancialRepairCommandInput{
			PaymentOrderID: command.PaymentOrderID, PaymentEvidenceID: command.PaymentEvidenceID,
		})
		if errors.Is(err, ErrApprovalSnapshotMismatch) || errors.Is(err, ErrNotFound) {
			return current, "DOMAIN_INVARIANT_FAILED", ownerState, nil
		}
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.ProviderTransactionID
		critical, financial := paymentFinancialRepairProofProjection(plan)
		current.CriticalResourceVersions, current.FinancialSnapshot = critical, financial
		if plan.OrderID != command.PaymentOrderID || plan.EvidenceID != command.PaymentEvidenceID {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	case refundExceptionRepairCommand:
		var command struct {
			RefundID          uuid.UUID `json:"refund_id"`
			PaymentEvidenceID uuid.UUID `json:"payment_evidence_id"`
			AnomalyCaseID     uuid.UUID `json:"anomaly_case_id"`
			ReasonCode        string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.RefundID == uuid.Nil ||
			command.PaymentEvidenceID == uuid.Nil || command.AnomalyCaseID == uuid.Nil ||
			strings.TrimSpace(command.ReasonCode) != refundExceptionRepairReason {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.refundExceptionRepairPlan(ctx, RefundExceptionRepairCommandInput{
			RefundID: command.RefundID, PaymentEvidenceID: command.PaymentEvidenceID, AnomalyCaseID: command.AnomalyCaseID,
		})
		if errors.Is(err, ErrApprovalSnapshotMismatch) || errors.Is(err, ErrNotFound) {
			return current, "DOMAIN_INVARIANT_FAILED", ownerState, nil
		}
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.ProviderRefundID
		critical, financial := refundExceptionRepairProofProjection(plan)
		current.CriticalResourceVersions, current.FinancialSnapshot = critical, financial
		if plan.RefundID != command.RefundID || plan.EvidenceID != command.PaymentEvidenceID || plan.AnomalyCaseID != command.AnomalyCaseID {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	case chargebackFinancialRepairCommand:
		var command struct {
			ChargebackCaseID    uuid.UUID `json:"chargeback_case_id"`
			PaymentEvidenceID   uuid.UUID `json:"payment_evidence_id"`
			ExpectedCaseVersion int64     `json:"expected_case_version"`
			ReasonCode          string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.ChargebackCaseID == uuid.Nil ||
			command.PaymentEvidenceID == uuid.Nil || command.ExpectedCaseVersion < 1 ||
			strings.TrimSpace(command.ReasonCode) != chargebackFinancialRepairReason {
			return current, "", "", ErrApprovalSnapshotMismatch
		}
		plan, err := repository.chargebackFinancialRepairPlan(ctx, ChargebackFinancialRepairCommandInput{
			ChargebackCaseID: command.ChargebackCaseID, PaymentEvidenceID: command.PaymentEvidenceID,
		})
		if errors.Is(err, ErrApprovalSnapshotMismatch) || errors.Is(err, ErrNotFound) {
			return current, "DOMAIN_INVARIANT_FAILED", ownerState, nil
		}
		if err != nil {
			return current, "", "", err
		}
		ownerState = plan.ProviderChargebackID
		critical, financial := chargebackFinancialRepairProofProjection(plan)
		current.CriticalResourceVersions, current.FinancialSnapshot = critical, financial
		if plan.ChargebackCaseID != command.ChargebackCaseID || plan.EvidenceID != command.PaymentEvidenceID ||
			plan.CaseVersion != command.ExpectedCaseVersion {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	default:
		return current, "", "", errors.New("unsupported Billing proof command")
	}
	return current, "", ownerState, nil
}
