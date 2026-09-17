package billing

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// billingTxProofFacts reads the entire owner revision from the same
// serializable snapshot in which the immutable attempt claim is inserted.
type billingTxProofFacts struct {
	repository *PostgresRepository
	tx         pgx.Tx
}

// A persisted claim is evidence of a possibly-started effect, not evidence
// that no effect occurred. The result-first consumer must keep it UNKNOWN.
func (r *PostgresRepository) ExistingBillingApprovalProofClaim(ctx context.Context, binding ApprovalAuthorizationBinding) (bool, error) {
	var eventID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT authorization_event_id FROM billing_approval_proof_claims WHERE approval_request_id=$1 OR authorization_event_id=$2`, binding.ApprovalRequestID, binding.EventID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (f billingTxProofFacts) AdjustmentForExecution(ctx context.Context, id uuid.UUID) (ManualAdjustment, error) {
	return getAdjustment(ctx, f.tx, id, false)
}

func (f billingTxProofFacts) GetWallet(ctx context.Context, id uuid.UUID) (Wallet, error) {
	return getWallet(ctx, f.tx, id, false)
}

func (f billingTxProofFacts) providerResumeVersions(ctx context.Context, requested ProviderResumeVersions) (ProviderResumeVersions, error) {
	return f.repository.providerResumeVersionsFrom(ctx, f.tx, requested)
}

func (f billingTxProofFacts) productResumeVersions(ctx context.Context, requested ProductResumeVersions) (ProductResumeVersions, error) {
	return f.repository.productResumeVersionsFrom(ctx, f.tx, requested)
}

func (f billingTxProofFacts) confirmedResumeProbe(ctx context.Context, providerID, providerVersion, productID string, orderID uuid.UUID) (bool, error) {
	return f.repository.confirmedResumeProbeFrom(ctx, f.tx, providerID, providerVersion, productID, orderID)
}

func (f billingTxProofFacts) entitlementCorrectionPlan(ctx context.Context, packageID, orderID uuid.UUID) (entitlementCorrectionReadPlan, error) {
	return entitlementCorrectionPlanFrom(ctx, f.tx, f.repository.now(), packageID, orderID)
}

// ClaimBillingApprovalProof is the last zero-effect authorization boundary.
// Once committed, a crash or provider failure is an unknown outcome, never
// permission to run a second probe under a newly issued proof.
func (r *PostgresRepository) ClaimBillingApprovalProof(ctx context.Context, binding ApprovalAuthorizationBinding, event approvalencoding.AuthorizationEvent) error {
	if binding.ApprovalRequestID == uuid.Nil || binding.ApprovalRequestID != event.ApprovalRequestID || binding.EventID == uuid.Nil || binding.EventID != event.EventID ||
		binding.CommandType != event.CommandType || binding.PolicyVersion != event.PolicyVersion ||
		binding.ApprovalEpoch <= 0 || binding.ApprovalEpoch != event.ApprovalControlEpoch {
		return ErrApprovalSnapshotMismatch
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: 1,
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: event.NormalizedCommand, CriticalResourceVersions: event.CriticalResourceVersions,
		FinancialSnapshot: event.FinancialSnapshot,
	})
	if err != nil || artifacts.CommandDigest != event.CommandDigest || artifacts.SnapshotDigest != event.SnapshotDigest {
		return ErrApprovalSnapshotMismatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// An existing claim means that an earlier attempt may already have probed.
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `SELECT authorization_event_id FROM billing_approval_proof_claims WHERE approval_request_id=$1 OR authorization_event_id=$2`, binding.ApprovalRequestID, binding.EventID).Scan(&existing)
	if err == nil {
		return ErrApprovalExecutionUnavailable
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var active ApprovalCommandActivation
	err = tx.QueryRow(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activations WHERE command_type=$1 AND policy_version=$2 FOR SHARE`, event.CommandType, event.PolicyVersion).
		Scan(&active.CommandType, &active.PolicyVersion, &active.SnapshotSchemaVersion, &active.State, &active.Version, &active.UpdatedAt)
	if err != nil {
		return err
	}
	if active.State != "ACTIVE" || active.Version < 1 || active.SnapshotSchemaVersion != 1 || active.UpdatedAt.IsZero() || active.UpdatedAt.After(r.now().Add(time.Second)) {
		return ErrApprovalSnapshotMismatch
	}
	frozen, err := approvalencoding.LegacyProofInputFromArtifacts(artifacts)
	if err != nil {
		return ErrApprovalSnapshotMismatch
	}
	current, reason, ownerState, err := currentBillingProofInput(ctx, billingTxProofFacts{repository: r, tx: tx}, frozen)
	if err != nil {
		return err
	}
	if reason != "" || !sameRawJSON(current.CriticalResourceVersions, event.CriticalResourceVersions) ||
		!sameRawJSON(current.FinancialSnapshot, event.FinancialSnapshot) ||
		billingProofRevision(event.CommandType, active.Version, current, ownerState) != event.DomainRevision {
		return ErrApprovalSnapshotMismatch
	}
	fresh, err := approvalencoding.ConvertLegacyProofInput(current)
	if err != nil || fresh.CommandDigest != event.CommandDigest || fresh.SnapshotDigest != event.SnapshotDigest {
		return ErrApprovalSnapshotMismatch
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_approval_proof_claims (approval_request_id,authorization_event_id,command_type,command_digest,snapshot_digest,domain_revision,approval_epoch) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		binding.ApprovalRequestID, binding.EventID, event.CommandType, event.CommandDigest, event.SnapshotDigest, event.DomainRevision, binding.ApprovalEpoch)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
