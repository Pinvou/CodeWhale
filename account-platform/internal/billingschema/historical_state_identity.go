package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// The rollback predicates describe these exact original SQL identities. Pin
// both directions before public installation, rollback or verification can
// trust those predicates. Earlier partial deliveries gain no dependency on an
// absent migration, and no historical SQL or applied ledger is rewritten.
func validateHistoricalStateRollbackIdentity(input []migrations.Migration) error {
	for _, migration := range input {
		for _, pin := range []struct {
			version        int64
			name, up, down string
		}{
			{10, "plan_catalog", "5fb4350e0b63eb7bd6a62e631be87b40027fe8f72162651bb322e14cd8fc65a2", "5ba08f243e0f7d493b83ed1d4e2d6561632696bf21746b225d963e08c2d6f51c"},
			{14, "payment_repairs", "416f0d1ff871a17cf2bdf498d8e0aeb01d436fbc9c3d45cc52459ad6a6a27e54", "727de1486a2e7c2ac1c2af948a84361da2d1be5780fe2e7d57f4a5655914f689"},
			{27, "targeted_refund_lifecycle", "65755ba6c88de24886417e30fa3ec0c0c706f302cd7db3362fa5de2c5247181d", "c24dc4b69e05d595220984d513298a2ce9f147e90395fbcf6fdbe1abf3c2e201"},
			{49, "local_payment_expiry", "13c2cfd865feb46bca7f4811b2b4ca6be96c9906195f45bfc7aa839da78a5866", "9bf90e923bf56cf23ed5e0211482367483848296347b21a6c7c31cfc3f2d201b"},
		} {
			up := sqlChecksum(migration.UpSQL)
			if migration.Version != pin.version && migration.Name != pin.name && migration.Checksum != pin.up && up != pin.up {
				continue
			}
			if migration.Version != pin.version || migration.Name != pin.name || migration.Checksum != pin.up || up != pin.up || sqlChecksum(migration.DownSQL) != pin.down {
				return errors.New("historical state rollback requires the exact original migration identity")
			}
		}
	}
	return nil
}
