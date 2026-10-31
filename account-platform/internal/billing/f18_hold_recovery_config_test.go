package billing

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// f18OwnedConnectionConfig admits only an explicitly declared, loopback-owned
// disposable PostgreSQL instance. The profile is opt-in so an ordinary local
// run still reports a clear conditional skip instead of borrowing a service.
func f18OwnedConnectionConfig(t *testing.T, raw string) (*pgx.ConnConfig, string, int) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("F18_TEST_DATABASE_PROFILE")) == "" {
		t.Skip("F18_TEST_DATABASE_PROFILE=local-owned is required")
	}
	if os.Getenv("F18_TEST_DATABASE_PROFILE") != "local-owned" {
		t.Fatal("unknown F18_TEST_DATABASE_PROFILE")
	}
	expectedUser := strings.TrimSpace(os.Getenv("F18_TEST_DATABASE_USER"))
	expectedDirectory := filepath.Clean(strings.TrimSpace(os.Getenv("F18_TEST_DATA_DIRECTORY")))
	portText := strings.TrimSpace(os.Getenv("F18_TEST_DATABASE_PORT"))
	portValue, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portValue < 1024 || strconv.FormatUint(portValue, 10) != portText || expectedUser == "" || !f18OwnedDirectoryAllowed(expectedDirectory) {
		t.Fatal("local-owned F18 identity configuration is incomplete or unsafe")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Host != "127.0.0.1" || config.Port != uint16(portValue) || config.User != expectedUser || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 || len(config.RuntimeParams) != 0 {
		t.Fatal("F18 PostgreSQL URL is not the declared local-owned instance")
	}
	return config, expectedDirectory, int(portValue)
}

func f18OwnedDirectoryAllowed(directory string) bool {
	clean := filepath.Clean(directory)
	if !filepath.IsAbs(directory) || filepath.Base(clean) != "data" {
		return false
	}
	parent := filepath.Dir(clean)
	if filepath.Clean(filepath.Dir(parent)) != filepath.Clean(os.TempDir()) {
		return false
	}
	const prefix = "pinvou-f18-billing-"
	name := filepath.Base(parent)
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	id := strings.TrimPrefix(name, prefix)
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func f18OwnedDirectoryMatches(actual, expected string) bool {
	return f18OwnedDirectoryAllowed(expected) && filepath.Clean(filepath.FromSlash(actual)) == filepath.Clean(expected)
}
