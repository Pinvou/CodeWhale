package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalProofCheckpointPostgresIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "proof-checkpoint-"+uuid.NewString())
	repository := &PostgresRepository{pool: db.runtimePool, now: func() time.Time { return time.Now().UTC() }}
	principal := approvalIntegrationPrincipal(approver)
	if _, err := repository.readApprovalProofCheckpoint(ctx, principal, uuid.New(), request.Version); !errors.Is(err, ErrApprovalRequestNotFound) {
		t.Fatalf("invisible request must precede snapshot/control: %v", err)
	}
	if _, err := repository.readApprovalProofCheckpoint(ctx, principal, request.ID, request.Version); !errors.Is(err, ErrApprovalAuthorizationControlChanged) {
		t.Fatalf("legacy gate must not issue a proof: %v", err)
	}
	for _, mode := range []string{"QUIESCING", "PROOF_REQUIRED"} {
		if mode == "PROOF_REQUIRED" {
			if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()
WHERE control_id='ADMIN_APPROVAL'`); err != nil {
				t.Fatalf("fence proof cutover: %v", err)
			}
		}
		if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET mode=$1,approval_wire_version=CASE WHEN $1='PROOF_REQUIRED' THEN 2 ELSE 1 END,
proof_required=($1='PROOF_REQUIRED'),approval_epoch=approval_epoch+1,version=version+1,updated_at=now()
WHERE control_id='ADMIN_APPROVAL'`, mode); err != nil {
			t.Fatalf("transition to %s: %v", mode, err)
		}
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now()
WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatalf("resume proof relay: %v", err)
	}
	checkpoint, err := repository.readApprovalProofCheckpoint(ctx, principal, request.ID, request.Version)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Request.ID != request.ID || checkpoint.ActorID != approver.ID || checkpoint.AuthorizationVersion < 1 ||
		checkpoint.Control.Mode != AdminApprovalModeProofRequired || checkpoint.Artifacts.CommandDigest == "" || checkpoint.Artifacts.SnapshotDigest == "" {
		t.Fatalf("incomplete proof checkpoint: %+v", checkpoint)
	}
	if _, err := repository.readApprovalProofCheckpoint(ctx, principal, request.ID, request.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version must not authorize domain call: %v", err)
	}
	var status string
	if err := db.adminPool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, request.ID).Scan(&status); err != nil || status != "SUBMITTED" {
		t.Fatalf("preflight changed request state: %s %v", status, err)
	}
}

func TestApprovalProofRecentAuthMatrix(t *testing.T) {
	for _, command := range []string{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		if !approvalProofNeedsRecentAuthentication(command) {
			t.Fatalf("missing step-up for %s", command)
		}
	}
	if approvalProofNeedsRecentAuthentication("WALLET_ADJUSTMENT") {
		t.Fatal("wallet policy v1 must not gain a new recent-auth requirement")
	}
}
