package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// Real public Account schema, repository submission, durable exact-CAS state-event
// receipts and independent approval. Token observation/proof/activation are controlled; no deployed
// Token authority, broker delivery or complete E2E acceptance is claimed.
func TestF04PublicTokenSubmitReceipts(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal("public Account migrations", err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, c)
	// Actual operator state transitions before any history is created.
	version := int64(1)
	for _, step := range []account.FreshApprovalStep{account.FreshApprovalQuiesce, account.FreshApprovalFence, account.FreshApprovalResources, account.FreshApprovalActivate, account.FreshApprovalResume} {
		control, e := account.RunFreshApprovalStep(ctx, f.serviceURL(t, c, "account_migrator"), version, step, func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
			return json.RawMessage(`{"controlled_broker_check":true}`), nil
		})
		if e != nil {
			t.Fatal("actual fresh operator transition", e)
		}
		version = control.Version
	}
	_, err = c.Exec(ctx, `INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES
('11111111-1111-4111-8111-111111111111','ACTIVE','owned requester','owned-requester','USR1111111111111111'||user_code_checksum('USR1111111111111111'),now(),now()),
('22222222-2222-4222-8222-222222222222','ACTIVE','owned approver','owned-approver','USR2222222222222222'||user_code_checksum('USR2222222222222222'),now(),now());
INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES
('41111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 day',now()),
('42222222-2222-4222-8222-222222222222','22222222-2222-4222-8222-222222222222','PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 day',now());
INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES
('https://owned.invalid','owned-requester','11111111-1111-4111-8111-111111111111',now(),now()),
('https://owned.invalid','owned-approver','22222222-2222-4222-8222-222222222222',now(),now());`)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err = repo.ConfigureApprovalWriter(ctx, f.serviceURL(t, c, "account_approval_writer")); err != nil {
		t.Fatal(err)
	}
	accountID, credentialID := uuid.New(), uuid.New()
	fingerprint := strings.Repeat("a", 64)
	credentialVersion := int64(3)
	projection := func() map[string]any {
		return map[string]any{"command_type": "TOKEN_CREDENTIAL_REVOKE", "target_resource_type": "PROVIDER_CREDENTIAL", "target_resource_id": credentialID,
			"critical_resource_versions": map[string]any{"token_account_id": accountID, "token_account_version": 7, "credential_id": credentialID, "credential_version": credentialVersion, "credential_status": "ACTIVE", "credential_fingerprint": fingerprint,
				"active_route_set_fingerprint": fingerprint, "provisioning_lease_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "replacement_credential_version": nil, "policy_version": 1},
			"financial_snapshot": map[string]any{}, "precheck_fingerprint": fingerprint, "token_observed_at": time.Now().UTC()}
	}
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer controlled-authority" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/internal/v1/approval-command-prechecks" {
			_ = json.NewEncoder(w).Encode(projection())
			return
		}
		if r.URL.Path != "/internal/v1/approval-command-proofs" {
			w.WriteHeader(404)
			return
		}
		raw, e := io.ReadAll(r.Body)
		request, e2 := approvalencoding.DecodeProofRequest(raw)
		if e != nil || e2 != nil {
			w.WriteHeader(400)
			return
		}
		now := time.Now().UTC().Truncate(time.Second)
		encoded, e := approvalencoding.EncodeProofResponse(approvalencoding.ProofResponse{ApprovalRequestID: request.ApprovalRequestID, CommandType: request.Artifacts.CommandType, PolicyVersion: request.Artifacts.PolicyVersion,
			ProofSchemaVersion: "1", CommandDigest: request.Artifacts.CommandDigest, SnapshotDigest: request.Artifacts.SnapshotDigest, DomainRevision: "controlled-current-credential", Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
		if e != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(encoded)
	}))
	defer authority.Close()
	snapshot, err := account.NewHTTPApprovalSnapshotSource(authority.URL, "controlled-authority")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := account.NewHTTPApprovalProofSourceWithToken(authority.URL, authority.URL, "controlled-authority")
	if err != nil {
		t.Fatal(err)
	}
	activation := &f04ControlledTokenActivation{}
	repo.WithApprovalSnapshotSource(snapshot).WithApprovalProofSource(proof).WithTokenGovernanceActivation(activation)
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-requester", SessionID: "owned-requester-session", AuthenticatedAt: time.Now()}
	command, _ := json.Marshal(map[string]any{"token_account_id": accountID, "expected_account_version": 7, "credential_id": credentialID, "credential_version": 3})
	draft := account.ApprovalDraft{CommandType: "TOKEN_CREDENTIAL_REVOKE", IdempotencyKey: "owned-token-authoritative", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command,
		Reason: "owned token revocation", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`), CriticalResourceVersions: json.RawMessage(`{"caller_forgery":true}`), FinancialSnapshot: json.RawMessage(`{"caller_forgery":true}`)}

	activation.active = true
	create := func(key string) (account.ApprovalRequest, account.ApprovalDraft) {
		t.Helper()
		original := draft
		original.IdempotencyKey = key
		item, err := repo.CreateApprovalDraft(ctx, principal, original)
		if err != nil {
			t.Fatal("actual create failed", err)
		}
		edit := original
		edit.IdempotencyKey = "edit-" + key
		edit.Reason = "owned exact edited revocation reason"
		return item, edit
	}
	assertUnchanged := func(id uuid.UUID, expected account.ApprovalRequest) {
		t.Helper()
		var count, version int64
		var status string
		if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='SUBMITTED'),version,status FROM approval_requests WHERE id=$1`, id).Scan(&count, &version, &status); err != nil || count != 1 || version != expected.Version || status != expected.Status {
			t.Fatalf("submit replay changed state: count=%d version=%d status=%s err=%v", count, version, status, err)
		}
	}
	newSubmitted := func(key string) (account.ApprovalRequest, account.ApprovalRequest) {
		t.Helper()
		item, _ := create(key)
		submitted, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
		if err != nil || submitted.Status != "SUBMITTED" {
			t.Fatal("actual submission failed", err)
		}
		return item, submitted
	}
	t.Run("committed-submit-lost-ack-replays-original-cas", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-exact")
		replayed, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
		if err != nil || replayed.Status != submitted.Status || replayed.Version != submitted.Version {
			t.Fatal("committed original submit receipt unavailable", err)
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("newer-cas-cannot-adopt-submission", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-cas")
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, submitted.Version); !errors.Is(err, account.ErrConflict) {
			t.Fatal("new CAS adopted original submission", err)
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("foreign-actor-cannot-replay", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-foreign")
		foreign := principal
		foreign.Subject = "owned-approver"
		if _, err := repo.SubmitApprovalRequest(ctx, foreign, item.ID, item.Version); err == nil {
			t.Fatal("foreign submit replay accepted")
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("withdrawn-submit-does-not-replay", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-withdrawn")
		withdrawn, err := repo.WithdrawApprovalRequest(ctx, principal, item.ID, submitted.Version)
		if err != nil {
			t.Fatal("actual withdrawal failed", err)
		}
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); !errors.Is(err, account.ErrConflict) {
			t.Fatal("withdrawn receipt replayed", err)
		}
		assertUnchanged(item.ID, withdrawn)
	})
	t.Run("inactive-domain-cannot-replay", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-inactive")
		activation.active = false
		defer func() { activation.active = true }()
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); err == nil {
			t.Fatal("inactive domain replay accepted")
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("expired-submit-does-not-replay", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-expired")
		if _, err := c.Exec(ctx, `UPDATE approval_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
			t.Fatal("expiry fixture", err)
		}
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); !errors.Is(err, account.ErrConflict) {
			t.Fatal("expired submit replayed", err)
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("later-edit-submit-cannot-adopt-older-cas", func(t *testing.T) {
		item, edit := create("submit-receipt-later-edit")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal("actual independent edit", err)
		}
		submitted, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, updated.Version)
		if err != nil {
			t.Fatal("actual edited submission", err)
		}
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); !errors.Is(err, account.ErrConflict) {
			t.Fatal("older CAS adopted later edited submit", err)
		}
		assertUnchanged(item.ID, submitted)
	})
	t.Run("approved-request-replays-same-submit-receipt", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-approved")
		approver := principal
		approver.Subject = "owned-approver"
		approved, err := repo.DecideApprovalRequest(ctx, approver, item.ID, submitted.Version, "APPROVED", "owned independent review", submitted.CriticalResourceVersions)
		if err != nil || approved.Status != "APPROVED" {
			t.Fatal("actual independent approval", err)
		}
		replayed, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
		if err != nil || replayed.Status != approved.Status || replayed.Version != approved.Version {
			t.Fatal("same submit after approval unavailable", err)
		}
		assertUnchanged(item.ID, approved)
	})
	t.Run("revoked-requester-cannot-replay", func(t *testing.T) {
		item, submitted := newSubmitted("submit-receipt-revoked")
		if _, err := c.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, "11111111-1111-4111-8111-111111111111"); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); err == nil {
			t.Fatal("revoked requester submit accepted")
		}
		assertUnchanged(item.ID, submitted)
	})
}
