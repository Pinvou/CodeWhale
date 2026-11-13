package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const unstartedGrantInventoryVersion int64 = 900022
const unstartedGrantInventoryName = "approval_unstarted_grants"
const unstartedGrantInventoryUp = "bb646987572a659091d293bdf781a39874e82d16aadd8346d9d740da7298fd10"
const unstartedGrantInventoryDown = "fa5e9919933b7ac6b29b44775dea9cd31584fd74b766c0a60458aa39d79fefdc"

func unstartedInventoryChecksum(sql string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
	return hex.EncodeToString(sum[:])
}

// Old partial deliveries do not gain a new dependency. When this protection is
// supplied, both directions must retain their published identity before any DB
// work; a renamed input must not turn the forward-only guard into custom SQL.
func validateUnstartedGrantInventory(input []migrations.Migration) error {
	seen := false
	for _, migration := range input {
		actualUp := unstartedInventoryChecksum(migration.UpSQL)
		if migration.Version != unstartedGrantInventoryVersion && migration.Name != unstartedGrantInventoryName && migration.Checksum != unstartedGrantInventoryUp && actualUp != unstartedGrantInventoryUp {
			continue
		}
		if seen || migration.Version != unstartedGrantInventoryVersion || migration.Name != unstartedGrantInventoryName || migration.Checksum != unstartedGrantInventoryUp || actualUp != unstartedGrantInventoryUp || unstartedInventoryChecksum(migration.DownSQL) != unstartedGrantInventoryDown {
			return errors.New("unstarted grant inventory requires its published forward-only migration")
		}
		seen = true
	}
	return nil
}
