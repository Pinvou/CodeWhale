package migrations

import (
	"encoding/json"
	"fmt"
	"strings"

	migrationmanifest "github.com/Pinvou/pinvou3/account-platform/migrations"
)

type rollbackIdentity struct {
	version int64
	up      string
}

// Published rollback bytes already have an immutable manifest. Reuse it rather
// than changing old ledger checksums or blessing the currently supplied Down SQL
// through a database backfill. Custom histories outside that inventory retain
// their existing behavior; the inventory does not attest their historical Down.
var publishedRollbackChecksums, publishedRollbackError = rollbackChecksums(migrationmanifest.JSON())

func rollbackChecksums(raw string) (map[rollbackIdentity]string, error) {
	var manifest struct {
		Version   int    `json:"schema_version"`
		Algorithm string `json:"algorithm"`
		Domains   map[string][]struct {
			File    string `json:"file"`
			Version int64  `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"domains"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil, fmt.Errorf("decode published migration inventory: %w", err)
	}
	if manifest.Version != 1 || manifest.Algorithm != "sha256-normalized-lf" || len(manifest.Domains) == 0 {
		return nil, fmt.Errorf("unsupported published migration inventory")
	}
	result := make(map[rollbackIdentity]string)
	for domain, entries := range manifest.Domains {
		byFile := make(map[string]string, len(entries))
		for _, entry := range entries {
			if _, exists := byFile[entry.File]; exists {
				return nil, fmt.Errorf("duplicate published migration %s/%s", domain, entry.File)
			}
			if len(entry.SHA256) != 64 || entry.Version <= 0 {
				return nil, fmt.Errorf("invalid published migration %s/%s", domain, entry.File)
			}
			byFile[entry.File] = entry.SHA256
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.File, ".up.sql") {
				continue
			}
			down, ok := byFile[strings.TrimSuffix(entry.File, ".up.sql")+".down.sql"]
			if !ok {
				return nil, fmt.Errorf("missing published rollback %s/%s", domain, entry.File)
			}
			identity := rollbackIdentity{version: entry.Version, up: entry.SHA256}
			if previous, exists := result[identity]; exists && previous != down {
				return nil, fmt.Errorf("ambiguous published rollback version %d", entry.Version)
			}
			result[identity] = down
		}
	}
	return result, nil
}

// VerifyPublishedRollbackPackage validates the supplied published identity
// before a trusted domain wrapper substitutes historical compatibility SQL.
// It does not certify unknown custom or retained SQL outside the inventory.
func VerifyPublishedRollbackPackage(loaded []Migration) error {
	return verifyPublishedRollbacks(loaded)
}

func verifyPublishedRollbacks(loaded []Migration) error {
	if publishedRollbackError != nil {
		return publishedRollbackError
	}
	for _, migration := range loaded {
		identity := rollbackIdentity{version: migration.Version, up: normalizedChecksum(migration.UpSQL)}
		if expected, known := publishedRollbackChecksums[identity]; known && normalizedChecksum(migration.DownSQL) != expected {
			return fmt.Errorf("migration %d rollback differs from published package", migration.Version)
		}
	}
	return nil
}
