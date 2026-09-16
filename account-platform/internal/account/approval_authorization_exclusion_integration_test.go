package account

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalAuthorizationExclusionSealsOnlyTerminalEffectFreeDeadLetterIntegration(t *testing.T) {
	db, _, store, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	var requestID uuid.UUID
	var digest string
	if err := db.adminPool.QueryRow(ctx, `SELECT o.aggregate_id,s.event_digest FROM public.outbox_events o
JOIN account_approval_authorization.approval_authorization_event_bytes s USING(event_id) WHERE o.event_id=$1`, eventID).Scan(&requestID, &digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=12,dead_lettered_at=transaction_timestamp(),last_error='EVENT_DELIVERY_FAILED' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET status='INVALIDATED',invalidation_code='EXECUTION_WINDOW_EXPIRED',
invalidation_details='{}',version=version+1,updated_at=transaction_timestamp() WHERE id=$1 AND status='APPROVED'`, requestID); err != nil {
		t.Fatal(err)
	}
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	var requestVersion int64
	if err := db.adminPool.QueryRow(ctx, `SELECT version FROM public.approval_requests WHERE id=$1`, requestID).Scan(&requestVersion); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.UnclassifiedDeadRows != 1 {
		t.Fatalf("pre-exclusion stats=%+v err=%v", stats, err)
	}
	var outboxBefore []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	auditID := uuid.Must(uuid.NewV7())
	verifiedAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.runtimePool.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,'approval.authorization-exclusion.effect-verified','approval_authorization_event',$2,'SUCCEEDED',$3,$3,
jsonb_build_object('request_id',$4::text,'event_digest',$5::text,'effect_absent',true),$6)`,
		auditID, eventID.String(), "exclusion-"+eventID.String(), requestID.String(), digest, verifiedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
		eventID, requestVersion, "CUTOVER_INVALIDATED_NO_EXECUTION", auditID); err != nil {
		t.Fatal(err)
	}
	// The exact replay is idempotent and does not rewrite the sealed control epoch.
	if _, err := db.runtimePool.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
		eventID, requestVersion, "CUTOVER_INVALIDATED_NO_EXECUTION", auditID); err != nil {
		t.Fatal(err)
	}
	stats, err = store.Stats(ctx)
	if err != nil || stats.UnclassifiedDeadRows != 0 || stats.Pending != 0 || stats.ActiveClaims != 0 || stats.ReclaimableClaims != 0 {
		t.Fatalf("sealed stats=%+v err=%v", stats, err)
	}
	var outboxAfter []byte
	var eventEpoch, sealedEpoch int64
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text,x.event_approval_epoch,x.sealed_at_control_approval_epoch
FROM public.outbox_events o JOIN account_approval_authorization.approval_authorization_delivery_exclusions x USING(event_id)
WHERE o.event_id=$1`, eventID).Scan(&outboxAfter, &eventEpoch, &sealedEpoch); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(outboxBefore, outboxAfter) || eventEpoch != 1 || sealedEpoch != 2 {
		t.Fatalf("exclusion rewrote history or epochs event=%d sealed=%d", eventEpoch, sealedEpoch)
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT event_id FROM account_approval_authorization.approval_authorization_delivery_exclusions WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("runtime read protected exclusion table directly")
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_authorization.approval_authorization_delivery_exclusions SET request_version=request_version+1 WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("exclusion was mutable")
	}
	if _, err := db.adminPool.Exec(ctx, `DELETE FROM account_approval_authorization.approval_authorization_delivery_exclusions WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("exclusion was deletable")
	}
	// Even a privileged-fixture corruption of a frozen request fact must make
	// the original dead letter visible to the readiness gate again.
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_requests DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET version=version+1 WHERE id=$1`, requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_requests ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	stats, err = store.Stats(ctx)
	if err != nil || stats.UnclassifiedDeadRows != 1 {
		t.Fatalf("mismatched exclusion was discounted: %+v err=%v", stats, err)
	}
}

func TestApprovalAuthorizationExclusionRejectsUnverifiedOrExecutedHistoryIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	var requestID uuid.UUID
	var digest string
	if err := db.adminPool.QueryRow(ctx, `SELECT o.aggregate_id,s.event_digest FROM public.outbox_events o
JOIN account_approval_authorization.approval_authorization_event_bytes s USING(event_id) WHERE o.event_id=$1`, eventID).Scan(&requestID, &digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=12,dead_lettered_at=transaction_timestamp(),last_error='EVENT_DELIVERY_FAILED' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET status='INVALIDATED',invalidation_code='EXECUTION_WINDOW_EXPIRED',
invalidation_details='{}',version=version+1,updated_at=transaction_timestamp() WHERE id=$1 AND status='APPROVED'`, requestID); err != nil {
		t.Fatal(err)
	}
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	var requestVersion int64
	if err := db.adminPool.QueryRow(ctx, `SELECT version FROM public.approval_requests WHERE id=$1`, requestID).Scan(&requestVersion); err != nil {
		t.Fatal(err)
	}
	missingAudit := uuid.Must(uuid.NewV7())
	if _, err := db.runtimePool.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
		eventID, requestVersion, "CUTOVER_INVALIDATED_NO_EXECUTION", missingAudit); err == nil {
		t.Fatal("missing effect verification was accepted")
	}
	auditID := uuid.Must(uuid.NewV7())
	if _, err := db.runtimePool.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,'approval.authorization-exclusion.effect-verified','approval_authorization_event',$2,'SUCCEEDED',$3,$3,
jsonb_build_object('request_id',$4::text,'event_digest',$5::text,'effect_absent',true),transaction_timestamp())`,
		auditID, eventID.String(), "exclusion-negative-"+eventID.String(), requestID.String(), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
		eventID, requestVersion, "SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION", auditID); err == nil {
		t.Fatal("SAFE exclusion reason was accepted while control was QUIESCING")
	}
	// Build an impossible production state with the privileged fixture so the
	// exclusion primitive independently proves it will not hide any execution.
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_executions DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `INSERT INTO public.approval_executions(id,approval_request_id,command_type,command_fingerprint,
execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at)
SELECT $1,r.id,r.command_type,r.command_fingerprint,'fixture:executed-history','billing-command-executor','fixture-job','EXECUTING','{}',
s.critical_resource_versions,s.financial_snapshot,transaction_timestamp() FROM public.approval_requests r
JOIN public.approval_request_snapshots s ON s.approval_request_id=r.id WHERE r.id=$2`, uuid.Must(uuid.NewV7()), requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_executions ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.runtimePool.Exec(ctx, `SELECT public.seal_approval_authorization_exclusion($1,$2,$3,$4)`,
		eventID, requestVersion, "CUTOVER_INVALIDATED_NO_EXECUTION", auditID); err == nil {
		t.Fatal("history with an execution was excluded")
	}
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM account_approval_authorization.approval_authorization_delivery_exclusions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected exclusion persisted count=%d err=%v", count, err)
	}
}
