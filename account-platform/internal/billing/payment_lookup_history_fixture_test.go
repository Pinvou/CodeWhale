package billing

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// This is the published F12 membership, not a copy of SQL or an inferred
// numeric prefix. Later features must not leak their base tables into the
// historical 900016 -> 900017 lookup upgrade.
//
//go:embed testdata/f12-billing-migration-baseline.json
var f12BillingMigrationBaseline []byte

type paymentLookupHistoricalMigration struct {
	Version      int64  `json:"version"`
	Name         string `json:"name"`
	UpChecksum   string `json:"up_checksum"`
	DownChecksum string `json:"down_checksum"`
}

func selectF12PaymentLookupStage(loaded []migrations.Migration, maximum int64) ([]migrations.Migration, error) {
	if maximum != 900016 && maximum != 900017 {
		return nil, fmt.Errorf("unsupported historical payment lookup stage")
	}
	var baseline struct {
		SourceCommit string                             `json:"source_commit"`
		Migrations   []paymentLookupHistoricalMigration `json:"migrations"`
	}
	if err := json.Unmarshal(f12BillingMigrationBaseline, &baseline); err != nil || baseline.SourceCommit != "b8e6651dca66956dcbf0d109e7c789b1588d7d5d" || len(baseline.Migrations) != 84 {
		return nil, fmt.Errorf("historical payment lookup membership is not registered")
	}
	byVersion := make(map[int64]migrations.Migration, len(loaded))
	for _, migration := range loaded {
		if _, exists := byVersion[migration.Version]; exists {
			return nil, fmt.Errorf("duplicate migration in historical fixture input")
		}
		byVersion[migration.Version] = migration
	}
	selected := make([]migrations.Migration, 0, len(baseline.Migrations))
	for _, expected := range baseline.Migrations {
		actual, exists := byVersion[expected.Version]
		if !exists || actual.Name != expected.Name || actual.Checksum != expected.UpChecksum || paymentLookupFixtureSQLChecksum(actual.UpSQL) != expected.UpChecksum || paymentLookupFixtureSQLChecksum(actual.DownSQL) != expected.DownChecksum {
			return nil, fmt.Errorf("published historical payment lookup migration identity differs at version %d", expected.Version)
		}
		if expected.Version <= maximum {
			selected = append(selected, actual)
		}
	}
	return selected, nil
}

func paymentLookupFixtureSQLChecksum(sql string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:])
}
