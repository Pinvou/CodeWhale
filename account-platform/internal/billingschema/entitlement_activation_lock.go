package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const entitlementActivationLockVersion int64 = 900013
const entitlementActivationLockChecksum = "b2442654c753e1c9ff68708926ec2274e9962d9264050ec4d86ff1cf0168d96f"
const entitlementActivationLockDownChecksum = "ceb9c3e3c96bad0bdb7755775b2ab0679a00b56d0c931b0587ddd6309a50e0d3"

func validateEntitlementActivationLock(input []migrations.Migration) error {
	var corrections bool
	var capability *migrations.Migration
	for _, migration := range input {
		if migration.Version == 59 {
			corrections = true
		}
		if migration.Version == entitlementActivationLockVersion {
			if capability != nil {
				return errors.New("duplicate entitlement activation lock migration")
			}
			copy := migration
			capability = &copy
		}
	}
	if corrections != (capability != nil) {
		return errors.New("entitlement corrections require the owner activation lock capability")
	}
	if capability != nil && (capability.Name != "entitlement_activation_execution_lock" || capability.Checksum != entitlementActivationLockChecksum || sqlChecksum(capability.UpSQL) != entitlementActivationLockChecksum || sqlChecksum(capability.DownSQL) != entitlementActivationLockDownChecksum) {
		return errors.New("entitlement activation lock is not the registered forward-only migration")
	}
	return nil
}
