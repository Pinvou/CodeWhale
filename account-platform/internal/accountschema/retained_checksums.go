package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const memberUnitAssignmentsVersion int64 = 900003
const memberUnitAssignmentsChecksum = "8cedb7a6a68e94af4f507282d35b3a40e7df4d50f99a3c259db409746d3feda5"
const memberUnitAssignmentsDownChecksum = "218b6c425a4b81e2afa415ef533680b79cd9449812723da8d70174c802106f76"

func validateMemberUnitAssignments(input []migrations.Migration) error {
	required := false
	var guard *migrations.Migration
	for _, item := range input {
		if item.Version == 39 {
			required = true
		}
		if item.Version == memberUnitAssignmentsVersion {
			if guard != nil {
				return errors.New("duplicate member unit assignments migration")
			}
			copy := item
			guard = &copy
		}
	}
	if guard == nil {
		if required {
			return errors.New("organization governance requires the frozen member unit assignments migration")
		}
		return nil // Historical partial packages before original 039 remain valid.
	}
	digest := func(source string) string {
		source = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(source))
		return hex.EncodeToString(sum[:])
	}
	if guard.Name != "member_unit_assignments" || guard.Checksum != memberUnitAssignmentsChecksum || digest(guard.UpSQL) != memberUnitAssignmentsChecksum || digest(guard.DownSQL) != memberUnitAssignmentsDownChecksum {
		return errors.New("member unit assignments migration is not the frozen SQL")
	}
	return nil
}

// Exact retained originals; business SQL bytes are never rewritten by hooks.
var bridgeChecksums = map[int64]string{
	27: "cfd14d187cd1c3fa3aab134de59cbfb88a44a7bbc88bc3e3406d8d6c7d2505db",
	28: "05eec7870758d9eb29248236a389f7facfbc4025d1fa711d40775d3fe96855b9",
	33: "7c7e8d6e507d22b28e63ffabec95fd3ae375f9f686207d17af3b7bd24b624bf6",
}
var retainedScopeChecksums = []string{
	"658324df04b4e19fb98531c192f409fb68b82f5d0f027fe6b65527ebddca8637",
	"7e4ea64626e271cb51b138b945812380e2b0ee4c2ab58300408e67baf66ab5d0",
}
