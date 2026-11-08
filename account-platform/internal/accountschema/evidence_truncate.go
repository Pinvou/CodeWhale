package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const organizationEvidenceGuardVersion int64 = 900015
const organizationEvidenceGuardName = "organization_evidence_truncate_guards"
const organizationEvidenceGuardUp = "8ef3aea3b9923be58e6aa4460886e03df786db8771697e53bd86bdeea5809e78"
const organizationEvidenceGuardDown = "e5ce11635ec7d8f8361e76b479416ca7a23b349e6c47b18854a42f21af95d2f5"

func evidenceGuardChecksum(sql string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
	return hex.EncodeToString(sum[:])
}

// Old partial deliveries do not gain a new dependency. When this protection is
// supplied, both directions must retain their published identity before any DB
// work; a renamed input must not turn the forward-only guard into custom SQL.
func validateOrganizationEvidenceGuard(input []migrations.Migration) error {
	seen := false
	for _, migration := range input {
		actualUp := evidenceGuardChecksum(migration.UpSQL)
		if migration.Version != organizationEvidenceGuardVersion && migration.Name != organizationEvidenceGuardName && migration.Checksum != organizationEvidenceGuardUp && actualUp != organizationEvidenceGuardUp {
			continue
		}
		if seen || migration.Version != organizationEvidenceGuardVersion || migration.Name != organizationEvidenceGuardName || migration.Checksum != organizationEvidenceGuardUp || actualUp != organizationEvidenceGuardUp || evidenceGuardChecksum(migration.DownSQL) != organizationEvidenceGuardDown {
			return errors.New("organization evidence protection requires its published forward-only migration")
		}
		seen = true
	}
	return nil
}
