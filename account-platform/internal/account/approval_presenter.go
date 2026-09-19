package account

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
)

var ErrApprovalDetailPresentationUnavailable = errors.New("approval detail presentation is unavailable")

// ApprovalV2Summary is deliberately independent of ApprovalRequest's JSON
// representation. Historical commands with no exact presenter get only this
// closed base plus a stable availability code, never command JSON.
type ApprovalV2Summary struct {
	ApprovalRequestID     uuid.UUID           `json:"approval_request_id"`
	CommandType           string              `json:"command_type"`
	PolicyVersion         int64               `json:"policy_version"`
	SnapshotSchemaVersion int                 `json:"snapshot_schema_version"`
	RiskClass             string              `json:"risk_class"`
	Target                ApprovalV2Target    `json:"target"`
	Requester             ApprovalV2Requester `json:"requester"`
	Status                string              `json:"status"`
	Version               int64               `json:"version"`
	SubmittedAt           *time.Time          `json:"submitted_at"`
	ExpiresAt             time.Time           `json:"expires_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
	SupersedesRequestID   *uuid.UUID          `json:"supersedes_request_id"`
	DetailAvailability    string              `json:"detail_availability"`
}

type ApprovalV2Target struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type ApprovalV2Requester struct {
	UserID uuid.UUID `json:"user_id"`
}

type ApprovalV2Detail struct {
	ApprovalV2Summary
	AllowedActions       []string             `json:"allowed_actions"`
	ActionControlVersion int64                `json:"action_control_version"`
	Command              any                  `json:"command"`
	Execution            *ApprovalV2Execution `json:"execution"`
}

type ApprovalV2Execution struct {
	AttemptStatus     string     `json:"attempt_status"`
	ErrorClass        *string    `json:"error_class"`
	Retryable         bool       `json:"retryable"`
	NextRetryAt       *time.Time `json:"next_retry_at"`
	ExecutionDeadline *time.Time `json:"execution_deadline"`
}

// Each variant has a distinct closed JSON shape. Never pass arbitrary
// normalized_command, financial_snapshot or evidence to a presenter.
type approvalWalletSummary struct {
	AdjustmentID       uuid.UUID `json:"adjustment_id"`
	WalletID           uuid.UUID `json:"wallet_id"`
	AmountMicrocredits string    `json:"amount_microcredits"`
}
type approvalProviderSummary struct {
	ProviderID             string `json:"provider_id"`
	ProviderControlVersion string `json:"provider_control_version"`
}
type approvalProductSummary struct {
	ProductID             string `json:"product_id"`
	ProviderID            string `json:"provider_id"`
	ProductControlVersion string `json:"product_control_version"`
}
type approvalLifecycleSummary struct {
	ModelID         uuid.UUID `json:"model_id"`
	TargetLifecycle string    `json:"target_lifecycle"`
	EffectiveAt     *string   `json:"effective_at"`
}
type approvalCatalogSummary struct {
	SourceCatalogVersion string `json:"source_catalog_version"`
}
type approvalCredentialSummary struct {
	CredentialID   uuid.UUID `json:"credential_id"`
	TokenAccountID uuid.UUID `json:"token_account_id"`
}

// approvalEntitlementCorrectionSummary is the closed read-only projection of
// the entitlement component correction command: only the target package and the
// operator-chosen reason code are presented, never caller-supplied amounts.
type approvalEntitlementCorrectionSummary struct {
	PackageID  uuid.UUID `json:"package_id"`
	ReasonCode string    `json:"reason_code"`
}

// approvalLedgerRepairSummary is the closed read-only projection of the ledger
// repair command: only the immutable target posting, the frozen owner revision
// digest and the operator-chosen reason code are presented, never a
// caller-supplied account, amount or free text.
type approvalLedgerRepairSummary struct {
	TargetJournalEntryID uuid.UUID `json:"target_journal_entry_id"`
	TargetPostingID      uuid.UUID `json:"target_posting_id"`
	EntryType            string    `json:"entry_type"`
	AmountMicrocredits   string    `json:"amount_microcredits"`
	PostingSetDigest     string    `json:"posting_set_digest"`
	ReasonCode           string    `json:"reason_code"`
}

// approvalReceivableWriteOffSummary is the closed read-only projection of the
// receivable write-off command: only the target receivable, the frozen owner
// amounts and the fixed reason code are presented, never caller-supplied
// accounts or free text. Settled amounts are shown as the resolved total, never
// as received cash.
type approvalReceivableWriteOffSummary struct {
	ReceivableID                 uuid.UUID `json:"receivable_id"`
	SourceType                   string    `json:"source_type"`
	SourceReference              string    `json:"source_reference"`
	OriginalMicrocredits         string    `json:"original_microcredits"`
	SettledMicrocredits          string    `json:"settled_microcredits"`
	OutstandingMicrocredits      string    `json:"outstanding_microcredits"`
	WriteOffMicrocredits         string    `json:"write_off_microcredits"`
	WalletReceivableMicrocredits string    `json:"wallet_receivable_microcredits"`
	FactsDigest                  string    `json:"facts_digest"`
	ReasonCode                   string    `json:"reason_code"`
}

// approvalPaymentFinancialRepairSummary is the closed read-only projection of
// the confirmed-payment repair command: the target order, the frozen amounts and
// the owner revision digest. Provider payloads, evidence internals and free text
// are never presented.
type approvalPaymentFinancialRepairSummary struct {
	PaymentOrderID       uuid.UUID `json:"payment_order_id"`
	AmountFen            string    `json:"amount_fen"`
	CreditMicrocredits   string    `json:"credit_microcredits"`
	AppliedToReceivables string    `json:"applied_to_receivables"`
	WalletVersion        string    `json:"wallet_version"`
	FactsDigest          string    `json:"facts_digest"`
	ReasonCode           string    `json:"reason_code"`
}

// approvalRefundExceptionRepairSummary is the closed read-only projection of
// the refund exception repair command: the target refund, the owner-derived
// revoked/shortfall split and the owner revision digest. Provider refund
// identifiers, evidence internals and free text are never presented.
type approvalRefundExceptionRepairSummary struct {
	RefundID              uuid.UUID `json:"refund_id"`
	WalletID              uuid.UUID `json:"wallet_id"`
	PackageID             uuid.UUID `json:"package_id"`
	AmountFen             string    `json:"amount_fen"`
	CreditMicrocredits    string    `json:"credit_microcredits"`
	RevokedMicrocredits   string    `json:"revoked_microcredits"`
	ShortfallMicrocredits string    `json:"shortfall_microcredits"`
	FactsDigest           string    `json:"facts_digest"`
	ReasonCode            string    `json:"reason_code"`
}

// approvalChargebackFinancialRepairSummary is the closed read-only projection of
// the chargeback BASE shortfall compensation: the target case, the owner-derived
// shortfall and both owner digests. Provider payloads, evidence internals and
// free text are never presented.
type approvalChargebackFinancialRepairSummary struct {
	ChargebackCaseID          uuid.UUID `json:"chargeback_case_id"`
	WalletID                  uuid.UUID `json:"wallet_id"`
	PackageID                 uuid.UUID `json:"package_id"`
	BaseShortfallMicrocredits string    `json:"base_shortfall_microcredits"`
	ComponentDigest           string    `json:"component_digest"`
	FactsDigest               string    `json:"facts_digest"`
	ReasonCode                string    `json:"reason_code"`
}

func approvalPresenterAvailable(command string, policyVersion int64, snapshotVersion int) bool {
	switch command {
	case "WALLET_ADJUSTMENT", "PAYMENT_PRODUCT_RESUME", "TOKEN_CREDENTIAL_REVOKE":
		return policyVersion == 1 && snapshotVersion == 1
	case "PAYMENT_PROVIDER_RESUME":
		return policyVersion == 2 && snapshotVersion == 1
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		return policyVersion == 2 && snapshotVersion == 2
	case "LEDGER_REPAIR":
		return policyVersion == 1 && snapshotVersion == 1
	case "RECEIVABLE_WRITE_OFF":
		return policyVersion == 1 && snapshotVersion == 1
	case "PAYMENT_FINANCIAL_REPAIR":
		return policyVersion == 1 && snapshotVersion == 1
	case "REFUND_EXCEPTION_REPAIR":
		return policyVersion == 1 && snapshotVersion == 1
	case "CHARGEBACK_FINANCIAL_REPAIR":
		return policyVersion == 1 && snapshotVersion == 1
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK":
		return policyVersion == 2 && snapshotVersion == 2
	default:
		return false
	}
}

func approvalPolicyPresenterReady(policy approvalpolicy.Policy) bool {
	return approvalPresenterAvailable(policy.CommandType, policy.PolicyVersion, policy.SnapshotSchemaVersion)
}

func approvalV2Summary(item ApprovalRequest) ApprovalV2Summary {
	availability := "AVAILABLE"
	if !approvalPresenterAvailable(item.CommandType, item.PolicyVersion, item.SnapshotSchemaVersion) || approvalV2Command(item) == nil {
		availability = "DETAIL_PRESENTATION_UNAVAILABLE"
	}
	target := ApprovalV2Target{ResourceType: item.TargetResourceType, ResourceID: approvalSafeTargetID(item.TargetResourceID)}
	if availability != "AVAILABLE" {
		// Unknown historical rows must not echo arbitrary resource text.
		target = ApprovalV2Target{}
	}
	return ApprovalV2Summary{
		ApprovalRequestID: item.ID, CommandType: item.CommandType,
		PolicyVersion: item.PolicyVersion, SnapshotSchemaVersion: item.SnapshotSchemaVersion,
		RiskClass: item.RiskClass,
		Target:    target,
		Requester: ApprovalV2Requester{UserID: item.RequesterUserID},
		Status:    item.Status, Version: item.Version, SubmittedAt: item.SubmittedAt, ExpiresAt: item.ExpiresAt,
		UpdatedAt: item.UpdatedAt, SupersedesRequestID: item.SupersedesRequestID,
		DetailAvailability: availability,
	}
}

func approvalSafeTargetID(value string) string {
	if len(value) == 0 || len(value) > 160 || strings.TrimSpace(value) != value {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return ""
		}
	}
	return value
}

func approvalV2Command(item ApprovalRequest) any {
	if !approvalPresenterAvailable(item.CommandType, item.PolicyVersion, item.SnapshotSchemaVersion) {
		return nil
	}
	value := approvalSafeTargetID(item.TargetResourceID)
	if value == "" {
		return nil
	}
	switch item.CommandType {
	case "WALLET_ADJUSTMENT":
		if item.TargetResourceType != "MANUAL_ADJUSTMENT" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalWalletSummary{AdjustmentID: id}
	case "PAYMENT_PROVIDER_RESUME":
		if item.TargetResourceType != "PAYMENT_PROVIDER_CONTROL" {
			return nil
		}
		return approvalProviderSummary{ProviderID: value}
	case "PAYMENT_PRODUCT_RESUME":
		if item.TargetResourceType != "PAYMENT_PRODUCT_CONTROL" {
			return nil
		}
		return approvalProductSummary{ProductID: value}
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		if item.TargetResourceType != "ENTITLEMENT_PACKAGE" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalEntitlementCorrectionSummary{PackageID: id}
	case "LEDGER_REPAIR":
		if item.TargetResourceType != "LEDGER_POSTING" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalLedgerRepairSummary{TargetPostingID: id}
	case "RECEIVABLE_WRITE_OFF":
		if item.TargetResourceType != "RECEIVABLE" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalReceivableWriteOffSummary{ReceivableID: id}
	case "PAYMENT_FINANCIAL_REPAIR":
		if item.TargetResourceType != "PAYMENT_ORDER" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalPaymentFinancialRepairSummary{PaymentOrderID: id}
	case "REFUND_EXCEPTION_REPAIR":
		if item.TargetResourceType != "REFUND" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalRefundExceptionRepairSummary{RefundID: id}
	case "CHARGEBACK_FINANCIAL_REPAIR":
		if item.TargetResourceType != "CHARGEBACK_CASE" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalChargebackFinancialRepairSummary{ChargebackCaseID: id}
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		if item.TargetResourceType != "MODEL" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalLifecycleSummary{ModelID: id}
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		if item.TargetResourceType != "MODEL_CATALOG" {
			return nil
		}
		if value[0] < '1' || value[0] > '9' {
			return nil
		}
		if parsed, err := strconv.ParseInt(value, 10, 64); err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
			return nil
		}
		return approvalCatalogSummary{value}
	case "TOKEN_CREDENTIAL_REVOKE":
		if item.TargetResourceType != "PROVIDER_CREDENTIAL" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalCredentialSummary{CredentialID: id}
	}
	return nil
}

func approvalV2Detail(item ApprovalRequest, controlVersion int64) (ApprovalV2Detail, error) {
	if approvalV2Command(item) == nil || controlVersion <= 0 {
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: item.CommandType, PolicyVersion: item.PolicyVersion,
		SnapshotSchemaVersion: item.SnapshotSchemaVersion,
		TargetResourceType:    item.TargetResourceType, TargetResourceID: item.TargetResourceID,
		NormalizedCommand: item.NormalizedCommand, CriticalResourceVersions: item.CriticalResourceVersions,
		FinancialSnapshot: item.FinancialSnapshot,
	})
	if err != nil {
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	var proof struct {
		Normalized struct {
			WalletID               uuid.UUID `json:"wallet_id"`
			AmountMicrocredits     string    `json:"amount_microcredits"`
			ProviderID             string    `json:"provider_id"`
			ProviderControlVersion string    `json:"provider_control_version"`
			ProductControlVersion  string    `json:"product_control_version"`
			TargetLifecycle        string    `json:"target_lifecycle"`
			EffectiveAt            *string   `json:"effective_at"`
			TokenAccountID         uuid.UUID `json:"token_account_id"`
			PackageID              uuid.UUID `json:"package_id"`
			TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
			TargetPostingID        uuid.UUID `json:"target_posting_id"`
			ReceivableID           uuid.UUID `json:"receivable_id"`
			PaymentOrderID         uuid.UUID `json:"payment_order_id"`
			RefundID               uuid.UUID `json:"refund_id"`
			ChargebackCaseID       uuid.UUID `json:"chargeback_case_id"`
			ReasonCode             string    `json:"reason_code"`
		} `json:"normalized_command"`
		Critical struct {
			EntryType             string `json:"entry_type"`
			PostingSetDigest      string `json:"posting_set_digest"`
			FactsDigest           string `json:"facts_digest"`
			PaymentOrderUpdatedAt string `json:"payment_order_updated_at"`
		} `json:"critical_resource_versions"`
		Financial struct {
			SourceType                   string `json:"source_type"`
			SourceReference              string `json:"source_reference"`
			OriginalMicrocredits         string `json:"original_microcredits"`
			SettledMicrocredits          string `json:"settled_microcredits"`
			OutstandingMicrocredits      string `json:"outstanding_microcredits"`
			WriteOffMicrocredits         string `json:"write_off_microcredits"`
			WalletReceivableMicrocredits string `json:"wallet_receivable_microcredits"`
			AmountFen                    string `json:"amount_fen"`
			CreditMicrocredits           string `json:"credit_microcredits"`
			AppliedToReceivables         string `json:"applied_to_receivables"`
			WalletVersion                string `json:"wallet_version"`
			WalletID                     string `json:"wallet_id"`
			PackageID                    string `json:"package_id"`
			RevokedMicrocredits          string `json:"revoked_microcredits"`
			ShortfallMicrocredits        string `json:"shortfall_microcredits"`
			BaseShortfallMicrocredits    string `json:"base_shortfall_microcredits"`
			ComponentDigest              string `json:"component_digest"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(artifacts.ProofInputJSON, &proof) != nil {
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	var command any
	switch target := approvalV2Command(item).(type) {
	case approvalWalletSummary:
		target.WalletID = proof.Normalized.WalletID
		target.AmountMicrocredits = proof.Normalized.AmountMicrocredits
		command = target
	case approvalProviderSummary:
		target.ProviderControlVersion = proof.Normalized.ProviderControlVersion
		command = target
	case approvalProductSummary:
		target.ProviderID = proof.Normalized.ProviderID
		target.ProductControlVersion = proof.Normalized.ProductControlVersion
		command = target
	case approvalLifecycleSummary:
		target.TargetLifecycle = proof.Normalized.TargetLifecycle
		target.EffectiveAt = proof.Normalized.EffectiveAt
		command = target
	case approvalCatalogSummary:
		command = target
	case approvalCredentialSummary:
		target.TokenAccountID = proof.Normalized.TokenAccountID
		command = target
	case approvalEntitlementCorrectionSummary:
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalLedgerRepairSummary:
		if proof.Normalized.TargetJournalEntryID == uuid.Nil || proof.Normalized.TargetPostingID != target.TargetPostingID ||
			proof.Critical.EntryType == "" || !approvalPaginationHexDigest(proof.Critical.PostingSetDigest) {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.TargetJournalEntryID = proof.Normalized.TargetJournalEntryID
		target.EntryType = proof.Critical.EntryType
		target.AmountMicrocredits = proof.Normalized.AmountMicrocredits
		target.PostingSetDigest = proof.Critical.PostingSetDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalReceivableWriteOffSummary:
		if proof.Normalized.ReceivableID != target.ReceivableID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) ||
			proof.Financial.SourceType != "USAGE_OVERAGE" || proof.Financial.SourceReference == "" ||
			proof.Normalized.ReasonCode != "APPROVED_RECEIVABLE_WRITE_OFF" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.SourceType = proof.Financial.SourceType
		target.SourceReference = proof.Financial.SourceReference
		target.OriginalMicrocredits = proof.Financial.OriginalMicrocredits
		target.SettledMicrocredits = proof.Financial.SettledMicrocredits
		target.OutstandingMicrocredits = proof.Financial.OutstandingMicrocredits
		target.WriteOffMicrocredits = proof.Financial.WriteOffMicrocredits
		target.WalletReceivableMicrocredits = proof.Financial.WalletReceivableMicrocredits
		target.FactsDigest = proof.Critical.FactsDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalPaymentFinancialRepairSummary:
		if proof.Normalized.PaymentOrderID != target.PaymentOrderID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) ||
			proof.Critical.PaymentOrderUpdatedAt == "" || proof.Normalized.ReasonCode != "CONFIRMED_PAYMENT_NOT_CREDITED" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.AmountFen = proof.Financial.AmountFen
		target.CreditMicrocredits = proof.Financial.CreditMicrocredits
		target.AppliedToReceivables = proof.Financial.AppliedToReceivables
		target.WalletVersion = proof.Financial.WalletVersion
		target.FactsDigest = proof.Critical.FactsDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalRefundExceptionRepairSummary:
		walletID, walletErr := uuid.Parse(proof.Financial.WalletID)
		packageID, packageErr := uuid.Parse(proof.Financial.PackageID)
		if proof.Normalized.RefundID != target.RefundID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) ||
			walletErr != nil || walletID == uuid.Nil || packageErr != nil || packageID == uuid.Nil ||
			proof.Normalized.ReasonCode != "PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.WalletID = walletID
		target.PackageID = packageID
		target.AmountFen = proof.Financial.AmountFen
		target.CreditMicrocredits = proof.Financial.CreditMicrocredits
		target.RevokedMicrocredits = proof.Financial.RevokedMicrocredits
		target.ShortfallMicrocredits = proof.Financial.ShortfallMicrocredits
		target.FactsDigest = proof.Critical.FactsDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalChargebackFinancialRepairSummary:
		walletID, walletErr := uuid.Parse(proof.Financial.WalletID)
		packageID, packageErr := uuid.Parse(proof.Financial.PackageID)
		if proof.Normalized.ChargebackCaseID != target.ChargebackCaseID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) ||
			walletErr != nil || walletID == uuid.Nil || packageErr != nil || packageID == uuid.Nil ||
			!approvalPaginationHexDigest(proof.Financial.ComponentDigest) ||
			proof.Normalized.ReasonCode != "BASE_SHORTFALL_COMPENSATION" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.WalletID = walletID
		target.PackageID = packageID
		target.BaseShortfallMicrocredits = proof.Financial.BaseShortfallMicrocredits
		target.ComponentDigest = proof.Financial.ComponentDigest
		target.FactsDigest = proof.Critical.FactsDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	default:
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	actions := append([]string{}, item.AllowedActions...)
	return ApprovalV2Detail{ApprovalV2Summary: approvalV2Summary(item), AllowedActions: actions,
		ActionControlVersion: controlVersion, Command: command}, nil
}
