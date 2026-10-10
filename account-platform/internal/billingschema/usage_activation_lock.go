package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const usageActivationLockVersion int64 = 900021
const usageActivationLockChecksum = "6499e5dc5fd073b1742e4399e9feb4bb596ebeef2a40701611794f6dbac2212f"
const usageActivationLockDownChecksum = "f250883cef733a0250f941249657d674f8cc7c5562964958a96914ca1fe03fe1"

func validateUsageActivationLock(input []migrations.Migration) error {
	var usageCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 74 {
			usageCases = true
		}
		if migration.Version == usageActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate usage activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if usageCases != (capability != nil) {
		return errors.New("usage exception cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "usage_activation_execution_lock" || capability.Checksum != usageActivationLockChecksum || sqlChecksum(capability.UpSQL) != usageActivationLockChecksum || sqlChecksum(capability.DownSQL) != usageActivationLockDownChecksum) {
		return errors.New("usage activation lock is not the registered forward-only migration")
	}
	return nil
}
