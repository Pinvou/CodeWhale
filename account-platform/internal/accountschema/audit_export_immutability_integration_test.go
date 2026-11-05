package accountschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Called by the existing named TestAuditExportRollbackRoleStorageGuard, whose
// finite CI entry already rejects zero cases and any skip/failure. This extends
// the real original-public-Up fixture; it does not synthesize migration SQL.
func verifyAuditExportPublicUpImmutability(t *testing.T, ctx context.Context, c *pgx.Conn, loaded []migrations.Migration) {
	t.Helper()
	userID, jobID, eventID, auditID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	code, err := account.GenerateUserCode()
	if err != nil {
		t.Fatal("fixture user code")
	}
	if _, err := c.Exec(ctx, `INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,$2,'ACTIVE',clock_timestamp(),clock_timestamp())`, userID, code); err != nil {
		t.Fatal("fixture user")
	}
	if _, err := c.Exec(ctx, `INSERT INTO audit_export_jobs(id,actor_user_id,request_digest,idempotency_key_digest,filter_snapshot,reason,include_pii,status,created_at,updated_at)
 VALUES($1,$2,repeat('a',64),repeat('b',64),'{}','controlled immutable evidence',false,'PENDING',clock_timestamp(),clock_timestamp())`, jobID, userID); err != nil {
		t.Fatal("fixture export job")
	}
	if _, err := c.Exec(ctx, `INSERT INTO audit_export_events(id,export_id,event_type,actor_user_id,snapshot,occurred_at) VALUES($1,$2,'CREATED',$3,'{}',clock_timestamp())`, eventID, jobID, userID); err != nil {
		t.Fatal("fixture export event")
	}
	if _, err := c.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,occurred_at) VALUES($1,$2,'security.immutable-fixture','AUDIT_EXPORT',$3,'SUCCEEDED','controlled-request','controlled-trace',clock_timestamp())`, auditID, userID, jobID.String()); err != nil {
		t.Fatal("fixture security audit event")
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
 'ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x),
 'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),
 'roles',(SELECT jsonb_agg(to_jsonb(x) ORDER BY code) FROM roles x),
 'permissions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY role_code,permission_code) FROM role_permissions x),
 'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),
 'jobs',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_export_jobs x),
 'events',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_export_events x),
 'audits',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x))::text`).Scan(&value); err != nil {
			t.Fatal("original immutable fact snapshot")
		}
		return value
	}
	guard := func() string {
		t.Helper()
		var value string
		if err := c.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(t.tgname,t.tgtype,t.tgenabled,t.tgfoid,pg_get_triggerdef(t.oid)) ORDER BY t.tgname),'[]')::text
 FROM pg_trigger t WHERE t.tgrelid='public.audit_export_events'::regclass AND NOT t.tgisinternal`).Scan(&value); err != nil {
			t.Fatal("immutable guard metadata")
		}
		return value
	}
	blocked := func(t *testing.T) {
		t.Helper()
		before := snapshot()
		for _, mutation := range []struct{ sql, message string }{
			{`TRUNCATE public.audit_export_events`, "audit export evidence is immutable"},
			{`UPDATE public.audit_export_events SET snapshot=snapshot WHERE id='` + eventID.String() + `'`, "audit export evidence is immutable"},
			{`DELETE FROM public.audit_export_events WHERE id='` + eventID.String() + `'`, "audit export evidence is immutable"},
			// Retained migration 088 strengthens the original 017 guard to all audit actions.
			{`TRUNCATE public.audit_logs`, "audit evidence is immutable"},
		} {
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal("mutation transaction")
			}
			_, mutationErr := tx.Exec(ctx, mutation.sql)
			rollbackErr := tx.Rollback(ctx)
			var pgError *pgconn.PgError
			if !errors.As(mutationErr, &pgError) || pgError.Message != mutation.message {
				t.Errorf("actual original immutable guard did not reject mutation %q: %v", strings.Fields(mutation.sql)[0], mutationErr)
			}
			if rollbackErr != nil {
				t.Fatal("mutation rollback")
			}
		}
		if snapshot() != before {
			t.Fatal("rejected mutation changed original data or ledger")
		}
	}
	t.Run("fresh_public_up_truncate_and_original_row_guards", blocked)
	t.Run("original_applied32_without_new_guard_repeated_public_up", func(t *testing.T) {
		// Recreate only the retained original object's missing statement guard,
		// never alter the original SQL files, row trigger or applied ledger.
		if _, err := c.Exec(ctx, `DROP TRIGGER IF EXISTS audit_export_events_no_truncate ON public.audit_export_events`); err != nil {
			t.Fatal("owned retained guard fixture")
		}
		before := snapshot()
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("already-applied original public Up hardening", err)
		}
		migrations.Unlock(ctx, c)
		if snapshot() != before {
			t.Fatal("forward hardening rewrote original data or ledger")
		}
		blocked(t)
	})
	t.Run("repeated_public_up_is_idempotent", func(t *testing.T) {
		before, metadata := snapshot(), guard()
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("repeat public Up", err)
		}
		migrations.Unlock(ctx, c)
		if snapshot() != before || guard() != metadata {
			t.Fatal("repeat hardening changed original facts or trigger identities")
		}
	})
	t.Run("wrong_original032_identity_rejected_before_ddl", func(t *testing.T) {
		before, metadata := snapshot(), guard()
		changed := append([]migrations.Migration(nil), loaded...)
		found := false
		for index := range changed {
			if changed[index].Version == 32 {
				changed[index].UpSQL += "\nSELECT 1;"
				found = true
			}
		}
		if !found || Up(ctx, c, changed) == nil {
			t.Fatal("edited original audit SQL accepted")
		}
		if snapshot() != before || guard() != metadata {
			t.Fatal("invalid original identity reached DDL or changed data")
		}
	})
}
