package accountschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentCreditClosureFenceVersion int64 = 900015
const paymentCreditClosureFenceName = "payment_credit_closure_fence"
const paymentCreditClosureFenceUpChecksum = "bcad7dc0e37298b45f4bd16cd356f1cd385a4500db867eb8016c7fbe9e33ac13"
const paymentCreditClosureFenceDownChecksum = "f60abcb676fdeeba7e45553783a0fac99190b664fa8b208a6759ec7b6c6d0c39"

func validatePaymentCreditClosureFence(input []migrations.Migration) error {
	found := false
	for _, m := range input {
		if m.Version != paymentCreditClosureFenceVersion {
			if m.Name == paymentCreditClosureFenceName || m.Checksum == paymentCreditClosureFenceUpChecksum || closureReplaySQLChecksum(m.UpSQL) == paymentCreditClosureFenceUpChecksum {
				return errors.New("payment credit closure fence migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != paymentCreditClosureFenceName || m.Checksum != paymentCreditClosureFenceUpChecksum || closureReplaySQLChecksum(m.UpSQL) != paymentCreditClosureFenceUpChecksum || closureReplaySQLChecksum(m.DownSQL) != paymentCreditClosureFenceDownChecksum {
			return errors.New("payment credit closure fence migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
