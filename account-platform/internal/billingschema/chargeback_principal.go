package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const chargebackPrincipalVersion int64 = 900044
const chargebackPrincipalChecksum = "baf6031690dc3598224b4298cd2b11c3c4b4f693b7ee0acbd0fc63f111c5bf62"
const chargebackPrincipalDownChecksum = "8d05baf139540fc3b3b7253103f88b6d3b6aae083b51c2b4c0759a110d4fda36"

func validateChargebackPrincipal(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "chargeback_cash_principal" || m.Checksum == chargebackPrincipalChecksum) && m.Version != chargebackPrincipalVersion {
			return errors.New("chargeback cash principal migration version mismatch")
		}
		if m.Version == chargebackPrincipalVersion && (m.Name != "chargeback_cash_principal" || m.Checksum != chargebackPrincipalChecksum || sqlChecksum(m.UpSQL) != chargebackPrincipalChecksum || sqlChecksum(m.DownSQL) != chargebackPrincipalDownChecksum) {
			return errors.New("chargeback cash principal migration identity mismatch")
		}
	}
	return nil
}
