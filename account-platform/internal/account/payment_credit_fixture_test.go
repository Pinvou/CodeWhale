package account

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
)

// Ledger-proof fixture setup controls an otherwise-unrelated Account credit
// admission. Boundary tests and the F12 integration use real Account authority.
type accountFixturePaymentCreditAdmission struct{}

func (accountFixturePaymentCreditAdmission) Admit(_ context.Context, input paymentcredit.Request) (paymentcredit.Decision, error) {
	if !input.Valid() {
		return paymentcredit.Decision{}, paymentcredit.ErrProtocol
	}
	return paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: "ADMITTED"}, nil
}
