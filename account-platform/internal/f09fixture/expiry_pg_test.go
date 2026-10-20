package f09fixture

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stageFixture struct {
	admin       *pgx.Conn
	config      *pgx.ConnConfig
	roles       []string
	dbs         map[string]string
	connections []*pgx.Conn
}

func newStageFixture(t *testing.T, ctx context.Context, raw string) *stageFixture {
	t.Helper()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect schema setup identity")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		admin.Close(context.Background())
		t.Fatal("parse schema fixture")
	}
	f := &stageFixture{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator", "token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"}}
	var count int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
		admin.Close(context.Background())
		t.Fatal("existing deployment actors: no fixture writes permitted")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, role := range f.roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" || role == "billing_service" {
			inherit = "INHERIT"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("exclusive actor creation rejected")
		}
	}
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE; GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		defer admin.Close(clean)
		for _, c := range f.connections {
			if err := c.Close(clean); err != nil {
				t.Error("owned migration connection close failed")
			}
		}
		allGone := true
		for name, owner := range f.dbs {
			var got string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			var queryErr error
			for {
				queryErr = admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&got, &active)
				if queryErr != nil || got != owner || active == 0 || !time.Now().Before(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if queryErr != nil || got != owner || active != 0 {
				t.Error("owned DB cleanup identity/connection guard failed")
				allGone = false
				continue
			}
			if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error("nonforce owned DB cleanup failed")
				allGone = false
			}
		}
		if !allGone {
			t.Error("owned actors retained after incomplete DB cleanup")
			return
		}
		var outside bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, f.roles).Scan(&outside); err != nil || outside {
			t.Error("external dependencies forbid actor cleanup")
			return
		}
		for i := len(f.roles) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{f.roles[i]}.Sanitize()); err != nil {
				t.Error("nonforce owned actor cleanup failed")
			}
		}
		if err := admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
			t.Error("owned actors remain after cleanup")
		}
	})
	return f
}

func TestF09ExpiryPublicCoordinator(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = newStageFixture(t, ctx, raw)
	dir := filepath.Join("..", "billing")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"test", "-race", "-json", "-count=1", "-mod=readonly", "-run", "^TestF09ExpiryAfterComponentLockPostgres$"}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			args = append(args, filepath.Join(dir, e.Name()))
		}
	}
	args = append(args, filepath.Join(dir, "retained_financial_integration_test.go"), filepath.Join(dir, "finance_refund_reconcile_integration_test.go"), filepath.Join(dir, "entitlement_correction_expiry_integration_test.go"))
	child := exec.CommandContext(ctx, os.Getenv("PINVOU_PRIVATE_GO"), args...)
	child.Dir = "."
	child.Env = append(os.Environ(), "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
	stream, err := os.Create(os.Getenv("PINVOU_F09_CHILD_STREAM"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	child.Stdout = stream
	child.Stderr = stream
	if err = child.Run(); err != nil {
		t.Fatalf("finite expiry runtime owner failed: %v", err)
	}
}
