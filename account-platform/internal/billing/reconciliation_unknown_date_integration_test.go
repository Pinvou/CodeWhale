package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconciliationUnknownDateOrphanLifecyclePostgres(t *testing.T) {
	for _, scenario := range []string{"cross-date-concurrency", "legacy-and-ignore", "observation-rollback"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			normal, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status='PAID',fulfillment_status='CREDITED',paid_at=NULL WHERE id=$1`, order.ID); err != nil {
				t.Fatal(err)
			}
			provider := &reconciliationFixtureProvider{retainedFinancialProvider: *normal.provider.(*retainedFinancialProvider)}
			coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			date := reconciliationFixtureDateForPaidAt(r.now()).AddDate(0, 0, -6)
			financial := func() string {
				t.Helper()
				var value string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),
'transactions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),
'lots',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),
'packages',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),
'components',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),
'journals',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),
'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x))::text`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := financial()
			read := func(run ReconciliationRun, status string) AdminReconciliationCase {
				t.Helper()
				cases, err := r.AdminReconciliationCases(ctx, run.ID, 10)
				if err != nil || run.Status != "COMPLETED" || run.CaseCount != 1 || len(cases) != 1 || cases[0].Status != status {
					t.Fatal("original pipeline/read projection did not retain the expected manual case", run, cases, err)
				}
				return cases[0]
			}
			observe := func(offset int) ReconciliationRun {
				t.Helper()
				run, err := coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date.AddDate(0, 0, offset))
				if err != nil {
					t.Fatal(err)
				}
				return run
			}
			switch scenario {
			case "cross-date-concurrency":
				var snapshot json.RawMessage
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('payment_order',to_jsonb(o),'payment_date_verification_required',true) FROM payment_orders o WHERE id=$1`, order.ID).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				difference := unknownDateOrphanOriginalDifference(AdminReconciliationCase{PaymentOrderID: &order.ID, MerchantOrderNo: order.MerchantOrderNo, CaseType: "PAYMENT_TRANSACTION_MISSING", InternalSnapshot: snapshot})
				caps := coordinator.Capabilities()
				key := reconciliationCaseIdentity(ReconciliationRun{ProviderID: caps.ProviderID, ProviderVersion: caps.Version, Channel: order.Channel}, difference)
				scope := "reconciliation-case-fact"
				lockKey := fmt.Sprintf("%d:%s%s", len(scope), scope, key)
				connection, err := r.pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, lockKey); err != nil {
					connection.Release()
					t.Fatal(err)
				}
				operation, cancel := context.WithCancel(ctx)
				type result struct {
					run ReconciliationRun
					err error
				}
				results := make(chan result, 2)
				joined, released := 0, false
				t.Cleanup(func() {
					cancel()
					if !released {
						_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
						connection.Release()
					}
					for joined < 2 {
						select {
						case <-results:
							joined++
						case <-time.After(5 * time.Second):
							t.Error("owned observer did not stop")
							return
						}
					}
				})
				for offset := 0; offset < 2; offset++ {
					go func(offset int) {
						run, err := coordinator.RunAdminReconciliation(operation, actor, order.Channel, date.AddDate(0, 0, offset))
						results <- result{run, err}
					}(offset)
				}
				deadline := time.Now().Add(8 * time.Second)
				for {
					var waiters int
					if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&waiters); err != nil {
						t.Fatal(err)
					}
					if waiters == 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("both original pipelines did not reach the case publication barrier", waiters)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if _, err := connection.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); err != nil {
					t.Fatal(err)
				}
				connection.Release()
				released = true
				var first uuid.UUID
				for joined < 2 {
					value := <-results
					joined++
					if value.err != nil {
						t.Fatal(value.err)
					}
					item := read(value.run, "OPEN")
					if first != uuid.Nil && item.ID != first {
						t.Fatal("concurrent dates split one investigation")
					}
					first = item.ID
				}
				var cases, observations int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_cases WHERE payment_order_id=$1),(SELECT count(*) FROM reconciliation_observation_progress WHERE case_id=$2)`, order.ID, first).Scan(&cases, &observations); err != nil || cases != 1 || observations != 2 {
					t.Fatal("concurrent observation publication lost atomic case associations", cases, observations, err)
				}
			case "legacy-and-ignore":
				firstRun := observe(0)
				first := read(firstRun, "OPEN")
				original := string(first.InternalSnapshot)
				legacyDigest := reconciliationDifferenceDigest(firstRun, unknownDateOrphanOriginalDifference(first))
				if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_cases SET fact_digest=$2 WHERE id=$1`, first.ID, legacyDigest); err != nil {
					t.Fatal("controlled old-writer identity", err)
				}
				command := func(action string) AdminReconciliationCase {
					t.Helper()
					item, err := r.UpdateReconciliationCase(ctx, first.ID, ReconciliationCaseCommand{Action: action, ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled manual orphan date review", ReviewResult: "controlled accepted original financial facts"})
					if err != nil {
						t.Fatal(action, err)
					}
					return item
				}
				command("CLAIM")
				command("IGNORE")
				if item := read(observe(1), "IGNORED"); item.ID != first.ID {
					t.Fatal("legacy ignored case was replaced")
				}
				if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET amount_fen=amount_fen+1 WHERE id=$1`, order.ID); err != nil {
					t.Fatal("controlled changed source fact", err)
				}
				if item := read(observe(2), "OPEN"); item.ID != first.ID || string(item.InternalSnapshot) != original {
					t.Fatal("changed fact replaced investigation or immutable snapshot")
				}
				command("CLAIM")
				command("IGNORE")
				if item := read(observe(3), "IGNORED"); item.ID != first.ID {
					t.Fatal("accepting changed evidence did not retain the investigation")
				}
				var cases, observations int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_cases WHERE payment_order_id=$1),(SELECT count(*) FROM reconciliation_observation_progress WHERE case_id=$2)`, order.ID, first.ID).Scan(&cases, &observations); err != nil || cases != 1 || observations != 4 {
					t.Fatal("legacy/ignore lifecycle lost evidence", cases, observations, err)
				}
			case "observation-rollback":
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION controlled_orphan_progress_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled observation insert failure'; END $$; CREATE TRIGGER controlled_orphan_progress_failure BEFORE INSERT ON reconciliation_observation_progress FOR EACH ROW EXECUTE FUNCTION controlled_orphan_progress_failure()`); err != nil {
					t.Fatal(err)
				}
				run, err := coordinator.RunAdminReconciliation(ctx, actor, order.Channel, date)
				if err == nil || run.Status == "COMPLETED" {
					t.Fatal("failed observation was reported completed")
				}
				var cases, manual, progress int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_cases),(SELECT count(*) FROM manual_review_cases),(SELECT count(*) FROM reconciliation_observation_progress)`).Scan(&cases, &manual, &progress); err != nil || cases != 0 || manual != 0 || progress != 0 {
					t.Fatal("observation failure left a half-published case", cases, manual, progress, err)
				}
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER controlled_orphan_progress_failure ON reconciliation_observation_progress; DROP FUNCTION controlled_orphan_progress_failure()`); err != nil {
					t.Fatal(err)
				}
				recovered, err := coordinator.resumeReconciliationRun(ctx, run.ID)
				if err != nil || recovered.ID != run.ID {
					t.Fatal("persisted original run did not recover", err)
				}
				read(recovered, "OPEN")
				read(observe(1), "OPEN")
			}
			if financial() != before || provider.queryCount != 0 {
				t.Fatal("manual orphan investigation performed a financial effect or provider order repair")
			}
			var dateUnproven bool
			if err := r.pool.QueryRow(ctx, `SELECT paid_at IS NULL FROM payment_orders WHERE id=$1`, order.ID).Scan(&dateUnproven); err != nil || !dateUnproven {
				t.Fatal("observation invented a payment date", err)
			}
		})
	}
}
