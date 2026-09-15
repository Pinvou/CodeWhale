package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
)

func TestApprovalConsumerSerializationRejectsLegacyOnDedicatedBillingIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	state := observeApprovalDelivery(t, ctx, repository, binding, 301)

	if _, err := repository.ResolveApprovalAuthorizationBinding(ctx, binding.EventID,
		"billing-wallet-adjustment-executor-p1-v1", binding.Workload); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("dedicated Billing durable resolved legacy wire: %v", err)
	}
	if _, err := db.adminPool.Exec(ctx, `INSERT INTO account_approval_authorization.approval_delivery_states(
schema_version,original_event_id,durable,approval_request_id,approved_request_version,event_digest,digest_kind,
serialization_version,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,state)
SELECT schema_version,original_event_id,'billing-wallet-adjustment-executor-p1-v1',approval_request_id,
approved_request_version,event_digest,digest_kind,serialization_version,provenance,approval_epoch,stream,
stream_sequence,command_type,policy_version,executor_identity,state
FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2`,
		binding.EventID, binding.Durable); postgresSQLState(err) != "23514" {
		t.Fatalf("protected state accepted legacy wire for dedicated durable: state=%s err=%v", postgresSQLState(err), err)
	}

	state, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, binding.ApprovalRequestID)
	wrong := binding
	wrong.SerializationVersion = approvalencoding.ApprovalSerializationVersion
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload,
		"wrong-legacy-wire", critical, financial, wrong); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("Begin accepted approval wire on legacy durable: %v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 0)
	if state.State != "DOMAIN_INFLIGHT" {
		t.Fatalf("unexpected state after attempt grant: %+v", state)
	}
}

func TestApprovalConsumerSerializationRejectsApprovalOnLegacyBillingIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	setApprovalConsumerControl(t, ctx, db, "PROOF_REQUIRED", "FENCED", 2, true, 3)
	setApprovalConsumerRelay(t, ctx, db, "RUNNING")
	binding := publishedProofApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	state := observeApprovalDelivery(t, ctx, repository, binding, 302)

	if _, err := repository.ResolveApprovalAuthorizationBinding(ctx, binding.EventID,
		approvalconsumer.LegacyBilling().Durable, binding.Workload); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("legacy Billing durable resolved approval wire: %v", err)
	}
	state, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, binding.ApprovalRequestID)
	wrong := binding
	wrong.SerializationVersion = approvalencoding.LegacySerializationVersion
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload,
		"wrong-approval-wire", critical, financial, wrong); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("Begin accepted legacy wire on dedicated durable: %v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 0)
	if state.State != "DOMAIN_INFLIGHT" {
		t.Fatalf("unexpected state after attempt grant: %+v", state)
	}
}
