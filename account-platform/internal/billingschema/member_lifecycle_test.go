package billingschema

import (
	"slices"
	"strings"
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

func TestF06MemberQuotaBatchMigrationFrozenIdentity(t *testing.T) {
	const originalVersion int64 = 900022
	input, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if slices.IndexFunc(input, func(m migrations.Migration) bool { return m.Version == originalVersion }) < 0 {
		t.Fatal("original member quota batch command migration is absent")
	}
	if _, err := Ordered(input); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"name", "version", "version-and-name", "up", "up-and-checksum", "down", "checksum", "duplicate"} {
		t.Run(mutation, func(t *testing.T) {
			changed := slices.Clone(input)
			for i := range changed {
				if changed[i].Version != originalVersion {
					continue
				}
				switch mutation {
				case "name":
					changed[i].Name = "other_member_quota_batch"
				case "version", "version-and-name":
					changed[i].Version = 990022
					if mutation == "version-and-name" {
						changed[i].Name = "other_member_quota_batch"
					}
				case "up", "up-and-checksum":
					changed[i].UpSQL += "\nSELECT 1;\n"
					if mutation == "up-and-checksum" {
						changed[i].Checksum = sqlChecksum(changed[i].UpSQL)
					}
				case "down":
					changed[i].DownSQL += "\nSELECT 1;\n"
				case "checksum":
					changed[i].Checksum = strings.Repeat("0", 64)
				case "duplicate":
					changed = append(changed, changed[i])
				}
				break
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("public ordering accepted altered member quota batch command identity")
			}
		})
	}
	t.Run("historical-without-new-batch", func(t *testing.T) {
		partial := slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == originalVersion })
		if _, err := Ordered(partial); err != nil {
			t.Fatal("historical delivery was made dependent on the later batch command migration", err)
		}
	})
	t.Run("canonical-line-endings", func(t *testing.T) {
		changed := slices.Clone(input)
		for i := range changed {
			if changed[i].Version == originalVersion {
				changed[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n"), "\r", "\n"), "\n", "\r\n")
				changed[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(changed[i].DownSQL, "\r\n", "\n"), "\r", "\n"), "\n", "\r\n")
			}
		}
		if _, err := Ordered(changed); err != nil {
			t.Fatal("canonical line endings changed the frozen batch command identity", err)
		}
	})
}
