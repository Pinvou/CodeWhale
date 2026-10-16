package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// The original 032 row trigger cannot see TRUNCATE. Harden the original public
// objects after both fresh and already-applied public Up, without rewriting SQL,
// changing their owner, granting authority or discarding historical rows.
func ensureAuditExportImmutability(ctx context.Context, conn *pgx.Conn, loaded []migrations.Migration) error {
	present := false
	legacyChecksum := ""
	for _, migration := range loaded {
		if migration.Version == 32 {
			present = true
			// Generic migrations already accept the original SQL's CRLF ledger
			// representation. Retain that exact compatibility, not arbitrary SQL.
			canonical := strings.ReplaceAll(strings.ReplaceAll(migration.UpSQL, "\r\n", "\n"), "\r", "\n")
			sum := sha256.Sum256([]byte(strings.ReplaceAll(canonical, "\n", "\r\n")))
			legacyChecksum = hex.EncodeToString(sum[:])
			break
		}
	}
	if !present {
		return nil
	}
	if err := validateAuditExportMigration(loaded); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var checksum string
	if err := tx.QueryRow(ctx, `SELECT checksum FROM public.schema_migrations WHERE version=32`).Scan(&checksum); err != nil {
		return err
	}
	if checksum != auditExportUpChecksum && checksum != legacyChecksum {
		return errors.New("audit export hardening requires original applied SQL identity")
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE public.audit_export_events IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var functionID uint32
	var body string
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT p.oid,p.prosrc,
 c.relkind='r' AND pg_get_userbyid(c.relowner)='account_service' AND p.proowner=c.relowner
 AND p.prorettype='trigger'::regtype AND p.pronargs=0 AND NOT p.prosecdef AND l.lanname='plpgsql'
 AND EXISTS(SELECT 1 FROM pg_trigger t WHERE t.tgrelid=c.oid AND t.tgname='audit_export_events_immutable'
  AND t.tgfoid=p.oid AND t.tgtype=27 AND t.tgenabled='O' AND NOT t.tgisinternal)
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_proc p ON p.oid=to_regprocedure('public.reject_audit_export_event_mutation()')
 JOIN pg_language l ON l.oid=p.prolang
 WHERE n.nspname='public' AND c.relname='audit_export_events'`).Scan(&functionID, &body, &valid); err != nil {
		return err
	}
	if !valid || strings.Join(strings.Fields(body), " ") != "BEGIN RAISE EXCEPTION 'audit export evidence is immutable'; END" {
		return errors.New("audit export immutable function or owner contract changed")
	}
	// Original 017 already blocks audit-log truncation at statement level. Do
	// not replace that trigger or relax its security-event update/delete guard.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger t
 JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relname='audit_logs' AND t.tgname='audit_logs_security_no_truncate'
 AND t.tgtype=34 AND t.tgenabled='O' AND NOT t.tgisinternal
 AND t.tgfoid=to_regprocedure('public.protect_security_audit_log()'))`).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("original audit log truncate guard changed")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='public.audit_export_events'::regclass AND tgname='audit_export_events_no_truncate')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(ctx, `CREATE TRIGGER audit_export_events_no_truncate BEFORE TRUNCATE ON public.audit_export_events FOR EACH STATEMENT EXECUTE FUNCTION public.reject_audit_export_event_mutation()`); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='public.audit_export_events'::regclass
 AND tgname='audit_export_events_no_truncate' AND tgfoid=$1 AND tgtype=34 AND tgenabled='O' AND NOT tgisinternal)`, functionID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("audit export truncate guard identity changed")
	}
	return tx.Commit(ctx)
}
