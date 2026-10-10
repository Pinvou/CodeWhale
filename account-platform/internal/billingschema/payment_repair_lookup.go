package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentRepairLookupVersion int64 = 900017
const paymentRepairLookupChecksum = "18d285451a58950e40ef90cf77ebe7455e86f2331db2187ba437f8c67f84a865"
const paymentRepairLookupDownChecksum = "dc6931cf460ee6287a1f6c49db5abb0be010bc40c53fb718bdc46490eabe26bc"

// The pre-index 900016 history remains a legitimate upgrade boundary.
func validatePaymentRepairLookup(input []migrations.Migration) error {
	var lookup *migrations.Migration
	for _, m := range input {
		if m.Version == paymentRepairLookupVersion {
			if lookup != nil {
				return errors.New("duplicate payment repair lookup migration")
			}
			copy := m
			lookup = &copy
		}
	}
	if lookup != nil && (lookup.Name != "payment_repair_order_lookup" || lookup.Checksum != paymentRepairLookupChecksum || sqlChecksum(lookup.UpSQL) != paymentRepairLookupChecksum || sqlChecksum(lookup.DownSQL) != paymentRepairLookupDownChecksum) {
		return errors.New("payment repair lookup is not the registered forward-only migration")
	}
	return nil
}
