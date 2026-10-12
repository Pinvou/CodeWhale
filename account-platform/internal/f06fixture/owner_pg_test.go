package f06fixture

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
	f := &stageFixture{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}}
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
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator`); err != nil {
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

func TestPrivateF06OrganizationFundsPG(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_F06_OWNER_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_F06_OWNER_DATABASE_URL is required for the private isolated owner runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	t.Cleanup(cancel)
	a, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal("admin connect")
	}
	var dd, u string
	var port, actors, sessions int
	e = a.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_setting('port')::int,(SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'pinvou_wave_test'),(SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`).Scan(&dd, &u, &port, &actors, &sessions)
	a.Close(ctx)
	if e != nil || u != "pinvou_wave_test" || port != 55506 || actors != 0 || sessions != 0 || strings.ReplaceAll(dd, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f06/data" {
		t.Fatal("owner/port/datadir/roles/sessions guard")
	}
	t.Log("runtimePre actor0/session0 exact owner datadir55506")
	// Registered before role fixture, so this independent post assertion runs after role cleanup.
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		p, e := pgx.Connect(c, raw)
		if e != nil {
			t.Error("post connect")
			return
		}
		defer p.Close(c)
		var actors, dbs, sessions int
		e = p.QueryRow(c, `SELECT (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'pinvou_wave_test'),(SELECT count(*) FROM pg_database WHERE (datname LIKE 'retained_finance_%' OR datname LIKE 'f04_token_management_%')),(SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`).Scan(&actors, &dbs, &sessions)
		if e != nil || actors != 0 || dbs != 0 || sessions != 0 {
			t.Errorf("runtimePost actors=%d DB=%d sessions=%d error=%v", actors, dbs, sessions, e)
		} else {
			t.Log("runtimePost actors0/ownedDB0/sessions0")
		}
	})
	newStageFixture(t, ctx, raw)
	out := os.Getenv("PINVOU_PRIVATE_OUTPUT_DIR")
	o, e := os.Create(filepath.Join(out, "child.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	s, e := os.Create(filepath.Join(out, "child.stderr.log"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	cmd := exec.CommandContext(ctx, os.Getenv("PINVOU_PRIVATE_GO"), "test", "-race", "-json", "-count=1", "-mod=readonly", "-run", "^TestOrganizationExecutionPublicPostgresFundsAndReplay$", "./internal/token")
	cmd.Dir = "../.."
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "PINVOU_TEST_BILLING_DATABASE_URL=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "PINVOU_TEST_BILLING_DATABASE_URL="+raw)
	cmd.Stdout = o
	cmd.Stderr = s
	if e = cmd.Run(); e != nil {
		t.Fatalf("original owner cases failed: %v; see preserved child streams", e)
	}
}
