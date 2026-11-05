package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const chargebackActivationLockVersion int64 = 900019
const chargebackActivationLockChecksum = "98a4640847b80cc79bb865a5cc462f42b5b75012fc66d988e120497381e051d1"
const chargebackActivationLockDownChecksum = "9f66a5d6a04b01806cbd921d06c27c3edd7c97fe0a80835ea4d1f7d6b1d3d3a6"

func validateChargebackActivationLock(input []migrations.Migration) error {
	var chargebackCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 70 {
			chargebackCases = true
		}
		if migration.Version == chargebackActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate chargeback activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if chargebackCases != (capability != nil) {
		return errors.New("chargeback exception cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "chargeback_activation_execution_lock" || capability.Checksum != chargebackActivationLockChecksum || sqlChecksum(capability.UpSQL) != chargebackActivationLockChecksum || sqlChecksum(capability.DownSQL) != chargebackActivationLockDownChecksum) {
		return errors.New("chargeback activation lock is not the registered forward-only migration")
	}
	return nil
}
