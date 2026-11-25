package billing

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Install the genuine pre-binding package, retain a real old command row, then
// use the production wrapper to append the binding migration. No ledger edits,
// timestamp backfill or retry token can turn that old UNKNOWN into execution.
func TestLatePaymentQueryGenerationUpgradePostgres(t *testing.T) {
	t.Run("published-pre48-UNKNOWN-and-new-explicit-recovery", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t, 900047)
		_, original, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		order, err := getPaymentOrderByID(ctx, r.pool, original.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		r.now = func() time.Time { return order.FinalArchiveEligibleAt.Add(time.Second) }
		item, err := r.EscalateLatePaymentReview(ctx, order.ID, "PENDING")
		if err != nil {
			t.Fatal(err)
		}
		actor := uuid.New()
		item, err = r.ClaimLatePaymentReview(ctx, item.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
		legacyKey := "query-legacy-" + uuid.NewString()
		hash, request := latePaymentQueryFingerprint(input)
		if _, err := r.pool.Exec(ctx, `INSERT INTO late_payment_query_commands(idempotency_key,case_id,actor_id,payment_order_id,request_hash,request_snapshot,started_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, legacyKey, item.ID, actor, order.ID, hash, request, r.now()); err != nil {
			t.Fatal("published 900030 writer fixture", err)
		}
		legacyRow := func() string {
			t.Helper()
			var row string
			if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(command)::text FROM late_payment_query_commands command WHERE idempotency_key=$1`, legacyKey).Scan(&row); err != nil {
				t.Fatal(err)
			}
			return row
		}
		before := legacyRow()
		loaded, err := migrations.Load("../../migrations/billing")
		if err != nil {
			t.Fatal(err)
		}
		config := r.pool.Config().ConnConfig.Copy()
		config.User, config.Password = "billing_migrator", ""
		operator, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer operator.Close(context.Background())
		if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		if err := billingschema.Up(ctx, operator, loaded); err != nil {
			t.Fatal("real forward binding upgrade", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, operator, loaded); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, operator)
		if before != legacyRow() || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_generations`) != 0 {
			t.Fatal("upgrade changed an old command or fabricated generation bindings")
		}
		// Approved deployment policy: an unchanged old writer must fail closed,
		// not publish a newly admitted but unbound command during a mixed rollout.
		oldWriterKey := "query-old-writer-" + uuid.NewString()
		_, err = r.pool.Exec(ctx, `INSERT INTO late_payment_query_commands(idempotency_key,case_id,actor_id,payment_order_id,request_hash,request_snapshot,started_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, oldWriterKey, item.ID, actor, order.ID, hash, request, r.now())
		var oldWriterError *pgconn.PgError
		if !errors.As(err, &oldWriterError) || oldWriterError.Code != "23514" || oldWriterError.Message != "new query command requires its current unknown generation" {
			t.Fatal("old writer did not hit the exact new commit-time refusal", err)
		}
		if lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands WHERE idempotency_key=$1`, oldWriterKey) != 0 || before != legacyRow() || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_generations`) != 0 {
			t.Fatal("rejected old writer left a marker, binding or rewrote legacy evidence")
		}
		for _, read := range []bool{true, false} {
			var retained LatePaymentQueryReceipt
			if read {
				retained, err = r.ReadLatePaymentQuery(ctx, item.ID, actor, legacyKey, input)
			} else {
				retained, err = r.PrepareLatePaymentQuery(ctx, item.ID, actor, legacyKey, input)
			}
			if err != nil || retained.State != "UNKNOWN" || retained.dispatch || retained.generation != uuid.Nil || before != legacyRow() {
				t.Fatal("old unbound command was upgraded into execution or a receipt", err)
			}
		}
		if _, err := r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
			t.Fatal("legacy UNKNOWN without a new current proof was admitted", err)
		}
		key := "query-current-" + uuid.NewString()
		accepted, err := r.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input)
		if err != nil || !accepted.dispatch || accepted.generation == uuid.Nil {
			t.Fatal("new explicit query was not atomically admitted", err)
		}
		if _, err := r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
			t.Fatal("current pending command allowed archive", err)
		}
		provider := &archiveAdmissionProvider{instructionCreateProvider: &instructionCreateProvider{retainedFinancialProvider: &retainedFinancialProvider{}, authoritative: true}, response: ProviderOrder{
			MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency,
			ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU, Status: "CLOSED", ProviderStatus: "PAY_CLOSED",
		}}
		coordinator, err := newBillingFixturePaymentCoordinator(r, provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		coordinator.now = r.now
		queried, err := coordinator.syncReviewedQueryCommand(ctx, order.ID, accepted.generation)
		if err != nil {
			t.Fatal("full real current query", err)
		}
		if completed, err := r.CompleteLatePaymentQuery(ctx, item.ID, actor, key, input, queried, nil); err != nil || completed.State != "COMPLETED" || completed.generation != accepted.generation {
			t.Fatal("current result and original receipt did not complete together", err)
		}
		if archived, err := r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); err != nil || archived.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" {
			t.Fatal("new current proof could not recover legacy UNKNOWN admission", err)
		}
		fullFacts := func() string {
			t.Helper()
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		 'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
		 'commands',(SELECT jsonb_agg(to_jsonb(c) ORDER BY idempotency_key) FROM late_payment_query_commands c),
		 'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY idempotency_key) FROM late_payment_query_generations b),
		 'archive',(SELECT jsonb_agg(to_jsonb(a) ORDER BY idempotency_key) FROM late_payment_review_archive_commands a),
		 'late',(SELECT to_jsonb(l) FROM late_payment_review_cases l WHERE l.id=$1),
		 'manual',(SELECT to_jsonb(m) FROM manual_review_cases m WHERE m.id=$1),
		 'order',(SELECT to_jsonb(o) FROM payment_orders o WHERE o.id=$2),
		 'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM late_payment_review_evidence e WHERE e.case_id=$1))::text`, item.ID, order.ID).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		stable, financial := fullFacts(), instructionCommandFactsSnapshot(t, r, ctx)
		if err := billingschema.Up(ctx, operator, loaded); err != nil {
			t.Fatal("complete package replay", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, operator, loaded); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, operator)
		if stable != fullFacts() || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
			t.Fatal("complete package replay changed full receipts, ledger or financial facts")
		}
		down, err := os.ReadFile("../../migrations/billing/900048_late_payment_query_generations.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		_, err = retainedFinanceOwnerExec(t, r, ctx, `BEGIN;`+string(down)+`COMMIT;`)
		var rollback *pgconn.PgError
		if !errors.As(err, &rollback) || rollback.Code != "P0001" || rollback.Message != "late payment query generation bindings are forward-only" {
			t.Fatal("binding Down did not preserve its exact forward-only refusal", err)
		}
		if stable != fullFacts() || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
			t.Fatal("refused binding rollback changed complete facts")
		}
		for _, read := range []bool{true, false} {
			var retained LatePaymentQueryReceipt
			if read {
				retained, err = r.ReadLatePaymentQuery(ctx, item.ID, actor, legacyKey, input)
			} else {
				retained, err = r.PrepareLatePaymentQuery(ctx, item.ID, actor, legacyKey, input)
			}
			if err != nil || retained.State != "UNKNOWN" || retained.dispatch || before != legacyRow() || provider.gets.Load() != 1 {
				t.Fatal("recovery rewrote or redispatched the original legacy invocation", err)
			}
		}
	})
}
