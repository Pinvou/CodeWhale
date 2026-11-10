package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const chargebackPrincipalVersion int64 = 900039
const chargebackPrincipalChecksum = "c952ae6e5148fab06ed71c2595a02570aa2e80e215d7a33a4f382578f3d2c758"
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
