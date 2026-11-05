package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const ledgerActivationLockVersion int64 = 900014
const ledgerActivationLockChecksum = "122f47cfb1a1d40edad6a656eb7bca757d892c4af9699361d68d2ceed2112452"
const ledgerActivationLockDownChecksum = "35421eeeef715d1a53ea7d2543ceaf0688282a83f16432208cd9f8f6e675037e"

func validateLedgerActivationLock(input []migrations.Migration) error {
	var ledgerCases bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 61 {
			ledgerCases = true
		}
		if migration.Version == ledgerActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate ledger activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if ledgerCases != (capability != nil) {
		return errors.New("ledger repair cases require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "ledger_activation_execution_lock" || capability.Checksum != ledgerActivationLockChecksum || sqlChecksum(capability.UpSQL) != ledgerActivationLockChecksum || sqlChecksum(capability.DownSQL) != ledgerActivationLockDownChecksum) {
		return errors.New("ledger activation lock is not the registered forward-only migration")
	}
	return nil
}
