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

type approvalEntitlementCorrectionSummary struct {
	PackageID      uuid.UUID `json:"package_id"`
	PaymentOrderID uuid.UUID `json:"payment_order_id"`
	ReasonCode     string    `json:"reason_code"`
}

type approvalLifecycleSummary struct {
	ModelID                 uuid.UUID  `json:"model_id"`
	TargetLifecycle         string     `json:"target_lifecycle"`
	EffectiveAt             *string    `json:"effective_at"`
	LifecyclePlanID         *uuid.UUID `json:"lifecycle_plan_id,omitempty"`
	LifecyclePlanVersion    *string    `json:"lifecycle_plan_version,omitempty"`
	ExistingSessionCutoffAt *string    `json:"existing_session_cutoff_at"`
	ReplacementModelID      *uuid.UUID `json:"replacement_model_id"`
}
type approvalCatalogSummary struct {
	SourceCatalogVersion string `json:"source_catalog_version"`
}
type approvalCredentialSummary struct {
	CredentialID           uuid.UUID `json:"credential_id"`
	TokenAccountID         uuid.UUID `json:"token_account_id"`
	ExpectedAccountVersion string    `json:"expected_account_version"`
	CredentialVersion      string    `json:"credential_version"`
}

type approvalLedgerRepairSummary struct {
	TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
	TargetPostingID        uuid.UUID `json:"target_posting_id"`
	WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
	CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
	EntryType              string    `json:"entry_type"`
	AmountMicrocredits     string    `json:"amount_microcredits"`
	PostingSetDigest       string    `json:"posting_set_digest"`
	ReasonCode             string    `json:"reason_code"`
}

func approvalPresenterAvailable(command string, policyVersion int64, snapshotVersion int) bool {
	switch command {
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		return policyVersion == 2 && snapshotVersion == 2
	case "PAYMENT_FINANCIAL_REPAIR", "RECEIVABLE_WRITE_OFF", "LEDGER_REPAIR", "WALLET_ADJUSTMENT", "PAYMENT_PRODUCT_RESUME", "TOKEN_CREDENTIAL_REVOKE":
		return policyVersion == 1 && snapshotVersion == 1
	case "PAYMENT_PROVIDER_RESUME":
		return policyVersion == 2 && snapshotVersion == 1
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
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value || item.TargetResourceType != "ENTITLEMENT_PACKAGE" {
			return nil
		}
		return approvalEntitlementCorrectionSummary{PackageID: id}
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

	case "LEDGER_REPAIR":
		if item.TargetResourceType != "LEDGER_POSTING" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return nil
		}
		return approvalLedgerRepairSummary{TargetPostingID: id}
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
	case "RECEIVABLE_WRITE_OFF":
		if item.TargetResourceType != "RECEIVABLE" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return nil
		}
		return approvalReceivableWriteOffSummary{ReceivableID: id}
	case "PAYMENT_FINANCIAL_REPAIR":
		if item.TargetResourceType != "PAYMENT_ORDER" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return nil
		}
		return approvalPaymentFinancialRepairSummary{PaymentOrderID: id}
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
			ReceivableID              uuid.UUID  `json:"receivable_id"`
			ExpectedReceivableVersion string     `json:"expected_receivable_version"`
			WriteOffMicrocredits      string     `json:"write_off_microcredits"`
			PaymentOrderID            uuid.UUID  `json:"payment_order_id"`
			ReasonCode                string     `json:"reason_code"`
			WalletID                  uuid.UUID  `json:"wallet_id"`
			AmountMicrocredits        string     `json:"amount_microcredits"`
			ProviderID                string     `json:"provider_id"`
			ProviderControlVersion    string     `json:"provider_control_version"`
			ProductControlVersion     string     `json:"product_control_version"`
			ModelID                   uuid.UUID  `json:"model_id"`
			TargetLifecycle           string     `json:"target_lifecycle"`
			EffectiveAt               *string    `json:"effective_at"`
			LifecyclePlanID           *uuid.UUID `json:"lifecycle_plan_id"`
			LifecyclePlanVersion      *string    `json:"lifecycle_plan_version"`
			ExistingSessionCutoffAt   *string    `json:"existing_session_cutoff_at"`
			ReplacementModelID        *uuid.UUID `json:"replacement_model_id"`
			TokenAccountID            uuid.UUID  `json:"token_account_id"`
			CredentialID              uuid.UUID  `json:"credential_id"`
			ExpectedAccountVersion    string     `json:"expected_account_version"`
			CredentialVersion         string     `json:"credential_version"`
			SourceCatalogVersion      string     `json:"source_catalog_version"`
			TargetJournalEntryID      uuid.UUID  `json:"target_journal_entry_id"`
			TargetPostingID           uuid.UUID  `json:"target_posting_id"`
			WrongLedgerAccountID      uuid.UUID  `json:"wrong_ledger_account_id"`
			CorrectLedgerAccountID    uuid.UUID  `json:"correct_ledger_account_id"`
			PackageID                 uuid.UUID  `json:"package_id"`
		} `json:"normalized_command"`
		Critical struct {
			FactsDigest      string `json:"facts_digest"`
			EntryType        string `json:"entry_type"`
			PostingSetDigest string `json:"posting_set_digest"`
		} `json:"critical_resource_versions"`
		Financial struct {
			WriteOffMicrocredits         string `json:"write_off_microcredits"`
			SourceType                   string `json:"source_type"`
			SourceReference              string `json:"source_reference"`
			OriginalMicrocredits         string `json:"original_microcredits"`
			SettledMicrocredits          string `json:"settled_microcredits"`
			OutstandingMicrocredits      string `json:"outstanding_microcredits"`
			WalletReceivableMicrocredits string `json:"wallet_receivable_microcredits"`
		} `json:"financial_snapshot"`
	}
	if json.Unmarshal(artifacts.ProofInputJSON, &proof) != nil {
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	var command any
	switch target := approvalV2Command(item).(type) {
	case approvalEntitlementCorrectionSummary:
		if proof.Normalized.PackageID != target.PackageID || proof.Normalized.PaymentOrderID == uuid.Nil {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.PaymentOrderID, target.ReasonCode = proof.Normalized.PaymentOrderID, proof.Normalized.ReasonCode
		command = target
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

	case approvalLedgerRepairSummary:
		if proof.Normalized.TargetJournalEntryID == uuid.Nil || proof.Normalized.TargetPostingID != target.TargetPostingID || proof.Normalized.WrongLedgerAccountID == uuid.Nil || proof.Normalized.CorrectLedgerAccountID == uuid.Nil || proof.Normalized.WrongLedgerAccountID == proof.Normalized.CorrectLedgerAccountID || proof.Critical.EntryType == "" || !approvalPaginationHexDigest(proof.Critical.PostingSetDigest) || proof.Normalized.ReasonCode != "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.TargetJournalEntryID = proof.Normalized.TargetJournalEntryID
		target.WrongLedgerAccountID = proof.Normalized.WrongLedgerAccountID
		target.CorrectLedgerAccountID = proof.Normalized.CorrectLedgerAccountID
		target.EntryType = proof.Critical.EntryType
		target.AmountMicrocredits = proof.Normalized.AmountMicrocredits
		target.PostingSetDigest = proof.Critical.PostingSetDigest
		target.ReasonCode = proof.Normalized.ReasonCode
		command = target
	case approvalLifecycleSummary:
		if proof.Normalized.ModelID != target.ModelID {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.TargetLifecycle = proof.Normalized.TargetLifecycle
		target.EffectiveAt = proof.Normalized.EffectiveAt
		target.LifecyclePlanID = proof.Normalized.LifecyclePlanID
		target.LifecyclePlanVersion = proof.Normalized.LifecyclePlanVersion
		target.ExistingSessionCutoffAt = proof.Normalized.ExistingSessionCutoffAt
		target.ReplacementModelID = proof.Normalized.ReplacementModelID
		command = target
	case approvalCatalogSummary:
		if proof.Normalized.SourceCatalogVersion != target.SourceCatalogVersion {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		command = target
	case approvalCredentialSummary:
		if proof.Normalized.CredentialID != target.CredentialID || proof.Normalized.TokenAccountID == uuid.Nil {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.TokenAccountID = proof.Normalized.TokenAccountID
		target.ExpectedAccountVersion = proof.Normalized.ExpectedAccountVersion
		target.CredentialVersion = proof.Normalized.CredentialVersion
		command = target

	case approvalReceivableWriteOffSummary:
		if proof.Normalized.ReceivableID != target.ReceivableID || !approvalPaginationHexDigest(proof.Critical.FactsDigest) ||
			proof.Financial.SourceType != "USAGE_OVERAGE" || proof.Financial.SourceReference == "" ||
			proof.Normalized.ReasonCode != "APPROVED_RECEIVABLE_WRITE_OFF" {
			return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
		}
		target.ExpectedReceivableVersion = proof.Normalized.ExpectedReceivableVersion
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
		selected, e := approvalPaymentFinancialDetail(target, artifacts.ProofInputJSON)
		if e != nil {
			return ApprovalV2Detail{}, e
		}
		command = selected
	}
	actions := append([]string{}, item.AllowedActions...)
	return ApprovalV2Detail{ApprovalV2Summary: approvalV2Summary(item), AllowedActions: actions,
		ActionControlVersion: controlVersion, Command: command}, nil
}
