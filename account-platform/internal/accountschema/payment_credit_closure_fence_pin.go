package accountschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentCreditClosureFenceVersion int64 = 900016
const paymentCreditClosureFenceName = "payment_credit_closure_fence"
const paymentCreditClosureFenceUpChecksum = "92b600b81e4fe589645fed6a0dbe4f4f857a1dc5b99d3de6be918caa742a5f90"
const paymentCreditClosureFenceDownChecksum = "ad5a114ce0cf45145fdea4fedd0d2ca8bb151f876ba6061a1783b4b84aabccda"

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
