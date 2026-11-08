package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

const identityRecoveryVersion int64 = 900007
const identityRecoveryName = "identity_recovery_claims"
const identityRecoveryUpChecksum = "4dfba349debeb57f14fc9b2b77a2b4ad8a913e5533ec2cea62b6f0f0feb539ac"
const identityRecoveryDownChecksum = "33c41d26f0b9eabcf66bab5f41e2d9b922e54b64cb97c8c712dedacffc904e96"

func validateIdentityRecoveryClaims(input []migrations.Migration) error {
	digest := func(sql string) string {
		sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
		return hex.EncodeToString(sum[:])
	}
	found := false
	for _, m := range input {
		if m.Version != identityRecoveryVersion {
			if m.Name == identityRecoveryName || m.Checksum == identityRecoveryUpChecksum || digest(m.UpSQL) == identityRecoveryUpChecksum {
				return errors.New("identity recovery migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != identityRecoveryName || m.Checksum != identityRecoveryUpChecksum || digest(m.UpSQL) != identityRecoveryUpChecksum || digest(m.DownSQL) != identityRecoveryDownChecksum {
			return errors.New("identity recovery migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
