package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// Freeze the forward index identity without editing any installed SQL bytes.
func validateOutboxStatisticsIndex(input []migrations.Migration) error {
	const version int64 = 900021
	const name = "outbox_dead_statistics_index"
	const up = "045599c0f220e53a2c369fb255a7518e8b33bd2e0019ee2e79526635050b9ec1"
	const down = "b45139e38cbddba9f8890a9363007eb37113f7a4a1bcb7a36b53d0694087c4d9"
	digest := func(sql string) string {
		sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
		return hex.EncodeToString(sum[:])
	}
	found := false
	for _, m := range input {
		if m.Version != version {
			if m.Name == name || m.Checksum == up || digest(m.UpSQL) == up {
				return errors.New("outbox statistics index has another frozen version")
			}
			continue
		}
		if found || m.Name != name || m.Checksum != up || digest(m.UpSQL) != up || digest(m.DownSQL) != down {
			return errors.New("outbox statistics index is not the frozen SQL")
		}
		found = true
	}
	return nil
}
