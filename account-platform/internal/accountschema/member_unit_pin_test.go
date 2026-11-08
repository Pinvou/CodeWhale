package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestF06MemberUnitMigrationFrozenIdentity(t *testing.T) {
	// Original public identity, independent of the new production constant so
	// the same formal also executes against the original unpinned Ordering.
	const originalMemberUnits int64 = 900003
	input, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(input); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"name", "up", "up-and-checksum", "down", "checksum", "duplicate", "required-missing"} {
		t.Run(side, func(t *testing.T) {
			changed := slices.Clone(input)
			for i := range changed {
				if changed[i].Version != originalMemberUnits {
					continue
				}
				switch side {
				case "name":
					changed[i].Name = "other_member_unit_assignments"
				case "up", "up-and-checksum":
					changed[i].UpSQL += "\n-- controlled changed SQL\n"
					if side == "up-and-checksum" {
						sum := sha256.Sum256([]byte(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n")))
						changed[i].Checksum = hex.EncodeToString(sum[:])
					}
				case "down":
					changed[i].DownSQL += "\n-- controlled changed inverse\n"
				case "checksum":
					changed[i].Checksum = strings.Repeat("0", 64)
				case "duplicate":
					changed = append(changed, changed[i])
				}
				break
			}
			if side == "required-missing" {
				changed = slices.DeleteFunc(changed, func(m migrations.Migration) bool { return m.Version == originalMemberUnits })
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("public ordering accepted altered or missing original member unit identity")
			}
		})
	}
	t.Run("legacy-partial", func(t *testing.T) {
		partial := slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == 39 || m.Version == originalMemberUnits })
		if _, err := Ordered(partial); err != nil {
			t.Fatal("historical package without original governance was made globally dependent on 900003", err)
		}
	})
	t.Run("canonical-newlines", func(t *testing.T) {
		changed := slices.Clone(input)
		for i := range changed {
			if changed[i].Version == originalMemberUnits {
				changed[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
				changed[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
			}
		}
		if _, err := Ordered(changed); err != nil {
			t.Fatal("canonical CRLF representation of unchanged SQL rejected", err)
		}
	})
}

func TestF06ModelAdminBindingMigrationFrozenIdentity(t *testing.T) {
	const originalVersion int64 = 900006
	input, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if slices.IndexFunc(input, func(m migrations.Migration) bool { return m.Version == originalVersion }) < 0 {
		t.Fatal("original model administrator binding migration is absent")
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
					changed[i].Name = "other_model_admin_binding"
				case "version", "version-and-name":
					changed[i].Version = 990006
					if mutation == "version-and-name" {
						changed[i].Name = "other_model_admin_binding"
					}
				case "up", "up-and-checksum":
					changed[i].UpSQL += "\nSELECT 1;\n"
					if mutation == "up-and-checksum" {
						canonical := strings.ReplaceAll(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n"), "\r", "\n")
						sum := sha256.Sum256([]byte(canonical))
						changed[i].Checksum = hex.EncodeToString(sum[:])
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
				t.Fatal("public ordering accepted altered model administrator binding identity")
			}
		})
	}
	t.Run("historical-without-new-binding", func(t *testing.T) {
		partial := slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == originalVersion })
		if _, err := Ordered(partial); err != nil {
			t.Fatal("historical delivery was made dependent on the later role binding migration", err)
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
			t.Fatal("canonical line endings changed the frozen role binding identity", err)
		}
	})
}
