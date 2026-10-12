package f06fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These are real public cases, not a guessed organization test name. The
// personal execution cases verify the shared Token/Billing execution layer;
// the member cases verify genuine organization grants and financial holds.
var fundsChildTargets = map[string]string{
	"github.com/Pinvou/pinvou3/account-platform/internal/token":   "TestPersonalExecutionPublicPostgresFundsAndReplay",
	"github.com/Pinvou/pinvou3/account-platform/internal/billing": "TestMemberHoldIdentityMutationBoundaries",
}

func validateFundsChildJSON(input io.Reader) (int, error) {
	decoder := json.NewDecoder(input)
	parents, leaves := make(map[string]bool), make(map[string]int)
	passes := 0
	for {
		var event struct{ Action, Package, Test string }
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return passes, fmt.Errorf("decode child evidence: %w", err)
		}
		if event.Action == "fail" || event.Action == "skip" {
			return passes, fmt.Errorf("child did not execute successfully: %s %s", event.Action, event.Test)
		}
		if event.Action != "pass" || event.Test == "" {
			continue
		}
		target, known := fundsChildTargets[event.Package]
		if !known || (event.Test != target && !strings.HasPrefix(event.Test, target+"/")) {
			return passes, fmt.Errorf("unexpected child test: %s", event.Test)
		}
		passes++
		if event.Test == target {
			parents[event.Package] = true
		} else {
			leaves[event.Package]++
		}
	}
	for pkg := range fundsChildTargets {
		if !parents[pkg] || leaves[pkg] == 0 {
			return passes, fmt.Errorf("no completed public cases for %s", pkg)
		}
	}
	return passes, nil
}

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
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE`); err != nil {
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
	config, e := pgx.ParseConfig(raw)
	if e != nil {
		t.Fatal("invalid explicit owner connection")
	}
	loopback := func(host string) bool {
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	if !loopback(config.Host) {
		t.Fatal("explicit owner runtime must be local")
	}
	for _, fallback := range config.Fallbacks {
		if !loopback(fallback.Host) || fallback.Port != config.Port {
			t.Fatal("owner connection fallback leaves the isolated runtime")
		}
	}
	expectedDirectory := os.Getenv("PINVOU_PRIVATE_OWNER_DATA_DIRECTORY")
	expectedUser := os.Getenv("PINVOU_PRIVATE_OWNER_USER")
	if !filepath.IsAbs(expectedDirectory) || expectedUser == "" {
		t.Fatal("explicit owner directory and user are required")
	}
	out := os.Getenv("PINVOU_PRIVATE_OUTPUT_DIR")
	goExecutable := os.Getenv("PINVOU_PRIVATE_GO")
	if !filepath.IsAbs(out) || !filepath.IsAbs(goExecutable) {
		t.Fatal("absolute private output directory and Go executable are required")
	}
	outputInfo, outputErr := os.Stat(out)
	goInfo, goErr := os.Stat(goExecutable)
	if outputErr != nil || !outputInfo.IsDir() || goErr != nil || !goInfo.Mode().IsRegular() {
		t.Fatal("private output directory or Go executable is unavailable")
	}
	a, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal("admin connect")
	}
	var dd, u string
	var port, actors, sessions int
	e = a.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_setting('port')::int,(SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>$1),(SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`, expectedUser).Scan(&dd, &u, &port, &actors, &sessions)
	a.Close(ctx)
	normalizeDirectory := func(value string) string { return filepath.ToSlash(filepath.Clean(value)) }
	if e != nil || u != expectedUser || port != int(config.Port) || actors != 0 || sessions != 0 || normalizeDirectory(dd) != normalizeDirectory(expectedDirectory) {
		t.Fatal("owner/port/datadir/roles/sessions guard")
	}
	t.Log("runtimePre: isolated owner, directory and port verified; actors0/sessions0")
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
		e = p.QueryRow(c, `SELECT (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>$1),(SELECT count(*) FROM pg_database WHERE (datname LIKE 'retained_finance_%' OR datname LIKE 'f04_token_management_%')),(SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`, expectedUser).Scan(&actors, &dbs, &sessions)
		if e != nil || actors != 0 || dbs != 0 || sessions != 0 {
			t.Errorf("runtimePost actors=%d DB=%d sessions=%d error=%v", actors, dbs, sessions, e)
		} else {
			t.Log("runtimePost actors0/ownedDB0/sessions0")
		}
	})
	newStageFixture(t, ctx, raw)
	o, e := os.OpenFile(filepath.Join(out, "child.jsonl"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer o.Close()
	s, e := os.OpenFile(filepath.Join(out, "child.stderr.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	cmd := exec.CommandContext(ctx, goExecutable, "test", "-p=1", "-race", "-json", "-count=1", "-mod=readonly", "-run", "^Test(PersonalExecutionPublicPostgresFundsAndReplay|MemberHoldIdentityMutationBoundaries)$", "./internal/token", "./internal/billing")
	cmd.Dir = "../.."
	for _, value := range os.Environ() {
		upper := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !(strings.HasPrefix(upper, "PINVOU_TEST_") && strings.Contains(upper, "DATABASE")) {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "PINVOU_TEST_BILLING_DATABASE_URL="+raw, "PINVOU_TEST_TOKEN_DATABASE_URL="+raw)
	cmd.Stdout = o
	cmd.Stderr = s
	if e = cmd.Run(); e != nil {
		t.Fatalf("original owner cases failed: %v; see preserved child streams", e)
	}
	if _, e = o.Seek(0, io.SeekStart); e != nil {
		t.Fatal("rewind child evidence")
	}
	passes, e := validateFundsChildJSON(o)
	if e != nil {
		t.Fatalf("public funds cases were not executed: %v; see preserved child streams", e)
	}
	t.Logf("actual public funds child: %d named passes, no fail/skip; controlled provider and identity facts", passes)
}
