package account

import (
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// GSA-06 tests exercise the historical 50 -> 51 writer/sidecar contract,
// including its down and reentry paths. Later Relay migrations have separate
// full-chain fixtures and must not silently change this historical baseline.
func loadApprovalAuthorizationBaselineMigrations() ([]migrations.Migration, error) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		return nil, err
	}
	return approvalAuthorizationBaselineMigrations(loaded)
}

func approvalAuthorizationBaselineMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 51 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("authorization integration fixture requires Account migration 51")
}

func TestApprovalAuthorizationHistoricalMigrationBoundary(t *testing.T) {
	for _, versions := range [][]int64{{49, 50, 51}, {49, 50, 51, 52, 53}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "original up", DownSQL: "original down", Checksum: "original checksum"}
		}
		baseline, err := approvalAuthorizationBaselineMigrations(loaded)
		if err != nil || len(baseline) != 3 || baseline[2].Version != 51 || baseline[2].DownSQL != "original down" || baseline[2].Checksum != "original checksum" {
			t.Fatalf("historical boundary changed migration contents: %v", err)
		}
		if len(loaded) != len(versions) || loaded[len(loaded)-1].Version != versions[len(versions)-1] {
			t.Fatal("historical selection changed the full-chain input")
		}
	}
	if _, err := approvalAuthorizationBaselineMigrations([]migrations.Migration{{Version: 50}, {Version: 52}}); err == nil {
		t.Fatal("missing historical baseline was accepted")
	}
}
