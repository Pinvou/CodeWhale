package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Only the owned UUID database is changed. The same-column view injects an
// execution-time error after PostgreSQL has described the real SELECT result.
func withHealthReadFault(t *testing.T, ctx context.Context, r *PostgresRepository, phase string, check func()) {
	t.Helper()
	selectors := map[string]string{
		"discovery":    `current_query() LIKE 'SELECT DISTINCT provider_id,%'`,
		"latest-five":  `current_query() LIKE 'SELECT id,outcome,logical_order_id%' AND position('5 minutes' in current_query())>0`,
		"failure-rate": `current_query() LIKE 'SELECT id,outcome,logical_order_id%' AND position('1 minute' in current_query())>0`,
	}
	selector, ok := selectors[phase]
	if !ok {
		t.Fatal("unknown controlled phase", phase)
	}
	config := r.pool.Config().ConnConfig.Copy()
	if !strings.HasPrefix(config.Database, "retained_finance_") {
		t.Fatal("owned UUID required")
	}
	config.User, config.Password = "billing_migrator", ""
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `CREATE FUNCTION public.controlled_health_read_fault() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$ BEGIN IF `+selector+` THEN RAISE EXCEPTION 'controlled-health-read-fault' USING ERRCODE='PZ001'; END IF; RETURN true; END $$;
 ALTER TABLE public.payment_provider_health_observations RENAME TO controlled_health_read_backing;
 CREATE VIEW public.payment_provider_health_observations AS SELECT * FROM public.controlled_health_read_backing WHERE public.controlled_health_read_fault();
 GRANT SELECT ON public.payment_provider_health_observations TO billing_service;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		r.pool.Reset()
		clean, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		restore, err := owner.Begin(clean)
		if err != nil {
			t.Error(err)
			return
		}
		defer restore.Rollback(context.Background())
		_, err = restore.Exec(clean, `DROP VIEW public.payment_provider_health_observations;
   ALTER TABLE public.controlled_health_read_backing RENAME TO payment_provider_health_observations;
   DROP FUNCTION public.controlled_health_read_fault();`)
		if err == nil {
			err = restore.Commit(clean)
		}
		if err != nil {
			t.Error("fault schema restoration failed", err)
		}
		r.pool.Reset()
	}()
	r.pool.Reset()
	check()
}

func TestProviderHealthReadFailurePostgres(t *testing.T) {
	for _, phase := range []string{"discovery", "latest-five", "failure-rate"} {
		t.Run(phase, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			now := time.Now().UTC().Truncate(time.Second)
			r.now = func() time.Time { return now }
			var orders []PaymentOrder
			for n := 0; n < 5; n++ {
				_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
				orders = append(orders, order)
			}
			for n := 0; n < 20; n++ {
				order := orders[n%5]
				attempt, err := r.beginProviderAttempt(ctx, order.ProviderID, order.ProviderVersion, "ORDER_QUERY", order.Channel, "controlled-read-recovery", providerCallScope(order))
				if err != nil {
					t.Fatal(err)
				}
				outcome := "SUCCESS"
				if n%2 == 0 {
					outcome = "INFRASTRUCTURE_FAILURE"
				}
				if err = r.finishProviderAttempt(ctx, attempt, outcome, nil); err != nil {
					t.Fatal(err)
				}
			}
			baseline, err := r.observationCandidates(ctx, now)
			if err != nil || len(baseline) != 1 || baseline[0].TriggerType != "FAILURE_RATE" || baseline[0].SampleCount != 20 || baseline[0].FailureCount != 10 || baseline[0].DistinctOrders != 5 {
				t.Fatal("qualified original baseline", baseline, err)
			}
			snapshot := func() string {
				t.Helper()
				var value string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_provider_attempts t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_provider_health_observations t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_orders t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM wallets t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM journal_entries t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM ledger_postings t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY provider_id,provider_version) FROM payment_provider_controls t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_stop_loss_evaluations t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM payment_stop_loss_evaluation_members t),
   (SELECT jsonb_agg(to_jsonb(t) ORDER BY event_id) FROM outbox_events t))::text`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			withHealthReadFault(t, ctx, r, phase, func() {
				candidates, err := r.observationCandidates(ctx, now)
				var databaseError *pgconn.PgError
				if len(candidates) != 0 || !errors.As(err, &databaseError) || databaseError.Code != "PZ001" || !strings.Contains(databaseError.Message, "controlled-health-read-fault") {
					t.Fatalf("read failure identity phase=%s candidates=%v err=%v", phase, candidates, err)
				}
				count, err := r.EvaluateProviderStopLosses(ctx, 100)
				if count != 0 || !errors.As(err, &databaseError) || databaseError.Code != "PZ001" {
					t.Fatal("failed evaluation must not report healthy success", phase, count, err)
				}
			})
			if snapshot() != before {
				t.Fatal("failed read changed evidence, control or money", phase)
			}
			recovered, err := r.observationCandidates(ctx, now)
			if err != nil || len(recovered) != 1 || recovered[0].TriggerType != "FAILURE_RATE" || recovered[0].SampleCount != 20 {
				t.Fatal("next read did not recover", phase, recovered, err)
			}
			if count, err := r.EvaluateProviderStopLosses(ctx, 100); err != nil || count != 1 {
				t.Fatal("next evaluation did not apply original threshold", phase, count, err)
			}
			var enabled bool
			var version int64
			if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id='controlled' AND provider_version='v1'`).Scan(&enabled, &version); err != nil || enabled || version != 1 {
				t.Fatal("recovered control", phase, enabled, version, err)
			}
			if count, err := r.EvaluateProviderStopLosses(ctx, 100); err != nil || count != 0 {
				t.Fatal("disabled control was reapplied", count, err)
			}
			stable := snapshot()
			if count, err := r.EvaluateProviderStopLosses(ctx, 100); err != nil || count != 0 || snapshot() != stable {
				t.Fatal("later evaluation repeated durable effects", count, err)
			}
		})
	}
}
