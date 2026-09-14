package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

var (
	ErrApprovalCommandNotExecutable = errors.New("approval command has no executable implementation")
	ErrBillingGovernanceInactive    = errors.New("Billing approval command governance is inactive")
	ErrBillingGovernanceUnavailable = errors.New("Billing approval command governance is unavailable")
)

// Cache one activation observation per read response, not per row. Failure
// closes advancement only; safe closure and historical reads remain available.
func (r *PostgresRepository) approvalAdvancementProjection(ctx context.Context) func(approvalpolicy.Policy) bool {
	tokenObserved, tokenActive := false, false
	billingObserved := false
	var billingSnapshot billinggovernance.Snapshot
	var billingErr error
	return func(policy approvalpolicy.Policy) bool {
		if !approvalpolicy.Executable(policy.CommandType) {
			return false
		}
		if approvalpolicy.TokenExecutableApproval(policy.CommandType) {
			if r.tokenGovernanceActivation == nil {
				return r.tokenApprovalCreateEnabledForTest(policy.CommandType)
			}
			if !tokenObserved {
				tokenActive = r.requireTokenApprovalActive(ctx, policy.CommandType) == nil
				tokenObserved = true
			}
			return tokenActive
		}
		if r.billingGovernanceActivation == nil {
			return false
		}
		if !billingObserved {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			billingSnapshot, billingErr = r.billingGovernanceActivation.Snapshot(readCtx)
			cancel()
			billingObserved = true
		}
		if billingErr != nil || billinggovernance.ValidateSnapshot(billingSnapshot) != nil {
			return false
		}
		activation, ok := billingSnapshot.Find(policy.CommandType, policy.PolicyVersion, policy.SnapshotSchemaVersion)
		return ok && activation.State == "ACTIVE"
	}
}

// Advancement requires an executable implementation even for historical rows
// created before the registry became fail-closed. Safe rejection/withdrawal
// does not call this guard: it grants no authorization to execute the command.
func (r *PostgresRepository) requireApprovalAdvancement(ctx context.Context, policy approvalpolicy.Policy) error {
	if !approvalpolicy.Executable(policy.CommandType) {
		return ErrApprovalCommandNotExecutable
	}
	if approvalpolicy.TokenExecutableApproval(policy.CommandType) {
		return r.requireTokenApprovalActive(ctx, policy.CommandType)
	}
	return r.requireBillingApprovalActive(ctx, policy)
}

func (r *PostgresRepository) WithTokenGovernanceActivation(reader tokengovernance.Reader) *PostgresRepository {
	r.tokenGovernanceActivation = reader
	return r
}

func (r *PostgresRepository) WithBillingGovernanceActivation(reader billinggovernance.Reader) *PostgresRepository {
	r.billingGovernanceActivation = reader
	return r
}

func (r *PostgresRepository) requireBillingApprovalActive(ctx context.Context, policy approvalpolicy.Policy) error {
	if r.billingGovernanceActivation == nil {
		return ErrBillingGovernanceUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot, err := r.billingGovernanceActivation.Snapshot(ctx)
	if err != nil || billinggovernance.ValidateSnapshot(snapshot) != nil {
		return ErrBillingGovernanceUnavailable
	}
	activation, ok := snapshot.Find(policy.CommandType, policy.PolicyVersion, policy.SnapshotSchemaVersion)
	if !ok {
		return ErrBillingGovernanceUnavailable
	}
	if activation.State != "ACTIVE" {
		return ErrBillingGovernanceInactive
	}
	return nil
}

func (r *PostgresRepository) requireTokenApprovalActive(ctx context.Context, command string) error {
	command = strings.ToUpper(strings.TrimSpace(command))
	if !approvalpolicy.TokenExecutableApproval(command) {
		return nil
	}
	if r.tokenGovernanceActivation == nil {
		// Package tests may directly exercise the dormant domain chain. The
		// field has no exported setter and production never configures it.
		if r.tokenApprovalCreateEnabledForTest(command) {
			return nil
		}
		return ErrTokenGovernanceInactive
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot, err := r.tokenGovernanceActivation.Snapshot(ctx)
	if err != nil || snapshot.State != "ACTIVE" || snapshot.Epoch < 1 {
		return ErrTokenGovernanceInactive
	}
	return nil
}
