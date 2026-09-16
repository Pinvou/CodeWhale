package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestProofRecoveryPlanExcludesLegacyAndNeverMutatesIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := repository.PlanProofApprovalRecovery(ctx, 0); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) {
		t.Fatalf("unbounded plan accepted: %v", err)
	}
	if _, err := repository.PlanProofApprovalRecovery(ctx, 201); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) {
		t.Fatalf("oversized plan accepted: %v", err)
	}
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	observeApprovalDelivery(t, ctx, repository, binding, 91)
	var before, after string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM public.approval_requests r WHERE id=$1`, binding.ApprovalRequestID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	plan, err := repository.PlanProofApprovalRecovery(ctx, 1)
	if err != nil || len(plan.Candidates) != 0 || plan.MoreCandidates || plan.ObservedAt.IsZero() || plan.Control.ApprovalEpoch < 1 {
		t.Fatalf("legacy delivery entered proof recovery: %+v err=%v", plan, err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM public.approval_requests r WHERE id=$1`, binding.ApprovalRequestID).Scan(&after); err != nil || before != after {
		t.Fatalf("planner changed request: before=%s after=%s err=%v", before, after, err)
	}
}

func TestProofRecoveryPlanReadsOriginalExecutionWithoutReplayIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	migrationConnection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := migrationConnection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, migrationConnection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, migrationConnection.Conn())
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
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	var vector authorizationProofVector
	for _, candidate := range fixtures.Vectors {
		if candidate.CommandType == "WALLET_ADJUSTMENT" {
			vector = candidate
			break
		}
	}
	if vector.CommandType == "" {
		t.Fatal("wallet proof golden fixture missing")
	}
	request := seedAuthorizationProofRequest(t, ctx, db, vector)
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "recovery-plan-"+uuid.NewString())
	repository.approvalProofSource = approvalProofSourceFunc(func(_ context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
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
		"APPROVED", "independent recovery plan proof", nil)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("approve proof=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, request.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	claims, err := relay.ClaimFrozen(ctx, "recovery-plan-fixture", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
		t.Fatalf("proof relay claim=%+v err=%v", claims, err)
	}
	if _, err := relay.ProcessFrozen(ctx, "recovery-plan-fixture", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != nil {
		t.Fatal(err)
	}
	durable := "billing-wallet-adjustment-executor-p1-v1"
	binding, err := repository.ResolveApprovalAuthorizationBinding(ctx, eventID, durable, approvalconsumer.BillingWorkload)
	if err != nil {
		t.Fatal(err)
	}
	state := observeApprovalDelivery(t, ctx, repository, binding, 101)
	state, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, eventID, durable, binding.Workload, state.Version)
	if err != nil || state.DomainAttemptCount != 1 {
		t.Fatalf("first grant=%+v err=%v", state, err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, request.ID)
	execution, err := repository.BeginApprovalExecutionWithAuthorization(ctx, request.ID, binding.Workload, eventID.String(), critical, financial, binding)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("proof begin=%+v err=%v", execution, err)
	}
	if err := repository.CompleteApprovalExecution(ctx, request.ID, binding.Workload, eventID.String(), "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_PRECONDITION_REJECTED"); err != nil {
		t.Fatal(err)
	}
	state, err = repository.RecordApprovalAuthorizationDomainFailure(ctx, eventID, durable, binding.Workload, state.Version, "DOMAIN_PRECONDITION_REJECTED")
	if err != nil || state.State != "DOMAIN_INFLIGHT" {
		t.Fatalf("proof failure=%+v err=%v", state, err)
	}
	plan, err := repository.PlanProofApprovalRecovery(ctx, 1)
	if err != nil || len(plan.Candidates) != 1 || plan.MoreCandidates {
		t.Fatalf("proof recovery plan=%+v err=%v", plan, err)
	}
	item := plan.Candidates[0]
	if item.RequestID != request.ID || item.OriginalEventID != eventID || item.OriginalKey != execution.ExecutionIdempotencyKey || item.Durable != durable || !approvaleffect.ValidFingerprint(item.CommandFingerprint) || item.ExecutionStatus != "FAILED" ||
		item.RequestStatus != "EXECUTION_FAILED" || item.Delivery.DomainAttemptCount != 1 || item.Investigation.Class != approvalconsumer.FailureUnknown ||
		item.Investigation.Retryable || !item.Investigation.QueryOnly {
		t.Fatalf("original execution not preserved for query-only investigation: %+v", item)
	}
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, request.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("planner created a second execution: count=%d err=%v", count, err)
	}
	var canLogin, canWrite bool
	if err := db.adminPool.QueryRow(ctx, `SELECT rolcanlogin,has_table_privilege('account_approval_recovery','public.approval_recovery_query_leases','INSERT,UPDATE,DELETE') FROM pg_roles WHERE rolname='account_approval_recovery'`).Scan(&canLogin, &canWrite); err != nil || canLogin || canWrite {
		t.Fatalf("recovery role enabled or has direct DML: login=%t write=%t err=%v", canLogin, canWrite, err)
	}
	var definerRoutines string
	if err := db.adminPool.QueryRow(ctx, `SELECT coalesce(string_agg(p.proname,',' ORDER BY p.proname),'') FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname='public' AND p.proname NOT IN ('claim_approval_recovery_query','release_approval_recovery_query')
  AND p.prosecdef AND has_function_privilege('account_approval_recovery',p.oid,'EXECUTE')`).Scan(&definerRoutines); err != nil {
		t.Fatal(err)
	}
	if definerRoutines != "" {
		t.Fatalf("recovery role inherited privileged routines: %s", definerRoutines)
	}
	connection, err := db.adminPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_approval_recovery`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		if _, err := connection.Exec(resetCtx, `RESET SESSION AUTHORIZATION`); err != nil {
			t.Errorf("reset isolated recovery test session: %v", err)
		}
	}()
	if _, err := connection.Exec(ctx, `SELECT * FROM public.approval_recovery_query_leases`); postgresSQLState(err) != "42501" {
		t.Fatalf("recovery role directly read private leases: SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	store, err := NewRecoveryQueryLeaseStore(connection)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, item)
	if err != nil || lease.Token == uuid.Nil || lease.QueryCount != 1 || lease.Version != 1 || !lease.Until.After(time.Now()) {
		t.Fatalf("original query lease invalid: lease=%+v err=%v", lease, err)
	}
	if _, err := store.Claim(ctx, item); !errors.Is(err, ErrApprovalRecoveryLeaseUnavailable) {
		t.Fatalf("active lease was stolen: %v", err)
	}
	wrong := lease
	wrong.Token = uuid.New()
	if _, err := store.Release(ctx, wrong); !errors.Is(err, ErrApprovalRecoveryLeaseUnavailable) {
		t.Fatalf("wrong token released lease: %v", err)
	}
	result := RecoveryResultObservation{State: "SUCCEEDED", ObservedAt: time.Now().UTC(),
		DomainResult:     json.RawMessage(`{"adjustment_id":"` + uuid.NewString() + `","wallet_id":"` + uuid.NewString() + `","status":"POSTED","version":2}`),
		LedgerReferences: json.RawMessage(`{"journal_entry_id":"` + uuid.NewString() + `"}`)}
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, item, wrong, result); !errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
		t.Fatalf("wrong recovery lease confirmed result: %v", err)
	}
	stale := item
	stale.RequestVersion++
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, stale, lease, result); !errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
		t.Fatalf("stale request version confirmed result: %v", err)
	}
	stale = item
	stale.CommandFingerprint = "bad-fingerprint"
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, stale, lease, result); !errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
		t.Fatalf("wrong fingerprint confirmed result: %v", err)
	}
	badResult := result
	badResult.LedgerReferences = json.RawMessage(`{}`)
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, item, lease, badResult); !errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
		t.Fatalf("incomplete billing positive result confirmed: %v", err)
	}
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, item, lease, result); err != nil {
		t.Fatalf("original result could not converge without a new domain call: %v", err)
	}
	var requestStatus, executionStatus string
	var domainResult json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT r.status,e.status,e.domain_result FROM approval_requests r JOIN approval_executions e ON e.approval_request_id=r.id WHERE r.id=$1`, request.ID).
		Scan(&requestStatus, &executionStatus, &domainResult); err != nil || requestStatus != "EXECUTED" || executionStatus != "EXECUTED" || len(domainResult) == 0 {
		t.Fatalf("positive result did not converge atomically: request=%s execution=%s result=%s err=%v", requestStatus, executionStatus, domainResult, err)
	}
	if err := repository.ConfirmOriginalApprovalSuccess(ctx, item, lease, result); !errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
		t.Fatalf("stale original candidate confirmed twice: %v", err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, request.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("positive confirmation created another execution: count=%d err=%v", count, err)
	}
	next, err := store.Release(ctx, lease)
	if err != nil || !next.After(time.Now()) {
		t.Fatalf("query lease release/backoff invalid: next=%v err=%v", next, err)
	}
	if _, err := store.Claim(ctx, item); !errors.Is(err, ErrApprovalRecoveryLeaseUnavailable) {
		t.Fatalf("backoff was bypassed: %v", err)
	}
}
