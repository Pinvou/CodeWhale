package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

var ErrApprovalCommandNotExecutable = errors.New("approval command has no executable implementation")

// Cache one activation observation per read response, not per row. Failure
// closes advancement only; safe closure and historical reads remain available.
func (r *PostgresRepository) approvalAdvancementProjection(ctx context.Context) func(string) bool {
	observed, active := false, false
	return func(command string) bool {
		if !approvalpolicy.Executable(command) {
			return false
		}
		if !approvalpolicy.TokenExecutableApproval(command) {
			return true
		}
		if r.tokenGovernanceActivation == nil {
			return r.tokenApprovalCreateEnabledForTest(command)
		}
		if !observed {
			active = r.requireTokenApprovalActive(ctx, command) == nil
			observed = true
		}
		return active
	}
}

// Advancement requires an executable implementation even for historical rows
// created before the registry became fail-closed. Safe rejection/withdrawal
// does not call this guard: it grants no authorization to execute the command.
func (r *PostgresRepository) requireApprovalAdvancement(ctx context.Context, command string) error {
	if !approvalpolicy.Executable(command) {
		return ErrApprovalCommandNotExecutable
	}
	return r.requireTokenApprovalActive(ctx, command)
}

func (r *PostgresRepository) WithTokenGovernanceActivation(reader tokengovernance.Reader) *PostgresRepository {
	r.tokenGovernanceActivation = reader
	return r
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
