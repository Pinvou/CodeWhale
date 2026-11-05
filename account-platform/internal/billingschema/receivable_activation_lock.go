package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const receivableActivationLockVersion int64 = 900015
const receivableActivationLockChecksum = "e8ce834446a7fb35132b6df60c0cbaaa86d5af93163eecbe449a150af1937c38"
const receivableActivationLockDownChecksum = "ac96192ff7f650e79f18c1a3d9d67d0a5c622773bdf930809d00c96eddd4dcd1"

func validateReceivableActivationLock(input []migrations.Migration) error {
	var receivableCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 63 {
			receivableCases = true
		}
		if migration.Version == receivableActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate receivable activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if receivableCases != (capability != nil) {
		return errors.New("receivable write-off cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "receivable_activation_execution_lock" || capability.Checksum != receivableActivationLockChecksum || sqlChecksum(capability.UpSQL) != receivableActivationLockChecksum || sqlChecksum(capability.DownSQL) != receivableActivationLockDownChecksum) {
		return errors.New("receivable activation lock is not the registered forward-only migration")
	}
	return nil
}
