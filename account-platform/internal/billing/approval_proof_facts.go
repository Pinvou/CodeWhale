package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
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
		nullableID := func(value uuid.UUID) any {
			if value == uuid.Nil {
				return nil
			}
			return value
		}
		ownerState = plan.Amounts.SourceType + "->" + plan.Amounts.TargetType
		current.CriticalResourceVersions = marshal(map[string]any{
			"package_id": plan.PackageID, "wallet_id": plan.WalletID, "wallet_version": plan.WalletVersion,
			"payment_order_id": plan.PaymentOrderID, "payment_transaction_id": plan.PaymentTransactionID,
			"source_component_id": plan.SourceComponentID, "source_lot_id": plan.SourceLotID,
			"target_component_id": nullableID(plan.TargetComponentID), "target_lot_id": nullableID(plan.TargetLotID),
			"source_type": plan.Amounts.SourceType, "target_type": plan.Amounts.TargetType,
			"amount_microcredits": plan.Amounts.Amount, "source_evidence_digest": plan.SourceEvidenceDigest,
			"facts_digest": plan.FactsDigest, "policy_version": 2,
		})
		current.FinancialSnapshot = marshal(map[string]any{
			"package_total_microcredits": plan.PackageTotal, "source_original_microcredits": plan.SourceOriginal,
			"target_original_microcredits": plan.TargetOriginal,
			"effective_total_microcredits": plan.SourceOriginal + plan.TargetOriginal,
		})
		if plan.PackageID != command.PackageID || plan.PaymentOrderID != command.PaymentOrderID {
			return current, "SNAPSHOT_MISMATCH", ownerState, nil
		}
	default:
		return current, "", "", errors.New("unsupported Billing proof command")
	}
	return current, "", ownerState, nil
}
