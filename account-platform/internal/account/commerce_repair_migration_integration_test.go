package account

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCommerceManualCompletionMigrationPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	for _, scenario := range []string{"nonempty-upgrade-and-forward-only", "late-catalog-collision-rolls-back"} {
		t.Run(scenario, func(t *testing.T) {
			r := f.repositoryThroughVersion(t, 900015, nil)
			ctx := f.ctx
			user := retainedWalletProvisioningUser(t, r)
			wallets, err := r.LeaseWalletProvisioningJobs(ctx, 1, time.Minute)
			if err != nil || len(wallets) != 1 || wallets[0].UserID != user {
				t.Fatal("public wallet predecessor", err)
			}
			if err = r.CompleteWalletProvisioning(ctx, wallets[0]); err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = enqueueCommerceCustomerSync(ctx, tx, "USER", user, "+8613800001234", "owned-upgrade", "v1", "CUSTOMER_PROVISIONING", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			// The unchanged enqueue producer supersedes this first generation;
			// keep its historical job and a genuine second generation pending.
			tx, err = r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = enqueueCommerceCustomerSync(ctx, tx, "USER", user, "+8613800005678", "owned-upgrade", "v2", "PROFILE_SYNC", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			facts := func() string {
				var s string
				if e := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'jobs',(SELECT jsonb_agg(to_jsonb(j)-'commerce_repair_case_id' ORDER BY id) FROM commerce_customer_sync_jobs j),
'mappings',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM commerce_customer_mappings m),
'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM commerce_customer_sync_evidence e),
'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u),
'wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY user_id) FROM wallet_provisioning_jobs w),
'audits',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM audit_logs a),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`).Scan(&s); e != nil {
					t.Fatal(e)
				}
				return s
			}
			ledger := func() string {
				var s string
				if e := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&s); e != nil {
					t.Fatal(e)
				}
				return s
			}
			before := facts()
			oldLedger := ledger()
			all, err := migrations.Load("../../migrations/account")
			if err != nil {
				t.Fatal(err)
			}
			var guard migrations.Migration
			for _, m := range all {
				if m.Version == 900025 {
					guard = m
				}
			}
			if guard.Version != 900025 {
				t.Fatal("public completion migration missing")
			}
			// Actual runtime credentials cannot invoke the new DDL carrier.
			_, err = r.pool.Exec(ctx, guard.UpSQL)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "42501" || facts() != before || ledger() != oldLedger {
				t.Fatal("runtime migration guard or zero-effect rejection", err)
			}
			cfg := r.pool.Config().ConnConfig.Copy()
			cfg.User = "account_migrator"
			cfg.Password = ""
			connection, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close(context.Background())
			if _, err = connection.Exec(ctx, `SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			if scenario == "late-catalog-collision-rolls-back" {
				// Controlled catalog collision occurs after the new DDL in the
				// published migration, not a hand-built substitute schema.
				if _, err = r.pool.Exec(ctx, `INSERT INTO permissions(code,scope,description) VALUES('platform.commerce.repairs.read','PLATFORM','owned migration fault')`); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					err = accountschema.Up(ctx, connection, all)
					if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "permissions_pkey" || facts() != before || ledger() != oldLedger {
						t.Fatal("late migration failure changed predecessor facts", err)
					}
					var clean bool
					if err = r.pool.QueryRow(ctx, `SELECT to_regclass('public.commerce_customer_repair_cases') IS NULL
AND to_regclass('public.commerce_customer_repair_observations') IS NULL
AND to_regclass('public.commerce_customer_repair_callbacks') IS NULL
AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='commerce_customer_sync_jobs' AND column_name='commerce_repair_case_id')
AND NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='public.commerce_customer_sync_jobs'::regclass AND conname='commerce_customer_sync_jobs_status_check' AND pg_get_constraintdef(oid) LIKE '%MANUAL_NO_EFFECT%')`).Scan(&clean); err != nil || !clean {
						t.Fatal("partial completion DDL survived failed public Up", err)
					}
				}
				if _, err = r.pool.Exec(ctx, `DELETE FROM permissions WHERE code='platform.commerce.repairs.read' AND description='owned migration fault'`); err != nil {
					t.Fatal(err)
				}
			}
			if err = accountschema.Up(ctx, connection, all); err != nil {
				t.Fatal("public forward install", err)
			}
			if _, err = accountschema.VerifyApplied(ctx, connection, all); err != nil {
				t.Fatal(err)
			}
			if _, err = approvalpolicy.VerifyPolicySet(ctx, r.pool); err != nil {
				t.Fatal("original Billing/Token governance policies changed", err)
			}
			for _,command:=range []string{"ACCOUNT_COMMERCE_REPAIR_QUARANTINE","ACCOUNT_COMMERCE_REPAIR_OBSERVE","ACCOUNT_COMMERCE_REPAIR_CONFIRM"} {
				policy,e:=approvalpolicy.Lookup(command);if e!=nil{t.Fatal(e)}
				fields,e:=json.Marshal(policy.CriticalResourceFields);if e!=nil{t.Fatal(e)}
				var matches bool
				e=r.pool.QueryRow(ctx,`SELECT policy_version=$2 AND approval_mode=$3 AND requester_permission=$4
AND coalesce(approver_permission,'')=$5 AND risk_class=$6 AND approval_count=$7 AND self_approval_forbidden=$8
AND snapshot_schema_version=$9 AND critical_resource_fields=$10::jsonb
AND expires_after=($11::bigint*interval '1 microsecond') AND execution_window=($12::bigint*interval '1 microsecond') AND executor_identity=$13
FROM approval_command_policies WHERE command_type=$1 ORDER BY policy_version DESC LIMIT 1`,command,policy.PolicyVersion,string(policy.Mode),policy.RequesterPermission,policy.ApproverPermission,policy.RiskClass,policy.ApprovalCount,policy.SelfApprovalForbidden,policy.SnapshotSchemaVersion,fields,policy.ExpiresAfter.Microseconds(),policy.ExecutionWindow.Microseconds(),policy.ExecutorIdentity).Scan(&matches)
				if e!=nil || !matches{t.Fatal("new management route policy differs from public seed",command,e)}
			}
			if facts() != before {
				t.Fatal("upgrade rewrote original synchronization or identity facts")
			}
			installedLedger := ledger()
			err = accountschema.DownOne(ctx, connection, all)
			if !errors.As(err, &pgerr) || pgerr.Code != "55000" || pgerr.Message != "commerce manual completion is forward-only" || facts() != before || ledger() != installedLedger {
				t.Fatal("forward-only completion guard", err)
			}
			var roles int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM roles WHERE code IN ('PLATFORM_COMMERCE_OPERATOR','PLATFORM_COMMERCE_REVIEWER')`).Scan(&roles); err != nil || roles != 2 {
				t.Fatal("usable repair roles missing", err)
			}
		})
	}
}
