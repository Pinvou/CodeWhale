package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type paymentCreditGuardQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func paymentCreditGuardFacts(t *testing.T, ctx context.Context, q paymentCreditGuardQuerier) string {
	t.Helper()
	var facts string
	if err := q.QueryRow(ctx, `SELECT jsonb_build_object(
'wallets',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM wallets x),
'orders',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM payment_orders x),
'transactions',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM payment_transactions x),
'attempts',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM payment_credit_attempts x),
'journals',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM journal_entries x),
'postings',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM ledger_postings x),
'lots',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM credit_lots x),
'packages',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM entitlement_packages x))::text`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}

func paymentCreditGuardRequest(order PaymentOrder, transaction uuid.UUID) paymentcredit.Request {
	return paymentcredit.Request{ID: uuid.New(), UserID: order.RequesterID, OrderID: order.ID, TransactionDigest: paymentcredit.Digest([]byte(transaction.String())), FactsDigest: paymentcredit.Digest([]byte(order.ID.String()))}
}

func paymentCreditGuardTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, order PaymentOrder, now time.Time) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	reference := "guard-" + id.String()
	if _, err := tx.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'PAID','PENDING',$7,$8,'QUERY',$9,$9,$9,$9)`, id, order.ID, order.ProviderID, order.ProviderVersion, order.Channel, reference, order.AmountFen, order.Currency, now); err != nil {
		t.Fatal(err)
	}
	return id, reference
}

// These deliberately inconsistent bindings/projections exercise the database
// and resolver's own defenses. They are not claims about a normal producer or
// an Account grant; matching positive journal probes use real Billing prepare.
func insertPaymentCreditGuardAttempt(t *testing.T, ctx context.Context, tx pgx.Tx, input paymentcredit.Request, transaction *uuid.UUID, state string, now time.Time) {
	t.Helper()
	var resolved any
	if state != "PREPARED" {
		resolved = now
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_credit_attempts(id,payer_user_id,payment_order_id,payment_transaction_id,transaction_digest,facts_digest,fingerprint,state,created_at,resolved_at,retained_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$9::timestamptz+interval '3 years')`, input.ID, input.UserID, input.OrderID, transaction, input.TransactionDigest, input.FactsDigest, input.Fingerprint(), state, now, resolved); err != nil {
		t.Fatal("controlled inconsistent attempt setup failed", err)
	}
}

func TestPaymentCreditJournalAdmissionGuardPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	var actor string
	var super bool
	if err := r.pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &super); err != nil || actor != "billing_service" || super {
		t.Fatal("journal guard requires genuine non-superuser Billing runtime", err)
	}
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	_, other, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	for _, kind := range []struct{ entry, prefix string }{{"RECHARGE", "payment-transaction:"}, {"REPAIR_CREDIT", "repair-payment-transaction:"}} {
		for _, mode := range []string{"missing", "wrong-order", "wrong-payer", "already-applied"} {
			t.Run(kind.entry+"-"+mode, func(t *testing.T) {
				baseline := paymentCreditGuardFacts(t, ctx, r.pool)
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				now := r.now()
				transaction, providerReference := paymentCreditGuardTransaction(t, ctx, tx, order, now)
				if _, err := tx.Exec(ctx, `SAVEPOINT binding_fixture`); err != nil {
					t.Fatal(err)
				}
				if mode != "missing" {
					input, state := paymentCreditGuardRequest(order, transaction), "PREPARED"
					switch mode {
					case "wrong-order":
						input = paymentCreditGuardRequest(other, transaction)
					case "wrong-payer":
						input.UserID = other.RequesterID
					case "already-applied":
						state = "APPLIED"
					}
					insertPaymentCreditGuardAttempt(t, ctx, tx, input, &transaction, state, now)
				}
				before := paymentCreditGuardFacts(t, ctx, tx)
				if _, err := tx.Exec(ctx, `SAVEPOINT journal_probe`); err != nil {
					t.Fatal(err)
				}
				reference := kind.prefix + transaction.String()
				_, err = tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'controlled direct guard probe',$4)`, uuid.New(), kind.entry, reference, now)
				var rejected *pgconn.PgError
				if !errors.As(err, &rejected) || rejected.Code != "55000" || rejected.Message != "PAYMENT_CREDIT_ADMISSION_REQUIRED" {
					t.Fatalf("journal guard did not reject %s: %v", mode, err)
				}
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT journal_probe`); err != nil {
					t.Fatal(err)
				}
				if paymentCreditGuardFacts(t, ctx, tx) != before {
					t.Fatal("rejected journal changed financial or admission facts")
				}
				// Remove only this uncommitted fault setup, then use genuine prepare
				// and the same transaction/entry/reference after the refusal.
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT binding_fixture`); err != nil {
					t.Fatal(err)
				}
				admission := prepareBillingFixturePaymentCredit(t, ctx, c, tx, order.ID, transaction, providerReference, now)
				journal := uuid.New()
				if command, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'matching prepared guard probe',$4)`, journal, kind.entry, reference, now); err != nil || command.RowsAffected() != 1 {
					t.Fatal("matching PREPARED could not continue after refusal", err)
				}
				var matching bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_credit_attempts a JOIN journal_entries j ON j.id=$2 WHERE a.id=$1 AND a.state='PREPARED' AND a.payment_transaction_id=$3 AND a.payment_order_id=$4 AND a.payer_user_id=$5 AND j.entry_type=$6 AND j.business_reference=$7)`, admission, journal, transaction, order.ID, order.RequesterID, kind.entry, reference).Scan(&matching); err != nil || !matching {
					t.Fatal("positive probe did not use its actual matching receipt", err)
				}
				// A journal header is not a completed balanced recharge. Roll back
				// every probe; do not commit partial financial effects as a fixture.
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if paymentCreditGuardFacts(t, ctx, r.pool) != baseline {
					t.Fatal("guard probe left financial or receipt residue")
				}
			})
		}
	}
}

func TestPaymentCreditResolverIntegrityPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, mode := range []string{"applied-without-effects", "prepared-with-effects", "missing-transaction", "fingerprint-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, mode == "prepared-with-effects")
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			now := r.now()
			if transaction == uuid.Nil && mode != "missing-transaction" {
				transaction, _ = paymentCreditGuardTransaction(t, ctx, tx, order, now)
			}
			input := paymentCreditGuardRequest(order, transaction)
			state := "PREPARED"
			if mode == "applied-without-effects" {
				state = "APPLIED"
			}
			var transactionID *uuid.UUID
			if transaction != uuid.Nil {
				transactionID = &transaction
			}
			insertPaymentCreditGuardAttempt(t, ctx, tx, input, transactionID, state, now)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			before := paymentCreditGuardFacts(t, ctx, r.pool)
			query, wanted := input, paymentcredit.ErrProtocol
			if mode == "fingerprint-mismatch" {
				query.FactsDigest = paymentcredit.Digest([]byte("different immutable financial facts"))
				wanted = ErrConflict
			}
			if _, err := r.ResolvePaymentCredit(ctx, query); !errors.Is(err, wanted) {
				t.Fatalf("resolver did not reject %s with its declared error: %v", mode, err)
			}
			if paymentCreditGuardFacts(t, ctx, r.pool) != before {
				t.Fatal("rejected resolution changed attempt or financial facts")
			}
			if mode == "fingerprint-mismatch" {
				decision, err := r.ResolvePaymentCredit(ctx, input)
				if err != nil || !decision.Matches(input) || decision.State != "ABORTED" {
					t.Fatal("valid no-effect resolution did not recover after wrong fingerprint", err)
				}
			} else if _, err := r.ResolvePaymentCredit(ctx, query); !errors.Is(err, wanted) || paymentCreditGuardFacts(t, ctx, r.pool) != before {
				t.Fatal("inconsistent projection was not stably rejected on the next call", err)
			}
		})
	}
}
