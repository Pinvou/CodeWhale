package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const refundActivationLockVersion int64 = 900018
const refundActivationLockChecksum = "96b284f182a33fce5c7636291be06e50c050e69ee26cf923033c2332b8a8d673"
const refundActivationLockDownChecksum = "c54df371d13f70f5490fba6497261e47dfeaf24d03a2fab6fdb67026018c5157"

func validateRefundActivationLock(input []migrations.Migration) error {
	var refundCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 67 {
			refundCases = true
		}
		if migration.Version == refundActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate refund activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if refundCases != (capability != nil) {
		return errors.New("refund exception cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "refund_activation_execution_lock" || capability.Checksum != refundActivationLockChecksum || sqlChecksum(capability.UpSQL) != refundActivationLockChecksum || sqlChecksum(capability.DownSQL) != refundActivationLockDownChecksum) {
		return errors.New("refund activation lock is not the registered forward-only migration")
	}
	return nil
}
