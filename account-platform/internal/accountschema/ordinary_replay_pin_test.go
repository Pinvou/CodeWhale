package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestOrdinaryReplayMigrationFrozenIdentity(t *testing.T) {
	input, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if slices.IndexFunc(input, func(m migrations.Migration) bool { return m.Version == ordinaryReplayVersion }) < 0 {
		t.Fatal("ordinary replay migration is absent")
	}
	if _, err := Ordered(input); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"name", "version", "version-and-name", "up", "up-and-checksum", "down", "checksum", "duplicate"} {
		t.Run(mutation, func(t *testing.T) {
			changed := slices.Clone(input)
			for i := range changed {
				if changed[i].Version != ordinaryReplayVersion {
					continue
				}
				switch mutation {
				case "name":
					changed[i].Name = "other_replay"
				case "version", "version-and-name":
					changed[i].Version = 994
					if mutation == "version-and-name" {
						changed[i].Name = "other_replay"
					}
				case "up", "up-and-checksum":
					changed[i].UpSQL += "\nSELECT 1;\n"
					if mutation == "up-and-checksum" {
						sum := sha256.Sum256([]byte(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n")))
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
				t.Fatal("public ordering accepted changed ordinary replay identity")
			}
		})
	}
	t.Run("historical-without-new-entry", func(t *testing.T) {
		old := slices.DeleteFunc(slices.Clone(input), func(m migrations.Migration) bool { return m.Version == ordinaryReplayVersion })
		if _, err := Ordered(old); err != nil {
			t.Fatal("earlier delivery acquired a new migration dependency", err)
		}
	})
	t.Run("canonical-line-endings", func(t *testing.T) {
		changed := slices.Clone(input)
		for i := range changed {
			if changed[i].Version == ordinaryReplayVersion {
				changed[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
				changed[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
			}
		}
		if _, err := Ordered(changed); err != nil {
			t.Fatal("canonical line endings changed identity", err)
		}
	})
}
