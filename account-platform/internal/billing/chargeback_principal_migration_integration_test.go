package billing

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func principalMigrationTool(t *testing.T) string {
	t.Helper()
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("actual PostgreSQL is required for migration CLI acceptance")
	}
	name := "migrate"
	goName := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
		goName += ".exe"
	}
	product := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "build", "-tags=nomsgpack", "-mod=readonly", "-o", product, "./cmd/migrate")
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build actual migration CLI: %v\n%s", err, output)
	}
	return product
}

func principalMigrationCLI(t *testing.T, ctx context.Context, product string, r *PostgresRepository, actor, direction string, allowed bool) {
	t.Helper()
	configuration := r.pool.Config().ConnConfig
	directory, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	// ConnString retains parsed input. Construct from the owned fixture's actual
	// database and actor fields rather than accidentally using its parent DSN.
	dsn := url.URL{Scheme: "postgres", User: url.User(actor), Host: net.JoinHostPort(configuration.Host, strconv.Itoa(int(configuration.Port))), Path: "/" + configuration.Database, RawQuery: "sslmode=disable"}
	child, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(child, product, "--dir", directory, "--direction", direction, "--timeout", "15s")
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "DATABASE_URL" && key != "MIGRATION_ROLE" && key != "MIGRATIONS_DIR" && key != "MIGRATION_TIMEOUT" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" && !strings.HasPrefix(key, "MIGRATION_BACKUP_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "DATABASE_URL="+dsn.String(), "MIGRATION_ROLE=billing_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
	output, err := command.CombinedOutput()
	if child.Err() != nil {
		t.Fatal("actual migration CLI exceeded its bounded operation")
	}
	if !allowed {
		if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
			t.Fatal("actual migration CLI did not reach its expected refusal", actor, direction)
		}
		return
	}
	var evidence struct {
		Event          string `json:"event"`
		Domain         string `json:"domain"`
		AppliedCount   int64  `json:"applied_count"`
		LatestVersion  int64  `json:"latest_version"`
		LatestChecksum string `json:"latest_checksum"`
	}
	var applied int64
	if queryErr := r.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); queryErr != nil {
		t.Fatal(queryErr)
	}
	if err != nil || json.Unmarshal(output, &evidence) != nil || evidence.Event != "migration.post_verification" || evidence.Domain != "billing" || evidence.AppliedCount != applied || evidence.LatestVersion != 900044 || evidence.LatestChecksum != "baf6031690dc3598224b4298cd2b11c3c4b4f693b7ee0acbd0fc63f111c5bf62" {
		t.Fatal("actual migration CLI failed its post-verification contract", actor, direction)
	}
}

func principalMigrationAuthoritySnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
 (SELECT jsonb_agg(jsonb_build_array(relname,pg_get_userbyid(relowner),relacl::text) ORDER BY relname) FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','v','S')),
 (SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgrelid,tgname) FROM pg_trigger WHERE NOT tgisinternal),
 (SELECT nspacl::text FROM pg_namespace WHERE oid='public'::regnamespace),
 pg_has_role('billing_service','billing_schema_owner','MEMBER'),has_schema_privilege('billing_service','public','CREATE'))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
