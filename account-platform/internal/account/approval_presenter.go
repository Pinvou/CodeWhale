package account

import (
	"errors"
	"strconv"
	"strings"
	"time"

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
	AllowedActions       []string `json:"allowed_actions"`
	ActionControlVersion int64    `json:"action_control_version"`
	Command              any      `json:"command"`
}

// Each variant has a distinct closed JSON shape. Never pass arbitrary
// normalized_command, financial_snapshot or evidence to a presenter.
type approvalWalletSummary struct {
	AdjustmentID uuid.UUID `json:"adjustment_id"`
}
type approvalProviderSummary struct {
	ProviderID string `json:"provider_id"`
}
type approvalProductSummary struct {
	ProductID string `json:"product_id"`
}
type approvalLifecycleSummary struct {
	ModelID uuid.UUID `json:"model_id"`
}
type approvalCatalogSummary struct {
	SourceCatalogVersion string `json:"source_catalog_version"`
}
type approvalCredentialSummary struct {
	CredentialID uuid.UUID `json:"credential_id"`
}

func approvalPresenterAvailable(command string, policyVersion int64, snapshotVersion int) bool {
	switch command {
	case "WALLET_ADJUSTMENT", "PAYMENT_PRODUCT_RESUME", "TOKEN_CREDENTIAL_REVOKE":
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
	return ApprovalV2Summary{
		ApprovalRequestID: item.ID, CommandType: item.CommandType,
		PolicyVersion: item.PolicyVersion, SnapshotSchemaVersion: item.SnapshotSchemaVersion,
		RiskClass: item.RiskClass,
		Target:    ApprovalV2Target{ResourceType: item.TargetResourceType, ResourceID: approvalSafeTargetID(item.TargetResourceID)},
		Requester: ApprovalV2Requester{UserID: item.RequesterUserID},
		Status:    item.Status, SubmittedAt: item.SubmittedAt, ExpiresAt: item.ExpiresAt,
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
		return approvalWalletSummary{id}
	case "PAYMENT_PROVIDER_RESUME":
		if item.TargetResourceType != "PAYMENT_PROVIDER_CONTROL" {
			return nil
		}
		return approvalProviderSummary{value}
	case "PAYMENT_PRODUCT_RESUME":
		if item.TargetResourceType != "PAYMENT_PRODUCT_CONTROL" {
			return nil
		}
		return approvalProductSummary{value}
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		if item.TargetResourceType != "MODEL" {
			return nil
		}
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return nil
		}
		return approvalLifecycleSummary{id}
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
		return approvalCredentialSummary{id}
	}
	return nil
}

func approvalV2Detail(item ApprovalRequest, controlVersion int64) (ApprovalV2Detail, error) {
	command := approvalV2Command(item)
	if command == nil || controlVersion <= 0 {
		return ApprovalV2Detail{}, ErrApprovalDetailPresentationUnavailable
	}
	actions := append([]string{}, item.AllowedActions...)
	return ApprovalV2Detail{ApprovalV2Summary: approvalV2Summary(item), AllowedActions: actions,
		ActionControlVersion: controlVersion, Command: command}, nil
}
