package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestBillingWalletApprovalClaimPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	walletID, ownerID, requesterID := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, walletID, ownerID); err != nil {
		t.Fatal(err)
	}
	adjustment, err := repository.RequestAdjustment(ctx, walletID, 370, "reviewed funding adjustment", requesterID)
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(value any) json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	command := marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": walletID, "amount_microcredits": 370, "requested_by": requesterID})
	critical := marshal(map[string]any{"adjustment_version": adjustment.Version})
	financial := marshal(map[string]any{"wallet_id": walletID, "delta_microcredits": 370})
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand: command, CriticalResourceVersions: critical, FinancialSnapshot: financial,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository)
	requestID, eventID := uuid.New(), uuid.New()
	allow, err := service.billingApprovalProof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: requestID, Artifacts: artifacts})
	if err != nil || allow.Verdict != "ALLOW" {
		t.Fatalf("wallet proof=%+v err=%v", allow, err)
	}
	event := approvalencoding.AuthorizationEvent{
		EventID: eventID, ApprovalRequestID: requestID, CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand: command, CriticalResourceVersions: critical, FinancialSnapshot: financial,
		CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
		DomainRevision: allow.DomainRevision, ApprovalControlEpoch: 2,
	}
	binding := ApprovalAuthorizationBinding{ApprovalRequestID: requestID, EventID: eventID, CommandType: event.CommandType, PolicyVersion: 1, ApprovalEpoch: 2}
	if err := repository.ClaimBillingApprovalProof(ctx, binding, event); err != nil {
		t.Fatalf("claim current wallet facts: %v", err)
	}
	if claimed, err := repository.ExistingBillingApprovalProofClaim(ctx, binding); err != nil || !claimed {
		t.Fatalf("persisted claim must be observable before any replay: claimed=%v err=%v", claimed, err)
	}
	mismatched := binding
	mismatched.ApprovalRequestID = uuid.New()
	if err := repository.ClaimBillingApprovalProof(ctx, mismatched, event); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("claim accepted a foreign approval request: %v", err)
	}
	if err := repository.ClaimBillingApprovalProof(ctx, binding, event); err == nil || errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("repeat claim must be unknown, not grant another effect: %v", err)
	}
	var claims int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1`, requestID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("durable claim count=%d err=%v", claims, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE wallets SET status='FROZEN',version=version+1,updated_at=now() WHERE id=$1`, walletID); err != nil {
		t.Fatal(err)
	}
	second := binding
	second.ApprovalRequestID, second.EventID = uuid.New(), uuid.New()
	event.EventID, event.ApprovalRequestID = second.EventID, second.ApprovalRequestID
	if err := repository.ClaimBillingApprovalProof(ctx, second, event); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("stale wallet proof was claimed: %v", err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("drift added claim count=%d err=%v", claims, err)
	}
	var status string
	if err := repository.pool.QueryRow(ctx, `SELECT status FROM manual_adjustments WHERE id=$1`, adjustment.ID).Scan(&status); err != nil || status != "PENDING_APPROVAL" {
		t.Fatalf("claim caused wallet effect status=%s err=%v", status, err)
	}
}
