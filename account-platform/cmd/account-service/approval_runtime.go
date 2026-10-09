package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

// The three Billing and three Token approval commands are installed. Missing independent
// writer availability closes APPROVE, while historical reads/rejection and
// ordinary account operations remain available through the mature pool.
func configureApprovals(ctx context.Context, repository *account.PostgresRepository, service *account.Service, credential, environment string) error {
	if err := configureApprovalPagination(ctx, repository); err != nil {
		return errors.New("ACCOUNT_APPROVAL_PAGINATION_CONFIGURATION_INVALID")
	}
	_ = repository.ConfigureApprovalWriter(ctx, os.Getenv("APPROVAL_WRITER_DATABASE_URL"))
	var err error
	billingURL := os.Getenv("BILLING_HTTP_URL")
	activation, err := billinggovernance.NewClient(billingURL, credential)
	if err != nil {
		return errors.New("ACCOUNT_BILLING_ACTIVATION_CONFIGURATION_INVALID")
	}
	repository.WithBillingGovernanceActivation(activation)
	tokenURL := os.Getenv("TOKEN_HTTP_URL")
	tokenActivation, err := tokengovernance.NewClient(tokenURL, credential, os.Getenv("TOKEN_GOVERNANCE_POLICY_SET_DIGEST"), os.Getenv("TOKEN_GOVERNANCE_HANDLER_SET_DIGEST"))
	if err != nil {
		return errors.New("ACCOUNT_TOKEN_ACTIVATION_CONFIGURATION_INVALID")
	}
	repository.WithTokenGovernanceActivation(tokenActivation)
	snapshots, err := account.NewHTTPApprovalSnapshotSource(tokenURL, credential)
	if err != nil {
		return errors.New("ACCOUNT_APPROVAL_SNAPSHOT_CONFIGURATION_INVALID")
	}
	repository.WithApprovalSnapshotSource(snapshots)
	proofs, err := account.NewHTTPApprovalProofSourceWithToken(billingURL, tokenURL, credential)
	if err != nil {
		return errors.New("ACCOUNT_APPROVAL_PROOF_CONFIGURATION_INVALID")
	}
	repository.WithApprovalProofSource(proofs)
	results, err := account.NewApprovalRecoveryResultClientWithToken(billingURL, tokenURL, credential)
	if err != nil {
		return errors.New("ACCOUNT_APPROVAL_RECOVERY_CONFIGURATION_INVALID")
	}
	effects, err := approvaleffect.NewClientWithToken(billingURL, tokenURL, credential)
	if err != nil {
		return errors.New("ACCOUNT_APPROVAL_EFFECT_CONFIGURATION_INVALID")
	}
	key, err := humandelegation.DecodeSigningKey(os.Getenv("HUMAN_DELEGATION_SIGNING_KEY_BASE64URL"))
	if err != nil {
		return errors.New("ACCOUNT_DELEGATION_KEY_INVALID")
	}
	signer, err := humandelegation.NewSigner(os.Getenv("HUMAN_DELEGATION_SIGNING_KEY_ID"), key)
	clear(key)
	if err != nil {
		return errors.New("ACCOUNT_DELEGATION_CONFIGURATION_INVALID")
	}
	service.WithHumanDelegations(signer).WithApprovalRecoveryResults(results).WithApprovalRecoveryEffects(effects)
	schedule, err := manualreview.ParseOnCallSchedule(os.Getenv("MANUAL_REVIEW_ON_CALL_USER_IDS"), strings.HasPrefix(strings.ToLower(environment), "production"))
	if err != nil {
		return errors.New("ACCOUNT_MANUAL_REVIEW_SCHEDULE_INVALID")
	}
	go observability.RunBackground(ctx, "approval-expiry", account.NewApprovalExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "approval-pagination-cleanup", account.NewApprovalPaginationCleanupWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "manual-review-sla", account.NewManualReviewSLAWorker(repository, schedule, time.Minute).Run)
	return nil
}
