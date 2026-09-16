package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// approvalProofCheckpoint is a read-side observation, never an authorization
// to commit. The final writer must recheck every mutable fact under its own
// control/request/authorization locks after the domain call.
type approvalProofCheckpoint struct {
	Request              ApprovalRequest
	ActorID              uuid.UUID
	BindingID            uuid.UUID
	AuthorizationVersion int64
	Control              AdminApprovalControl
	Artifacts            approvalencoding.ProofArtifacts
}

func (r *PostgresRepository) readApprovalProofCheckpoint(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion int64) (approvalProofCheckpoint, error) {
	if !humanApprovalPrincipal(principal) || requestID == uuid.Nil || expectedVersion < 1 {
		return approvalProofCheckpoint{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return approvalProofCheckpoint{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		if err != nil && !errors.Is(err, ErrForbidden) {
			return approvalProofCheckpoint{}, err
		}
		return approvalProofCheckpoint{}, ErrForbidden
	}
	// The narrow visibility query precedes all command/snapshot/control reads.
	if _, err := approvalRequestVisibilityForActor(ctx, tx, *actorID, requestID); err != nil {
		return approvalProofCheckpoint{}, err
	}
	item, err := approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return approvalProofCheckpoint{}, approvalVisibleRequestReadError(err)
	}
	if item.Status != "SUBMITTED" || item.Version != expectedVersion {
		return approvalProofCheckpoint{}, ErrConflict
	}
	if item.RequesterUserID == *actorID {
		return approvalProofCheckpoint{}, ErrApprovalSelfDecision
	}
	if !item.ExpiresAt.After(r.now()) {
		return approvalProofCheckpoint{}, ErrApprovalExpired
	}
	policy, err := approvalPolicyForItem(item)
	if err != nil {
		return approvalProofCheckpoint{}, ErrApprovalInvalidated
	}
	if err := assertStoredApprovalPolicy(ctx, tx, policy); err != nil {
		return approvalProofCheckpoint{}, ErrApprovalInvalidated
	}
	bindingID, authorizationVersion, _, err := strongPlatformAuthorization(ctx, tx, *actorID, policy.ApproverPermission)
	if errors.Is(err, ErrForbidden) {
		return approvalProofCheckpoint{}, ErrApprovalRequestNotFound
	}
	if err != nil {
		return approvalProofCheckpoint{}, err
	}
	if approvalProofNeedsRecentAuthentication(item.CommandType) && !recentlyAuthenticated(principal.AuthenticatedAt, r.now()) {
		return approvalProofCheckpoint{}, ErrRecentAuthenticationRequired
	}
	control, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return approvalProofCheckpoint{}, err
	}
	if control.Mode != AdminApprovalModeProofRequired || !control.ProofRequired || control.ApprovalWireVersion != 2 || control.AuthorizationRelayState != AdminApprovalRelayRunning {
		return approvalProofCheckpoint{}, ErrApprovalAuthorizationControlChanged
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, SnapshotSchemaVersion: item.SnapshotSchemaVersion,
		TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID,
		NormalizedCommand: item.NormalizedCommand, CriticalResourceVersions: item.CriticalResourceVersions,
		FinancialSnapshot: item.FinancialSnapshot,
	})
	if err != nil {
		return approvalProofCheckpoint{}, ErrApprovalProofInvalid
	}
	if err := tx.Commit(ctx); err != nil {
		return approvalProofCheckpoint{}, err
	}
	return approvalProofCheckpoint{Request: item, ActorID: *actorID, BindingID: bindingID, AuthorizationVersion: authorizationVersion, Control: control, Artifacts: artifacts}, nil
}

func approvalProofNeedsRecentAuthentication(command string) bool {
	switch command {
	case "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
		return true
	default:
		return false
	}
}
