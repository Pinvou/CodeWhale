package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type pricingBindingErrorTx struct {
	pgx.Tx
	err error
}

func (tx pricingBindingErrorTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return pricingResolutionErrorRow(func(...any) error { return tx.err })
}

func TestValidatePriceConfigurationQueryErrorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"missing-binding", pgx.ErrNoRows},
		{"serialization-failure", &pgconn.PgError{Code: "40001"}},
		{"deadline", context.DeadlineExceeded},
		{"transport-failure", errors.New("controlled database read failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price := int64(1)
			units := []PriceUnitConfiguration{{UnitDefinitionID: uuid.New(), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &price}}
			err := validatePriceUnitConfigurations(context.Background(), pricingBindingErrorTx{err: tc.err}, "GLOBAL", "REQUEST", uuid.New(), units)
			want := tc.err
			if errors.Is(tc.err, pgx.ErrNoRows) {
				want = ErrPriceMissing
			}
			if !errors.Is(err, want) || want != ErrPriceMissing && errors.Is(err, ErrPriceMissing) {
				t.Fatalf("binding query error identity: got %v, want %v", err, want)
			}
		})
	}
}
