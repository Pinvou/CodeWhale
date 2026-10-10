package accountschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestRegistrationBonusGuardRequiresFrozenPair(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var pair []migrations.Migration
	for _, item := range loaded {
		if item.Version == 87 || item.Version == registrationBonusGuardVersion {
			pair = append(pair, item)
		}
	}
	if len(pair) != 2 {
		t.Fatal("complete original/guard pair is required")
	}
	if err = validateRegistrationBonusGuard(pair); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{87, registrationBonusGuardVersion} {
		t.Run("missing-"+map[int64]string{87: "original", registrationBonusGuardVersion: "guard"}[version], func(t *testing.T) {
			partial := slices.DeleteFunc(slices.Clone(pair), func(item migrations.Migration) bool { return item.Version == version })
			if validateRegistrationBonusGuard(partial) == nil {
				t.Fatal("incomplete delivery accepted")
			}
		})
	}
	for _, target := range []string{"original-up", "original-down", "original-name", "guard-name", "guard-up", "guard-down", "guard-checksum", "duplicate"} {
		t.Run(target, func(t *testing.T) {
			changed := slices.Clone(pair)
			for i := range changed {
				if changed[i].Version == 87 && target == "original-up" {
					changed[i].UpSQL += "\n-- edited"
				}
				if changed[i].Version == 87 && target == "original-down" {
					changed[i].DownSQL = "SELECT 1;"
				}
				if changed[i].Version == 87 && target == "original-name" {
					changed[i].Name = "other"
				}
				if changed[i].Version == registrationBonusGuardVersion {
					switch target {
					case "guard-name":
						changed[i].Name = "other"
					case "guard-up":
						changed[i].UpSQL += "\n-- edited"
					case "guard-down":
						changed[i].DownSQL = "SELECT 1;"
					case "guard-checksum":
						changed[i].Checksum = "forged"
					}
				}
			}
			if target == "duplicate" {
				changed = append(changed, pair[0])
			}
			if validateRegistrationBonusGuard(changed) == nil {
				t.Fatal("changed frozen pair accepted")
			}
		})
	}
	if validateRegistrationBonusGuard(nil) != nil {
		t.Fatal("unrelated earlier delivery unexpectedly rejected")
	}
}
