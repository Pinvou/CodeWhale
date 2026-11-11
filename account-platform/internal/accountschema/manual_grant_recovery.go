package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const manualGrantRecoveryVersion int64 = 900023
const manualGrantRecoveryName = "approval_grant_manual_recovery"
const manualGrantRecoveryUp = "9ecb8e4aacdbacaf90a60621f3bc2cee09c82561ab042f78616875e4a883885e"
const manualGrantRecoveryDown = "418626025543dde9ce6b36e9ad35200f974f5d05a4b74e3b4a71aaeecd3dbf86"

func manualGrantRecoveryChecksum(sql string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
	return hex.EncodeToString(sum[:])
}

// Old partial deliveries do not gain a new dependency. When this protection is
// supplied, both directions must retain their published identity before any DB
// work; a renamed input must not turn the forward-only guard into custom SQL.
func validateManualGrantRecovery(input []migrations.Migration) error {
	seen := false
	for _, migration := range input {
		actualUp := manualGrantRecoveryChecksum(migration.UpSQL)
		if migration.Version != manualGrantRecoveryVersion && migration.Name != manualGrantRecoveryName && migration.Checksum != manualGrantRecoveryUp && actualUp != manualGrantRecoveryUp {
			continue
		}
		if seen || migration.Version != manualGrantRecoveryVersion || migration.Name != manualGrantRecoveryName || migration.Checksum != manualGrantRecoveryUp || actualUp != manualGrantRecoveryUp || manualGrantRecoveryChecksum(migration.DownSQL) != manualGrantRecoveryDown {
			return errors.New("manual grant recovery requires its published forward-only migration")
		}
		seen = true
	}
	return nil
}
