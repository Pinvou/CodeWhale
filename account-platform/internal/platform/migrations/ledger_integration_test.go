package migrations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPinnedMigrationLedgerPostgresIntegration(t *testing.T) {
	rawURL := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if rawURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is required for real PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	control, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(context.Background())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	role := "ledger_pin_" + suffix
	// Exercise identifier quoting rather than assuming a deployment schema is
	// always a lower-case, unquoted SQL token.
	schema, shadow := `ledger "original_`+suffix, "ledger_shadow_"+suffix
	quotedRole := pgx.Identifier{role}.Sanitize()
	quotedSchema, quotedShadow := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{shadow}.Sanitize()
	password := strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := control.Exec(ctx, "CREATE ROLE "+quotedRole+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '"+password+"'; CREATE SCHEMA "+quotedSchema+" AUTHORIZATION "+quotedRole+"; CREATE SCHEMA "+quotedShadow+" AUTHORIZATION "+quotedRole); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE; DROP SCHEMA "+quotedShadow+" CASCADE; DROP ROLE "+quotedRole); err != nil {
			t.Error("retire owned ledger fixture", err)
		}
	}()
	configuration, err := pgx.ParseConfig(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.User, configuration.Password = role, password
	configuration.RuntimeParams["search_path"] = quotedSchema
	writer, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	var super bool
	var current string
	if err := writer.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&current, &super); err != nil || current != role || super {
		t.Fatal("actual non-superuser migration actor required", err)
	}
	pinned, err := PinLedger(ctx, writer)
	if err != nil {
		t.Fatal(err)
	}
	ledger := pgx.Identifier{schema, "schema_migrations"}.Sanitize()
	shadowLedger := pgx.Identifier{shadow, "schema_migrations"}.Sanitize()
	if name, err := LedgerTable(pinned, writer); err != nil || name != ledger {
		t.Fatal("fresh ledger did not retain its original namespace", name, err)
	}
	var created bool
	if err := writer.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`, ledger).Scan(&created); err != nil || created {
		t.Fatal("binding unexpectedly created a ledger before migration admission", err)
	}
	if _, err := writer.Exec(ctx, "CREATE TABLE "+shadowLedger+"(version bigint primary key,name text,checksum text); INSERT INTO "+shadowLedger+" VALUES(1,'shadow_fact','must-remain')"); err != nil {
		t.Fatal(err)
	}
	first := Migration{Version: 1, Name: "change_path", UpSQL: "CREATE TABLE " + quotedSchema + `.probe(id integer primary key); SET search_path=` + quotedShadow, DownSQL: "SET search_path=" + quotedShadow + "; DROP TABLE " + quotedSchema + ".probe"}
	first.Checksum = normalizedChecksum(first.UpSQL)
	second := Migration{Version: 2, Name: "original_namespace", UpSQL: "INSERT INTO " + quotedSchema + ".probe VALUES(7)", DownSQL: "SET search_path=" + quotedShadow + "; DELETE FROM " + quotedSchema + ".probe WHERE id=7"}
	second.Checksum = normalizedChecksum(second.UpSQL)
	loaded := []Migration{first, second}
	shadowFacts := func() string {
		t.Helper()
		var facts string
		if err := writer.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM "+shadowLedger+" x").Scan(&facts); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	beforeShadow := shadowFacts()
	t.Run("up-path-change-keeps-original-ledger", func(t *testing.T) {
		if err := Up(ctx, writer, loaded); err != nil {
			t.Fatal("search_path changed the migration bookkeeping target", err)
		}
		Unlock(ctx, writer)
		if _, err := VerifyApplied(pinned, writer, loaded); err != nil {
			t.Fatal("post-Up verification abandoned the pinned ledger", err)
		}
		if beforeShadow != shadowFacts() {
			t.Fatal("migration mutated the unrelated shadow ledger")
		}
		if _, err := control.Exec(ctx, "REVOKE CREATE ON SCHEMA "+quotedSchema+" FROM "+quotedRole); err != nil {
			t.Fatal(err)
		}
		// Ownership itself implies CREATE, so transfer only the schema (not its
		// existing objects) before checking the original no-CREATE upgrade path.
		if _, err := control.Exec(ctx, "ALTER SCHEMA "+quotedSchema+" OWNER TO CURRENT_USER; GRANT USAGE ON SCHEMA "+quotedSchema+" TO "+quotedRole); err != nil {
			t.Fatal(err)
		}
		var create bool
		if err := writer.QueryRow(ctx, `SELECT has_schema_privilege($1,'CREATE')`, schema).Scan(&create); err != nil || create {
			t.Fatal("fixture did not revoke original schema CREATE", err)
		}
		if err := Up(pinned, writer, loaded); err != nil {
			t.Fatal("idempotent upgrade required schema CREATE or followed shadow", err)
		}
		Unlock(ctx, writer)
	})
	t.Run("verify-rejects-original-drift-despite-shadow", func(t *testing.T) {
		if _, err := writer.Exec(ctx, "UPDATE "+ledger+" SET checksum='altered' WHERE version=2"); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(pinned, writer, loaded); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatal("verification did not inspect the original ledger", err)
		}
		if _, err := writer.Exec(ctx, "UPDATE "+ledger+" SET checksum=$1 WHERE version=2", second.Checksum); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyApplied(pinned, writer, loaded); err != nil {
			t.Fatal("verification could not recover after original history was restored", err)
		}
	})
	t.Run("down-path-change-removes-only-original-row", func(t *testing.T) {
		if err := DownOne(pinned, writer, loaded); err != nil {
			t.Fatal(err)
		}
		Unlock(ctx, writer)
		if _, err := VerifyApplied(pinned, writer, []Migration{first}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := writer.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".probe").Scan(&count); err != nil || count != 0 || beforeShadow != shadowFacts() {
			t.Fatal("rollback redirected its ledger or failed its business effect", err)
		}
		if err := Up(pinned, writer, loaded); err != nil {
			t.Fatal("correct next invocation failed", err)
		}
		Unlock(ctx, writer)
	})
	t.Run("actual-cli-retains-bound-namespace", func(t *testing.T) {
		directory := t.TempDir()
		third := Migration{Version: 3, Name: "cli_change_path",
			UpSQL:   "INSERT INTO " + quotedSchema + ".probe VALUES(8); SET search_path=" + quotedShadow,
			DownSQL: "SET search_path=" + quotedShadow + "; DELETE FROM " + quotedSchema + ".probe WHERE id=8"}
		for _, migration := range append(append([]Migration{}, loaded...), third) {
			for direction, body := range map[string]string{"up": migration.UpSQL, "down": migration.DownSQL} {
				name := fmt.Sprintf("%06d_%s.%s.sql", migration.Version, migration.Name, direction)
				if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		endpoint, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal("parse owned CLI endpoint")
		}
		endpoint.User = url.UserPassword(role, password)
		parameters := endpoint.Query()
		parameters.Set("search_path", quotedSchema)
		endpoint.RawQuery = parameters.Encode()
		goTool := os.Getenv("PINVOU_ACCOUNT_TEST_GO")
		if goTool == "" {
			goTool = "go"
		}
		run := func(direction string) []byte {
			t.Helper()
			command := exec.CommandContext(ctx, goTool, "run", "-race", "-tags=nomsgpack", "-mod=readonly", "./cmd/migrate", "--dir", directory, "--direction", direction)
			command.Dir = filepath.Join("..", "..", "..")
			command.Env = append(os.Environ(), "DATABASE_URL="+endpoint.String(), "MIGRATION_ROLE=", "PINVOU_DEPLOYMENT_ENVIRONMENT=ci")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("actual migrate %s did not retain its ledger: %v\n%s", direction, err, output)
			}
			return output
		}
		var evidence struct {
			Event string `json:"event"`
			Verification
		}
		if err := json.Unmarshal(run("up"), &evidence); err != nil || evidence.Event != "migration.post_verification" || evidence.AppliedCount != 3 || evidence.LatestVersion != 3 {
			t.Fatal("CLI did not verify the original three-row ledger", err)
		}
		run("down")
		var count int
		if _, err := VerifyApplied(pinned, writer, loaded); err != nil {
			t.Fatal("CLI did not restore the original ledger", err)
		}
		if err := writer.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".probe WHERE id=8").Scan(&count); err != nil || count != 0 || beforeShadow != shadowFacts() {
			t.Fatal("actual CLI changed shadow facts or left its business effect", err)
		}
	})
	t.Run("binding-cannot-cross-connections", func(t *testing.T) {
		if err := Up(pinned, control, loaded); err == nil || !strings.Contains(err.Error(), "another connection") {
			t.Fatal("another connection reused migration ledger authority", err)
		}
		if _, err := VerifyApplied(pinned, writer, loaded); err != nil || beforeShadow != shadowFacts() {
			t.Fatal("rejected cross-connection call changed the original workflow", err)
		}
	})
	t.Run("missing-search-path-schema-rejects-before-preparation", func(t *testing.T) {
		if _, err := writer.Exec(ctx, `SET search_path=''`); err != nil {
			t.Fatal(err)
		}
		if _, err := PinLedger(ctx, writer); err == nil || !strings.Contains(err.Error(), "existing schema") {
			t.Fatal("missing namespace silently selected another schema", err)
		}
		if _, err := VerifyApplied(pinned, writer, loaded); err != nil || beforeShadow != shadowFacts() {
			t.Fatal("failed namespace selection damaged the pinned workflow", err)
		}
		if _, err := writer.Exec(ctx, "SET search_path="+quotedShadow); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("another-workflow-selects-its-own-visible-ledger", func(t *testing.T) {
		other, err := PinLedger(ctx, writer)
		if err != nil {
			t.Fatal(err)
		}
		if name, err := LedgerTable(other, writer); err != nil || name != shadowLedger {
			t.Fatal("independent custom schema workflow inherited a sticky binding", name, err)
		}
	})
}
