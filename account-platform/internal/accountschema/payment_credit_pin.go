package accountschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentCreditAdmissionVersion int64 = 95
const paymentCreditAdmissionUpChecksum = "2e54561f58463b9c12abd699c6685fcd5e6827cc7992b5198fe8290ba89be285"
const paymentCreditAdmissionDownChecksum = "d75574c134ac440670ca70c90b38b379864717b1a484a5158798f5f146190e87"

func validatePaymentCreditAdmissions(input []migrations.Migration) error {
	found := false
	for _, m := range input {
		if m.Version != paymentCreditAdmissionVersion {
			if m.Name == "payment_credit_admissions" || m.Checksum == paymentCreditAdmissionUpChecksum || closureReplaySQLChecksum(m.UpSQL) == paymentCreditAdmissionUpChecksum {
				return errors.New("payment credit admission migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != "payment_credit_admissions" || m.Checksum != paymentCreditAdmissionUpChecksum || closureReplaySQLChecksum(m.UpSQL) != paymentCreditAdmissionUpChecksum || closureReplaySQLChecksum(m.DownSQL) != paymentCreditAdmissionDownChecksum {
			return errors.New("payment credit admission migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
