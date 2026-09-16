package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestApprovalProofSixDecisionsPostgresIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
	data, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Vectors []authorizationProofVector `json:"proof_vectors"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil || len(fixtures.Vectors) != 6 {
		t.Fatalf("expected six golden commands: %v", err)
	}
	for _, vector := range fixtures.Vectors {
		t.Run(vector.CommandType, func(t *testing.T) {
			request := seedAuthorizationProofRequest(t, ctx, db, vector)
			approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "proof-six-"+uuid.NewString())
			repository := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool, now: func() time.Time { return time.Now().UTC() }}).
				WithTokenGovernanceActivation(&advancementActivationReader{state: "ACTIVE"}).
				WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
			calls := 0
			repository.approvalProofSource = approvalProofSourceFunc(func(_ context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
				calls++
				if artifacts.CommandType != vector.CommandType || artifacts.CommandDigest != vector.CommandDigest || artifacts.SnapshotDigest != vector.SnapshotDigest ||
					string(artifacts.ProofInputJSON) != vector.ProofJSON {
					t.Error("proof source received non-golden frozen request")
				}
				issued := time.Now().UTC()
				response := approvalencoding.ProofResponse{
					ApprovalRequestID: id, CommandType: artifacts.CommandType, PolicyVersion: artifacts.PolicyVersion,
					CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
					ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "test-revision-1",
					Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: issued, ExpiresAt: issued.Add(time.Minute),
				}
				digest, err := approvalencoding.DeriveProofDigest(response)
				return ApprovalProofResult{Response: response, Digest: hex.EncodeToString(digest[:])}, err
			})
			approved, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version,
				"APPROVED", "independent proof approval", nil)
			if err != nil || approved.Status != "APPROVED" || calls != 1 {
				t.Fatalf("proof decision %s: %v status=%s calls=%d", vector.CommandType, err, approved.Status, calls)
			}
			var wire string
			if err := db.adminPool.QueryRow(ctx, `SELECT s.serialization_version
			FROM account_approval_authorization.approval_authorization_event_bytes s
			JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1`, request.ID).Scan(&wire); err != nil || wire != approvalencoding.ApprovalSerializationVersion {
				t.Fatalf("proof wire %s: %s %v", vector.CommandType, wire, err)
			}
		})
	}
}
