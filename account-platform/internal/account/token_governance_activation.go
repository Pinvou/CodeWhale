package account

import (
	"context"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

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
