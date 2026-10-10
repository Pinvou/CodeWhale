package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestF12PaymentActivationOwnerCapabilityPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned Billing fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture control connection failed")
	}
	t.Cleanup(func() {
		if err := admin.Close(context.Background()); err != nil {
			t.Error("fixture control close failed")
		}
	})
	role := "f12_cap_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil || exists {
		t.Fatal("owned capability grantee must be absent")
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal("owned capability grantee creation failed")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		var dependent bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid=(SELECT oid FROM pg_roles WHERE rolname=$1) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=$1) OR roleid=(SELECT oid FROM pg_roles WHERE rolname=$1))`, role).Scan(&dependent); err != nil || dependent {
			t.Error("owned grantee retained due to dependency guard")
			return
		}
		if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Error("owned capability grantee cleanup failed")
		}
	})
	_, pool, operator := newEntitlementCrossDomainBillingFixture(t, ctx, raw, func(setup *pgx.Conn) {
		for _, sql := range []string{"ALTER DEFAULT PRIVILEGES FOR ROLE billing_schema_owner GRANT ALL ON SCHEMAS TO " + pgx.Identifier{role}.Sanitize(), "ALTER DEFAULT PRIVILEGES FOR ROLE billing_schema_owner GRANT EXECUTE ON FUNCTIONS TO " + pgx.Identifier{role}.Sanitize()} {
			if _, err := setup.Exec(ctx, sql); err != nil {
				t.Fatal("owned database default privilege fixture failed")
			}
		}
	})
	qualification := func() string {
		var owner, schemaOwner, body, acl string
		var definer, execute, create, update, thirdExecute, thirdCreate, publicExecute bool
		var config []string
		var arguments int
		err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),pg_get_userbyid(n.nspowner),p.prosecdef,p.proconfig,p.pronargs,p.prosrc,COALESCE(p.proacl::text,''),has_function_privilege('billing_service',p.oid,'EXECUTE'),has_schema_privilege('billing_service',n.oid,'CREATE'),has_table_privilege('billing_service','public.billing_approval_command_activations','UPDATE'),has_function_privilege($1,p.oid,'EXECUTE'),has_schema_privilege($1,n.oid,'CREATE'),EXISTS(SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE') FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.oid='billing_approval_execution.lock_payment_activation()'::regprocedure`, role).Scan(&owner, &schemaOwner, &definer, &config, &arguments, &body, &acl, &execute, &create, &update, &thirdExecute, &thirdCreate, &publicExecute)
		if err != nil || owner != "billing_schema_owner" || schemaOwner != owner || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog" || arguments != 0 || !execute || create || update || thirdExecute || thirdCreate || publicExecute {
			t.Fatal("fixed owner capability effective ACL/search path differs")
		}
		var receiptRead, receiptInsert, receiptUpdate, receiptDelete bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service','public.payment_order_repairs','SELECT'),has_table_privilege('billing_service','public.payment_order_repairs','INSERT'),has_table_privilege('billing_service','public.payment_order_repairs','UPDATE'),has_table_privilege('billing_service','public.payment_order_repairs','DELETE')`).Scan(&receiptRead, &receiptInsert, &receiptUpdate, &receiptDelete); err != nil {
			t.Fatal("payment receipt ACL query failed")
		}
		if !receiptRead || !receiptInsert || receiptUpdate || receiptDelete {
			t.Fatalf("payment receipt ACL read=%t insert=%t update=%t delete=%t", receiptRead, receiptInsert, receiptUpdate, receiptDelete)
		}
		for _, query := range []string{`UPDATE public.payment_order_repairs SET reason=reason WHERE false`, `DELETE FROM public.payment_order_repairs WHERE false`} {
			_, err := pool.Exec(ctx, query)
			var denied *pgconn.PgError
			if !errors.As(err, &denied) || denied.Code != "42501" {
				t.Fatal("runtime payment receipt mutation must be denied")
			}
		}
		return body + "|" + acl
	}
	before := qualification()
	var active bool
	if err := pool.QueryRow(ctx, `SELECT active FROM billing_approval_execution.lock_payment_activation()`).Scan(&active); err != nil || active {
		t.Fatal("installation must not activate the command")
	}
	for _, sql := range []string{`SET ROLE billing_schema_owner`, `ALTER FUNCTION billing_approval_execution.lock_payment_activation() SECURITY INVOKER`, `CREATE TABLE billing_approval_execution.runtime_forbidden(id int)`} {
		_, err := pool.Exec(ctx, sql)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "42501" {
			t.Fatal("runtime owner/DDL elevation must fail with 42501")
		}
	}
	// Even an operator selecting runtime does not become the runtime login.
	if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	err = operator.QueryRow(ctx, `SELECT active FROM billing_approval_execution.lock_payment_activation()`).Scan(&active)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "42501" {
		t.Fatal("capability must reject wrong session_user")
	}
	// The capability holds the precise activation row until the financial
	// transaction ends; an operator cannot deactivate it midway through execution.
	runtimeTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("runtime lock transaction failed")
	}
	defer runtimeTx.Rollback(ctx)
	if err := runtimeTx.QueryRow(ctx, `SELECT active FROM billing_approval_execution.lock_payment_activation()`).Scan(&active); err != nil || active {
		t.Fatal("inactive capability row lock failed")
	}
	operatorTx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal("operator lock transaction failed")
	}
	defer operatorTx.Rollback(ctx)
	if _, err := operatorTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal("operator owner qualification failed")
	}
	var operatorPID int
	if err := operatorTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&operatorPID); err != nil {
		t.Fatal("operator PID failed")
	}
	updated := make(chan error, 1)
	go func() {
		_, err := operatorTx.Exec(ctx, `UPDATE public.billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1`)
		updated <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		select {
		case <-updated:
			t.Fatal("operator bypassed runtime activation row lock")
		default:
		}
		if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, operatorPID).Scan(&blocked); err != nil {
			t.Fatal("activation lock observation failed")
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("operator did not wait on runtime activation row lock")
	}
	if err := runtimeTx.Rollback(ctx); err != nil {
		t.Fatal("runtime lock release failed")
	}
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal("operator update failed after runtime transaction ended")
		}
	case <-ctx.Done():
		t.Fatal("operator did not resume")
	}
	if err := operatorTx.Rollback(ctx); err != nil {
		t.Fatal("operator fixture rollback failed")
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := billingschema.Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	err = migrations.DownOne(ctx, operator, ordered)
	migrations.Unlock(ctx, operator)
	if !errors.As(err, &pgError) || pgError.Code != "55000" {
		t.Fatal("official highest Payment capability Down must explicitly reject rollback")
	}
	if _, err := migrations.VerifyApplied(ctx, operator, loaded); err != nil {
		t.Fatal("forward-only Down changed the installed ledger")
	}
	if qualification() != before {
		t.Fatal("forward-only Down changed the capability or receipt ACL")
	}
	futureSQL := `CREATE TABLE public.f12_capability_future(id integer PRIMARY KEY)`
	digest := sha256.Sum256([]byte(futureSQL))
	future := migrations.Migration{Version: 900022, Name: "f12_capability_future", UpSQL: futureSQL, DownSQL: `DO $$ BEGIN RAISE EXCEPTION 'forward only' USING ERRCODE='55000'; END $$;`, Checksum: hex.EncodeToString(digest[:])}
	all := append(append([]migrations.Migration{}, loaded...), future)
	if err := billingschema.Up(ctx, operator, all); err != nil {
		t.Fatal("official future prepare/finish rejected valid capability")
	}
	migrations.Unlock(ctx, operator)
	if _, err := migrations.VerifyApplied(ctx, operator, all); err != nil {
		t.Fatal("official future migration ledger verification failed")
	}
	if after := qualification(); after != before {
		t.Fatal("future prepare/finish altered owner capability body or ACL")
	}
	badSQL := `CREATE FUNCTION public.f12_unreviewed_capability() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`
	badDigest := sha256.Sum256([]byte(badSQL))
	bad := migrations.Migration{Version: 900023, Name: "f12_unreviewed_capability", UpSQL: badSQL, DownSQL: "SELECT 1", Checksum: hex.EncodeToString(badDigest[:])}
	if err := billingschema.Up(ctx, operator, append(all, bad)); err == nil {
		t.Fatal("future hardener must reject unreviewed public SECURITY DEFINER")
	}
	migrations.Unlock(ctx, operator)
	var absent bool
	if err := pool.QueryRow(ctx, `SELECT to_regprocedure('public.f12_unreviewed_capability()') IS NULL AND NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900023)`).Scan(&absent); err != nil || !absent {
		t.Fatal("failed future capability migration must roll back function and ledger")
	}
	if after := qualification(); after != before {
		t.Fatal("failed future migration weakened capability")
	}
	t.Log("official Up/Verify and future hooks preserve fixed private owner capability; injected default grants, wrong login and runtime DDL denied")
}
