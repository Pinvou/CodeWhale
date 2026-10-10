package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const reconciliationActivationLockVersion int64 = 900020
const reconciliationActivationLockChecksum = "eecee295b9924f92e99faf22ff45c38f8e3da450f369367910d0ce32e9a0f665"
const reconciliationActivationLockDownChecksum = "8945126650e2fac724f5c436ce485a2ff98d571fe937ad2e2b3af539be3f4ce4"

func validateReconciliationActivationLock(input []migrations.Migration) error {
	var reconciliationCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 72 {
			reconciliationCases = true
		}
		if migration.Version == reconciliationActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate reconciliation activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if reconciliationCases != (capability != nil) {
		return errors.New("reconciliation exception cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "reconciliation_activation_execution_lock" || capability.Checksum != reconciliationActivationLockChecksum || sqlChecksum(capability.UpSQL) != reconciliationActivationLockChecksum || sqlChecksum(capability.DownSQL) != reconciliationActivationLockDownChecksum) {
		return errors.New("reconciliation activation lock is not the registered forward-only migration")
	}
	return nil
}
