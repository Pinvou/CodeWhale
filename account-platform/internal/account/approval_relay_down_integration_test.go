package account

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestApprovalRelayDownReentryIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := loadApprovalRelayBaselineMigrations()
	if err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := db.adminPool.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&version); err != nil || version != 51 {
		t.Fatalf("down did not restore migration 51: %d %v", version, err)
	}
	var clean bool
	if err := db.adminPool.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.outbox_events'::regclass
   AND attname IN ('relay_control_epoch','relay_claim_token','relay_claim_worker') AND NOT attisdropped)
 AND to_regclass('public.outbox_events_authorization_relay_idx') IS NULL
 AND to_regclass('account_approval_authorization.authorization_delivery_pending') IS NULL
 AND to_regprocedure('public.inspect_authorization_dead_letter(uuid)') IS NULL
 AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname IN ('event_replay_authorization_guard','approval_authorization_delivery_pending_guard'))
 AND pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='public.event_replay_requests'::regclass))='account_service'
 AND pg_get_userbyid((SELECT proowner FROM pg_proc WHERE oid='public.reject_event_replay_audit_mutation()'::regprocedure))='account_service'
 AND pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='public.outbox_events'::regclass))='account_approval_authorization_owner'
 AND NOT pg_has_role('account_service','account_approval_authorization_owner','MEMBER')
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE roleid='account_approval_relay'::regrole OR member='account_approval_relay'::regrole)
 AND NOT EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname IN ('account_approval_authorization','account_approval_control')
   AND has_schema_privilege('account_approval_relay',n.oid,'USAGE,CREATE'))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
   WHERE n.nspname='account_approval_authorization' AND has_function_privilege('account_approval_relay',p.oid,'EXECUTE'))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control') AND c.relkind IN ('r','p','v','m','f')
   AND (has_any_column_privilege('account_approval_relay',c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
     OR has_table_privilege('account_approval_relay',c.oid,'DELETE,TRUNCATE,TRIGGER')))
 AND has_table_privilege('account_service','public.event_replay_requests','SELECT,INSERT')`).Scan(&clean); err != nil || !clean {
		t.Fatalf("down retained Relay authority or changed baseline ownership: %v", err)
	}
	if store.Ready(ctx) == nil {
		t.Fatal("removed Relay schema remained ready")
	}
	for _, function := range []string{
		"account_approval_authorization.reject_unfinished_delivery()",
		"account_approval_authorization.authorization_relay_mode_allows(text,text,bigint,bigint,bigint)",
		"account_approval_authorization.claim_authorization_events(text,integer,integer)",
		"account_approval_authorization.open_authorization_delivery(uuid,uuid,bigint,text)",
		"account_approval_authorization.finish_authorization_delivery(uuid,uuid,bigint,text,text,timestamp with time zone,text)",
		"account_approval_authorization.authorization_relay_outcome(uuid,uuid,bigint,text)",
		"account_approval_authorization.authorization_relay_readiness()",
		"account_approval_authorization.guard_authorization_replay_insert()",
		"public.inspect_authorization_dead_letter(uuid)",
	} {
		var absent bool
		if err := db.adminPool.QueryRow(ctx, `SELECT to_regprocedure($1) IS NULL`, function).Scan(&absent); err != nil || !absent {
			t.Fatalf("down retained dedicated function %s: %v", function, err)
		}
	}
	if err := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool}).ApprovalWriterReady(ctx); err != nil {
		t.Fatal("down broke original writer readiness")
	}
	if err := ApprovalBackfillReady(ctx, db.backfillPool); err != nil {
		t.Fatal("down broke original backfill readiness")
	}
	// Verify a real ordinary event write, not just restored catalog grants.
	tx, err := db.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := events.AppendOutbox(ctx, tx, "account.fixture.relay-down.v1", uuid.New(), 1, map[string]any{"compatible": true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal("Relay readiness did not recover after reentry")
	}
}

func TestApprovalRelayDownPreservesClaimHistoryIntegration(t *testing.T) {
	for _, state := range []string{"claimed", "published", "retry"} {
		t.Run(state, func(t *testing.T) {
			db, store := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			claims, err := store.ClaimFrozen(ctx, "history-worker", 1, 30*time.Second)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claim: %v", err)
			}
			if state != "claimed" {
				outcome := events.FrozenPublished
				if state == "retry" {
					outcome = events.FrozenRetry
				}
				if got, err := store.ProcessFrozen(ctx, "history-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
					return events.FrozenPublishResult{Disposition: outcome, ErrorCode: "EVENT_DELIVERY_FAILED"}
				}); err != nil || got != outcome {
					t.Fatalf("history preparation outcome=%s expected=%s error=%v", got, outcome, err)
				}
			}
			before := relayDownOutboxSnapshot(t, ctx, db, eventID)
			loaded, err := loadApprovalRelayBaselineMigrations()
			if err != nil {
				t.Fatal(err)
			}
			connection := authorizationTestConnection(t, ctx, db.migratorPool)
			if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			defer migrations.Unlock(ctx, connection.Conn())
			requireControlSQLState(t, migrations.DownOne(ctx, connection.Conn(), loaded), "55000")
			if after := relayDownOutboxSnapshot(t, ctx, db, eventID); after != before {
				t.Fatal("refused down changed persisted delivery/claim/attempt audit")
			}
			var version int64
			var token uuid.UUID
			if err := db.adminPool.QueryRow(ctx, `SELECT (SELECT max(version) FROM public.schema_migrations),relay_claim_token FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&version, &token); err != nil || version != 52 || token != claims[0].ClaimToken {
				t.Fatalf("refused down lost claim audit: %v", err)
			}
		})
	}
}

func TestApprovalRelayDownRejectsAdvancedEpochIntegration(t *testing.T) {
	for _, epoch := range []string{"approval", "relay"} {
		t.Run(epoch, func(t *testing.T) {
			db, _ := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
			var forward, back string
			if epoch == "approval" {
				forward = `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1`
				back = `UPDATE account_approval_control.admin_approval_control SET mode='LEGACY',approval_epoch=approval_epoch+1,version=version+1`
			} else {
				forward = `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1`
				back = `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1`
			}
			if _, err := owner.Exec(ctx, forward); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, back); err != nil {
				t.Fatal(err)
			}
			var controlBefore string
			if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM account_approval_control.admin_approval_control c WHERE control_id='ADMIN_APPROVAL'`).Scan(&controlBefore); err != nil {
				t.Fatal(err)
			}
			// Returning to LEGACY/RUNNING must not erase either epoch history.
			loaded, err := loadApprovalRelayBaselineMigrations()
			if err != nil {
				t.Fatal(err)
			}
			connection := authorizationTestConnection(t, ctx, db.migratorPool)
			if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			defer migrations.Unlock(ctx, connection.Conn())
			requireControlSQLState(t, migrations.DownOne(ctx, connection.Conn(), loaded), "55000")
			var controlAfter string
			if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM account_approval_control.admin_approval_control c WHERE control_id='ADMIN_APPROVAL'`).Scan(&controlAfter); err != nil || controlAfter != controlBefore {
				t.Fatal("refused down changed control mode/state/epochs/version")
			}
			var version int64
			if err := db.adminPool.QueryRow(ctx, `SELECT max(version) FROM public.schema_migrations`).Scan(&version); err != nil || version != 52 {
				t.Fatalf("refused epoch rollback changed schema: %v", err)
			}
		})
	}
}

// Compare the complete persisted row, including attempt/lease/terminal/error
// and immutable event/epoch facts, without printing fixture payload contents.
func relayDownOutboxSnapshot(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, eventID uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&snapshot); err != nil {
		t.Fatal("read isolated outbox snapshot")
	}
	return snapshot
}
