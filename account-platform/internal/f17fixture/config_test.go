package f17fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestOwnedAuditFixtureConfiguration(t *testing.T) {
	directory := filepath.Join(os.TempDir(), "pinvou-f17-audit-11111111-1111-4111-8111-111111111111", "data")
	for _, tc := range []struct {
		name, url, port, directory string
		allowed                    bool
	}{
		{"owned", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=disable", "55474", directory, true},
		{"remote", "postgres://pinvou_audit_fixture@192.0.2.1:55474/postgres?sslmode=disable", "55474", directory, false},
		{"wrong_actor", "postgres://postgres@127.0.0.1:55474/postgres?sslmode=disable", "55474", directory, false},
		{"wrong_port", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=disable", "55475", directory, false},
		{"noncanonical_port", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=disable", "055474", directory, false},
		{"deployment_database", "postgres://pinvou_audit_fixture@127.0.0.1:55474/deployment?sslmode=disable", "55474", directory, false},
		{"fallback", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=prefer", "55474", directory, false},
		{"unowned_directory", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=disable", "55474", filepath.Join(os.TempDir(), "deployment", "data"), false},
		{"missing_directory", "postgres://pinvou_audit_fixture@127.0.0.1:55474/postgres?sslmode=disable", "55474", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := pgx.ParseConfig(tc.url)
			if err != nil {
				t.Fatal("synthetic configuration rejected")
			}
			if OwnedConfigurationAllowed(c, tc.port, tc.directory) != tc.allowed {
				t.Fatal("fixture admission mismatch")
			}
		})
	}
	if !DirectoryMatches(filepath.ToSlash(directory), directory) || DirectoryMatches(filepath.Join(os.TempDir(), "outside", "data"), directory) {
		t.Fatal("server directory identity mismatch")
	}
}
