package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const providerHealthRecentVersion int64 = 900040
const providerHealthRecentName = "provider_health_recent"
const providerHealthRecentUp = "0d1b5cc17cce2342540c4c75d15c783f53f37177412247ca5eefcd24d942d961"
const providerHealthRecentDown = "07fe0346755f15f629f927ca3ac040e1b07e59b9fa248c3e932ce8197dd1d3ef"

// The supplementary index is optional for older delivered carriers. If present,
// both SQL directions and its registered identity must match before touching DB.
func validateProviderHealthRecent(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		up := sqlChecksum(m.UpSQL)
		if m.Version != providerHealthRecentVersion && m.Name != providerHealthRecentName && m.Checksum != providerHealthRecentUp && up != providerHealthRecentUp {
			continue
		}
		if seen || m.Version != providerHealthRecentVersion || m.Name != providerHealthRecentName || m.Checksum != providerHealthRecentUp || up != providerHealthRecentUp || sqlChecksum(m.DownSQL) != providerHealthRecentDown {
			return errors.New("provider health recent requires its published migration identity")
		}
		seen = true
	}
	return nil
}
