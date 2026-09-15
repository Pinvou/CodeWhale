package account

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func publishedApprovalDeliveryBinding(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, repository *PostgresRepository, relay *ApprovalRelayStore, durable string) ApprovalAuthorizationBinding {
	t.Helper()
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "consumer-binding-approver-"+uuid.NewString())
	approved, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version,
		"APPROVED", "independent approval consumer binding", request.CriticalResourceVersions)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("approve authorization fixture status=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT event_id FROM public.outbox_events
WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, request.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	claims, err := relay.ClaimFrozen(ctx, "consumer-binding-fixture", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
		t.Fatalf("authorization relay claims=%+v err=%v", claims, err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "consumer-binding-fixture", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatalf("publish authorization fixture disposition=%s err=%v", disposition, err)
	}
	binding, err := repository.ResolveApprovalAuthorizationBinding(ctx, eventID, durable, "billing-command-executor")
	if err != nil {
		var diagnostic ApprovalAuthorizationBinding
		diagnosticErr := db.runtimePool.QueryRow(ctx, `SELECT approval_request_id,approved_request_version,approval_epoch,serialization_version,
provenance,event_digest,digest_kind,command_type,policy_version,executor_identity
FROM public.resolve_approval_authorization_binding($1,$2,$3)`, eventID, durable, "billing-command-executor").Scan(
			&diagnostic.ApprovalRequestID, &diagnostic.ApprovedRequestVersion, &diagnostic.ApprovalEpoch,
			&diagnostic.SerializationVersion, &diagnostic.Provenance, &diagnostic.EventDigest, &diagnostic.DigestKind,
			&diagnostic.CommandType, &diagnostic.PolicyVersion, &diagnostic.ExecutorIdentity,
		)
		var postgresError *pgconn.PgError
		if errors.As(diagnosticErr, &postgresError) {
			t.Logf("binding resolution SQLSTATE=%s routine=%s message=%s", postgresError.Code, postgresError.Routine, postgresError.Message)
		} else {
			t.Logf("binding resolution classified_error=%t", diagnosticErr != nil)
		}
		t.Fatal(err)
	}
	return binding
}

func observeApprovalDelivery(t *testing.T, ctx context.Context, repository *PostgresRepository, binding ApprovalAuthorizationBinding, sequence approvalconsumer.Sequence) ApprovalDeliveryState {
	t.Helper()
	state, err := repository.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{
		Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestApprovalAuthorizationBindingBeginIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	state := observeApprovalDelivery(t, ctx, repository, binding, 41)
	if state != (ApprovalDeliveryState{State: "READY", Version: 1}) {
		t.Fatalf("initial state=%+v", state)
	}
	state, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil || state.DomainAttemptCount != 1 || state.State != "DOMAIN_INFLIGHT" || state.Version != 2 {
		t.Fatalf("attempt grant state=%+v err=%v", state, err)
	}
	var criticalVersions, financialSnapshot json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT s.critical_resource_versions,s.financial_snapshot
FROM public.approval_request_snapshots s WHERE s.approval_request_id=$1`, binding.ApprovalRequestID).Scan(&criticalVersions, &financialSnapshot); err != nil {
		t.Fatal(err)
	}
	execution, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "authorization-job-1", criticalVersions, financialSnapshot, binding)
	if err != nil || !execution.FirstClaim || execution.AuthorizationEventID == nil || *execution.AuthorizationEventID != binding.EventID ||
		execution.AuthorizationDurable != binding.Durable || execution.AuthorizationEventDigest != binding.EventDigest {
		t.Fatalf("bound execution=%+v err=%v", execution, err)
	}
	repeated, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "authorization-job-1", criticalVersions, financialSnapshot, binding)
	if err != nil || repeated.FirstClaim || repeated.ExecutionIdempotencyKey != execution.ExecutionIdempotencyKey ||
		repeated.AuthorizationEventID == nil || *repeated.AuthorizationEventID != binding.EventID {
		t.Fatalf("repeated bound execution=%+v err=%v", repeated, err)
	}
	wrong := binding
	wrong.EventDigest = "0" + binding.EventDigest[1:]
	if binding.EventDigest[0] == '0' {
		wrong.EventDigest = "1" + binding.EventDigest[1:]
	}
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "authorization-job-1", criticalVersions, financialSnapshot, wrong); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("wrong binding error=%v", err)
	}
	if err := repository.CompleteApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "authorization-job-1", "EXECUTED", json.RawMessage(`{"applied":true}`), json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	state, err = repository.RecordApprovalAuthorizationResult(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil || state.State != "RESULT_CONFIRMED" || state.Version != 3 {
		t.Fatalf("result state=%+v err=%v", state, err)
	}
	state, err = repository.RecordApprovalAuthorizationTerminal(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil || state.State != "TERMINAL_CONFIRMED" || state.Version != 4 {
		t.Fatalf("terminal state=%+v err=%v", state, err)
	}
	redelivery := observeApprovalDelivery(t, ctx, repository, binding, 99)
	if redelivery != state {
		t.Fatalf("redelivery state=%+v want=%+v", redelivery, state)
	}
	var anchoredSequence int64
	if err := db.adminPool.QueryRow(ctx, `SELECT stream_sequence FROM account_approval_authorization.approval_delivery_states
WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&anchoredSequence); err != nil || anchoredSequence != 41 {
		t.Fatalf("anchored sequence=%d err=%v", anchoredSequence, err)
	}
}

func TestBillingApprovalDeliveryAttemptLimitIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	state := observeApprovalDelivery(t, ctx, repository, binding, 51)
	for attempt := int16(1); attempt <= 12; attempt++ {
		var err error
		state, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
		if err != nil || state.DomainAttemptCount != attempt || state.State != "DOMAIN_INFLIGHT" {
			t.Fatalf("grant attempt=%d state=%+v err=%v", attempt, state, err)
		}
		state, err = repository.RecordApprovalAuthorizationDomainFailure(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version, "DOMAIN_REJECTED")
		wantState := "READY"
		if attempt == 12 {
			wantState = "DOMAIN_INFLIGHT"
		}
		if err != nil || state.DomainAttemptCount != attempt || state.State != wantState {
			t.Fatalf("failure attempt=%d state=%+v err=%v", attempt, state, err)
		}
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("thirteenth attempt error=%v", err)
	}
	messageID := binding.EventID.String() + ":" + binding.Durable + ":dead-letter"
	state, err := repository.RecordApprovalAuthorizationDLQPubAck(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version, messageID, 88)
	if err != nil || state.State != "DLQ_PUBLISHED_ACK_PENDING" || state.DomainAttemptCount != 12 {
		t.Fatalf("DLQ PubAck state=%+v err=%v", state, err)
	}
	state, err = repository.RecordApprovalAuthorizationTerminal(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil || state.State != "TERMINAL_CONFIRMED" || state.DomainAttemptCount != 12 {
		t.Fatalf("DLQ terminal state=%+v err=%v", state, err)
	}
	if _, err := repository.RecordApprovalAuthorizationDomainFailure(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version, "DOMAIN_REJECTED"); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("terminal state revived: %v", err)
	}
}

func TestApprovalAuthorizationBindingRejectsUnpublishedIntegration(t *testing.T) {
	db, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	if _, err := repository.ResolveApprovalAuthorizationBinding(ctx, eventID, "billing-wallet-adjustment-executor-p1-v1", "billing-command-executor"); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("unpublished binding error=%v", err)
	}
	var count int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM account_approval_authorization.approval_delivery_states`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unpublished delivery rows=%d err=%v", count, err)
	}
}

func TestApprovalAuthorizationBindingRejectsUnknownPhysicalIdentity(t *testing.T) {
	if validApprovalAuthorizationLookup(uuid.New(), " billing-wallet-adjustment-executor-p1-v1", "billing-command-executor") ||
		validApprovalAuthorizationLookup(uuid.New(), "billing-wallet-adjustment-executor-p1-v1", "billing-command-executor\n") {
		t.Fatal("noncanonical physical delivery identity was accepted")
	}
}

func TestApprovalDeliverySequenceUint64Integration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, sequence := range []approvalconsumer.Sequence{
		1,
		approvalconsumer.Sequence(9007199254740993),
		approvalconsumer.Sequence(9223372036854775808),
		approvalconsumer.Sequence(^uint64(0)),
	} {
		binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
		state := observeApprovalDelivery(t, ctx, repository, binding, sequence)
		if state != (ApprovalDeliveryState{State: "READY", Version: 1}) {
			t.Fatalf("sequence=%s state=%+v", sequence.String(), state)
		}
		var stored string
		if err := db.adminPool.QueryRow(ctx, `SELECT stream_sequence::TEXT FROM account_approval_authorization.approval_delivery_states
WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&stored); err != nil || stored != sequence.String() {
			t.Fatalf("sequence=%s stored=%s err=%v", sequence.String(), stored, err)
		}
	}

	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	for _, invalid := range []string{"0", "-1", "1.5", "18446744073709551616", "NaN", "Infinity"} {
		_, err := db.runtimePool.Exec(ctx, `SELECT * FROM public.observe_approval_authorization_delivery(
$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::NUMERIC,$12,$13,$14)`,
			binding.EventID, binding.Durable, binding.Workload, binding.EventDigest, binding.DigestKind,
			binding.SerializationVersion, binding.Provenance, binding.ApprovedRequestVersion, binding.ApprovalEpoch,
			"ACCOUNT_PLATFORM", invalid, binding.CommandType, binding.PolicyVersion, binding.ExecutorIdentity)
		if postgresSQLState(err) != "42501" {
			t.Fatalf("invalid sequence=%s state=%s err=%v", invalid, postgresSQLState(err), err)
		}
	}
	var rows int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM account_approval_authorization.approval_delivery_states
WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("invalid sequences changed state rows=%d err=%v", rows, err)
	}

	dlqBinding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	dlqState := observeApprovalDelivery(t, ctx, repository, dlqBinding, approvalconsumer.Sequence(91))
	dlqMessageID := dlqBinding.EventID.String() + ":" + dlqBinding.Durable + ":dead-letter"
	maximum := approvalconsumer.Sequence(^uint64(0))
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		dlqState, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, dlqBinding.EventID, dlqBinding.Durable, dlqBinding.Workload, dlqState.Version)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 11 {
			var before, after string
			if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::TEXT
FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`,
				dlqBinding.EventID, dlqBinding.Durable).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.RecordApprovalAuthorizationDLQPubAck(ctx, dlqBinding.EventID, dlqBinding.Durable, dlqBinding.Workload, dlqState.Version, dlqMessageID, maximum); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
				t.Fatalf("twelfth unconfirmed attempt admitted DLQ err=%v", err)
			}
			if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::TEXT
FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`,
				dlqBinding.EventID, dlqBinding.Durable).Scan(&after); err != nil || after != before {
				t.Fatalf("rejected unconfirmed DLQ changed row=%t err=%v", after != before, err)
			}
		}
		dlqState, err = repository.RecordApprovalAuthorizationDomainFailure(ctx, dlqBinding.EventID, dlqBinding.Durable, dlqBinding.Workload, dlqState.Version, "DOMAIN_REJECTED")
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{"0", "1.5", "18446744073709551616", "NaN", "Infinity"} {
		_, err := db.runtimePool.Exec(ctx, `SELECT * FROM public.record_approval_authorization_dlq_puback(
$1,$2,$3,$4,$5,$6::NUMERIC)`, dlqBinding.EventID, dlqBinding.Durable, dlqBinding.Workload, dlqState.Version, dlqMessageID, invalid)
		if postgresSQLState(err) != "42501" {
			t.Fatalf("invalid DLQ sequence=%s state=%s err=%v", invalid, postgresSQLState(err), err)
		}
		var persisted ApprovalDeliveryState
		if err := db.adminPool.QueryRow(ctx, `SELECT domain_attempt_count,state,version
FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2`,
			dlqBinding.EventID, dlqBinding.Durable).Scan(&persisted.DomainAttemptCount, &persisted.State, &persisted.Version); err != nil || persisted != dlqState {
			t.Fatalf("invalid DLQ sequence changed CAS state=%+v want=%+v err=%v", persisted, dlqState, err)
		}
	}
	dlqState, err = repository.RecordApprovalAuthorizationDLQPubAck(ctx, dlqBinding.EventID, dlqBinding.Durable, dlqBinding.Workload, dlqState.Version, dlqMessageID, maximum)
	if err != nil || dlqState.State != "DLQ_PUBLISHED_ACK_PENDING" {
		t.Fatalf("maximum DLQ sequence state=%+v err=%v", dlqState, err)
	}
	var storedDLQ, storedError string
	if err := db.adminPool.QueryRow(ctx, `SELECT dlq_stream_sequence::TEXT,last_error_code
FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2`,
		dlqBinding.EventID, dlqBinding.Durable).Scan(&storedDLQ, &storedError); err != nil || storedDLQ != maximum.String() || storedError != "DOMAIN_REJECTED" {
		t.Fatalf("maximum DLQ sequence stored=%s error=%s err=%v", storedDLQ, storedError, err)
	}
}
