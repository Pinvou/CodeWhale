package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type fixedRecoveryResultReader struct {
	result RecoveryResultObservation
	calls  int
}

func (reader *fixedRecoveryResultReader) ObserveRecoveryResult(context.Context, ApprovalRecoveryCandidate) (RecoveryResultObservation, error) {
	reader.calls++
	return reader.result, nil
}

// The approval writer readiness probe is a heavyweight ACL check with a
// five-second budget, so a loaded fixture machine can miss it before any lock
// or commit is taken. Wait for readiness outside the assertion instead of
// letting an environment hiccup look like a behaviour regression.
func awaitApprovalWriterReady(t *testing.T, ctx context.Context, repository *PostgresRepository) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if err := repository.ApprovalWriterReady(ctx); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("approval writer never became ready")
		}
		time.Sleep(2 * time.Second)
	}
}

// seedZeroEffectRecoveryFailure drives a fresh proof-era request through the
// real approval, relay, delivery and execution path until it fails before any
// effect. The execution window is left untouched, so the caller decides when
// the candidate becomes eligible. It proves nothing on its own.
func seedZeroEffectRecoveryFailure(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, repository *PostgresRepository, relay *ApprovalRelayStore, vector authorizationProofVector, durable, failureCode string, sequence approvalconsumer.Sequence) ApprovalRequest {
	t.Helper()
	request := seedAuthorizationProofRequest(t, ctx, db, vector)
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "recovery-zero-effect-"+uuid.NewString())
	awaitApprovalWriterReady(t, ctx, repository)
	approved, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version,
		"APPROVED", "independent zero effect recovery proof", nil)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("approve zero-effect proof=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT event_id FROM public.outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, request.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	claims, err := relay.ClaimFrozen(ctx, "recovery-zero-effect-fixture", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
		t.Fatalf("zero-effect proof relay claim=%+v err=%v", claims, err)
	}
	if _, err := relay.ProcessFrozen(ctx, "recovery-zero-effect-fixture", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := repository.ResolveApprovalAuthorizationBinding(ctx, eventID, durable, approvalconsumer.BillingWorkload)
	if err != nil || binding.ApprovalRequestID != request.ID {
		t.Fatalf("zero-effect binding=%+v err=%v", binding, err)
	}
	state := observeApprovalDelivery(t, ctx, repository, binding, sequence)
	state, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, eventID, durable, binding.Workload, state.Version)
	if err != nil || state.DomainAttemptCount != 1 || state.State != "DOMAIN_INFLIGHT" {
		t.Fatalf("zero-effect grant=%+v err=%v", state, err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, request.ID)
	execution, err := repository.BeginApprovalExecutionWithAuthorization(ctx, request.ID, binding.Workload, eventID.String(), critical, financial, binding)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("zero-effect begin=%+v err=%v", execution, err)
	}
	if err := repository.CompleteApprovalExecution(ctx, request.ID, binding.Workload, eventID.String(), "FAILED",
		json.RawMessage(`{}`), json.RawMessage(`{}`), failureCode); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RecordApprovalAuthorizationDomainFailure(ctx, eventID, durable, binding.Workload, state.Version, failureCode); err != nil {
		t.Fatal(err)
	}
	return request
}

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
	awaitApprovalWriterReady(t, ctx, repository)
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
	// The zero-effect invalidation is the only terminal transition this worker
	// may trigger, so it must refuse everything the confirm path refuses and must
	// leave the request untouched while doing so.
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, stale, lease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("stale request version invalidated zero effect: %v", err)
	}
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, item, wrong); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("wrong recovery lease invalidated zero effect: %v", err)
	}
	undecided := item
	undecided.WindowElapsed = true
	undecided.Investigation = approvalconsumer.RecoveryAssessment{Class: approvalconsumer.FailureUnknown, QueryOnly: true, MaxDomainAttempts: 1}
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, undecided, lease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("unknown classification invalidated zero effect: %v", err)
	}
	var zeroEffectStatus, zeroEffectCode string
	if err := db.adminPool.QueryRow(ctx, `SELECT status,coalesce(invalidation_code,'') FROM approval_requests WHERE id=$1`, item.RequestID).
		Scan(&zeroEffectStatus, &zeroEffectCode); err != nil || zeroEffectStatus == "INVALIDATED" || zeroEffectCode != "" {
		t.Fatalf("refused zero-effect invalidation still mutated the request: status=%s code=%s err=%v", zeroEffectStatus, zeroEffectCode, err)
	}
	confirmation, err := json.Marshal(approvalRecoveryConfirmation{Candidate: item, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	serviceContext, err := serviceauth.ContextForJob(ctx, "account-approval-recovery", "APPROVAL_RECOVERY_QUERY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	confirmRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-recovery/confirm", bytes.NewReader(confirmation)).WithContext(serviceContext)
	confirmRequest.Header.Set("Content-Type", "application/json")
	confirmResponse := httptest.NewRecorder()
	reader := &fixedRecoveryResultReader{result: result}
	NewService(repository, nil).WithApprovalRecoveryResults(reader).internalConfirmApprovalRecovery(confirmResponse, confirmRequest)
	if confirmResponse.Code != http.StatusNoContent || reader.calls != 1 {
		t.Fatalf("independent original-result recheck did not converge: status=%d reads=%d body=%s", confirmResponse.Code, reader.calls, confirmResponse.Body.String())
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
	// A request that already converged to EXECUTED is terminal: the zero-effect
	// invalidation must refuse it and must not append a second state event, even
	// when the caller replays the original candidate and lease.
	var stateEventsBefore int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1`, request.ID).Scan(&stateEventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, item, lease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("an executed request was invalidated by the recovery path: %v", err)
	}
	var stateEventsAfter int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1`, request.ID).Scan(&stateEventsAfter); err != nil {
		t.Fatal(err)
	}
	if stateEventsAfter != stateEventsBefore {
		t.Fatalf("refused invalidation appended a state event: before=%d after=%d", stateEventsBefore, stateEventsAfter)
	}
	// The positive scenario: a definite pre-effect failure whose execution
	// window has elapsed and whose owner authorities keep reporting an absent
	// effect and no positive result must actually converge to INVALIDATED, and
	// must do so only through the governed Account surface.
	zeroEffectRequest := seedZeroEffectRecoveryFailure(t, ctx, db, repository, relay, vector, durable,
		"DOMAIN_RESOURCE_NOT_FOUND", 102)
	planZeroEffect := func() ApprovalRecoveryCandidate {
		t.Helper()
		plan, planErr := repository.PlanProofApprovalRecovery(ctx, 5)
		if planErr != nil {
			t.Fatal(planErr)
		}
		for _, candidate := range plan.Candidates {
			if candidate.RequestID != zeroEffectRequest.ID {
				continue
			}
			if candidate.Investigation.Class != approvalconsumer.FailurePermanent || candidate.Investigation.Retryable ||
				!candidate.Investigation.QueryOnly || candidate.Investigation.MaxDomainAttempts != 1 ||
				candidate.RequestStatus != "EXECUTION_FAILED" || candidate.ExecutionStatus != "FAILED" {
				t.Fatalf("zero-effect candidate is not eligible: %+v", candidate)
			}
			return candidate
		}
		t.Fatalf("zero-effect candidate missing from the recovery plan: %+v", plan)
		return ApprovalRecoveryCandidate{}
	}
	openCandidate := planZeroEffect()
	if openCandidate.WindowElapsed {
		t.Fatalf("seeded request had already left its execution window: %+v", openCandidate)
	}
	zeroEffectLease, err := store.Claim(ctx, openCandidate)
	if err != nil || zeroEffectLease.RequestID != zeroEffectRequest.ID || zeroEffectLease.Token == uuid.Nil {
		t.Fatalf("zero-effect query lease invalid: lease=%+v err=%v", zeroEffectLease, err)
	}
	// The window is rechecked from the request row under lock, so a candidate
	// that merely claims the window has elapsed must still be refused while the
	// row says otherwise - and the refusal must write nothing.
	forged := openCandidate
	forged.WindowElapsed = true
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, forged, zeroEffectLease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("a forged window claim invalidated the original execution: %v", err)
	}
	var openStatus, openCode string
	if err := db.adminPool.QueryRow(ctx, `SELECT status,coalesce(invalidation_code,'') FROM public.approval_requests WHERE id=$1`, zeroEffectRequest.ID).
		Scan(&openStatus, &openCode); err != nil || openStatus != "EXECUTION_FAILED" || openCode != "" {
		t.Fatalf("refused forged window claim mutated the request: status=%s code=%s err=%v", openStatus, openCode, err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET execution_deadline=now()-interval '1 second',updated_at=now() WHERE id=$1`, zeroEffectRequest.ID); err != nil {
		t.Fatal(err)
	}
	zeroEffectCandidate := planZeroEffect()
	if !zeroEffectCandidate.WindowElapsed {
		t.Fatalf("elapsed execution window was not observed: %+v", zeroEffectCandidate)
	}
	zeroEffectBody, err := json.Marshal(approvalRecoveryConfirmation{Candidate: zeroEffectCandidate, Lease: zeroEffectLease})
	if err != nil {
		t.Fatal(err)
	}
	postZeroEffect := func(effects approvaleffect.Reader, results ApprovalRecoveryResultReader) *httptest.ResponseRecorder {
		t.Helper()
		invalidateContext, contextErr := serviceauth.ContextForJob(ctx, "account-approval-recovery", "APPROVAL_RECOVERY_QUERY", uuid.NewString())
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		invalidateRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-recovery/zero-effect", bytes.NewReader(zeroEffectBody)).WithContext(invalidateContext)
		invalidateRequest.Header.Set("Content-Type", "application/json")
		invalidateResponse := httptest.NewRecorder()
		NewService(repository, nil).WithApprovalRecoveryResults(results).WithApprovalRecoveryEffects(effects).
			internalInvalidateApprovalRecoveryZeroEffect(invalidateResponse, invalidateRequest)
		return invalidateResponse
	}
	unconfirmed := &fixedRecoveryResultReader{result: RecoveryResultObservation{State: "UNCONFIRMED", ObservedAt: time.Now().UTC()}}
	var zeroEffectEventsBefore int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1`, zeroEffectRequest.ID).Scan(&zeroEffectEventsBefore); err != nil {
		t.Fatal(err)
	}
	var zeroEffectExecutionBefore, zeroEffectDeliveryBefore string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM public.approval_executions e WHERE e.approval_request_id=$1`, zeroEffectRequest.ID).Scan(&zeroEffectExecutionBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM account_approval_authorization.approval_delivery_states d WHERE d.original_event_id=$1`, zeroEffectCandidate.OriginalEventID).Scan(&zeroEffectDeliveryBefore); err != nil {
		t.Fatal(err)
	}
	// An observed effect is never invalidated, however definite the failure is.
	if response := postZeroEffect(&recoveryEffectStub{state: approvaleffect.StateObserved}, unconfirmed); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "APPROVAL_RECOVERY_EFFECT_OBSERVED") {
		t.Fatalf("observed effect did not stop the invalidation: status=%d body=%s", response.Code, response.Body.String())
	}
	// A positive domain result always wins over an absent effect.
	positive := &fixedRecoveryResultReader{result: RecoveryResultObservation{State: "SUCCEEDED", ObservedAt: time.Now().UTC(),
		DomainResult:     json.RawMessage(`{"adjustment_id":"` + uuid.NewString() + `","wallet_id":"` + uuid.NewString() + `","status":"POSTED","version":2}`),
		LedgerReferences: json.RawMessage(`{"journal_entry_id":"` + uuid.NewString() + `"}`)}}
	if response := postZeroEffect(&recoveryEffectStub{state: approvaleffect.StateAbsent}, positive); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "APPROVAL_RECOVERY_UNCONFIRMED") {
		t.Fatalf("positive result did not stop the invalidation: status=%d body=%s", response.Code, response.Body.String())
	}
	var untouchedStatus, untouchedCode string
	var untouchedEvents int64
	if err := db.adminPool.QueryRow(ctx, `SELECT status,coalesce(invalidation_code,'') FROM public.approval_requests WHERE id=$1`, zeroEffectRequest.ID).
		Scan(&untouchedStatus, &untouchedCode); err != nil {
		t.Fatal(err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1`, zeroEffectRequest.ID).Scan(&untouchedEvents); err != nil {
		t.Fatal(err)
	}
	if untouchedStatus != "EXECUTION_FAILED" || untouchedCode != "" || untouchedEvents != zeroEffectEventsBefore {
		t.Fatalf("refused zero-effect invalidations mutated the request: status=%s code=%s events=%d", untouchedStatus, untouchedCode, untouchedEvents)
	}
	// Only the fully proven case converges, and the Account service re-reads
	// both owner authorities itself before the single CAS.
	if response := postZeroEffect(&recoveryEffectStub{state: approvaleffect.StateAbsent}, unconfirmed); response.Code != http.StatusNoContent {
		t.Fatalf("proven zero effect did not converge: status=%d body=%s", response.Code, response.Body.String())
	}
	var provenStatus, provenCode string
	var provenDetails map[string]any
	if err := db.adminPool.QueryRow(ctx, `SELECT status,invalidation_code,invalidation_details FROM public.approval_requests WHERE id=$1`, zeroEffectRequest.ID).
		Scan(&provenStatus, &provenCode, &provenDetails); err != nil {
		t.Fatal(err)
	}
	if provenStatus != "INVALIDATED" || provenCode != "RECOVERY_ZERO_EFFECT_PROVEN" {
		t.Fatalf("proven zero effect landed as status=%s code=%s", provenStatus, provenCode)
	}
	if provenDetails["effect_observation"] != "ABSENT" || provenDetails["result_observation"] != "UNCONFIRMED" ||
		provenDetails["failure_class"] != string(approvalconsumer.FailurePermanent) || provenDetails["durable"] != durable ||
		provenDetails["original_event_id"] != zeroEffectCandidate.OriginalEventID.String() ||
		provenDetails["approval_epoch"] != float64(zeroEffectCandidate.ApprovalEpoch) {
		t.Fatalf("zero-effect evidence is incomplete: %+v", provenDetails)
	}
	var zeroEffectFrom, zeroEffectTo, zeroEffectActor, zeroEffectReason string
	if err := db.adminPool.QueryRow(ctx, `SELECT coalesce(from_status,''),to_status,actor_id,reason_code FROM public.approval_state_events
WHERE approval_request_id=$1 AND to_status='INVALIDATED'`, zeroEffectRequest.ID).Scan(&zeroEffectFrom, &zeroEffectTo, &zeroEffectActor, &zeroEffectReason); err != nil {
		t.Fatal(err)
	}
	if zeroEffectFrom != "EXECUTION_FAILED" || zeroEffectTo != "INVALIDATED" || zeroEffectActor != "account-approval-recovery" ||
		zeroEffectReason != "RECOVERY_ZERO_EFFECT_PROVEN" {
		t.Fatalf("zero-effect state event=%s -> %s by %s (%s)", zeroEffectFrom, zeroEffectTo, zeroEffectActor, zeroEffectReason)
	}
	// The transition is Account-only: no second domain attempt, no result, no
	// rewrite of the original execution or its delivery binding.
	var zeroEffectExecutionAfter, zeroEffectDeliveryAfter string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM public.approval_executions e WHERE e.approval_request_id=$1`, zeroEffectRequest.ID).Scan(&zeroEffectExecutionAfter); err != nil {
		t.Fatal(err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM account_approval_authorization.approval_delivery_states d WHERE d.original_event_id=$1`, zeroEffectCandidate.OriginalEventID).Scan(&zeroEffectDeliveryAfter); err != nil {
		t.Fatal(err)
	}
	if zeroEffectExecutionAfter != zeroEffectExecutionBefore || zeroEffectDeliveryAfter != zeroEffectDeliveryBefore {
		t.Fatalf("zero-effect invalidation rewrote domain state: execution=%s->%s delivery=%s->%s",
			zeroEffectExecutionBefore, zeroEffectExecutionAfter, zeroEffectDeliveryBefore, zeroEffectDeliveryAfter)
	}
	var zeroEffectExecutions int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_executions WHERE approval_request_id=$1`, zeroEffectRequest.ID).Scan(&zeroEffectExecutions); err != nil || zeroEffectExecutions != 1 {
		t.Fatalf("zero-effect invalidation created another execution: count=%d err=%v", zeroEffectExecutions, err)
	}
	// The same evidence must never converge twice, through either surface.
	if response := postZeroEffect(&recoveryEffectStub{state: approvaleffect.StateAbsent}, unconfirmed); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "APPROVAL_RECOVERY_CONFLICT") {
		t.Fatalf("replayed zero-effect evidence converged again: status=%d body=%s", response.Code, response.Body.String())
	}
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, zeroEffectCandidate, zeroEffectLease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("replayed zero-effect evidence reached the repository: %v", err)
	}
	var zeroEffectEventsAfter int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1`, zeroEffectRequest.ID).Scan(&zeroEffectEventsAfter); err != nil {
		t.Fatal(err)
	}
	if zeroEffectEventsAfter != zeroEffectEventsBefore+1 {
		t.Fatalf("zero-effect invalidation appended %d state events", zeroEffectEventsAfter-zeroEffectEventsBefore)
	}
	closed, err := repository.PlanProofApprovalRecovery(ctx, 5)
	if err != nil || len(closed.Candidates) != 0 {
		t.Fatalf("an invalidated request stayed in the recovery plan: %+v err=%v", closed, err)
	}
	// The failure classification is worker-supplied over the wire, so the CAS
	// must re-derive it from the persisted row under lock: `DOMAIN_PRECONDITION_REJECTED`
	// is a mixed code that classifies as UNKNOWN, and a candidate that merely
	// claims PERMANENT must never converge that execution.
	unknownRequest := seedZeroEffectRecoveryFailure(t, ctx, db, repository, relay, vector, durable,
		"DOMAIN_PRECONDITION_REJECTED", 103)
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET execution_deadline=now()-interval '1 second',updated_at=now() WHERE id=$1`, unknownRequest.ID); err != nil {
		t.Fatal(err)
	}
	var unknownCandidate ApprovalRecoveryCandidate
	unknownPlan, err := repository.PlanProofApprovalRecovery(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range unknownPlan.Candidates {
		if candidate.RequestID == unknownRequest.ID {
			unknownCandidate = candidate
		}
	}
	if unknownCandidate.RequestID == uuid.Nil || !unknownCandidate.WindowElapsed ||
		unknownCandidate.Investigation.Class != approvalconsumer.FailureUnknown {
		t.Fatalf("mixed-code failure is not a query-only UNKNOWN candidate: %+v", unknownCandidate)
	}
	unknownLease, err := store.Claim(ctx, unknownCandidate)
	if err != nil || unknownLease.RequestID != unknownRequest.ID {
		t.Fatalf("unknown-class query lease invalid: lease=%+v err=%v", unknownLease, err)
	}
	forgedClass := unknownCandidate
	forgedClass.Investigation.Class = approvalconsumer.FailurePermanent
	if err := repository.InvalidateOriginalApprovalZeroEffect(ctx, forgedClass, unknownLease); !errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
		t.Fatalf("a forged failure classification invalidated an UNKNOWN failure: %v", err)
	}
	var unknownStatus, unknownCode string
	var unknownEvents int64
	if err := db.adminPool.QueryRow(ctx, `SELECT status,coalesce(invalidation_code,'') FROM public.approval_requests WHERE id=$1`, unknownRequest.ID).
		Scan(&unknownStatus, &unknownCode); err != nil {
		t.Fatal(err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1 AND to_status='INVALIDATED'`, unknownRequest.ID).
		Scan(&unknownEvents); err != nil {
		t.Fatal(err)
	}
	if unknownStatus != "EXECUTION_FAILED" || unknownCode != "" || unknownEvents != 0 {
		t.Fatalf("refused classification forgery mutated the request: status=%s code=%s invalidated_events=%d", unknownStatus, unknownCode, unknownEvents)
	}
}
