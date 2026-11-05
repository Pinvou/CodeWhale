package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const registrationBonusGuardVersion int64 = 900004
const registrationBonusOriginalChecksum = "1d76a6e9aa6b5fcee84e2eabe8912cbfb8fa53c79ba29ce2bb4c29852be070e1"
const registrationBonusOriginalDownChecksum = "51536cdba4eb470a6d3eb226a7e61e6e8f84a15dc57e11852bdba18c6a9e6ca9"
const registrationBonusGuardChecksum = "ddfdb70370c3900c113efc918fb5586ab1a2cb977337b8df35e565e1a250772f"
const registrationBonusGuardDownChecksum = "f49e09aea96c5f5ae27c8a9ddc9b3ecd0d2d2087deb6cc41a1c1428b43f708b0"

// Ordered must call this before either public Up or Down. Original 087 and
// its required-facts guard are one delivery; neither can be selected alone.
func validateRegistrationBonusGuard(input []migrations.Migration) error {
	var original, guard *migrations.Migration
	for _, item := range input {
		switch item.Version {
		case 87:
			if original != nil {
				return errors.New("duplicate registration bonus original migration")
			}
			copy := item
			original = &copy
		case registrationBonusGuardVersion:
			if guard != nil {
				return errors.New("duplicate registration bonus required facts guard")
			}
			copy := item
			guard = &copy
		}
	}
	if (original == nil) != (guard == nil) {
		return errors.New("registration bonus migration and required facts guard must be delivered together")
	}
	if original == nil {
		return nil
	}
	digest := func(source string) string {
		canonical := strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(canonical))
		return hex.EncodeToString(sum[:])
	}
	if original.Name != "registration_bonus_fingerprint" || guard.Name != "registration_bonus_required_facts" || original.Checksum != registrationBonusOriginalChecksum || digest(original.UpSQL) != registrationBonusOriginalChecksum || digest(original.DownSQL) != registrationBonusOriginalDownChecksum || guard.Checksum != registrationBonusGuardChecksum || digest(guard.UpSQL) != registrationBonusGuardChecksum || digest(guard.DownSQL) != registrationBonusGuardDownChecksum {
		return errors.New("registration bonus required facts delivery is not the frozen SQL")
	}
	return nil
}
