package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

type approvalProofSourceFunc func(context.Context, uuid.UUID, approvalencoding.ProofArtifacts) (ApprovalProofResult, error)

func (fn approvalProofSourceFunc) Proof(ctx context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
	return fn(ctx, id, artifacts)
}

func TestApprovalProofDecisionPostgresIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "proof-decision-"+uuid.NewString())
	for _, statement := range []string{
		`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now()`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()`,
		`UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1,updated_at=now()`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()`,
	} {
		if _, err := db.adminPool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	repository := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool, now: func() time.Time { return time.Now().UTC() }}).
		WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
	principal := approvalIntegrationPrincipal(approver)
	if _, err := repository.DecideApprovalRequest(ctx, principal, request.ID, request.Version, "APPROVED", "independent proof", nil); !errors.Is(err, ErrApprovalProofUnavailable) {
		t.Fatalf("unconfigured domain proof must fail closed: %v", err)
	}
	calls := 0
	deny, fence := false, false
	repository.approvalProofSource = approvalProofSourceFunc(func(_ context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
		calls++
		if fence {
			if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()
WHERE control_id='ADMIN_APPROVAL'`); err != nil {
			return ApprovalProofResult{}, err
			}
		}
		issued := time.Now().UTC()
		response := approvalencoding.ProofResponse{
			ApprovalRequestID: id, CommandType: artifacts.CommandType, PolicyVersion: artifacts.PolicyVersion,
			CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
			ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "test-domain-revision-1",
			Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: issued, ExpiresAt: issued.Add(time.Minute),
		}
		if deny {
			response.Verdict, response.ReasonCode = "DENY", "RESOURCE_STATE_CHANGED"
		}
		digest, err := approvalencoding.DeriveProofDigest(response)
		if err != nil {
			return ApprovalProofResult{}, err
		}
		result := ApprovalProofResult{Response: response, Digest: hex.EncodeToString(digest[:])}
		if deny {
			return result, ErrApprovalProofRejected
		}
		return result, nil
	})
	assertNoAuthorization := func(label string) {
		t.Helper()
		var status string
		var decisions, outboxRows int
		if err := db.adminPool.QueryRow(ctx, `SELECT status,
		 (SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
		 (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1)
		 FROM approval_requests WHERE id=$1`, request.ID).Scan(&status, &decisions, &outboxRows); err != nil || status != "SUBMITTED" || decisions != 0 || outboxRows != 0 {
			t.Fatalf("%s leaked approval: %s decisions=%d outbox=%d error=%v", label, status, decisions, outboxRows, err)
		}
	}
	deny = true
	if _, err := repository.DecideApprovalRequest(ctx, principal, request.ID, request.Version, "APPROVED", "independent proof", nil); !errors.Is(err, ErrApprovalProofRejected) {
		t.Fatalf("domain DENY must be authoritative: %v", err)
	}
	assertNoAuthorization("DENY")
	deny, fence = false, true
	if _, err := repository.DecideApprovalRequest(ctx, principal, request.ID, request.Version, "APPROVED", "independent proof", nil); !errors.Is(err, ErrApprovalRelayFenced) {
		t.Fatalf("fence racing with proof must discard it: %v", err)
	}
	assertNoAuthorization("FENCE")
	fence = false
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()
WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatal(err)
	}
	// v1 accepts the legacy transport field, but it is not an authorization
	// input once the singleton is proof-required.
	approved, err := repository.DecideApprovalRequest(ctx, principal, request.ID, request.Version, "APPROVED", "independent proof", json.RawMessage(`{"forged":999}`))
	if err != nil || approved.Status != "APPROVED" || calls != 3 {
		t.Fatalf("proof decision failed or used observed versions: %v status=%s calls=%d", err, approved.Status, calls)
	}
	var serialization string
	var decisions, outboxRows, sidecars int
	if err := db.adminPool.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
	 (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),
	 (SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes s JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1),
	 (SELECT serialization_version FROM account_approval_authorization.approval_authorization_event_bytes s JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1)`, request.ID).
		Scan(&decisions, &outboxRows, &sidecars, &serialization); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || outboxRows != 1 || sidecars != 1 || serialization != approvalencoding.ApprovalSerializationVersion {
		t.Fatalf("proof authorization facts not atomic: decisions=%d outbox=%d sidecars=%d wire=%s", decisions, outboxRows, sidecars, serialization)
	}
}
