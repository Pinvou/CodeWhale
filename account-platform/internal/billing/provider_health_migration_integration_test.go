package billing

import (
	"context"
	"github.com/jackc/pgx/v5"
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

func providerHealthMigrationTool(t *testing.T) string {
	t.Helper()
	name := "migrate"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	product := filepath.Join(t.TempDir(), name)
	goTool := os.Getenv("PINVOU_ACCOUNT_TEST_GO")
	if goTool == "" {
		var err error
		goTool, err = exec.LookPath("go")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, goTool, "build", "-tags=nomsgpack", "-mod=readonly", "-o", product, "./cmd/migrate")
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build actual migration command: %v\n%s", err, output)
	}
	return product
}

func providerHealthCLI(ctx context.Context, product string, r *PostgresRepository, actor, direction string) ([]byte, error) {
	configuration := r.pool.Config().ConnConfig.Copy()
	directory, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, product, "--dir", directory, "--direction", direction)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "DATABASE_URL" && key != "MIGRATION_ROLE" && key != "MIGRATIONS_DIR" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" && !strings.HasPrefix(key, "MIGRATION_BACKUP_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "DATABASE_URL="+providerHealthDSN(configuration, actor), "MIGRATION_ROLE=billing_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
	return command.CombinedOutput()
}

// ConnConfig.ConnString retains the original parsed text, so changing its
// fields must not be used to choose a different fixture database or actor.
func providerHealthDSN(configuration *pgx.ConnConfig, actor string) string {
	endpoint := url.URL{Scheme: "postgres", User: url.User(actor), Host: net.JoinHostPort(configuration.Host, strconv.Itoa(int(configuration.Port))), Path: "/" + configuration.Database, RawQuery: "sslmode=disable"}
	return endpoint.String()
}
