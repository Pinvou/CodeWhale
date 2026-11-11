package billing

import (
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
)

type financialJSONColumn struct{ table, column, constraint string }

var billingFinancialJSONColumns = []financialJSONColumn{
	{"manual_review_case_events", "details", "manual_review_events_details_object_check"},
	{"late_payment_review_cases", "provider_capability_snapshot", "late_review_provider_capability_object_check"},
	{"late_payment_review_cases", "policy_snapshot", "late_review_policy_snapshot_object_check"},
	{"late_payment_review_cases", "evidence_summary", "late_review_evidence_summary_object_check"},
	{"late_payment_review_evidence", "payload_snapshot", "late_review_evidence_payload_object_check"},
}

func jsonContractBillingWrite(ctx context.Context, r *PostgresRepository, item LatePaymentReviewCase, col financialJSONColumn, value string) error {
	var err error
	switch col.table {
	case "manual_review_case_events":
		_, err = r.pool.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'CONTROLLED_SHAPE',$3::jsonb,clock_timestamp())`, uuid.New(), item.ID, value)
	case "late_payment_review_evidence":
		_, err = r.pool.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'USER_PAYMENT_CLAIM',encode(sha256($3::bytea),'hex'),$4::jsonb,clock_timestamp())`, uuid.New(), item.ID, []byte(uuid.NewString()), value)
	case "late_payment_review_cases":
		_, err = r.pool.Exec(ctx, fmt.Sprintf(`UPDATE late_payment_review_cases SET %s=$2::jsonb WHERE id=$1`, pgx.Identifier{col.column}.Sanitize()), item.ID, value)
	default:
		return fmt.Errorf("unsupported controlled column")
	}
	return err
}
func jsonContractBillingFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var s string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('orders',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),'transactions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),'reviews',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM late_payment_review_cases x),'cases',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_cases x),'events',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_case_events x),'evidence',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM late_payment_review_evidence x),'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x))::text`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestBillingFinancialJSONObjectsPostgres(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")) == "" {
		t.Skip("owned PostgreSQL required")
	}
	scenarios := []string{"fresh-current-producer", "upgrade-current-producer"}
	for _, col := range billingFinancialJSONColumns {
		scenarios = append(scenarios, "dirty-"+col.column)
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			source, dir := "", ""
			if scenario != "fresh-current-producer" {
				source, dir = stageFinancialJSONPackage(t)
			}
			r, ctx := retainedFinanceRepository(t)
			config := r.pool.Config().ConnConfig.Copy()
			config.User = "billing_migrator"
			config.Password = ""
			operator, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer operator.Close(context.Background())
			if _, err = operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
				t.Fatal(err)
			}
			create := func() LatePaymentReviewCase {
				_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
				item, e := r.EscalateLatePaymentReview(ctx, order.ID, "CLOSED")
				if e != nil {
					t.Fatal(e)
				}
				return item
			}
			item := create()
			dirty := strings.HasPrefix(scenario, "dirty-")
			if dirty {
				for _, col := range billingFinancialJSONColumns {
					if scenario == "dirty-"+col.column {
						if err = jsonContractBillingWrite(ctx, r, item, col, "[]"); err != nil {
							t.Fatal("published predecessor did not admit controlled historical shape", err)
						}
					}
				}
			}
			before := jsonContractBillingFacts(t, r, ctx)
			if source != "" {
				addFinancialJSONGuard(t, source, dir)
			} else {
				dir = "../../migrations/billing"
			}
			loaded, err := migrations.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if dirty {
				var ledgerBefore, ledgerAfter string
				if err = operator.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					err = billingschema.Up(ctx, operator, loaded)
					var pg *pgconn.PgError
					if !errors.As(err, &pg) || pg.Code != "23514" {
						t.Fatal("invalid history did not block public upgrade", err)
					}
					if err = operator.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerAfter); err != nil || ledgerBefore != ledgerAfter || jsonContractBillingFacts(t, r, ctx) != before {
						t.Fatal("failed upgrade rewrote facts", err)
					}
					var n int
					if err = operator.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname=ANY($1::text[])`, []string{"manual_review_events_details_object_check", "late_review_provider_capability_object_check", "late_review_policy_snapshot_object_check", "late_review_evidence_summary_object_check", "late_review_evidence_payload_object_check"}).Scan(&n); err != nil || n != 0 {
						t.Fatal("partial contract persisted", n, err)
					}
				}
				return
			}
			if err = billingschema.Up(ctx, operator, loaded); err != nil {
				t.Fatal(err)
			}
			if _, err = billingschema.VerifyApplied(ctx, operator, loaded); err != nil {
				t.Fatal(err)
			}
			if jsonContractBillingFacts(t, r, ctx) != before {
				t.Fatal("valid financial facts changed")
			}
			for _, col := range billingFinancialJSONColumns {
				for _, bad := range []string{`[]`, `null`, `42`, `true`, `"text"`} {
					before = jsonContractBillingFacts(t, r, ctx)
					err = jsonContractBillingWrite(ctx, r, item, col, bad)
					var pg *pgconn.PgError
					if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != col.constraint {
						t.Fatal("wrong shape not rejected by exact JSON contract", col.column, err)
					}
					if jsonContractBillingFacts(t, r, ctx) != before {
						t.Fatal("rejected JSON changed facts")
					}
				}
			}
			_ = create() // Original producer still writes all five object-shaped fields.
			before = jsonContractBillingFacts(t, r, ctx)
			err = billingschema.DownOne(ctx, operator, loaded)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "55000" || pg.Message != "financial JSON object contracts are forward-only" || jsonContractBillingFacts(t, r, ctx) != before {
				t.Fatal("down removed guard or financial facts", err)
			}
			var actualRole string
			if err = operator.QueryRow(ctx, `SELECT current_user`).Scan(&actualRole); err != nil || actualRole != "billing_service" {
				t.Fatal("migration role leaked", actualRole, err)
			}
		})
	}
}
