package migrations_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
)

func TestPostgresMigrationCheckoutLineEndingsAndTamperProtection(t *testing.T) {
	pool := testdb.Open(t, "account")
	ctx := context.Background()
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	directory := t.TempDir()
	lf := "CREATE TABLE eol_regression(id BIGINT PRIMARY KEY);\n-- original migration\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	file := filepath.Join(directory, "000003_eol_regression.up.sql")
	load := func(sql string) []migrations.Migration {
		t.Helper()
		if err := os.WriteFile(file, []byte(sql), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := migrations.Load(directory)
		if err != nil || len(loaded) != 1 {
			t.Fatalf("load count=%d error=%v", len(loaded), err)
		}
		return loaded
	}
	up := func(loaded []migrations.Migration) error {
		defer migrations.Unlock(ctx, connection.Conn())
		return migrations.Up(ctx, connection.Conn(), loaded)
	}
	if err := up(load(crlf)); err != nil {
		t.Fatal(err)
	}
	var newChecksum string
	if err := connection.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=3`).Scan(&newChecksum); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(lf))
	if newChecksum != hex.EncodeToString(digest[:]) {
		t.Fatal("new migrations must store the canonical LF checksum")
	}
	for _, legacy := range []string{lf, crlf} {
		legacyDigest := sha256.Sum256([]byte(legacy))
		legacyChecksum := hex.EncodeToString(legacyDigest[:])
		if _, err := connection.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=3`, legacyChecksum); err != nil {
			t.Fatal(err)
		}
		for _, checkout := range []string{lf, crlf} {
			if err := up(load(checkout)); err != nil {
				t.Fatalf("identical SQL across checkout line endings rejected: %v", err)
			}
		}
		if err := up(load(strings.Replace(lf, "BIGINT", "INTEGER", 1))); err == nil || !strings.Contains(err.Error(), "checksum changed") {
			t.Fatalf("real SQL tamper not rejected: %v", err)
		}
		var retained string
		if err := connection.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=3`).Scan(&retained); err != nil || retained != legacyChecksum {
			t.Fatalf("legacy checksum rewritten: error=%v", err)
		}
	}
}
