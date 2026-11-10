package billingschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// Freeze the forward index identity without editing any installed SQL bytes.
func validateOutboxStatisticsIndex(input []migrations.Migration) error {
	const version int64 = 900040
	const name = "outbox_dead_statistics_index"
	const up = "fdb45f949ce1fe97935022f6ffe057003243ba5b5cd1c6e863ffcb32cf1fed40"
	const down = "bc2912f5c3ebef282384628699361874ba5dda08c0334815c54fbaabd8d41241"
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
