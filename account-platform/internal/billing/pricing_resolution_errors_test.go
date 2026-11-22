package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type pricingResolutionErrorRow func(...any) error

func (row pricingResolutionErrorRow) Scan(dest ...any) error { return row(dest...) }

type pricingResolutionErrorTx struct {
	pgx.Tx
	t       *testing.T
	unitID  uuid.UUID
	failure error
	rows    *pricingResolutionErrorRows
}

func (tx *pricingResolutionErrorTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	tx.t.Helper()
	switch {
	case strings.Contains(query, "FROM billing_protocol_formula_bindings"):
		return pricingResolutionErrorRow(func(dest ...any) error {
			*dest[0].(*[]uuid.UUID) = []uuid.UUID{tx.unitID}
			*dest[1].(*[]byte) = []byte(`{}`)
			return nil
		})
	case strings.Contains(query, "FROM metering_unit_definitions"):
		return pricingResolutionErrorRow(func(...any) error { return tx.failure })
	default:
		tx.t.Fatalf("unexpected production query: %s", query)
		return nil
	}
}

func (tx *pricingResolutionErrorTx) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	tx.t.Helper()
	if !strings.Contains(query, "FROM pricing_version_unit_configs") {
		tx.t.Fatalf("unexpected production rows query: %s", query)
	}
	tx.rows = &pricingResolutionErrorRows{unitID: tx.unitID}
	return tx.rows, nil
}

type pricingResolutionErrorRows struct {
	pgx.Rows
	unitID       uuid.UUID
	read, closed bool
}

func (rows *pricingResolutionErrorRows) Next() bool {
	if rows.read {
		return false
	}
	rows.read = true
	return true
}
func (rows *pricingResolutionErrorRows) Scan(dest ...any) error {
	*dest[0].(*uuid.UUID) = rows.unitID
	*dest[1].(*string) = "FIXED_PRICE"
	price := int64(1)
	*dest[2].(**int64) = &price
	return nil
}
func (rows *pricingResolutionErrorRows) Err() error { return nil }
func (rows *pricingResolutionErrorRows) Close()     { rows.closed = true }

// Inject at the actual SQL row boundary: this tests error identity, not a
// simulated successful transaction after a PostgreSQL query has failed.
func TestResolvePriceUnitQueryErrorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"missing-unit", pgx.ErrNoRows},
		{"serialization-failure", &pgconn.PgError{Code: "40001"}},
		{"deadline", context.DeadlineExceeded},
		{"transport-failure", errors.New("controlled database read failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &pricingResolutionErrorTx{t: t, unitID: uuid.New(), failure: tc.err}
			_, err := resolvePriceCandidate(context.Background(), tx, priceCandidate{id: uuid.New(), formulaID: uuid.New(), scopeType: "GLOBAL", protocol: "REQUEST", creditUnit: "MICROCREDIT", effectiveFrom: time.Now().Add(-time.Hour)}, "controlled-model", time.Now())
			want := tc.err
			if errors.Is(tc.err, pgx.ErrNoRows) {
				want = ErrPriceMissing
			}
			if !errors.Is(err, want) || want != ErrPriceMissing && errors.Is(err, ErrPriceMissing) {
				t.Fatalf("SQL row error identity: got %v, want %v", err, want)
			}
			if tx.rows == nil || !tx.rows.closed {
				t.Fatal("unit configuration rows were not closed")
			}
		})
	}
}
