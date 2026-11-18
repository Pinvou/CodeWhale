package token

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Consume the actual operator carrier's DO body. Test inputs use transaction-local
// settings just as the psql preamble does; no copy of the guarded SQL is tested.
func assertEmergencyCaseOnlyOps(t *testing.T, f modelExpiryObservationFixture, caseID, taskID, resourceID uuid.UUID) {
	t.Helper()
	raw, err := os.ReadFile("../../ops/close-emergency-cancellation-case.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "-- BEGIN CASE-ONLY BODY\n")
	if len(parts) != 2 {
		t.Fatal("operator carrier body marker missing")
	}
	end := strings.Split(parts[1], "-- END CASE-ONLY BODY")
	if len(end) != 2 {
		t.Fatal("operator carrier end marker missing")
	}
	body := end[0]
	cfg := f.r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_migrator"
	cfg.Password = ""
	operator, err := cifixture.ConnectConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	var evidenceID uuid.UUID
	if err = f.r.pool.QueryRow(f.ctx, `SELECT id FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='MANUAL_REVIEW'`, taskID).Scan(&evidenceID); err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	values := map[string]string{"expected_database": f.r.pool.Config().ConnConfig.Database, "case_id": caseID.String(), "task_id": taskID.String(), "resource_id": resourceID.String(), "evidence_id": evidenceID.String(), "actor_id": actor.String(), "reason": "controlled operator evidence review completed"}
	facts := func() string {
		var value string
		if err := f.r.pool.QueryRow(f.ctx, `SELECT jsonb_build_object('usage',to_jsonb(u),'task',to_jsonb(c),'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM model_emergency_cancellation_evidence e WHERE emergency_cancellation_id=$2),'usage_evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM usage_cancellation_evidence e WHERE usage_request_id=$1),'convergence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM usage_convergence_evidence e WHERE usage_request_id=$1))::text FROM usage_requests u JOIN model_emergency_cancellations c ON c.usage_request_id=u.id WHERE u.id=$1 AND c.id=$2`, resourceID, taskID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	snapshot := func() string {
		var value string
		if err := f.r.pool.QueryRow(f.ctx, `SELECT jsonb_build_object('case',to_jsonb(c),'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM manual_review_case_events e WHERE case_id=$1))::text FROM manual_review_cases c WHERE c.id=$1`, caseID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	// A real overdue candidate must be selected before closure, not a future SLA.
	if _, err = f.r.pool.Exec(f.ctx, `UPDATE manual_review_cases SET resolution_due_at=clock_timestamp()-interval '1 second' WHERE id=$1`, caseID); err != nil {
		t.Fatal(err)
	}
	otherCases := func() string {
		var value string
		if err := f.r.pool.QueryRow(f.ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'type',case_type,'status',status,'resolved_at',resolved_at) ORDER BY id),'[]'::jsonb)::text FROM manual_review_cases WHERE id<>$1`, caseID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	otherBefore := otherCases()
	var financialCase uuid.UUID
	if err = f.r.pool.QueryRow(f.ctx, `SELECT id FROM manual_review_cases WHERE resource_id=$1 AND case_type='USAGE_RECONCILIATION_REVIEW'`, resourceID).Scan(&financialCase); err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	before, fundsBefore, caseBefore := facts(), f05CancellationClosureFundsFacts(t, f.bp, f.ctx), snapshot()
	invoke := func(params map[string]string, runtime bool) (pgx.Tx, error) {
		var tx pgx.Tx
		var err error
		if runtime {
			tx, err = f.r.pool.Begin(f.ctx)
		} else {
			tx, err = operator.Begin(f.ctx)
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_gateway`); err != nil {
			tx.Rollback(f.ctx)
			return nil, err
		}
		for k, v := range params {
			if _, err = tx.Exec(f.ctx, `SELECT set_config($1,$2,true)`, "pinvou.ops."+k, v); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
		}
		if params["case_type_probe"] != "" {
			if _, err = tx.Exec(f.ctx, `UPDATE manual_review_cases SET case_type='CONTROLLED_WRONG_CASE' WHERE id=$1`, caseID); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
		}
		if params["resource_type_probe"] != "" {
			if _, err = tx.Exec(f.ctx, `UPDATE manual_review_cases SET resource_type='CONTROLLED_WRONG_RESOURCE' WHERE id=$1`, caseID); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
		}
		if params["task_state_probe"] != "" {
			if _, err = tx.Exec(f.ctx, `UPDATE model_emergency_cancellations SET status='PENDING' WHERE id=$1`, taskID); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
		}
		if params["audit_permission_probe"] != "" {
			if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner; REVOKE INSERT ON manual_review_case_events FROM token_gateway; SET LOCAL ROLE token_gateway`); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
		}
		if params["shadow_path_probe"] != "" {
			if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner;
 CREATE TEMP TABLE model_emergency_cancellations (LIKE public.model_emergency_cancellations INCLUDING ALL);
 CREATE TEMP TABLE manual_review_cases (LIKE public.manual_review_cases INCLUDING ALL);
 CREATE TEMP TABLE model_emergency_cancellation_evidence (LIKE public.model_emergency_cancellation_evidence INCLUDING ALL);
 CREATE TEMP TABLE manual_review_case_events (LIKE public.manual_review_case_events INCLUDING ALL);
 SET LOCAL ROLE token_gateway; SET LOCAL search_path=pg_temp,public,pg_catalog`); err != nil {
				tx.Rollback(f.ctx)
				return nil, err
			}
			var inherited string
			if err = tx.QueryRow(f.ctx, `SHOW search_path`).Scan(&inherited); err != nil || inherited != "pg_temp, public, pg_catalog" {
				tx.Rollback(f.ctx)
				return nil, fmt.Errorf("shadow search path precondition failed: %w", err)
			}
		}
		_, err = tx.Exec(f.ctx, body)
		return tx, err
	}
	for _, key := range []string{"expected_database", "case_id", "task_id", "resource_id", "evidence_id", "actor_id", "reason", "reason_controls", "reason_too_long", "runtime", "case_type_probe", "resource_type_probe", "task_state_probe", "audit_permission_probe", "financial_case_probe"} {
		params := map[string]string{}
		for k, v := range values {
			params[k] = v
		}
		if key == "reason" {
			params[key] = " "
		} else if key != "runtime" {
			params[key] = uuid.Nil.String()
		}
		if key == "reason_controls" {
			params["reason"] = "\n\t"
		}
		if key == "reason_too_long" {
			params["reason"] = strings.Repeat("x", 1001)
		}
		if key == "financial_case_probe" {
			if financialCase == uuid.Nil {
				continue
			}
			params["case_id"] = financialCase.String()
		}
		tx, err := invoke(params, key == "runtime")
		if tx != nil {
			tx.Rollback(f.ctx)
		}
		if err == nil || snapshot() != caseBefore || otherCases() != otherBefore || facts() != before || f05CancellationClosureFundsFacts(t, f.bp, f.ctx) != fundsBefore {
			t.Fatal("invalid operator input changed facts", key, err)
		}
	}
	// The real carrier must choose public even when the controlled session already
	// has all four shadow relations and explicitly put pg_temp first. The marker's
	// SET LOCAL runs before DO declaration/ROWTYPE compilation, not inside BEGIN.
	shadow := map[string]string{}
	for k, v := range values {
		shadow[k] = v
	}
	shadow["shadow_path_probe"] = "1"
	// A successful body is still atomic with the surrounding operator transaction.
	tx, err := invoke(shadow, false)
	if err != nil {
		if tx != nil {
			tx.Rollback(f.ctx)
		}
		t.Fatal(err)
	}
	var pinnedPath string
	var publicExact, shadowUntouched bool
	// Inspect the fixture-owned shadows through their original owner; no runtime
	// grants are added to the operator carrier or the real schema.
	if _, err = tx.Exec(f.ctx, `SET LOCAL ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(f.ctx, `SELECT current_setting('search_path'),
 (SELECT c.status='RESOLVED' AND c.resolved_at IS NOT NULL AND EXISTS(SELECT 1 FROM public.manual_review_case_events e WHERE e.case_id=c.id AND e.actor_id=$2 AND e.event_type='EMERGENCY_CANCELLATION_CASE_CLOSED' AND e.details->>'task_id'=$3 AND e.details->>'evidence_id'=$4 AND e.details->>'reason'=$5) FROM public.manual_review_cases c WHERE c.id=$1),
 (SELECT count(*)=0 FROM pg_temp.model_emergency_cancellations) AND
 (SELECT count(*)=0 FROM pg_temp.manual_review_cases) AND
 (SELECT count(*)=0 FROM pg_temp.model_emergency_cancellation_evidence) AND
 (SELECT count(*)=0 FROM pg_temp.manual_review_case_events)`, caseID, actor, taskID.String(), evidenceID.String(), values["reason"]).Scan(&pinnedPath, &publicExact, &shadowUntouched); err != nil || pinnedPath != "pg_catalog, public, pg_temp" || !publicExact || !shadowUntouched {
		t.Fatal("operator search path did not preserve exact public identity and shadows", err)
	}
	if err = tx.Rollback(f.ctx); err != nil || snapshot() != caseBefore {
		t.Fatal("rolled-back closure survived", err)
	}
	tx, err = invoke(values, false)
	if err != nil {
		if tx != nil {
			tx.Rollback(f.ctx)
		}
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Let the real SLA worker select OPEN, then block at this transaction's case
	// lock. After commit its guarded UPDATE must observe RESOLVED and do nothing.
	var blockerPID int
	if err = tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	slaCtx, stopSLA := context.WithCancel(f.ctx)
	finished := make(chan struct{})
	var slaErr error
	go func() {
		defer close(finished)
		_, slaErr = f.r.EscalateOverdueManualReviews(slaCtx, manualreview.OnCallSchedule{}, 100)
	}()
	defer func() { stopSLA(); <-finished }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.r.pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SLA did not reach the real case lock barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	<-finished
	if slaErr != nil {
		t.Fatal(slaErr)
	}
	var valid bool
	if err = f.r.pool.QueryRow(f.ctx, `SELECT c.status='RESOLVED' AND c.resolved_at IS NOT NULL AND (SELECT count(*)=1 FROM manual_review_case_events e WHERE e.case_id=c.id AND e.event_type='EMERGENCY_CANCELLATION_CASE_CLOSED' AND e.actor_id=$2 AND e.created_at=c.resolved_at AND e.details->>'case_only'='true' AND e.details->>'reason'=$3 AND e.details->>'task_id'=$4 AND e.details->>'evidence_id'=$5) FROM manual_review_cases c WHERE c.id=$1`, caseID, actor, values["reason"], taskID.String(), evidenceID.String()).Scan(&valid); err != nil || !valid {
		t.Fatal("case closure lost exact actor/reason/evidence audit", err)
	}
	closed := snapshot()
	tx, err = invoke(values, false)
	if tx != nil {
		tx.Rollback(f.ctx)
	}
	if err == nil || snapshot() != closed {
		t.Fatal("repeated closure changed audit", err)
	}
	if _, err = f.r.SyncManualReviewCases(f.ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.EscalateOverdueManualReviews(f.ctx, manualreview.OnCallSchedule{}, 100); err != nil {
		t.Fatal(err)
	}
	if snapshot() != closed || otherCases() != otherBefore || facts() != before || f05CancellationClosureFundsFacts(t, f.bp, f.ctx) != fundsBefore {
		t.Fatal("sync/SLA revived closed case or case-only closure changed task, usage or funds")
	}
}
