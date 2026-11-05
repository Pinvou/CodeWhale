package billingschema

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

// Real Account/Billing migrations, proof-era request transitions, persisted
// delivery and 15-second query lease. Domain proof/result and broker ACK are
// controlled fixtures; no financial executor or vendor acceptance is claimed.
// Each case exclusively owns the existing absent-role fixture and UUID DBs.
func TestRetainedRecoveryTerminalLeaseClock(t *testing.T) {
	t.Run("expired-confirm", func(t *testing.T) { retainedRecoveryLeaseScenario(t, false, true) })
	t.Run("live-confirm", func(t *testing.T) { retainedRecoveryLeaseScenario(t, false, false) })
	t.Run("expired-zero-effect", func(t *testing.T) { retainedRecoveryLeaseScenario(t, true, true) })
}

type retainedRecoveryActivationReader struct{ c *pgx.Conn }

func (v retainedRecoveryActivationReader) Snapshot(ctx context.Context) (billinggovernance.Snapshot, error) {
	s := billinggovernance.Snapshot{SchemaVersion: 1}
	rows, err := v.c.Query(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activations ORDER BY command_type`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var a billinggovernance.Activation
		if err = rows.Scan(&a.CommandType, &a.PolicyVersion, &a.SnapshotSchemaVersion, &a.State, &a.Version, &a.UpdatedAt); err != nil {
			return s, err
		}
		s.Activations = append(s.Activations, a)
	}
	return s, rows.Err()
}

type retainedRecoveryProof struct{}

func (retainedRecoveryProof) Proof(_ context.Context, id uuid.UUID, a approvalencoding.ProofArtifacts) (account.ApprovalProofResult, error) {
	now := time.Now().UTC().Truncate(time.Second)
	v := approvalencoding.ProofResponse{ApprovalRequestID: id, CommandType: a.CommandType, PolicyVersion: a.PolicyVersion, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: a.CommandDigest, SnapshotDigest: a.SnapshotDigest, DomainRevision: "owned-current-revision", Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	digest, e := approvalencoding.DeriveProofDigest(v)
	return account.ApprovalProofResult{Response: v, Digest: hex.EncodeToString(digest[:])}, e
}
func retainedRecoveryLeaseScenario(t *testing.T, zeroEffect, blockUntilExpired bool, hooks ...func(context.Context, *stageFixture, *pgx.Conn, *pgx.Conn, *account.PostgresRepository, account.ApprovalRecoveryCandidate)) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, stop := context.WithTimeout(context.Background(), 7*time.Minute)
	defer stop()
	f := newStageFixture(t, ctx, raw)
	aa, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("load Account source")
	}
	c := f.database(t, ctx, "account", "account_service")
	if err = accountschema.Up(ctx, c, aa); err != nil {
		t.Fatal("install current real Account schema")
	}
	migrations.Unlock(ctx, c)
	bb, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal("load Billing source")
	}
	b := f.database(t, ctx, "billing", "billing_schema_owner")
	if err = Up(ctx, b, bb); err != nil {
		t.Fatal("install current real Billing schema")
	}
	migrations.Unlock(ctx, b)
	_, err = c.Exec(ctx, `
INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES
('11111111-1111-4111-8111-111111111111','ACTIVE','owned requester','own-requester','USR1111111111111111'||user_code_checksum('USR1111111111111111'),now(),now()),
('22222222-2222-4222-8222-222222222222','ACTIVE','owned approver','own-approver','USR2222222222222222'||user_code_checksum('USR2222222222222222'),now(),now());
INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES
('41111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','PLATFORM_BILLING_OPERATOR','ACCOUNT',now()-interval '1 day',now()),
('42222222-2222-4222-8222-222222222222','22222222-2222-4222-8222-222222222222','PLATFORM_BILLING_APPROVER','ACCOUNT',now()-interval '1 day',now());
INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES
('https://owned.invalid','owned-requester','11111111-1111-4111-8111-111111111111',now(),now()),
('https://owned.invalid','owned-approver','22222222-2222-4222-8222-222222222222',now(),now());`)
	if err != nil {
		t.Fatal("seed two distinct genuine permission-bound actors")
	}
	repo, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("open real Account repository")
	}
	defer repo.Close()
	repo.WithBillingGovernanceActivation(retainedRecoveryActivationReader{b}).WithApprovalProofSource(retainedRecoveryProof{})
	// Genuine fresh-mode state transitions; broker verification callback is controlled and disclosed.
	version := int64(1)
	for _, step := range []account.FreshApprovalStep{account.FreshApprovalQuiesce, account.FreshApprovalFence, account.FreshApprovalResources, account.FreshApprovalActivate, account.FreshApprovalResume} {
		control, e := account.RunFreshApprovalStep(ctx, f.serviceURL(t, c, "account_migrator"), version, step, func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
			return json.RawMessage(`{"controlled_broker_check":true}`), nil
		})
		if e != nil {
			t.Fatal("real fresh-mode transition failed")
		}
		version = control.Version
	}

	if err = repo.ConfigureApprovalWriter(ctx, f.serviceURL(t, c, "account_approval_writer")); err != nil {
		t.Fatal("real isolated writer readiness")
	}
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-requester", SessionID: "owned-requester-session", AuthenticatedAt: time.Now()}
	approver := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-approver", SessionID: "owned-approver-session", AuthenticatedAt: time.Now()}
	draft := account.ApprovalDraft{CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: "owned-independent-financial-freeze", NormalizedCommand: json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","amount_microcredits":100,"requested_by":"11111111-1111-4111-8111-111111111111"}`), TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "0198f110-f538-7f30-a8b1-f3984fd459a0", Reason: "owned independent financial freeze", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{"delta":100}`), CriticalResourceVersions: json.RawMessage(`{"adjustment_version":1}`), FinancialSnapshot: json.RawMessage(`{"wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","delta_microcredits":100}`)}
	item, err := repo.CreateApprovalDraft(ctx, principal, draft)
	if err != nil {
		t.Fatal("real CreateApprovalDraft failed")
	}
	item, err = repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
	if err != nil {
		t.Fatal("real SubmitApprovalRequest failed")
	}
	item, err = repo.DecideApprovalRequest(ctx, approver, item.ID, item.Version, "APPROVED", "owned independent decision", draft.CriticalResourceVersions)
	if err != nil || item.Status != "APPROVED" {
		t.Fatal("real proof Decide failed")
	}

	relay, err := account.NewApprovalRelayStore(ctx, f.serviceURL(t, c, "account_approval_relay"))
	if err != nil {
		diagnostic, connectErr := pgx.Connect(ctx, f.serviceURL(t, c, "account_approval_relay"))
		if connectErr != nil {
			t.Logf("relay diagnostic connect timeout=%t cancelled=%t", errors.Is(connectErr, context.DeadlineExceeded), errors.Is(connectErr, context.Canceled))
		} else {
			defer diagnostic.Close(context.Background())
			var actor, login, inherit, privileged, member, databaseCreate, publicCreate, sidecarCreate bool
			diagErr := diagnostic.QueryRow(ctx, `SELECT session_user='account_approval_relay' AND current_user=session_user,rolcanlogin,rolinherit,rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls,EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid),pg_catalog.has_database_privilege(current_user,current_database(),'CREATE'),pg_catalog.has_schema_privilege(current_user,'public','CREATE'),pg_catalog.has_schema_privilege(current_user,'account_approval_authorization','CREATE') FROM pg_catalog.pg_roles r WHERE rolname=session_user`).Scan(&actor, &login, &inherit, &privileged, &member, &databaseCreate, &publicCreate, &sidecarCreate)
			t.Logf("relay diagnostic actor=%t login=%t inherit=%t privileged=%t member=%t database_create=%t public_create=%t sidecar_create=%t query_timeout=%t", actor, login, inherit, privileged, member, databaseCreate, publicCreate, sidecarCreate, errors.Is(diagErr, context.DeadlineExceeded))
			var p, a, r, d int64
			diagErr = diagnostic.QueryRow(ctx, `SELECT * FROM account_approval_authorization.authorization_relay_readiness()`).Scan(&p, &a, &r, &d)
			var sqlErr interface{ SQLState() string }
			state := ""
			if errors.As(diagErr, &sqlErr) {
				state = sqlErr.SQLState()
			}
			t.Logf("relay diagnostic sidecar_error=%t sqlstate=%s timeout=%t", diagErr != nil, state, errors.Is(diagErr, context.DeadlineExceeded))
		}
		t.Fatal("real relay readiness")
	}
	defer relay.Close()
	claims, err := relay.ClaimFrozen(ctx, "owned-independent-relay", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatal("real relay claim failed")
	}
	disposition, err := relay.ProcessFrozen(ctx, "owned-independent-relay", claims[0], 12, time.Now(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatal("real relay persistence failed")
	}
	durable := "billing-wallet-adjustment-executor-p1-v1"
	binding, err := repo.ResolveApprovalAuthorizationBinding(ctx, claims[0].EventID, durable, "billing-command-executor")
	if err != nil {
		t.Fatal("real frozen binding failed")
	}
	if binding.SerializationVersion != "APPROVAL_RFC8785_DECIMAL_V1" {
		t.Fatal("not proof-era binding")
	}
	delivery, err := repo.ObserveApprovalAuthorizationDelivery(ctx, account.ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: 1})
	if err != nil {
		t.Fatal("real delivery observation failed")
	}
	delivery, err = repo.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, durable, binding.Workload, delivery.Version)
	if err != nil || delivery.DomainAttemptCount != 1 {
		t.Fatal("real unique domain grant failed")
	}
	execution, err := repo.BeginApprovalExecutionWithAuthorization(ctx, item.ID, binding.Workload, binding.EventID.String(), draft.CriticalResourceVersions, draft.FinancialSnapshot, binding)
	if err != nil || execution.Status != "EXECUTING" {
		t.Fatal("real proof Begin failed")
	}
	if zeroEffect {
		if _, e := repo.RecordApprovalAuthorizationDomainFailure(ctx, binding.EventID, durable, binding.Workload, delivery.Version, "DOMAIN_RESOURCE_NOT_FOUND"); e != nil {
			t.Fatal("persist actual permanent delivery failure", e)
		}
		if e := repo.CompleteApprovalExecution(ctx, item.ID, binding.Workload, binding.EventID.String(), "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_RESOURCE_NOT_FOUND"); e != nil {
			t.Fatal("complete actual failed execution", e)
		}
		// Do not shorten policy windows or rewrite frozen timestamps. Wait for the
		// real installed five-minute execution window to elapse on the DB clock.
		var deadline time.Time
		if e := c.QueryRow(ctx, `SELECT execution_deadline FROM approval_requests WHERE id=$1`, item.ID).Scan(&deadline); e != nil {
			t.Fatal(e)
		}
		for {
			var live time.Time
			if e := c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&live); e != nil {
				t.Fatal(e)
			}
			if !deadline.After(live) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("real execution window did not elapse within fixture budget")
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	plan, err := repo.PlanProofApprovalRecovery(ctx, 1)
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatal("real recovery plan failed")
	}
	candidate := plan.Candidates[0]
	if len(hooks) > 0 {
		for _, hook := range hooks {
			hook(ctx, f, c, b, repo, candidate)
		}
		return
	}
	// Owned fixture admin assumes a pre-existing NOLOGIN recovery role only for this connection.
	// No ALTER ROLE/grant/table mutation; session_user and SECURITY DEFINER checks run unchanged.
	config := f.config.Copy()
	config.Database = c.Config().Database
	recovery, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("open owned recovery fixture connection")
	}
	defer recovery.Close(context.Background())
	if _, err = recovery.Exec(ctx, `SET SESSION AUTHORIZATION account_approval_recovery`); err != nil {
		t.Fatal("assume isolated recovery session")
	}
	var actor string
	if err = recovery.QueryRow(ctx, `SELECT session_user`).Scan(&actor); err != nil || actor != "account_approval_recovery" {
		t.Fatal("recovery identity mismatch")
	}
	leaser, err := account.NewRecoveryQueryLeaseStore(recovery)
	if err != nil {
		t.Fatal("construct real query lease store")
	}
	lease, err := leaser.Claim(ctx, candidate)
	if err != nil {
		t.Fatal("real query lease claim failed")
	}
	var dbClock time.Time
	if err = c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbClock); err != nil {
		t.Fatal("read live DB clock")
	}
	if remaining := lease.Until.Sub(dbClock); remaining < 14*time.Second || remaining > 16*time.Second {
		t.Fatal("actual claim duration not fifteen seconds")
	}
	t.Log("REAL_PIPELINE_PROOF_DELIVERY_ONE_LEASE_FIFTEEN_SECONDS")
	result := account.RecoveryResultObservation{State: "SUCCEEDED", ObservedAt: time.Now().UTC(), DomainResult: json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","status":"POSTED","version":1}`), LedgerReferences: json.RawMessage(`{"journal_entry_id":"0198f110-f538-7f30-a8b1-f3984fd459a2"}`)}
	if !blockUntilExpired {
		if e := repo.ConfirmOriginalApprovalSuccess(ctx, candidate, lease, result); e != nil {
			t.Fatal("live lease did not confirm", e)
		}
		var status string
		if e := c.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, item.ID).Scan(&status); e != nil || status != "EXECUTED" {
			t.Fatal("live confirmation did not converge", e)
		}
		var exact bool
		if e := c.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(domain_result=$2::jsonb AND ledger_references=$3::jsonb) FROM approval_executions WHERE approval_request_id=$1`, item.ID, result.DomainResult, result.LedgerReferences).Scan(&exact); e != nil || !exact {
			t.Fatal("live confirmation changed the original result or created another execution", e)
		}
		return
	}
	var before string
	if e := c.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(r),'execution',to_jsonb(e))::text FROM approval_requests r JOIN approval_executions e ON e.approval_request_id=r.id WHERE r.id=$1`, item.ID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	// No row update: only a competing owner holds the request lock while time passes.
	locker, err := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("open isolated locker")
	}
	defer locker.Close(context.Background())
	blocker, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal("begin competing owner")
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `SELECT id FROM approval_requests WHERE id=$1 FOR UPDATE`, item.ID); err != nil {
		t.Fatal("hold request lock")
	}
	untilStart := time.Until(lease.Until.Add(-3 * time.Second))
	if untilStart > 0 {
		time.Sleep(untilStart)
	}
	callctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if zeroEffect {
			done <- repo.InvalidateOriginalApprovalZeroEffect(callctx, candidate, lease)
		} else {
			done <- repo.ConfirmOriginalApprovalSuccess(callctx, candidate, lease, result)
		}
	}()
	var blocked bool
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if err = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event_type='Lock' AND query LIKE '%approval_requests%')`, c.Config().Database).Scan(&blocked); err != nil {
			t.Fatal("read actual blocker")
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("actual terminal transaction did not wait on request lock")
	}
	if pause := time.Until(lease.Until.Add(250 * time.Millisecond)); pause > 0 {
		time.Sleep(pause)
	}
	if err = c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbClock); err != nil || !dbClock.After(lease.Until) {
		t.Fatal("live database lease did not expire")
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal("release competing owner lock")
	}
	err = <-done
	var status string
	if e := c.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, item.ID).Scan(&status); e != nil {
		t.Fatal("read actual terminal state")
	}
	t.Logf("safe terminal facts: blocked=true expired_at_release=true call_error=%v executed=%v", err != nil, status == "EXECUTED")
	want := account.ErrApprovalRecoveryConfirmConflict
	if zeroEffect {
		want = account.ErrApprovalRecoveryZeroEffectConflict
	}
	if !errors.Is(err, want) {
		t.Fatalf("expired lease must reject at authority fence, got %v", err)
	}
	var after string
	if e := c.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(r),'execution',to_jsonb(e))::text FROM approval_requests r JOIN approval_executions e ON e.approval_request_id=r.id WHERE r.id=$1`, item.ID).Scan(&after); e != nil || after != before {
		t.Fatal("expired lease rejection changed original request or execution", e)
	}
}
