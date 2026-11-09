package billing

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Original 900003 hardens all Billing tables, including these original 030
// relations. Prove that existing coverage instead of adding duplicate DDL.
func TestClosedGrantEvidenceCurrentRoleGuardsPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User, configuration.Password = "billing_migrator", ""
	owner, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, "SET ROLE billing_schema_owner"); err != nil {
		t.Fatal(err)
	}
	var session, current string
	var super bool
	if err := owner.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); err != nil || session != "billing_migrator" || current != "billing_schema_owner" || super {
		t.Fatal("actual independent owner identity required", err)
	}
	for _, table := range []string{"account_closure_grant_isolations", "account_closed_grant_reclaim_commands"} {
		t.Run(table, func(t *testing.T) {
			qualified := pgx.Identifier{"public", table}.Sanitize()
			var runtimeActor, tableOwner string
			var runtimeSuper, truncate, insert, update, remove bool
			if err := r.pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid=to_regclass($1))),has_table_privilege(current_user,$1,'TRUNCATE'),has_table_privilege(current_user,$1,'INSERT'),has_table_privilege(current_user,$1,'UPDATE'),has_table_privilege(current_user,$1,'DELETE')`, qualified).Scan(&runtimeActor, &runtimeSuper, &tableOwner, &truncate, &insert, &update, &remove); err != nil || runtimeActor != "billing_service" || runtimeSuper || tableOwner != "billing_schema_owner" || truncate || !insert || !update || !remove {
				t.Fatal("original runtime/owner and DML boundary changed", err)
			}
			facts := func() string {
				t.Helper()
				var result string
				if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text)::text,'[]') FROM `+qualified+` x`).Scan(&result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			before := facts()
			for _, query := range []string{"TRUNCATE " + qualified, "SET ROLE billing_schema_owner", "ALTER TABLE " + qualified + " DISABLE TRIGGER billing_no_truncate"} {
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, denied := tx.Exec(ctx, query)
				rollback := tx.Rollback(ctx)
				var permission *pgconn.PgError
				if rollback != nil || !errors.As(denied, &permission) || permission.Code != "42501" {
					t.Fatal("runtime acquired destructive owner authority", denied, rollback)
				}
			}
			// Empty tables still fire a statement-level trigger. Using the actual
			// owner separates that protection from the runtime's earlier ACL denial.
			for _, query := range []string{"TRUNCATE " + qualified, "TRUNCATE " + qualified + " CASCADE"} {
				tx, err := owner.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, denied := tx.Exec(ctx, query)
				rollback := tx.Rollback(ctx)
				var guard *pgconn.PgError
				if rollback != nil || !errors.As(denied, &guard) || guard.Code != "55000" {
					t.Fatal("original global statement guard is missing", denied, rollback)
				}
			}
			if facts() != before {
				t.Fatal("rejected destructive command changed the original evidence")
			}
		})
	}
}
