package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Both current install and a real 900032 upgrade use the production pipeline,
// public migrations and restricted Billing actor in fresh owned UUID databases.
func TestReconciliationOrphanPaidOrderPostgres(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh-install"
		if upgrade {
			name = "historical-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			var r *PostgresRepository
			var ctx context.Context
			var operator *pgx.Conn
			var all []migrations.Migration
			var orphanPrefix []migrations.Migration
			if upgrade {
				r, ctx = retainedFinanceRepository(t, 900032)
			} else {
				r, ctx = retainedFinanceRepository(t)
			}
			// The upgrade case mixes a valid transaction after the orphan in the
			// SQL ordering. The fresh case has no transaction to audit at all.
			normal, normalOrder, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			normalProvider := normal.provider.(*retainedFinancialProvider)
			reference := "zzz-normal-payment"
			normalProvider.result.ChannelTransactionID = &reference
			if upgrade {
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, _, err := normal.mergeSuccessfulPaymentWithinTransaction(ctx, tx, normalOrder, "QUERY", "normal-payment", []byte(`{"controlled":true}`), normalProvider.result, false, r.now()); err != nil {
					t.Fatal("create real normal transaction", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			var paidAt any = r.now()
			if !upgrade {
				paidAt = nil
			}
			if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status='PAID',fulfillment_status='CREDITED',paid_at=$2 WHERE id=$1`, order.ID, paidAt); err != nil {
				t.Fatal("controlled historical orphan order", err)
			}
			provider := &reconciliationFixtureProvider{retainedFinancialProvider: *normalProvider}
			if upgrade {
				provider.rows = []ProviderBillEntry{{EntryType: "PAYMENT", ProviderReferenceID: reference, ChannelTransactionID: reference, MerchantOrderNo: normalOrder.MerchantOrderNo, Channel: normalOrder.Channel, AmountFen: normalOrder.AmountFen, Currency: normalOrder.Currency, Status: "SUCCESS", PaidAt: *normalProvider.result.PaidAt}}
			}
			coordinator, err := newBillingFixturePaymentCoordinator(r, provider, "test")
			if err != nil {
				t.Fatal(err)
			}
			financialFacts := func() string {
				t.Helper()
				var value string
				err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'orders',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),
 'transactions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),
 'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),
 'lots',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),
 'packages',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),
 'components',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),
 'journals',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),
 'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x)
)::text`).Scan(&value)
				if err != nil {
					t.Fatal("snapshot original financial facts", err)
				}
				return value
			}
			before := financialFacts()
			date := r.now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
			actor := uuid.New()
			service := NewService(r).WithPayments(coordinator)
			// Use the real HTTP normalizer, repository snapshot selection and
			// stored-case projection. Human decoding is controlled; dedicated
			// permission/OIDC middleware remains a separate existing contract.
			readCases := func(runID uuid.UUID, kind string) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(http.MethodGet, "/admin/v1/reconciliation-cases?run_id="+runID.String()+"&case_type="+kind, nil).WithContext(ctx)
				request.Header.Set("Authorization", "Bearer controlled-human")
				response := httptest.NewRecorder()
				identity.Middleware(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
				}, http.HandlerFunc(service.adminReconciliationCases)).ServeHTTP(response, request)
				return response
			}
			if upgrade {
				_, err := coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
				var check *pgconn.PgError
				if !errors.As(err, &check) || check.Code != "23514" || check.ConstraintName != "reconciliation_cases_case_type_check" {
					t.Fatalf("historical prefix must reject the new case classification: %v; cause=%v; pg=%+v", err, errors.Unwrap(err), check)
				}
				cfg := r.pool.Config().ConnConfig.Copy()
				cfg.User, cfg.Password = "billing_migrator", ""
				operator, err = pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer operator.Close(context.Background())
				if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
					t.Fatal(err)
				}
				all, err = migrations.Load("../../migrations/billing")
				if err != nil {
					t.Fatal(err)
				}
				for _, migration := range all {
					if migration.Version <= 900033 {
						orphanPrefix = append(orphanPrefix, migration)
					}
				}
				if len(orphanPrefix) == 0 || orphanPrefix[len(orphanPrefix)-1].Version != 900033 {
					t.Fatal("exact orphan migration prefix is unavailable")
				}
				if err := billingschema.Up(ctx, operator, orphanPrefix); err != nil {
					t.Fatal("official orphan boundary upgrade", err)
				}
				if _, err := billingschema.VerifyApplied(ctx, operator, orphanPrefix); err != nil {
					t.Fatal("verify upgraded migration ledger", err)
				}
				migrations.Unlock(ctx, operator)
				if financialFacts() != before {
					t.Fatal("upgrade changed original financial facts")
				}
			}
			var firstCase uuid.UUID
			for attempt := 0; attempt < 2; attempt++ {
				run, err := coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
				if err != nil || run.Status != "COMPLETED" || run.CaseCount != 1 {
					t.Fatalf("orphan reconciliation did not complete %s: %v; cause=%v", run.Status, err, errors.Unwrap(err))
				}
				var id uuid.UUID
				var count int
				if err := r.pool.QueryRow(ctx, `SELECT c.id FROM reconciliation_cases c JOIN reconciliation_case_runs association ON association.case_id=c.id JOIN manual_review_cases m ON m.id=c.manual_review_case_id WHERE association.run_id=$1 AND c.payment_order_id=$2 AND c.case_type='PAYMENT_TRANSACTION_MISSING' AND c.severity='P0' AND c.status='OPEN' AND c.internal_snapshot->'payment_order'->>'id'=$2::text AND c.ledger_snapshot='[]'::jsonb AND m.domain='BILLING' AND m.case_type='RECONCILIATION_DIFFERENCE' AND m.status='OPEN'`, run.ID, order.ID).Scan(&id); err != nil {
					t.Fatal("missing persisted linked manual review and original order snapshot", err)
				}
				var needsDateReview, originalDateMissing bool
				if err := r.pool.QueryRow(ctx, `SELECT (internal_snapshot->>'payment_date_verification_required')::boolean,internal_snapshot->'payment_order'->'paid_at'='null'::jsonb FROM reconciliation_cases WHERE id=$1`, id).Scan(&needsDateReview, &originalDateMissing); err != nil || needsDateReview != !upgrade || originalDateMissing != !upgrade {
					t.Fatal("payment date was invented or missing manual verification evidence", needsDateReview, originalDateMissing, err)
				}
				if upgrade {
					var audited int
					if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_observation_facts fact JOIN reconciliation_runs run ON run.active_observation_id=fact.observation_id WHERE run.id=$1 AND fact.payment_order_id=$2 AND fact.ledger_snapshot->'order'->>'id'=$2::text AND fact.ledger_snapshot->'transaction'->>'payment_order_id'=$2::text`, run.ID, normalOrder.ID).Scan(&audited); err != nil || audited != 1 {
						t.Fatal("normal transaction lost its own audited ledger evidence", audited, err)
					}
				}
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases WHERE payment_order_id=$1`, order.ID).Scan(&count); err != nil || count != 1 {
					t.Fatal("replay duplicated the orphan review case", count, err)
				}
				if attempt == 0 {
					firstCase = id
				} else if id != firstCase {
					t.Fatal("replay replaced original manual review identity")
				}
				for _, selection := range []struct {
					run  uuid.UUID
					kind string
					want int
				}{
					{run.ID, "PAYMENT_TRANSACTION_MISSING", 1},
					{run.ID, "PAYMENT_LEDGER_MISSING", 0},
					{uuid.New(), "PAYMENT_TRANSACTION_MISSING", 0},
				} {
					response := readCases(selection.run, selection.kind)
					var page struct {
						Items []struct {
							ID   uuid.UUID `json:"case_id"`
							Kind string    `json:"case_type"`
						} `json:"items"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK || len(page.Items) != selection.want {
						t.Fatalf("public case filter/run scope failed: status=%d items=%d want=%d err=%v", response.Code, len(page.Items), selection.want, err)
					}
					if selection.want == 1 && (page.Items[0].ID != id || page.Items[0].Kind != selection.kind) {
						t.Fatal("public filter returned another case identity/type")
					}
				}
				invalid := readCases(run.ID, "PAYMENT_TRANSACTION_MISSING_UNKNOWN")
				if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_FILTER"`) {
					t.Fatal("unknown case type bypassed the finite public filter")
				}
				if financialFacts() != before || provider.queryCount != 0 {
					t.Fatal("manual classification changed funds or queried an order to repair it")
				}
			}
			if upgrade {
				var ledgerBefore, ledgerAfter string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
				if err := billingschema.Up(ctx, operator, orphanPrefix); err != nil {
					t.Fatal("replay forward migration", err)
				}
				var refused *pgconn.PgError
				err := billingschema.DownOne(ctx, operator, orphanPrefix)
				if !errors.As(err, &refused) || refused.Code != "55000" || refused.Message != "orphan payment order classification is forward-only" {
					t.Fatal("orphan case migration must refuse rollback", err)
				}
				if _, err := billingschema.VerifyApplied(ctx, operator, orphanPrefix); err != nil {
					t.Fatal("verify after refused rollback", err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledgerAfter); err != nil || ledgerAfter != ledgerBefore || financialFacts() != before {
					t.Fatal("replay or refused rollback changed migration ledger or funds", err)
				}
				// Check 900033's refusal at its exact boundary, then retain the
				// complete current upgrade and replay contract independently.
				if err := billingschema.Up(ctx, operator, all); err != nil {
					t.Fatal("official complete forward upgrade", err)
				}
				if _, err := billingschema.VerifyApplied(ctx, operator, all); err != nil {
					t.Fatal("verify complete forward upgrade", err)
				}
				if financialFacts() != before {
					t.Fatal("complete forward upgrade changed original financial facts")
				}
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
				if err := billingschema.Up(ctx, operator, all); err != nil {
					t.Fatal("replay complete forward upgrade", err)
				}
				if _, err := billingschema.VerifyApplied(ctx, operator, all); err != nil {
					t.Fatal("verify complete forward replay", err)
				}
				migrations.Unlock(ctx, operator)
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledgerAfter); err != nil || ledgerAfter != ledgerBefore || financialFacts() != before {
					t.Fatal("complete upgrade replay changed migration ledger or funds", err)
				}
				var retained int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases c JOIN manual_review_cases m ON m.id=c.manual_review_case_id WHERE c.id=$1 AND c.case_type='PAYMENT_TRANSACTION_MISSING' AND c.status='OPEN' AND m.status='OPEN'`, firstCase).Scan(&retained); err != nil || retained != 1 {
					t.Fatal("replay or refused rollback lost the manual review", retained, err)
				}
			}
		})
	}
}
