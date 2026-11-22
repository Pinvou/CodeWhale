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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type f04ControlledTokenActivation struct{ active bool }

func (v *f04ControlledTokenActivation) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	state := "PRE_ACTIVATION"
	if v.active {
		state = "ACTIVE"
	}
	return tokengovernance.Snapshot{State: state, Epoch: 1}, nil
}

// Account public schema and the genuine proof-era writer/relay/execution chain.
// Token snapshot/proof/activation and broker ACK are controlled authorities;
// this does not claim real Token activation, credential revocation or NATS acceptance.
func TestF04PublicTokenApprovalSnapshots(t *testing.T) {
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
	approver := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-approver", SessionID: "owned-approver-session", AuthenticatedAt: time.Now()}
	command, _ := json.Marshal(map[string]any{"token_account_id": accountID, "expected_account_version": 7, "credential_id": credentialID, "credential_version": 3})
	draft := account.ApprovalDraft{CommandType: "TOKEN_CREDENTIAL_REVOKE", IdempotencyKey: "owned-token-authoritative", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command,
		Reason: "owned token revocation", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`), CriticalResourceVersions: json.RawMessage(`{"caller_forgery":true}`), FinancialSnapshot: json.RawMessage(`{"caller_forgery":true}`)}
	t.Run("inactive-denied", func(t *testing.T) {
		if _, err := repo.CreateApprovalDraft(ctx, principal, draft); !errors.Is(err, account.ErrTokenGovernanceInactive) {
			t.Fatal("inactive Token command was not denied", err)
		}
	})
	activation.active = true
	create := func(key string) account.ApprovalRequest {
		t.Helper()
		draft.IdempotencyKey = key
		item, e := repo.CreateApprovalDraft(ctx, principal, draft)
		if e != nil || strings.Contains(string(item.CriticalResourceVersions), "caller_forgery") || string(item.FinancialSnapshot) != "{}" {
			t.Fatal("authoritative draft failed", e)
		}
		return item
	}
	t.Run("original-request-replay", func(t *testing.T) {
		item := create("owned-original-replay")
		replayed, e := repo.CreateApprovalDraft(ctx, principal, draft)
		if e != nil || replayed.ID != item.ID || replayed.Version != item.Version || string(replayed.CriticalResourceVersions) != string(item.CriticalResourceVersions) {
			t.Fatal("same original body replay failed", e)
		}
		var requestCount, snapshotCount int
		if e = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_requests WHERE id=$1),(SELECT count(*) FROM approval_request_snapshots WHERE approval_request_id=$1)`, item.ID).Scan(&requestCount, &snapshotCount); e != nil || requestCount != 1 || snapshotCount != 1 {
			t.Fatal("replay mutated persisted request/snapshot", e, requestCount, snapshotCount)
		}
		changed := draft
		changed.Reason = "different intent"
		if _, e = repo.CreateApprovalDraft(ctx, principal, changed); !errors.Is(e, account.ErrConflict) {
			t.Fatal("same key changed intent accepted", e)
		}
	})
	t.Run("submit-snapshot-drift", func(t *testing.T) {
		item := create("owned-drift")
		credentialVersion++
		if _, e := repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version); !errors.Is(e, account.ErrApprovalInvalidated) {
			t.Fatal("changed credential was not invalidated", e)
		}
		credentialVersion--
	})
	t.Run("proof-and-execution", func(t *testing.T) {
		item := create("owned-positive")
		item, err = repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
		if err != nil {
			t.Fatal(err)
		}
		item, err = repo.DecideApprovalRequest(ctx, approver, item.ID, item.Version, "APPROVED", "owned independent approval", item.CriticalResourceVersions)
		if err != nil || item.Status != "APPROVED" {
			t.Fatal("proof-era approval failed", err)
		}
		relay, e := account.NewApprovalRelayStore(ctx, f.serviceURL(t, c, "account_approval_relay"))
		if e != nil {
			t.Fatal(e)
		}
		defer relay.Close()
		claims, e := relay.ClaimFrozen(ctx, "owned-token-relay", 1, time.Minute)
		if e != nil || len(claims) != 1 {
			t.Fatal("actual frozen claim failed", e)
		}
		_, e = relay.ProcessFrozen(ctx, "owned-token-relay", claims[0], 12, time.Now(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		})
		if e != nil {
			t.Fatal(e)
		}
		binding, e := repo.ResolveApprovalAuthorizationBinding(ctx, claims[0].EventID, "token-credential-revoke-executor-v1", "token-command-executor")
		if e != nil {
			t.Fatal(e)
		}
		delivery, e := repo.ObserveApprovalAuthorizationDelivery(ctx, account.ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: 1})
		if e != nil {
			t.Fatal(e)
		}
		_, e = repo.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, delivery.Version)
		// Token has a result-first single-claim path, not Billing's retry budget.
		if !errors.Is(e, account.ErrForbidden) {
			t.Fatal("Token must not spend a Billing attempt", e)
		}
		execution, e := repo.BeginApprovalExecutionWithAuthorization(ctx, item.ID, binding.Workload, binding.EventID.String(), item.CriticalResourceVersions, item.FinancialSnapshot, binding)
		if e != nil || execution.Status != "EXECUTING" || !execution.FirstClaim {
			t.Fatal("actual execution failed", e)
		}

		plan, e := repo.PlanProofApprovalRecovery(ctx, 1)
		if e != nil || len(plan.Candidates) != 1 {
			t.Fatal("actual Token recovery candidate unavailable", e)
		}
		candidate := plan.Candidates[0]
		if candidate.Delivery.DomainAttemptCount != 0 {
			t.Fatal("Token recovery spent a Billing attempt")
		}
		configuration := f.config.Copy()
		configuration.Database = c.Config().Database
		recovery, e := pgx.ConnectConfig(ctx, configuration)
		if e != nil {
			t.Fatal(e)
		}
		defer recovery.Close(context.Background())
		if _, e = recovery.Exec(ctx, `SET SESSION AUTHORIZATION account_approval_recovery`); e != nil {
			t.Fatal(e)
		}
		leaser, e := account.NewRecoveryQueryLeaseStore(recovery)
		if e != nil {
			t.Fatal(e)
		}
		lease, e := leaser.Claim(ctx, candidate)
		if e != nil {
			t.Fatal("real Token query lease", e)
		}
		reference := uuid.New()
		result := account.RecoveryResultObservation{State: "SUCCEEDED", ObservedAt: time.Now().UTC(), CommandReference: &reference}
		wrong := lease
		wrong.Token = uuid.New()
		if e = repo.ConfirmOriginalApprovalSuccess(ctx, candidate, wrong, result); !errors.Is(e, account.ErrApprovalRecoveryConfirmConflict) {
			t.Fatal("foreign lease accepted", e)
		}
		invalid := result
		invalid.CommandReference = nil
		if e = repo.ConfirmOriginalApprovalSuccess(ctx, candidate, lease, invalid); !errors.Is(e, account.ErrApprovalRecoveryConfirmConflict) {
			t.Fatal("missing Token result identity accepted", e)
		}
		invalid = result
		invalid.LedgerReferences = json.RawMessage(`{}`)
		if e = repo.ConfirmOriginalApprovalSuccess(ctx, candidate, lease, invalid); !errors.Is(e, account.ErrApprovalRecoveryConfirmConflict) {
			t.Fatal("foreign financial result accepted", e)
		}
		if e = repo.ConfirmOriginalApprovalSuccess(ctx, candidate, lease, result); e != nil {
			t.Fatal("genuine Token result recovery failed", e)
		}
		var exact bool
		if e = c.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(status='EXECUTED' AND domain_result=jsonb_build_object('command_reference',$2::text,'status','SUCCEEDED') AND ledger_references='{}'::jsonb) FROM approval_executions WHERE approval_request_id=$1`, item.ID, reference.String()).Scan(&exact); e != nil || !exact {
			t.Fatal("Token result identity or single execution lost", e)
		}
		if e = repo.ConfirmOriginalApprovalSuccess(ctx, candidate, lease, result); !errors.Is(e, account.ErrApprovalRecoveryConfirmConflict) {
			t.Fatal("stale repeated terminal CAS accepted", e)
		}
	})
}
