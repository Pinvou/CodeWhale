package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const usageWaiverVersion int64 = 900024
const usageWaiverChecksum = "b2fb89f6d0a8a952e1f770d3757c917d60369625962154d91ffac0b399d1b5d3"
const usageWaiverDownChecksum = "9de9c448e642eed9685b2bf309999e3bbc547dda74088e7930df6c39fafd0e96"

func validateUsageWaivers(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "usage_waiver_commands" || m.Checksum == usageWaiverChecksum) && m.Version != usageWaiverVersion {
			return errors.New("usage waiver command migration has a different frozen version")
		}
		if m.Version == usageWaiverVersion && (m.Name != "usage_waiver_commands" || m.Checksum != usageWaiverChecksum || sqlChecksum(m.UpSQL) != usageWaiverChecksum || sqlChecksum(m.DownSQL) != usageWaiverDownChecksum) {
			return errors.New("usage waiver commands are not the registered forward-only identity")
		}
	}
	return nil
}
