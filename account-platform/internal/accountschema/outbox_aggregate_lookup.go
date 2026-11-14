package accountschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const outboxAggregateLookupVersion int64 = 900020
const outboxAggregateLookupName = "outbox_aggregate_lookup"
const outboxAggregateLookupUp = "c4ad07bf49d5faa3f7493df352ca7885a3be3c23c49a618a804fcc4825e2e615"
const outboxAggregateLookupDown = "edb996ce80f4cbbdfd1ac3c6ca87a06ba3086743a5a04e8fa0cb35ee2194752a"

// This owner-role DDL must keep its published identity in either direction.
// Earlier installations which do not carry the index gain no dependency.
func validateOutboxAggregateLookup(input []migrations.Migration) error {
	seen := false
	for _, item := range input {
		up := closureReplaySQLChecksum(item.UpSQL)
		if item.Version != outboxAggregateLookupVersion && item.Name != outboxAggregateLookupName && item.Checksum != outboxAggregateLookupUp && up != outboxAggregateLookupUp {
			continue
		}
		if seen || item.Version != outboxAggregateLookupVersion || item.Name != outboxAggregateLookupName || item.Checksum != outboxAggregateLookupUp || up != outboxAggregateLookupUp || closureReplaySQLChecksum(item.DownSQL) != outboxAggregateLookupDown {
			return errors.New("outbox aggregate lookup requires its published migration identity")
		}
		seen = true
	}
	return nil
}
