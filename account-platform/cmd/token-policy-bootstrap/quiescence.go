package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// checkWriterQuiescence checks a database-enforced maintenance fence, including
// old binaries and background creators which do not consult the HTTP gate.
// Operations keep the verified runtime-role fence until cutover completes;
// only trusted deployment logins are opened for each controlled migration step.
// This command never changes grants, terminates sessions, or drains work itself.
func checkWriterQuiescence(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var canInspect bool
	if err = tx.QueryRow(ctx, `SELECT rolsuper OR pg_has_role(current_user,'pg_read_all_stats','MEMBER') FROM pg_roles WHERE rolname=current_user`).Scan(&canInspect); err != nil || !canInspect {
		return fmt.Errorf("quiescence requires database activity inspection permission")
	}
	// NOWAIT detects in-flight writers; holding the locks also keeps the queue
	// and role checks in one stable window. Runtime logins must already be fenced.
	for _, table := range []string{"token_accounts", "token_account_provisioning_reservations", "token_command_results"} {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if table == "token_accounts" {
				return fmt.Errorf("quiescence account facts are unavailable")
			}
			continue // first migration has no Saga or command history yet
		}
		if _, err = tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{"public", table}.Sanitize()+" IN ACCESS EXCLUSIVE MODE NOWAIT"); err != nil {
			return fmt.Errorf("quiescence cannot lock creation facts")
		}
		var pending int64
		switch table {
		case "token_account_provisioning_reservations":
			err = tx.QueryRow(ctx, `SELECT count(*) FROM public.token_account_provisioning_reservations WHERE status IN ('PENDING','MANUAL_REVIEW') OR lease_owner IS NOT NULL`).Scan(&pending)
		case "token_command_results":
			err = tx.QueryRow(ctx, `SELECT count(*) FROM public.token_command_results WHERE command_type='TOKEN_ACCOUNT_CREATE' AND status='PENDING'`).Scan(&pending)
		}
		if err != nil || pending != 0 {
			return fmt.Errorf("quiescence creation queue is not drained or cannot be verified")
		}
	}
	var unsafeRoles int64
	err = tx.QueryRow(ctx, `
WITH writers AS (
 SELECT r.oid,r.rolname,r.rolcanlogin FROM pg_roles r
 WHERE (NOT r.rolsuper OR r.rolname='token_gateway') AND (
   EXISTS (
    SELECT 1 FROM pg_roles g WHERE (g.oid=r.oid OR pg_has_role(r.oid,g.oid,'MEMBER')) AND (
     has_table_privilege(g.oid,'public.token_accounts','INSERT,UPDATE') OR
     EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
       WHERE n.nspname='public' AND p.prosecdef AND
         has_function_privilege(g.oid,p.oid,'EXECUTE')))))
)
SELECT count(*) FROM writers w WHERE w.rolcanlogin OR EXISTS (
 SELECT 1 FROM pg_stat_activity a WHERE a.usesysid=w.oid AND a.pid<>pg_backend_pid())`).Scan(&unsafeRoles)
	if err != nil || unsafeRoles != 0 {
		return fmt.Errorf("quiescence writer logins, inherited privileges, or old sessions remain")
	}
	return tx.Commit(ctx)
}
