package billing

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestBillingWalletApprovalProofPostgresIntegration(t *testing.T) {
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
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand:        marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": walletID, "amount_microcredits": 370, "requested_by": requesterID}),
		CriticalResourceVersions: marshal(map[string]any{"adjustment_version": adjustment.Version}),
		FinancialSnapshot:        marshal(map[string]any{"wallet_id": walletID, "delta_microcredits": 370}),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository)
	request := approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: artifacts}
	allow, err := service.billingApprovalProof(ctx, request)
	if err != nil || allow.Verdict != "ALLOW" || allow.ReasonCode != "PROOF_VALID" {
		t.Fatalf("real wallet owner proof=%+v err=%v", allow, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE wallets SET status='FROZEN',version=version+1,updated_at=now() WHERE id=$1`, walletID); err != nil {
		t.Fatal(err)
	}
	deny, err := service.billingApprovalProof(ctx, request)
	if err != nil || deny.Verdict != "DENY" || deny.ReasonCode != "RESOURCE_STATE_CHANGED" || deny.DomainRevision == allow.DomainRevision {
		t.Fatalf("frozen wallet proof=%+v err=%v", deny, err)
	}
	var status string
	var version int64
	if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM manual_adjustments WHERE id=$1`, adjustment.ID).Scan(&status, &version); err != nil || status != "PENDING_APPROVAL" || version != adjustment.Version {
		t.Fatalf("read-only proof changed adjustment status=%s version=%d err=%v", status, version, err)
	}
}
