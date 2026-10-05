package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSortsAndPairsMigrations(t *testing.T) {
	directory := t.TempDir()
	writeMigration(t, directory, "000002_second.up.sql", "SELECT 2;")
	writeMigration(t, directory, "000001_first.down.sql", "SELECT -1;")
	writeMigration(t, directory, "000001_first.up.sql", "SELECT 1;")
	writeMigration(t, directory, "README.md", "ignored")

	loaded, err := Load(directory)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(loaded) != 2 || loaded[0].Version != 1 || loaded[1].Version != 2 {
		t.Fatalf("unexpected migration order: %+v", loaded)
	}
	if loaded[0].DownSQL != "SELECT -1;" || loaded[0].Checksum == "" {
		t.Fatalf("migration was not paired or hashed: %+v", loaded[0])
	}
}

func TestLoadNormalizesLineEndingsForChecksum(t *testing.T) {
	directory := t.TempDir()
	writeMigration(t, directory, "000001_first.up.sql", "SELECT 1;\r\nSELECT 2;\r\n")

	loaded, err := Load(directory)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	digest := sha256.Sum256([]byte("SELECT 1;\nSELECT 2;\n"))
	want := hex.EncodeToString(digest[:])
	if got := loaded[0].Checksum; got != want {
		t.Fatalf("Checksum = %q, want %q", got, want)
	}
}

func TestChecksumMatchesLegacyLineEndingsOnly(t *testing.T) {
	lf := "SELECT 1;\nSELECT 2;\n"
	crlf := "SELECT 1;\r\nSELECT 2;\r\n"

	if !matchesChecksum(checksum(crlf), lf) {
		t.Fatal("checksumMatches rejected the equivalent legacy CRLF checksum")
	}
	if !matchesChecksum(checksum(lf), crlf) {
		t.Fatal("checksumMatches rejected the equivalent normalized LF checksum")
	}
	if matchesChecksum(checksum("SELECT 3;\n"), lf) {
		t.Fatal("checksumMatches accepted changed SQL")
	}
}

func TestLoadRejectsMissingUpAndConflictingNames(t *testing.T) {
	t.Run("missing up", func(t *testing.T) {
		directory := t.TempDir()
		writeMigration(t, directory, "000001_first.down.sql", "SELECT 1;")
		if _, err := Load(directory); err == nil {
			t.Fatal("Load succeeded, want error")
		}
	})
	t.Run("conflicting name", func(t *testing.T) {
		directory := t.TempDir()
		writeMigration(t, directory, "000001_first.up.sql", "SELECT 1;")
		writeMigration(t, directory, "000001_other.down.sql", "SELECT 1;")
		if _, err := Load(directory); err == nil {
			t.Fatal("Load succeeded, want error")
		}
	})
}

func writeMigration(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write migration fixture: %v", err)
	}
}
