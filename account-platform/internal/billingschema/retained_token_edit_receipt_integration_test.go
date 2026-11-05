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

// Real public Account schema, repository edits, immutable state-event receipts
// and submission. Token observation/proof/activation are controlled; no deployed
// Token authority, broker delivery or complete E2E acceptance is claimed.
func TestF04PublicTokenEditReceipts(t *testing.T) {
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
	assertSingleReceipt := func(id uuid.UUID, version int64) {
		t.Helper()
		var events, savedVersion int64
		if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='DRAFT_UPDATED'),version FROM approval_requests WHERE id=$1`, id).Scan(&events, &savedVersion); err != nil || events != 1 || savedVersion != version {
			t.Fatalf("receipt replay mutated state: events=%d version=%d err=%v", events, savedVersion, err)
		}
	}
	t.Run("same-key-cas-input-replay-no-mutation", func(t *testing.T) {
		item, edit := create("receipt-exact")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil || updated.Version != item.Version+1 {
			t.Fatal("actual edit failed", err)
		}
		// Token input observations are ignored; Account owns the saved snapshot.
		edit.CriticalResourceVersions = json.RawMessage(`{"different_untrusted_observation":true}`)
		replayed, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil || replayed.ID != item.ID || replayed.Version != updated.Version || string(replayed.CriticalResourceVersions) != string(updated.CriticalResourceVersions) {
			t.Fatal("same original edit replay failed", err)
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
	t.Run("same-key-changed-reason-rejected", func(t *testing.T) {
		item, edit := create("receipt-reason")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		edit.Reason = "different original editor reason"
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit); !errors.Is(err, account.ErrConflict) {
			t.Fatal("same key changed facts accepted", err)
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
	t.Run("same-key-current-cas-still-cannot-change-input", func(t *testing.T) {
		item, edit := create("receipt-current-key")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		edit.Reason = "different reason with the newer CAS"
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, updated.Version, edit); !errors.Is(err, account.ErrConflict) {
			t.Fatal("same key became a new invocation", err)
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
	t.Run("different-key-cannot-adopt-equal-edit", func(t *testing.T) {
		item, edit := create("receipt-key")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		edit.IdempotencyKey = "another-editor-invocation"
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit); !errors.Is(err, account.ErrConflict) {
			t.Fatal("different key adopted another edit", err)
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
	t.Run("foreign-requester-cannot-replay", func(t *testing.T) {
		item, edit := create("receipt-actor")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		foreign := principal
		foreign.Subject = "owned-approver"
		if _, err := repo.UpdateApprovalDraft(ctx, foreign, item.ID, item.Version, edit); err == nil {
			t.Fatal("foreign actor replay accepted")
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
	t.Run("later-edit-does-not-recover-older-intent", func(t *testing.T) {
		item, edit := create("receipt-later")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		later := edit
		later.Reason = "later independently reviewed intent"
		later.IdempotencyKey = "later-independent-edit"
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, updated.Version, later); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit); !errors.Is(err, account.ErrConflict) {
			t.Fatal("older receipt adopted later intent", err)
		}
	})
	t.Run("submitted-lost-ack-replays-exact-edit", func(t *testing.T) {
		item, edit := create("receipt-submitted")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		submitted, err := repo.SubmitApprovalRequest(ctx, principal, item.ID, updated.Version)
		if err != nil {
			t.Fatal("real submit failed", err)
		}
		replayed, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil || replayed.Status != "SUBMITTED" || replayed.Version != submitted.Version {
			t.Fatal("submitted exact edit recovery failed", err)
		}
		assertSingleReceipt(item.ID, submitted.Version)
	})
	t.Run("immutable-lineage-change-rejected", func(t *testing.T) {
		item, edit := create("receipt-lineage")
		lineage := uuid.New()
		edit.SupersedesRequestID = &lineage
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit); !errors.Is(err, account.ErrConflict) {
			t.Fatal("immutable lineage changed", err)
		}
	})
	t.Run("revoked-editor-cannot-replay", func(t *testing.T) {
		item, edit := create("receipt-revoked")
		updated, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, "11111111-1111-4111-8111-111111111111"); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdateApprovalDraft(ctx, principal, item.ID, item.Version, edit); err == nil {
			t.Fatal("revoked editor replay accepted")
		}
		assertSingleReceipt(item.ID, updated.Version)
	})
}
