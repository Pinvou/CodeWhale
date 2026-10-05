package testdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresExtensionSurvivesOtherFixtureCleanup(t *testing.T) {
	if strings.TrimSpace(os.Getenv(DatabaseURLEnv)) == "" {
		t.Skip(DatabaseURLEnv + " is not configured; real PostgreSQL test not executed")
	}
	var survivor *pgxpool.Pool
	t.Run("extension-creator", func(child *testing.T) {
		_ = Open(child, "account")
		// The surviving fixture belongs to the parent, so the creator's cleanup
		// executes while the survivor still needs the extension functions.
		survivor = Open(t, "account")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var size int
	var digest string
	if err := survivor.QueryRow(ctx, `SELECT octet_length(gen_random_bytes(16)), encode(digest(convert_to('fixture', 'UTF8'), 'sha256'), 'hex')`).Scan(&size, &digest); err != nil {
		t.Fatalf("another fixture's cleanup removed required extension functions: %v", err)
	}
	if size != 16 || digest != "f16d05ec6b29248d2c61adb1e9263f78e4f7bace1b955014a2d17872cfe4064d" {
		t.Fatalf("unexpected extension output: %d/%s", size, digest)
	}
}
