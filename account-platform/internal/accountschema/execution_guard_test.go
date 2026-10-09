package accountschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestExecutionFactsGuardPinAndMissingPrerequisite(t *testing.T) {
	input, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(input); err != nil {
		t.Fatal(err)
	}
	without := slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == executionGuardVersion })
	if _, err := Ordered(without); err == nil {
		t.Fatal("original binding migration accepted without frozen facts repair")
	}
	without = slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == 53 })
	if _, err := Ordered(without); err == nil {
		t.Fatal("guard accepted without original binding schema")
	}
	for _, side := range []string{"up", "down"} {
		t.Run(side, func(t *testing.T) {
			edited := slices.Clone(input)
			for index := range edited {
				if edited[index].Version == executionGuardVersion {
					if side == "up" {
						edited[index].UpSQL += "\nSELECT 1;"
					} else {
						edited[index].DownSQL = "SELECT 1;"
					}
				}
			}
			if _, err := Ordered(edited); err == nil {
				t.Fatal("edited guard accepted with forged original checksum")
			}
		})
	}
}
