package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const packageExpiryScanVersion int64 = 900043
const packageExpiryScanName = "package_expiry_scan"
const packageExpiryScanUp = "010fb10ad9c02c80f86fece213a603b602dca7b3e19a263d5a498a51270dd205"
const packageExpiryScanDown = "0a2edbe127d2abfdbe978298cf6b1e84bbef14e217fd7fbf9a653810fac334d6"

// The supplementary index is optional for older delivered carriers. If present,
// both SQL directions and its registered identity must match before touching DB.
func validatePackageExpiryScan(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		up := sqlChecksum(m.UpSQL)
		if m.Version != packageExpiryScanVersion && m.Name != packageExpiryScanName && m.Checksum != packageExpiryScanUp && up != packageExpiryScanUp {
			continue
		}
		if seen || m.Version != packageExpiryScanVersion || m.Name != packageExpiryScanName || m.Checksum != packageExpiryScanUp || up != packageExpiryScanUp || sqlChecksum(m.DownSQL) != packageExpiryScanDown {
			return errors.New("package expiry scan requires its published migration identity")
		}
		seen = true
	}
	return nil
}
