package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const paymentCreditAttemptVersion int64 = 900039
const paymentCreditAttemptUpChecksum = "722e0334e4072c841704015f1f0a5b40e412f6b4a82a7c8477c4b1f6600df7e9"
const paymentCreditAttemptDownChecksum = "65553f8114e3778b096386114aea689afbd3fb222147a6f8a7451d8912d62d24"

func validatePaymentCreditAttempts(input []migrations.Migration) error {
	found := false
	for _, m := range input {
		if m.Version != paymentCreditAttemptVersion {
			if m.Name == "payment_credit_attempts" || m.Checksum == paymentCreditAttemptUpChecksum || sqlChecksum(m.UpSQL) == paymentCreditAttemptUpChecksum {
				return errors.New("payment credit attempt migration has a different frozen version")
			}
			continue
		}
		if found || m.Name != "payment_credit_attempts" || m.Checksum != paymentCreditAttemptUpChecksum || sqlChecksum(m.UpSQL) != paymentCreditAttemptUpChecksum || sqlChecksum(m.DownSQL) != paymentCreditAttemptDownChecksum {
			return errors.New("payment credit attempt migration is not the frozen SQL")
		}
		found = true
	}
	return nil
}
