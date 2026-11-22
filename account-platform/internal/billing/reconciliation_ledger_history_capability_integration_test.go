package billing

import (
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// The official migration wrapper and a non-superuser Billing session exercise
// the existing read-only history privileges in a fresh owned database.
func TestLedgerRepairHistoryCapabilityPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	var session string
	var super bool
	if err := r.pool.QueryRow(ctx, `SELECT session_user,rolsuper FROM pg_catalog.pg_roles WHERE rolname=session_user`).Scan(&session, &super); err != nil || session != "billing_service" || super {
		t.Fatal("fixture does not use the genuine non-super Billing identity", session, super, err)
	}
	var read, insert, update, remove, truncate bool
	if err := r.pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service','public.billing_approval_command_activation_history','SELECT'),has_table_privilege('billing_service','public.billing_approval_command_activation_history','INSERT'),has_table_privilege('billing_service','public.billing_approval_command_activation_history','UPDATE'),has_table_privilege('billing_service','public.billing_approval_command_activation_history','DELETE'),has_table_privilege('billing_service','public.billing_approval_command_activation_history','TRUNCATE')`).Scan(&read, &insert, &update, &remove, &truncate); err != nil || !read || insert || update || remove || truncate {
		t.Fatal("existing history privilege boundary changed", read, insert, update, remove, truncate, err)
	}
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	version := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM public.billing_approval_command_activation_history WHERE command_type='LEDGER_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state='ACTIVE' AND version=$1`, version).Scan(&count); err != nil || count != 1 {
		t.Fatal("controlled historical version unavailable", count, err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	connection := ledgerActivationFixtureOperator(t, ctx, r.pool)
	if _, err := connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal("official migration actor cannot assume runtime migration role", err)
	}
	if _, err := billingschema.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal("official migration verification", err)
	}
}
