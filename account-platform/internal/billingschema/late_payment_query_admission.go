package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const latePaymentQueryAdmissionVersion int64 = 900046
const latePaymentQueryAdmissionName = "late_payment_query_admission"
const latePaymentQueryAdmissionUp = "5de84e5e0aa20f152aa63bd598fed1e5e7810031215ba43bfcccab241f6c9c1d"
const latePaymentQueryAdmissionDown = "0525e581974cdd9b85cf7c2cbf46aa89a85b04b300c8405d814c31d9c778959a"

// Older carriers omit these facts; if delivered, their exact identity is required.
// Existing rows stay unknown, never backfilled from historical timestamps.
// both SQL directions and its registered identity must match before touching DB.
func validateLatePaymentQueryAdmission(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		up := sqlChecksum(m.UpSQL)
		if m.Version != latePaymentQueryAdmissionVersion && m.Name != latePaymentQueryAdmissionName && m.Checksum != latePaymentQueryAdmissionUp && up != latePaymentQueryAdmissionUp {
			continue
		}
		if seen || m.Version != latePaymentQueryAdmissionVersion || m.Name != latePaymentQueryAdmissionName || m.Checksum != latePaymentQueryAdmissionUp || up != latePaymentQueryAdmissionUp || sqlChecksum(m.DownSQL) != latePaymentQueryAdmissionDown {
			return errors.New("late payment query admission requires its published migration identity")
		}
		seen = true
	}
	return nil
}
