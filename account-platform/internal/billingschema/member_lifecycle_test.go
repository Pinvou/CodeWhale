package billingschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestMemberLifecycleMigrationIdentityFence(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ordered, func(m migrations.Migration) bool { return m.Version == memberIdentityVersion })
	if i < 1 || ordered[i-1].Version != 20 {
		t.Fatal("membership event identity must follow its original lifecycle table")
	}
	for _, mutation := range []string{"up", "up-and-checksum", "down", "missing-table", "missing-fence"} {
		t.Run(mutation, func(t *testing.T) {
			input := make([]migrations.Migration, 0, len(loaded))
			for _, m := range loaded {
				if mutation == "missing-table" && m.Version == 20 || mutation == "missing-fence" && m.Version == memberIdentityVersion {
					continue
				}
				if m.Version == memberIdentityVersion {
					switch mutation {
					case "up", "up-and-checksum":
						m.UpSQL += "\nSELECT 1;\n"
						if mutation == "up-and-checksum" {
							m.Checksum = sqlChecksum(m.UpSQL)
						}
					case "down":
						m.DownSQL = "ALTER TABLE organization_member_lifecycle_states DROP COLUMN membership_aggregate_id;"
					}
				}
				input = append(input, m)
			}
			if _, err := Ordered(input); err == nil {
				t.Fatal("edited or incomplete durable membership schema accepted")
			}
		})
	}
}
