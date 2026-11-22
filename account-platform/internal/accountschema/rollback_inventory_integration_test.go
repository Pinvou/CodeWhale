package accountschema

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAccount039RollbackInventoryPostgres(t *testing.T) {
	raw := os.Getenv("F17_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Fatal("psql is required for the 039 inventory contract")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	f := newAuditExportGuardFixture(t, ctx, raw)
	const reader = "account039_inventory_reader"
	if _, err := f.admin.Exec(ctx, "CREATE ROLE "+reader+" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	f.roles = append(f.roles, reader)
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}

	open := func(t *testing.T, latest int64) *pgx.Conn {
		t.Helper()
		c := f.database(t, ctx, "account", "account_service")
		var prefix []migrations.Migration
		for _, migration := range all {
			if migration.Version <= latest {
				prefix = append(prefix, migration)
			}
		}
		if latest > 0 && (len(prefix) == 0 || prefix[len(prefix)-1].Version != latest) {
			t.Fatal("historical migration prefix is missing")
		}
		if err := migrations.Up(ctx, c, prefix); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, c)
		if _, err := migrations.VerifyApplied(ctx, c, prefix); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+reader+"; GRANT SELECT(version,name,checksum) ON public.schema_migrations TO "+reader); err != nil {
			t.Fatal(err)
		}
		if latest >= 18 {
			if _, err := c.Exec(ctx, "GRANT SELECT(command_type,policy_version) ON public.approval_requests TO "+reader); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	run := func(c *pgx.Conn, expectedDatabase string) (string, string, error) {
		child := exec.CommandContext(ctx, psql, "-X", "--no-password", "--quiet", "--csv", "--tuples-only",
			"--dbname="+f.serviceURL(t, c, reader), "--set=expected_database="+expectedDatabase,
			"--set=VERBOSITY=verbose", "--file=../../deploy/runbooks/sql/account-039-rollback-inventory.sql")
		var stdout, stderr strings.Builder
		child.Stdout, child.Stderr = &stdout, &stderr
		for _, item := range os.Environ() {
			key, _, _ := strings.Cut(item, "=")
			if !strings.HasPrefix(strings.ToUpper(key), "PG") {
				child.Env = append(child.Env, item)
			}
		}
		err := child.Run()
		return stdout.String(), stderr.String(), err
	}
	ledger := func(t *testing.T, c *pgx.Conn) string {
		t.Helper()
		var snapshot string
		if err := c.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY version), '[]'::jsonb)::text FROM schema_migrations m").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	expect := func(t *testing.T, c *pgx.Conn, latest int64, references, assessment string) {
		t.Helper()
		before := ledger(t, c)
		out, diagnostic, err := run(c, c.Config().Database)
		if err != nil || diagnostic != "" {
			t.Fatalf("inventory failed: %v %s", err, diagnostic)
		}
		parser := csv.NewReader(strings.NewReader(out))
		parser.FieldsPerRecord = -1
		rows, err := parser.ReadAll()
		if err != nil || len(rows) != 2 || len(rows[0]) != 1 || rows[0][0] != "1" || len(rows[1]) != 7 {
			t.Fatalf("unexpected inventory wire: %q", out)
		}
		row := rows[1]
		highest, name, checksum, identity := "", "", "", ""
		if latest > 0 {
			highest = strconv.FormatInt(latest, 10)
		}
		if latest >= 39 {
			name, identity = "organization_governance_permissions", "t"
			if err := c.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version=39").Scan(&checksum); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := time.Parse("2006-01-02 15:04:05", row[0]); err != nil ||
			row[1] != highest || row[2] != name || row[3] != checksum || row[4] != identity || row[5] != references || row[6] != assessment {
			t.Fatalf("wrong inventory output: %q", row)
		}
		if ledger(t, c) != before {
			t.Fatal("inventory changed the migration ledger")
		}
	}
	for _, tc := range []struct {
		name       string
		latest     int64
		assessment string
	}{
		{"empty-ledger", 0, "EMPTY_MIGRATION_LEDGER"},
		{"before-approval-schema", 17, "039_NOT_INSTALLED"},
		{"installed-039", 39, "039_RESTORE_REHEARSAL_REQUIRED"},
		{"higher-version", 40, "LATER_MIGRATIONS_PRESENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := open(t, tc.latest)
			expect(t, c, tc.latest, "0", tc.assessment)
		})
	}
	c := open(t, 39)
	t.Run("retained-v2-reference", func(t *testing.T) {
		user, binding := uuid.New(), uuid.New()
		for _, fixture := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,generate_unique_user_code(),'ACTIVE',now(),now())`, []any{user}},
			{`INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) SELECT $1,$2,code,'ACCOUNT',now(),now() FROM roles WHERE scope='PLATFORM' ORDER BY code LIMIT 1`, []any{binding, user}},
			{`INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,created_at,updated_at)
VALUES($1,'ACCOUNT_ORGANIZATION_RENAME',2,$2,$3,1,'controlled-039-reference','{}',decode(repeat('00',32),'hex'),'ORGANIZATION',$4,'controlled inventory','[]','{}','DRAFT',now()+interval '1 hour',now(),now())`, []any{uuid.New(), user, binding, uuid.NewString()}},
		} {
			tag, err := c.Exec(ctx, fixture.sql, fixture.args...)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("seed retained approval: %v", err)
			}
		}
		expect(t, c, 39, "1", "039_V2_POLICY_REFERENCED")
	})
	expectFailure := func(t *testing.T, expectedDatabase, expectedGuardOutput string) {
		t.Helper()
		before := ledger(t, c)
		out, diagnostic, err := run(c, expectedDatabase)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(diagnostic, "22012") || strings.TrimSpace(out) != expectedGuardOutput {
			t.Fatalf("inventory did not fail closed: exit=%v stdout=%q stderr=%q", err, out, diagnostic)
		}
		if ledger(t, c) != before {
			t.Fatal("failed inventory changed the ledger")
		}
	}
	t.Run("wrong-database", func(t *testing.T) {
		expectFailure(t, c.Config().Database+"_different", "")
	})
	t.Run("missing-approval-column-grant", func(t *testing.T) {
		if _, err := c.Exec(ctx, "REVOKE SELECT(policy_version) ON public.approval_requests FROM "+reader); err != nil {
			t.Fatal(err)
		}
		expectFailure(t, c.Config().Database, "1")
	})
}
