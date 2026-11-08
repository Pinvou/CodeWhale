package accountschema

import (
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestOrderedEnforcesRegistrationBonusDelivery(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal("complete frozen migration delivery rejected", err)
	}
	for _, target := range []string{"original-up", "guard-down", "missing-original", "missing-guard"} {
		t.Run(target, func(t *testing.T) {
			changed := slices.Clone(loaded)
			for i := range changed {
				if target == "original-up" && changed[i].Version == 87 {
					changed[i].UpSQL += "\n-- controlled changed original\n"
				}
				if target == "guard-down" && changed[i].Version == registrationBonusGuardVersion {
					changed[i].DownSQL += "\n-- controlled changed inverse\n"
				}
			}
			if target == "missing-original" {
				changed = slices.DeleteFunc(changed, func(item migrations.Migration) bool { return item.Version == 87 })
			}
			if target == "missing-guard" {
				changed = slices.DeleteFunc(changed, func(item migrations.Migration) bool { return item.Version == registrationBonusGuardVersion })
			}
			if _, err := Ordered(changed); err == nil || !strings.Contains(err.Error(), "registration bonus") {
				t.Fatalf("public ordering must reject incomplete or altered registration bonus delivery: %v", err)
			}
		})
	}
}
