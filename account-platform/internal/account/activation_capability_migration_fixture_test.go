package account

import (
	"errors"
	"math"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// capabilityFixtureFutureVersions keeps synthetic hook probes strictly after
// every delivered migration, including supplemental migrations in later waves.
func capabilityFixtureFutureVersions(loaded []migrations.Migration) (int64, int64, error) {
	var highest int64
	for _, migration := range loaded {
		if migration.Version > highest {
			highest = migration.Version
		}
	}
	if highest > math.MaxInt64-2 {
		return 0, 0, errors.New("capability fixture requires two unused migration versions")
	}
	return highest + 1, highest + 2, nil
}

func TestCapabilityFixtureFutureVersions(t *testing.T) {
	for _, test := range []struct {
		name     string
		versions []int64
		future   int64
		bad      int64
		wantErr  bool
	}{
		{"unsorted-current-delivery", []int64{900022, 80, 900020, 900021}, 900023, 900024, false},
		{"later-delivery", []int64{900027, 900022, 900025}, 900028, 900029, false},
		{"last-safe-pair", []int64{math.MaxInt64 - 2}, math.MaxInt64 - 1, math.MaxInt64, false},
		{"overflow", []int64{math.MaxInt64 - 1, 900022}, 0, 0, true},
		{"maximum-overflow", []int64{math.MaxInt64}, 0, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded := make([]migrations.Migration, len(test.versions))
			for i, version := range test.versions {
				loaded[i].Version = version
			}
			future, bad, err := capabilityFixtureFutureVersions(loaded)
			if (err != nil) != test.wantErr || future != test.future || bad != test.bad {
				t.Fatalf("future=%d bad=%d error=%v", future, bad, err)
			}
		})
	}
}
