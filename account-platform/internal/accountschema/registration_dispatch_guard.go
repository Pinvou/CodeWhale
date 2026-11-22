package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

const registrationDispatchVersion int64 = 900008
const registrationDispatchName = "registration_create_dispatch"
const registrationDispatchUpChecksum = "016367fffffcb4b994ab4ac64840813fa19e99c45ef209e938d85024e4bb5ff9"
const registrationDispatchDownChecksum = "4d19d7ec30faa0064970635c065e60b2aa5d5e3f7011032f2d8120a246d327a5"

func validateRegistrationDispatch(input []migrations.Migration) error {
	digest := func(sql string) string {
		sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
		return hex.EncodeToString(sum[:])
	}
	found := false
	for _, m := range input {
		if m.Version != registrationDispatchVersion {
			if m.Name == registrationDispatchName || m.Checksum == registrationDispatchUpChecksum || digest(m.UpSQL) == registrationDispatchUpChecksum {
				return errors.New("registration dispatch migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != registrationDispatchName || m.Checksum != registrationDispatchUpChecksum || digest(m.UpSQL) != registrationDispatchUpChecksum || digest(m.DownSQL) != registrationDispatchDownChecksum {
			return errors.New("registration dispatch migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
