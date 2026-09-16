package account

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestApprovalConsumerDeliveryStateReaderIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	state := observeApprovalDelivery(t, ctx, repository, binding, 71)
	read, err := repository.ApprovalAuthorizationDeliveryState(ctx, binding.EventID, binding.Durable, binding.Workload)
	if err != nil || read != state || read.LastErrorCode != nil {
		t.Fatalf("initial state read=%+v want=%+v err=%v", read, state, err)
	}
	state, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	var critical, financial json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM public.approval_request_snapshots WHERE approval_request_id=$1`, binding.ApprovalRequestID).Scan(&critical, &financial); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), critical, financial, binding); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_PRECONDITION_REJECTED"); err != nil {
		t.Fatal(err)
	}
	read, err = repository.ApprovalAuthorizationDeliveryState(ctx, binding.EventID, binding.Durable, binding.Workload)
	if err != nil || read.State != "DOMAIN_INFLIGHT" || read.LastErrorCode != nil || read.ExecutionStatus == nil || *read.ExecutionStatus != "FAILED" || read.ExecutionErrorClass == nil || *read.ExecutionErrorClass != "DOMAIN_PRECONDITION_REJECTED" {
		t.Fatalf("completed failure state read=%+v err=%v", read, err)
	}
	state, err = repository.RecordApprovalAuthorizationDomainFailure(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version, "DOMAIN_PRECONDITION_REJECTED")
	if err != nil {
		t.Fatal(err)
	}
	read, err = repository.ApprovalAuthorizationDeliveryState(ctx, binding.EventID, binding.Durable, binding.Workload)
	if err != nil || read.State != "READY" || read.Version != state.Version || read.LastErrorCode == nil || *read.LastErrorCode != "DOMAIN_PRECONDITION_REJECTED" {
		t.Fatalf("failure state read=%+v err=%v", read, err)
	}
	var before, after string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::TEXT FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		eventID  uuid.UUID
		durable  string
		workload string
	}{
		{uuid.New(), binding.Durable, binding.Workload},
		{binding.EventID, binding.Durable, "token-command-executor"},
		{binding.EventID, "token-model-lifecycle-executor-v1", binding.Workload},
	} {
		if _, err := repository.ApprovalAuthorizationDeliveryState(ctx, candidate.eventID, candidate.durable, candidate.workload); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
			t.Fatalf("invalid delivery identity was enumerable: %+v err=%v", candidate, err)
		}
	}
	if _, err := db.adminPool.Exec(ctx, `SELECT * FROM public.read_approval_authorization_delivery_state($1,$2,$3)`, binding.EventID, binding.Durable, binding.Workload); postgresSQLState(err) != "42501" {
		t.Fatalf("wrong session reader SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(d)::TEXT FROM account_approval_authorization.approval_delivery_states d WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).Scan(&after); err != nil || after != before {
		t.Fatalf("read probes changed state before=%s after=%s err=%v", before, after, err)
	}
}

func TestProofEraBillingFailureRemainsQueryOnlyIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("apply forward-only recovery default: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
	var ownerCanCreate bool
	if err := db.adminPool.QueryRow(ctx, `SELECT has_schema_privilege('account_approval_authorization_owner','public','CREATE')`).Scan(&ownerCanCreate); err != nil || ownerCanCreate {
		t.Fatalf("migration leaked public CREATE to approval owner: granted=%v err=%v", ownerCanCreate, err)
	}
	legacy := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	observeApprovalDelivery(t, ctx, repository, legacy, 71)
	// Exercise the DB transition in isolation, with the same event identity but
	// a second test-only proof-era delivery row. This is not a claim that the
	// legacy outbox message itself was converted to proof-era wire.
	proof := legacy
	proof.Durable = "billing-wallet-adjustment-executor-p1-v1"
	proof.SerializationVersion = "APPROVAL_RFC8785_DECIMAL_V1"
	_, err = db.adminPool.Exec(ctx, `INSERT INTO account_approval_authorization.approval_delivery_states
	(schema_version,original_event_id,durable,approval_request_id,approved_request_version,event_digest,digest_kind,serialization_version,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,state)
	SELECT schema_version,original_event_id,$2,approval_request_id,approved_request_version,event_digest,digest_kind,$3,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,'READY'
	FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$4`, legacy.EventID, proof.Durable, proof.SerializationVersion, legacy.Durable)
	if err != nil {
		t.Fatal(err)
	}
	state, err := repository.ApprovalAuthorizationDeliveryState(ctx, proof.EventID, proof.Durable, proof.Workload)
	if err != nil || state.State != "READY" {
		t.Fatalf("proof delivery fixture=%+v err=%v", state, err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_authorization.approval_delivery_states SET last_error_code='DOMAIN_REVISION_CHANGED',version=version+1,updated_at=now() WHERE original_event_id=$1 AND durable=$2`, proof.EventID, proof.Durable); err != nil {
		t.Fatal(err)
	}
	state, err = repository.ApprovalAuthorizationDeliveryState(ctx, proof.EventID, proof.Durable, proof.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, proof.EventID, proof.Durable, proof.Workload, state.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("preexisting proof READY failure was retried: %v", err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_authorization.approval_delivery_states SET last_error_code=NULL,version=version+1,updated_at=now() WHERE original_event_id=$1 AND durable=$2`, proof.EventID, proof.Durable); err != nil {
		t.Fatal(err)
	}
	state, err = repository.ApprovalAuthorizationDeliveryState(ctx, proof.EventID, proof.Durable, proof.Workload)
	if err != nil {
		t.Fatal(err)
	}
	state, err = repository.GrantApprovalAuthorizationDomainAttempt(ctx, proof.EventID, proof.Durable, proof.Workload, state.Version)
	if err != nil || state.DomainAttemptCount != 1 || state.State != "DOMAIN_INFLIGHT" {
		t.Fatalf("first proof attempt=%+v err=%v", state, err)
	}
	state, err = repository.RecordApprovalAuthorizationDomainFailure(ctx, proof.EventID, proof.Durable, proof.Workload, state.Version, "DOMAIN_REVISION_CHANGED")
	if err != nil || state.State != "DOMAIN_INFLIGHT" || state.DomainAttemptCount != 1 {
		t.Fatalf("proof failure became retryable: %+v err=%v", state, err)
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, proof.EventID, proof.Durable, proof.Workload, state.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("proof failure permitted a second domain call: %v", err)
	}
	if read, err := repository.ApprovalAuthorizationDeliveryState(ctx, proof.EventID, proof.Durable, proof.Workload); err != nil || read.State != "DOMAIN_INFLIGHT" || read.LastErrorCode == nil || *read.LastErrorCode != "DOMAIN_REVISION_CHANGED" {
		t.Fatalf("proof failure lost query-only evidence: %+v err=%v", read, err)
	}
}

func TestProofEraHistoricalReadyFailureIsFencedByMigrationIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	boundary := -1
	for index, item := range loaded {
		if item.Version == 65 {
			boundary = index + 1
			break
		}
	}
	if boundary < 1 || boundary == len(loaded) || loaded[boundary].Version != 66 {
		t.Fatal("proof-era migration boundary is incomplete")
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded[:boundary]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection.Conn())
	legacy := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	observeApprovalDelivery(t, ctx, repository, legacy, 81)
	proofDurable := "billing-wallet-adjustment-executor-p1-v1"
	_, err = db.adminPool.Exec(ctx, `INSERT INTO account_approval_authorization.approval_delivery_states
	(schema_version,original_event_id,durable,approval_request_id,approved_request_version,event_digest,digest_kind,serialization_version,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,state,domain_attempt_count,last_error_code)
	SELECT schema_version,original_event_id,$2,approval_request_id,approved_request_version,event_digest,digest_kind,'APPROVAL_RFC8785_DECIMAL_V1',provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,'READY',1,'DOMAIN_REVISION_CHANGED'
	FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$3`, legacy.EventID, proofDurable, legacy.Durable)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("apply proof retry fence: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
	read, err := repository.ApprovalAuthorizationDeliveryState(ctx, legacy.EventID, proofDurable, legacy.Workload)
	if err != nil || read.State != "DOMAIN_INFLIGHT" || read.DomainAttemptCount != 1 || read.Version != 2 || read.LastErrorCode == nil || *read.LastErrorCode != "DOMAIN_REVISION_CHANGED" {
		t.Fatalf("historical failure was not fenced: %+v err=%v", read, err)
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, legacy.EventID, proofDurable, legacy.Workload, read.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("historical failure was retried after migration: %v", err)
	}
	legacyState, err := repository.ApprovalAuthorizationDeliveryState(ctx, legacy.EventID, legacy.Durable, legacy.Workload)
	if err != nil || legacyState.State != "READY" || legacyState.DomainAttemptCount != 0 {
		t.Fatalf("legacy delivery was changed by proof fence: %+v err=%v", legacyState, err)
	}
}

func TestProofEraHistoricalAttemptWithoutErrorIsFencedByMigrationIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	boundary := -1
	for index, item := range loaded {
		if item.Version == 66 {
			boundary = index + 1
			break
		}
	}
	if boundary < 1 || boundary == len(loaded) || loaded[boundary].Version != 67 {
		t.Fatal("proof-era single-attempt migration boundary is incomplete")
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded[:boundary]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection.Conn())
	legacy := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	observeApprovalDelivery(t, ctx, repository, legacy, 81)
	proofDurable := "billing-wallet-adjustment-executor-p1-v1"
	_, err = db.adminPool.Exec(ctx, `INSERT INTO account_approval_authorization.approval_delivery_states
	(schema_version,original_event_id,durable,approval_request_id,approved_request_version,event_digest,digest_kind,serialization_version,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,state,domain_attempt_count)
	SELECT schema_version,original_event_id,$2,approval_request_id,approved_request_version,event_digest,digest_kind,'APPROVAL_RFC8785_DECIMAL_V1',provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,'READY',1
	FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$3`, legacy.EventID, proofDurable, legacy.Durable)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("apply proof single-attempt fence: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
	read, err := repository.ApprovalAuthorizationDeliveryState(ctx, legacy.EventID, proofDurable, legacy.Workload)
	if err != nil || read.State != "DOMAIN_INFLIGHT" || read.DomainAttemptCount != 1 || read.Version != 2 || read.LastErrorCode != nil {
		t.Fatalf("historical attempt without error was not fenced: %+v err=%v", read, err)
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, legacy.EventID, proofDurable, legacy.Workload, read.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("historical attempt was retried: %v", err)
	}
	// Even a later mixed-version writer putting the row back into READY cannot
	// obtain another grant from Account, independent of Billing's local guard.
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_authorization.approval_delivery_states SET state='READY',version=version+1,updated_at=now() WHERE original_event_id=$1 AND durable=$2`, legacy.EventID, proofDurable); err != nil {
		t.Fatal(err)
	}
	read, err = repository.ApprovalAuthorizationDeliveryState(ctx, legacy.EventID, proofDurable, legacy.Workload)
	if err != nil || read.State != "READY" {
		t.Fatalf("synthetic mixed-version state: %+v err=%v", read, err)
	}
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, legacy.EventID, proofDurable, legacy.Workload, read.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("direct proof regrant was accepted: %v", err)
	}
	legacyState, err := repository.ApprovalAuthorizationDeliveryState(ctx, legacy.EventID, legacy.Durable, legacy.Workload)
	if err != nil || legacyState.State != "READY" || legacyState.DomainAttemptCount != 0 {
		t.Fatalf("legacy delivery changed: %+v err=%v", legacyState, err)
	}
}

func TestApprovalConsumerLegacyReadyResultRecoveryIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	state := observeApprovalDelivery(t, ctx, repository, binding, 81)
	var critical, financial json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM public.approval_request_snapshots WHERE approval_request_id=$1`, binding.ApprovalRequestID).Scan(&critical, &financial); err != nil {
		t.Fatal(err)
	}
	execution, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), critical, financial)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("legacy execution=%+v err=%v", execution, err)
	}
	if err := repository.CompleteApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), "EXECUTED", json.RawMessage(`{"legacy":true}`), json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	recovered, err := repository.RecordApprovalAuthorizationResult(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil || recovered.State != "RESULT_CONFIRMED" || recovered.DomainAttemptCount != 0 {
		t.Fatalf("legacy result recovery=%+v err=%v", recovered, err)
	}

	other := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	otherState := observeApprovalDelivery(t, ctx, repository, other, 82)
	if err := db.adminPool.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM public.approval_request_snapshots WHERE approval_request_id=$1`, other.ApprovalRequestID).Scan(&critical, &financial); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginApprovalExecution(ctx, other.ApprovalRequestID, other.Workload, "wrong-job", critical, financial); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteApprovalExecution(ctx, other.ApprovalRequestID, other.Workload, "wrong-job", "EXECUTED", json.RawMessage(`{}`), json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	wrong, err := repository.ApprovalAuthorizationDeliveryState(ctx, other.EventID, other.Durable, other.Workload)
	if err != nil || wrong.ExecutionStatus != nil || wrong.ExecutionErrorClass != nil {
		t.Fatalf("wrong execution identity was exposed by state reader: state=%+v err=%v", wrong, err)
	}
	if _, err := repository.RecordApprovalAuthorizationResult(ctx, other.EventID, other.Durable, other.Workload, otherState.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("wrong job identity recovered READY result: %v", err)
	}
}

func TestApprovalConsumerSerializationBindingDownForbiddenIntegration(t *testing.T) {
	db, _, _, loaded := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	before := approvalConsumerSecuritySnapshot(t, ctx, db)
	err := migrations.DownOne(ctx, connection.Conn(), loaded)
	if postgresSQLState(err) != "55000" || postgresSQLMessage(err) != "000060 approval exclusion reason binding is forward-only; database down is forbidden" {
		t.Fatalf("unexpected 000060 down result state=%s message=%s err=%v", postgresSQLState(err), postgresSQLMessage(err), err)
	}
	after := approvalConsumerSecuritySnapshot(t, ctx, db)
	if before != after {
		t.Fatalf("forbidden down changed protected facts before=%s after=%s", before, after)
	}
}

func approvalConsumerSecuritySnapshot(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase) string {
	t.Helper()
	var snapshot string
	if err := db.adminPool.QueryRow(ctx, `SELECT jsonb_build_object(
 'version',(SELECT max(version) FROM public.schema_migrations),
 'reader',to_regprocedure('public.read_approval_authorization_delivery_state(uuid,text,text)')::TEXT,
 'result_owner',(SELECT pg_catalog.pg_get_userbyid(proowner) FROM pg_catalog.pg_proc WHERE oid='public.record_approval_authorization_result(uuid,text,text,bigint)'::regprocedure),
 'reader_runtime',pg_catalog.has_function_privilege('account_service','public.read_approval_authorization_delivery_state(uuid,text,text)','EXECUTE'),
 'reader_public',pg_catalog.has_function_privilege('public','public.read_approval_authorization_delivery_state(uuid,text,text)','EXECUTE'),
 'rows',(SELECT COALESCE(jsonb_agg(to_jsonb(d) ORDER BY d.original_event_id,d.durable),'[]'::jsonb) FROM account_approval_authorization.approval_delivery_states d)
)::TEXT`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
