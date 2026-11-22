package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const latePaymentQueryVersion int64 = 900030
const latePaymentQueryChecksum = "00ebf3987ce4718c538bb78035bc3ff5ca4a3dc64709db78ee6a97e09c55da6a"
const latePaymentQueryDownChecksum = "ab49b51e3596b78aa088bfafbffac83203bc65d97e2da587c08f235afba6f867"

func validateLatePaymentQueryCommands(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "late_payment_query_commands" || m.Checksum == latePaymentQueryChecksum) && m.Version != latePaymentQueryVersion {
			return errors.New("late payment query command has a different registered version")
		}
		if m.Version == latePaymentQueryVersion && (m.Name != "late_payment_query_commands" || m.Checksum != latePaymentQueryChecksum || sqlChecksum(m.UpSQL) != latePaymentQueryChecksum || sqlChecksum(m.DownSQL) != latePaymentQueryDownChecksum) {
			return errors.New("late payment query command is not the registered forward-only identity")
		}
	}
	return nil
}
