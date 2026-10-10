package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentActivationLockVersion int64 = 900016
const paymentActivationLockChecksum = "e91164261390798fd3b98be27d91ff4442b30ed3aebb72002b180104d465a6a1"
const paymentActivationLockDownChecksum = "13f596b47cd1664d1407e4e2a67c4c1da6ccea2ce55352b2f7d19fbe78b2a8d8"

func validatePaymentActivationLock(input []migrations.Migration) error {
	var paymentCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 65 {
			paymentCases = true
		}
		if migration.Version == paymentActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate payment activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if paymentCases != (capability != nil) {
		return errors.New("payment write-off cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "payment_activation_execution_lock" || capability.Checksum != paymentActivationLockChecksum || sqlChecksum(capability.UpSQL) != paymentActivationLockChecksum || sqlChecksum(capability.DownSQL) != paymentActivationLockDownChecksum) {
		return errors.New("payment activation lock is not the registered forward-only migration")
	}
	return nil
}
