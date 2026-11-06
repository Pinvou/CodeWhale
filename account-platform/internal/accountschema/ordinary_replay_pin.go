package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const ordinaryReplayVersion int64 = 94
const ordinaryReplayName = "ordinary_dead_letter_replay_runtime"
const ordinaryReplayUpChecksum = "eea5cccf8417dc013b56b459da58335391228fb51d71a4a9afe5cbc178618fb1"
const ordinaryReplayDownChecksum = "7a814d2c6acf9d7f4558284196f5093a0b483fe6275e87b6f4f735d254666764"

func validateOrdinaryReplayMigration(input []migrations.Migration) error {
	digest := func(sql string) string {
		sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
		return hex.EncodeToString(sum[:])
	}
	found := false
	for _, m := range input {
		if m.Version != ordinaryReplayVersion {
			if m.Name == ordinaryReplayName || m.Checksum == ordinaryReplayUpChecksum || digest(m.UpSQL) == ordinaryReplayUpChecksum {
				return errors.New("ordinary replay migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != ordinaryReplayName || m.Checksum != ordinaryReplayUpChecksum ||
			digest(m.UpSQL) != ordinaryReplayUpChecksum || digest(m.DownSQL) != ordinaryReplayDownChecksum {
			return errors.New("ordinary replay migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
