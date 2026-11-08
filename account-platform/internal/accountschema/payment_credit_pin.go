package accountschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentCreditAdmissionVersion int64 = 95
const paymentCreditAdmissionUpChecksum = "e86c64509388bb4257fc36d0d6acaed70627270c167409ffded4a8ec1f08bdb7"
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
	return validatePaymentCreditClosureFence(input)
}
