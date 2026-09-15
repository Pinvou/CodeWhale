package account

import (
	"context"
	"testing"
	"time"
)

func TestApprovalWriterReadinessPrivilegesIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	repository := &PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool}
	if err := repository.approvalWriterReadiness(ctx); err != nil {
		t.Fatalf("correct isolated writer is not ready: %v", err)
	}
	for _, mutation := range []struct{ name, grant, revoke string }{
		{"unrelated user insert", `GRANT INSERT ON public.users TO account_approval_writer`, `REVOKE INSERT ON public.users FROM account_approval_writer`},
		{"unrelated user column update", `GRANT UPDATE(id) ON public.users TO account_approval_writer`, `REVOKE UPDATE(id) ON public.users FROM account_approval_writer`},
		{"unrelated user column reference", `GRANT REFERENCES(id) ON public.users TO account_approval_writer`, `REVOKE REFERENCES(id) ON public.users FROM account_approval_writer`},
		{"writer inherits service role", `GRANT account_service TO account_approval_writer WITH INHERIT FALSE, SET TRUE`, `REVOKE account_service FROM account_approval_writer`},
		{"delegated writer role", `GRANT account_approval_writer TO account_service WITH INHERIT FALSE, SET TRUE`, `REVOKE account_approval_writer FROM account_service`},
		{"extra outbox read", `GRANT SELECT ON public.outbox_events TO account_approval_writer`, `REVOKE SELECT ON public.outbox_events FROM account_approval_writer`},
		{"pending delete", `GRANT DELETE ON account_approval_authorization.authorization_event_pending TO account_approval_writer`, `REVOKE DELETE ON account_approval_authorization.authorization_event_pending FROM account_approval_writer`},
		{"sidecar column insert", `GRANT INSERT(event_id) ON account_approval_authorization.approval_authorization_event_bytes TO account_approval_writer`, `REVOKE INSERT(event_id) ON account_approval_authorization.approval_authorization_event_bytes FROM account_approval_writer`},
		{"backfill execution", `GRANT EXECUTE ON FUNCTION account_approval_authorization.backfill_authorization_sidecar(UUID,BYTEA,TEXT) TO account_approval_writer`, `REVOKE EXECUTE ON FUNCTION account_approval_authorization.backfill_authorization_sidecar(UUID,BYTEA,TEXT) FROM account_approval_writer`},
		{"backfill outcome execution", `GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_outcome(UUID,TEXT,TEXT) TO account_approval_writer`, `REVOKE EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_outcome(UUID,TEXT,TEXT) FROM account_approval_writer`},
		{"missing outcome execution", `REVOKE EXECUTE ON FUNCTION account_approval_authorization.authorization_commit_outcome(UUID,UUID,BIGINT,TEXT,TEXT,BIGINT,UUID,UUID) FROM account_approval_writer`, `GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_commit_outcome(UUID,UUID,BIGINT,TEXT,TEXT,BIGINT,UUID,UUID) TO account_approval_writer`},
		{"missing terminal outcome", `REVOKE EXECUTE ON FUNCTION account_approval_authorization.approval_terminal_commit_outcome(UUID,BIGINT,TEXT,UUID,TEXT) FROM account_approval_writer`, `GRANT EXECUTE ON FUNCTION account_approval_authorization.approval_terminal_commit_outcome(UUID,BIGINT,TEXT,UUID,TEXT) TO account_approval_writer`},
		{"public schema create", `GRANT CREATE ON SCHEMA public TO account_approval_writer`, `REVOKE CREATE ON SCHEMA public FROM account_approval_writer`},
		{"outbox epoch update", `GRANT UPDATE(approval_control_epoch) ON public.outbox_events TO account_approval_writer`, `REVOKE UPDATE(approval_control_epoch) ON public.outbox_events FROM account_approval_writer`},
		{"request command update", `GRANT UPDATE(command_type) ON public.approval_requests TO account_approval_writer`, `REVOKE UPDATE(command_type) ON public.approval_requests FROM account_approval_writer`},
		{"decision delete", `GRANT DELETE ON public.approval_decisions TO account_approval_writer`, `REVOKE DELETE ON public.approval_decisions FROM account_approval_writer`},
		{"missing request update", `REVOKE UPDATE(status) ON public.approval_requests FROM account_approval_writer`, `GRANT UPDATE(status) ON public.approval_requests TO account_approval_writer`},
		{"missing decision insert", `REVOKE INSERT ON public.approval_decisions FROM account_approval_writer`, `GRANT INSERT ON public.approval_decisions TO account_approval_writer`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if _, err := db.adminPool.Exec(ctx, mutation.grant); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := db.adminPool.Exec(ctx, mutation.revoke); err != nil {
					t.Fatal(err)
				}
			}()
			if err := repository.ApprovalWriterReady(ctx); err != ErrApprovalWriterUnavailable {
				t.Fatalf("privilege mutation did not fail closed: %v", err)
			}
		})
		if err := repository.ApprovalWriterReady(ctx); err != nil {
			t.Fatalf("restored writer did not recover: %v", err)
		}
	}
	for _, pool := range []*PostgresRepository{
		{pool: db.runtimePool},
		{pool: db.runtimePool, approvalWriterPool: db.runtimePool},
		{pool: db.runtimePool, approvalWriterPool: db.backfillPool},
		{pool: db.runtimePool, approvalWriterPool: db.adminPool},
	} {
		if err := pool.ApprovalWriterReady(ctx); err != ErrApprovalWriterUnavailable {
			t.Fatalf("wrong actual identity passed readiness: %v", err)
		}
	}
}

func TestApprovalBackfillReadinessPrivilegesIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	if err := ApprovalBackfillReady(ctx, db.backfillPool); err != nil {
		t.Fatalf("correct isolated backfill identity is not ready: %v", err)
	}
	for _, mutation := range []struct{ name, grant, revoke string }{
		{"unrelated user insert", `GRANT INSERT ON public.users TO account_approval_backfill`, `REVOKE INSERT ON public.users FROM account_approval_backfill`},
		{"unrelated user column update", `GRANT UPDATE(id) ON public.users TO account_approval_backfill`, `REVOKE UPDATE(id) ON public.users FROM account_approval_backfill`},
		{"unrelated user column reference", `GRANT REFERENCES(id) ON public.users TO account_approval_backfill`, `REVOKE REFERENCES(id) ON public.users FROM account_approval_backfill`},
		{"backfill inherits service role", `GRANT account_service TO account_approval_backfill WITH INHERIT FALSE, SET TRUE`, `REVOKE account_service FROM account_approval_backfill`},
		{"delegated backfill role", `GRANT account_approval_backfill TO account_service WITH INHERIT FALSE, SET TRUE`, `REVOKE account_approval_backfill FROM account_service`},
		{"direct outbox read", `GRANT SELECT(event_id) ON public.outbox_events TO account_approval_backfill`, `REVOKE SELECT(event_id) ON public.outbox_events FROM account_approval_backfill`},
		{"pending delete", `GRANT DELETE ON account_approval_authorization.authorization_event_pending TO account_approval_backfill`, `REVOKE DELETE ON account_approval_authorization.authorization_event_pending FROM account_approval_backfill`},
		{"writer prepare", `GRANT EXECUTE ON FUNCTION account_approval_authorization.prepare_authorization_event(UUID,UUID,BIGINT,JSONB,TEXT,BIGINT,TEXT) TO account_approval_backfill`, `REVOKE EXECUTE ON FUNCTION account_approval_authorization.prepare_authorization_event(UUID,UUID,BIGINT,JSONB,TEXT,BIGINT,TEXT) FROM account_approval_backfill`},
		{"public schema create", `GRANT CREATE ON SCHEMA public TO account_approval_backfill`, `REVOKE CREATE ON SCHEMA public FROM account_approval_backfill`},
		{"request status update", `GRANT UPDATE(status) ON public.approval_requests TO account_approval_backfill`, `REVOKE UPDATE(status) ON public.approval_requests FROM account_approval_backfill`},
		{"outbox epoch update", `GRANT UPDATE(approval_control_epoch) ON public.outbox_events TO account_approval_backfill`, `REVOKE UPDATE(approval_control_epoch) ON public.outbox_events FROM account_approval_backfill`},
		{"missing readiness", `REVOKE EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_readiness() FROM account_approval_backfill`, `GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_readiness() TO account_approval_backfill`},
		{"missing outcome proof", `REVOKE EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_outcome(UUID,TEXT,TEXT) FROM account_approval_backfill`, `GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_outcome(UUID,TEXT,TEXT) TO account_approval_backfill`},
		{"extra terminal outcome", `GRANT EXECUTE ON FUNCTION account_approval_authorization.approval_terminal_commit_outcome(UUID,BIGINT,TEXT,UUID,TEXT) TO account_approval_backfill`, `REVOKE EXECUTE ON FUNCTION account_approval_authorization.approval_terminal_commit_outcome(UUID,BIGINT,TEXT,UUID,TEXT) FROM account_approval_backfill`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if _, err := db.adminPool.Exec(ctx, mutation.grant); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := db.adminPool.Exec(ctx, mutation.revoke); err != nil {
					t.Fatal(err)
				}
			}()
			if err := ApprovalBackfillReady(ctx, db.backfillPool); err != ErrApprovalBackfillUnavailable {
				t.Fatalf("privilege mutation did not fail closed: %v", err)
			}
		})
		if err := ApprovalBackfillReady(ctx, db.backfillPool); err != nil {
			t.Fatalf("restored backfill identity did not recover: %v", err)
		}
	}
	if ApprovalBackfillReady(ctx, db.writerPool) != ErrApprovalBackfillUnavailable || ApprovalBackfillReady(ctx, db.runtimePool) != ErrApprovalBackfillUnavailable {
		t.Fatal("backfill accepted a writer or ordinary service identity")
	}
}
