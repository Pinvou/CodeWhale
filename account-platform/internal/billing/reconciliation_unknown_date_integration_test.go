package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

func TestReconciliationUnknownDateOrphanLifecyclePostgres(t *testing.T) {
	for _, scenario := range []string{"cross-date-concurrency", "legacy-and-ignore", "mixed-canonical-and-legacy", "observation-rollback", "stale-review-binding", "query-index-upgrade", "case-identity-recovery-fence", "legacy-unbound-recovery-fence"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "case-identity-recovery-fence" || scenario == "legacy-unbound-recovery-fence" {
				testUnknownDateCaseIdentityRecovery(t, scenario == "legacy-unbound-recovery-fence")
				return
			}
			var r *PostgresRepository
			var ctx context.Context
			if scenario == "query-index-upgrade" {
				r, ctx = retainedFinanceRepository(t, 900038)
			} else {
				r, ctx = retainedFinanceRepository(t)
			}
			indexes := func(want bool) {
				t.Helper()
				for name, definition := range map[string]string{
					"reconciliation_cases_payment_order_created_idx": "CREATE INDEX reconciliation_cases_payment_order_created_idx ON public.reconciliation_cases USING btree (payment_order_id, created_at, id)",
					"reconciliation_observation_progress_case_idx":   "CREATE INDEX reconciliation_observation_progress_case_idx ON public.reconciliation_observation_progress USING btree (case_id, observation_id)",
					"reconciliation_observation_facts_order_idx":     "CREATE INDEX reconciliation_observation_facts_order_idx ON public.reconciliation_observation_facts USING btree (observation_id, payment_order_id)",
				} {
					var actual string
					err := r.pool.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname=$1 AND i.indisvalid AND i.indisready AND pg_get_userbyid(c.relowner)='billing_schema_owner'`, name).Scan(&actual)
					if want && (err != nil || actual != definition) || !want && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal("public orphan query index contract", name, actual, want, err)
					}
				}
			}
			indexes(scenario != "query-index-upgrade")
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
			type historicalPair struct {
				first, later AdminReconciliationCase
				laterRun     ReconciliationRun
			}
			prepareHistoricalPair := func() historicalPair {
				t.Helper()
				clock := r.now().UTC()
				coordinator.now = func() time.Time { return clock }
				firstRun := observe(0)
				first := read(firstRun, "OPEN")
				legacyDigest := reconciliationDifferenceDigest(firstRun, unknownDateOrphanOriginalDifference(first))
				if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_cases SET fact_digest=$2 WHERE id=$1`, first.ID, legacyDigest); err != nil {
					t.Fatal("controlled old-writer identity", err)
				}
				// Use the real producer for both complete observations and manual
				// histories. The temporary generation isolates only fixture setup;
				// restore it before testing a historical old/new-writer pair.
				if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET provider_version='controlled-legacy-seed' WHERE id=$1`, firstRun.ID); err != nil {
					t.Fatal("isolate controlled historical seed", err)
				}
				clock = clock.Add(time.Second)
				laterRun := observe(-1)
				later := read(laterRun, "OPEN")
				if later.ID == first.ID {
					t.Fatal("historical fixture needs two actual investigations")
				}
				if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET provider_version=$2 WHERE id=$1`, firstRun.ID, firstRun.ProviderVersion); err != nil {
					t.Fatal("restore controlled historical provider generation", err)
				}
				var ordered bool
				if err := r.pool.QueryRow(ctx, `SELECT first.created_at<later.created_at FROM reconciliation_cases first,reconciliation_cases later WHERE first.id=$1 AND later.id=$2`, first.ID, later.ID).Scan(&ordered); err != nil || !ordered {
					t.Fatal("historical pair needs distinct ordered creation times", err)
				}
				clock = clock.Add(time.Second)
				return historicalPair{first: first, later: later, laterRun: laterRun}
			}
			command := func(caseID uuid.UUID, action string) AdminReconciliationCase {
				t.Helper()
				command := ReconciliationCaseCommand{Action: action, ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled manual orphan date review", ReviewResult: "controlled accepted original financial facts"}
				if action == "IGNORE" {
					items, err := r.AdminReconciliationCasesByIDs(ctx, []uuid.UUID{caseID})
					if err != nil || len(items) != 1 {
						t.Fatal("read actual review observation", err)
					}
					var observation struct {
						ID uuid.UUID `json:"observation_id"`
					}
					if err := json.Unmarshal(items[0].LatestObservation, &observation); err != nil {
						t.Fatal(err)
					}
					command.ObservedObservationID = observation.ID
				}
				item, err := r.UpdateReconciliationCase(ctx, caseID, command)
				if err != nil {
					t.Fatal(action, err)
				}
				return item
			}
			switch scenario {
			case "query-index-upgrade":
				first := read(observe(0), "OPEN")
				reviewFacts := func() string {
					t.Helper()
					var facts string
					if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'runs',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_runs x),
'cases',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_cases x),
'case_runs',(SELECT jsonb_agg(to_jsonb(x) ORDER BY run_id,case_id) FROM reconciliation_case_runs x),
'observations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_observations x),
'facts',(SELECT jsonb_agg(to_jsonb(x) ORDER BY observation_id,row_number) FROM reconciliation_observation_facts x),
'progress',(SELECT jsonb_agg(to_jsonb(x) ORDER BY observation_id,fact_digest) FROM reconciliation_observation_progress x),
'manual_cases',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_cases x),
'manual_events',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_case_events x))::text`).Scan(&facts); err != nil {
						t.Fatal("snapshot original investigation facts", err)
					}
					return facts
				}
				originalReviews := reviewFacts()
				cfg := r.pool.Config().ConnConfig.Copy()
				cfg.User, cfg.Password = "billing_migrator", ""
				operator, err := pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer operator.Close(context.Background())
				if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
					t.Fatal(err)
				}
				all, err := migrations.Load("../../migrations/billing")
				if err != nil {
					t.Fatal(err)
				}
				var prior, prefix []migrations.Migration
				for _, migration := range all {
					if migration.Version < 900040 {
						prior = append(prior, migration)
					}
					if migration.Version <= 900040 {
						prefix = append(prefix, migration)
					}
				}
				if len(prefix) == 0 || prefix[len(prefix)-1].Version != 900040 {
					t.Fatal("exact query index migration boundary is unavailable")
				}
				if err := billingschema.Up(ctx, operator, prior); err != nil {
					t.Fatal("official query index predecessor upgrade", err)
				}
				var ledgerBefore, ledgerAfter string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
				if err := billingschema.Up(ctx, operator, prefix); err != nil {
					t.Fatal("official query index upgrade", err)
				}
				indexes(true)
				// Down uses the real deployment connection and its existing owner
				// role. Runtime never owns indexes or writes the migration ledger.
				if _, err := operator.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
					t.Fatal("enter index rollback owner", err)
				}
				if err := billingschema.DownOne(ctx, operator, prefix); err != nil {
					t.Fatal("official index-only rollback", err)
				}
				if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
					t.Fatal("restore ordinary migration entry role", err)
				}
				indexes(false)
				if _, err := billingschema.VerifyApplied(ctx, operator, prior); err != nil {
					t.Fatal("verify index rollback predecessor", err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerAfter); err != nil || ledgerAfter != ledgerBefore || reviewFacts() != originalReviews || financial() != before {
					t.Fatal("index roundtrip changed ledger or original investigation/financial facts", err)
				}
				for replay := 0; replay < 2; replay++ {
					if err := billingschema.Up(ctx, operator, all); err != nil {
						t.Fatal("complete current index upgrade/replay", err)
					}
					if _, err := billingschema.VerifyApplied(ctx, operator, all); err != nil {
						t.Fatal("verify complete index upgrade/replay", err)
					}
					indexes(true)
					if reviewFacts() != originalReviews || financial() != before {
						t.Fatal("index upgrade/replay changed original facts")
					}
				}
				migrations.Unlock(ctx, operator)
				if item := read(observe(1), "OPEN"); item.ID != first.ID {
					t.Fatal("indexed consumer replaced the original investigation")
				}
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
				pair := prepareHistoricalPair()
				first, duplicateRun, duplicate := pair.first, pair.laterRun, pair.later
				original := string(first.InternalSnapshot)
				duplicateDigest := reconciliationDifferenceDigest(duplicateRun, unknownDateOrphanOriginalDifference(duplicate))
				if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_cases SET fact_digest=$2 WHERE id=$1`, duplicate.ID, duplicateDigest); err != nil {
					t.Fatal("controlled second old-writer identity", err)
				}
				var correctlyOrdered bool
				var duplicateBefore, duplicateAfter string
				if err := r.pool.QueryRow(ctx, `SELECT first.created_at<later.created_at,to_jsonb(later)::text FROM reconciliation_cases first,reconciliation_cases later WHERE first.id=$1 AND later.id=$2`, first.ID, duplicate.ID).Scan(&correctlyOrdered, &duplicateBefore); err != nil || !correctlyOrdered {
					t.Fatal("historical duplicate fixture must have distinct ordered creation times", err)
				}
				command(first.ID, "CLAIM")
				command(first.ID, "IGNORE")
				repeated := observe(1)
				var selected uuid.UUID
				if err := r.pool.QueryRow(ctx, `SELECT case_id FROM reconciliation_case_runs WHERE run_id=$1`, repeated.ID).Scan(&selected); err != nil || selected != first.ID {
					t.Fatal("legacy duplicate reuse must select the earliest created investigation", selected, first.ID, err)
				}
				read(repeated, "IGNORED")
				if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET amount_fen=amount_fen+1 WHERE id=$1`, order.ID); err != nil {
					t.Fatal("controlled changed source fact", err)
				}
				if item := read(observe(2), "OPEN"); item.ID != first.ID || string(item.InternalSnapshot) != original {
					t.Fatal("changed fact replaced investigation or immutable snapshot")
				}
				command(first.ID, "CLAIM")
				command(first.ID, "IGNORE")
				if item := read(observe(3), "IGNORED"); item.ID != first.ID {
					t.Fatal("accepting changed evidence did not retain the investigation")
				}
				var cases, observations int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_cases WHERE payment_order_id=$1),(SELECT count(*) FROM reconciliation_observation_progress WHERE case_id=$2)`, order.ID, first.ID).Scan(&cases, &observations); err != nil || cases != 2 || observations != 4 {
					t.Fatal("legacy/ignore lifecycle lost evidence", cases, observations, err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM reconciliation_cases c WHERE id=$1`, duplicate.ID).Scan(&duplicateAfter); err != nil || duplicateAfter != duplicateBefore {
					t.Fatal("historical duplicate was deleted or rewritten", err)
				}
			case "mixed-canonical-and-legacy":
				pair := prepareHistoricalPair()
				// Keep the later producer's canonical key. This supported rolling
				// history must use that case's version, never the older legacy's.
				var legacyBefore, legacyAfter string
				if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM reconciliation_cases c WHERE id=$1`, pair.first.ID).Scan(&legacyBefore); err != nil {
					t.Fatal(err)
				}
				command(pair.later.ID, "CLAIM")
				ignored := command(pair.later.ID, "IGNORE")
				if ignored.Version == pair.first.Version {
					t.Fatal("mixed history must exercise distinct canonical and legacy versions")
				}
				if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET amount_fen=amount_fen+1 WHERE id=$1`, order.ID); err != nil {
					t.Fatal("controlled changed source fact", err)
				}
				changed := read(observe(1), "OPEN")
				if changed.ID != pair.later.ID || changed.Version <= ignored.Version {
					t.Fatal("changed facts must reopen the same canonical investigation", changed, ignored)
				}
				if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM reconciliation_cases c WHERE id=$1`, pair.first.ID).Scan(&legacyAfter); err != nil || legacyAfter != legacyBefore {
					t.Fatal("canonical review changed the retained legacy investigation", err)
				}
			case "stale-review-binding":
				firstRun := observe(0)
				first := read(firstRun, "OPEN")
				var originalObservation struct {
					ID uuid.UUID `json:"observation_id"`
				}
				if err := json.Unmarshal(first.LatestObservation, &originalObservation); err != nil || originalObservation.ID == uuid.Nil {
					t.Fatal("original case read lacks its review observation", err)
				}
				if _, err := r.UpdateReconciliationCase(ctx, first.ID, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled claim before new observation"}); err != nil {
					t.Fatal(err)
				}
				// A reachable audit repair/corruption changes the source while the
				// operator still has the first case page open. No financial API is
				// claimed to produce this controlled damaged-data fixture.
				if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET amount_fen=amount_fen+1 WHERE id=$1`, order.ID); err != nil {
					t.Fatal(err)
				}
				latestRun := observe(1)
				latest := read(latestRun, "IN_REVIEW")
				var currentObservation struct {
					ID uuid.UUID `json:"observation_id"`
				}
				if err := json.Unmarshal(latest.LatestObservation, &currentObservation); err != nil || currentObservation.ID == originalObservation.ID {
					t.Fatal("changed observation was hidden", err)
				}
				service := NewService(r).WithPayments(coordinator)
				request := httptest.NewRequest(http.MethodGet, "/admin/v1/reconciliation-cases?run_id="+latestRun.ID.String(), nil).WithContext(ctx)
				request.Header.Set("Authorization", "Bearer controlled-human")
				response := httptest.NewRecorder()
				identity.Middleware(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
				}, http.HandlerFunc(service.adminReconciliationCases)).ServeHTTP(response, request)
				var page struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK || len(page.Items) != 1 {
					t.Fatal("original public case read lost the current review evidence", response.Code, err)
				}
				t.Logf("synthetic_orphan_review_wire=%s", page.Items[0])
				post := func(observed uuid.UUID, key string, want int) string {
					t.Helper()
					body := map[string]any{"action": "IGNORE", "reason": "controlled review of displayed orphan facts", "review_result": "controlled explicit review decision"}
					if observed != uuid.Nil {
						body["observed_observation_id"] = observed
					}
					raw, _ := json.Marshal(body)
					request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-cases/"+first.ID.String()+"/commands", strings.NewReader(string(raw))).WithContext(ctx)
					request.SetPathValue("case_id", first.ID.String())
					request.Header.Set("Authorization", "Bearer controlled-human")
					request.Header.Set("Idempotency-Key", key)
					response := httptest.NewRecorder()
					identity.Middleware(func(context.Context, string) (identity.Principal, error) {
						return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
					}, http.HandlerFunc(service.adminUpdateReconciliationCase)).ServeHTTP(response, request)
					if response.Code != want {
						t.Fatal("actual IGNORE handler observation binding", response.Code, want, response.Body.String())
					}
					return response.Body.String()
				}
				post(originalObservation.ID, uuid.NewString(), http.StatusConflict)
				post(uuid.Nil, uuid.NewString(), http.StatusConflict)
				unchanged := read(latestRun, "IN_REVIEW")
				if unchanged.Version != latest.Version {
					t.Fatal("stale review modified state/version")
				}
				key := uuid.NewString()
				receipt := post(currentObservation.ID, key, http.StatusOK)
				var wire struct {
					Internal struct {
						Order struct {
							Amount string `json:"amount_fen"`
						} `json:"payment_order"`
					} `json:"internal_snapshot"`
					Latest struct {
						ID       uuid.UUID `json:"observation_id"`
						Internal struct {
							Order struct {
								Amount string `json:"amount_fen"`
							} `json:"payment_order"`
						} `json:"internal_snapshot"`
					} `json:"latest_observation"`
				}
				if err := json.Unmarshal([]byte(receipt), &wire); err != nil || wire.Latest.ID != currentObservation.ID || wire.Internal.Order.Amount != fmt.Sprint(order.AmountFen) || wire.Latest.Internal.Order.Amount != fmt.Sprint(order.AmountFen+1) {
					t.Fatal("actual wire lost original/latest exact-money evidence", receipt, err)
				}
				read(observe(2), "IGNORED")
				if replay := post(currentObservation.ID, key, http.StatusOK); replay != receipt {
					t.Fatal("lost ACK replay adopted later evidence")
				}
				post(originalObservation.ID, key, http.StatusConflict)
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
