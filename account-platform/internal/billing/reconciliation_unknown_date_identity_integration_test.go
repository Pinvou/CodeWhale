package billing

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"testing"
	"time"
)

// Exercise capture and recovery across a real uncommitted canonical publication.
// The temporary provider generation only constructs an old-writer visibility window;
// this does not claim a live two-binary deployment or a normal financial rewrite API.
func testUnknownDateCaseIdentityRecovery(t *testing.T, legacySource bool) {
	t.Helper()
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
	observe := func(operation context.Context, offset int) (ReconciliationRun, error) {
		return coordinator.RunAdminReconciliation(operation, actor, order.Channel, date.AddDate(0, 0, offset))
	}
	read := func(runID uuid.UUID) AdminReconciliationCase {
		t.Helper()
		items, err := r.AdminReconciliationCases(ctx, runID, 10)
		if err != nil || len(items) != 1 {
			t.Fatal("read associated case", len(items), err)
		}
		return items[0]
	}
	ignore := func(item AdminReconciliationCase) AdminReconciliationCase {
		t.Helper()
		if _, err := r.UpdateReconciliationCase(ctx, item.ID, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled claim for version ownership"}); err != nil {
			t.Fatal(err)
		}
		var observation struct {
			ID uuid.UUID `json:"observation_id"`
		}
		if err := json.Unmarshal(item.LatestObservation, &observation); err != nil || observation.ID == uuid.Nil {
			t.Fatal("actual observation binding required", err)
		}
		ignored, err := r.UpdateReconciliationCase(ctx, item.ID, ReconciliationCaseCommand{Action: "IGNORE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled accept actual observed facts", ReviewResult: "reviewed original snapshot", ObservedObservationID: observation.ID})
		if err != nil || ignored.Status != "IGNORED" {
			t.Fatal("real ignore command", err)
		}
		return ignored
	}
	legacyRun, err := observe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := read(legacyRun.ID)
	difference := unknownDateOrphanOriginalDifference(legacy)
	legacyDigest := reconciliationDifferenceDigest(legacyRun, difference)
	identity := reconciliationCaseIdentity(legacyRun, difference)
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_cases SET fact_digest=$2 WHERE id=$1`, legacy.ID, legacyDigest); err != nil {
		t.Fatal(err)
	}
	legacy = ignore(legacy)
	var legacyBefore string
	if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM reconciliation_cases c WHERE id=$1`, legacy.ID).Scan(&legacyBefore); err != nil {
		t.Fatal(err)
	}
	// Reproduce the visibility of an overlapping old writer while letting the
	// actual new producer and publication transaction generate the canonical row.
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET provider_version='controlled-pending-legacy' WHERE id=$1`, legacyRun.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION public.unknown_date_hold_progress() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN PERFORM pg_advisory_xact_lock(94247701::bigint); RETURN NEW; END $fn$;
CREATE TRIGGER unknown_date_hold_progress BEFORE INSERT ON public.reconciliation_observation_progress FOR EACH ROW EXECUTE FUNCTION public.unknown_date_hold_progress()`); err != nil {
		t.Fatal(err)
	}
	barrier, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_lock(94247701::bigint)`); err != nil {
		barrier.Release()
		t.Fatal(err)
	}
	locked := true
	unblock := func() {
		if !locked {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := barrier.Exec(cleanup, `SELECT pg_advisory_unlock(94247701::bigint)`); err != nil {
			t.Error("release private barrier", err)
		}
		barrier.Release()
		locked = false
	}
	canonicalCtx, cancelCanonical := context.WithCancel(ctx)
	olderCtx, cancelOlder := context.WithCancel(ctx)
	type result struct {
		run ReconciliationRun
		err error
	}
	canonicalResults, olderResults := make(chan result, 1), make(chan result, 1)
	canonicalPending, olderPending := true, false
	t.Cleanup(func() {
		cancelCanonical()
		cancelOlder()
		unblock()
		for _, pending := range []struct {
			enabled bool
			results chan result
		}{{canonicalPending, canonicalResults}, {olderPending, olderResults}} {
			if pending.enabled {
				select {
				case <-pending.results:
				case <-time.After(6 * time.Second):
					t.Error("owned observer did not stop")
				}
			}
		}
	})
	go func() { run, err := observe(canonicalCtx, -1); canonicalResults <- result{run, err} }()
	waitFor := func(count int) {
		t.Helper()
		deadline := time.Now().Add(9 * time.Second)
		for {
			var got int
			err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&got)
			if err != nil {
				t.Fatal(err)
			}
			if got == count {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("publication barriers were not reached", count, got)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(1) // canonical has INSERTed the case and is waiting before progress/commit.
	if legacySource {
		// An insert-boundary fixture reproduces the published old envelope.
		// It leaves all financial bindings/versions intact and never rewrites
		// an existing immutable observation or the recovery reader under test.
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION public.unknown_date_old_source() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN NEW.source_bindings := NEW.source_bindings - 'case_ids'; RETURN NEW; END $fn$;
CREATE TRIGGER unknown_date_old_source BEFORE INSERT ON public.reconciliation_observations FOR EACH ROW EXECUTE FUNCTION public.unknown_date_old_source()`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET provider_version=$2 WHERE id=$1`, legacyRun.ID, legacyRun.ProviderVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET amount_fen=amount_fen+1 WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	olderPending = true
	go func() { run, err := observe(olderCtx, 1); olderResults <- result{run, err} }()
	waitFor(2) // older observation is already durable and waits for canonical's case lock.
	var observedID uuid.UUID
	var source []byte
	var progress int
	if err := r.pool.QueryRow(ctx, `SELECT o.id,o.source_bindings,(SELECT count(*) FROM reconciliation_observation_progress p WHERE p.observation_id=o.id) FROM reconciliation_runs run JOIN reconciliation_observations o ON o.id=run.active_observation_id WHERE run.business_date=$1 AND run.provider_version=$2`, date.AddDate(0, 0, 1).Format("2006-01-02"), legacyRun.ProviderVersion).Scan(&observedID, &source, &progress); err != nil || progress != 0 {
		t.Fatal("observation must be durable and unpublished", progress, err)
	}
	versions, caseIDs, err := reconciliationObservedCaseState(source)
	if err != nil || versions[identity] != legacy.Version {
		t.Fatal("older immutable observation must capture only legacy version", versions, err)
	}
	if legacySource {
		if caseIDs != nil {
			t.Fatal("historical envelope gained case identity authority")
		}
	} else if caseIDs[identity] != legacy.ID {
		t.Fatal("immutable authority must bind the actual observed legacy case", caseIDs)
	}
	beforeRecovery := entitlementFinancialFactsSnapshot(t, ctx, r)
	var visibleCanonical int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases WHERE fact_digest=$1`, identity).Scan(&visibleCanonical); err != nil || visibleCanonical != 0 {
		t.Fatal("canonical must still be invisible to older observation", visibleCanonical, err)
	}
	cancelOlder()
	older := <-olderResults
	olderPending = false
	if older.err == nil || older.run.ID == uuid.Nil {
		t.Fatal("older operation did not leave a resumable run", older.err)
	}
	if legacySource {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER unknown_date_old_source ON public.reconciliation_observations; DROP FUNCTION public.unknown_date_old_source()`); err != nil {
			t.Fatal(err)
		}
	}
	unblock()
	canonical := <-canonicalResults
	canonicalPending = false
	if canonical.err == nil || canonical.run.ID == uuid.Nil {
		t.Fatal("canonical's stale facts must fail final current-facts check", canonical.err)
	}
	newCase := read(canonical.run.ID)
	if newCase.ID == legacy.ID {
		t.Fatal("expected distinct canonical case")
	}
	newIgnored := ignore(newCase)
	if newIgnored.Version != legacy.Version {
		t.Fatal("mixed version collision precondition missing", newIgnored.Version, legacy.Version)
	}
	resumed, err := coordinator.resumeReconciliationRun(ctx, older.run.ID)
	if err != nil || resumed.Status != "COMPLETED" {
		t.Fatal("resume original durable observation", resumed.Status, err)
	}
	var afterID uuid.UUID
	var afterSource []byte
	if err := r.pool.QueryRow(ctx, `SELECT run.active_observation_id,observation.source_bindings FROM reconciliation_runs run JOIN reconciliation_observations observation ON observation.id=run.active_observation_id WHERE run.id=$1`, resumed.ID).Scan(&afterID, &afterSource); err != nil || afterID != observedID || string(afterSource) != string(source) {
		t.Fatal("original immutable observation must be replayed", err)
	}
	after := read(resumed.ID)
	if after.ID != newCase.ID {
		t.Fatal("recovery did not reuse existing canonical case")
	}
	if after.Status != "IGNORED" || after.Version != newIgnored.Version {
		t.Fatalf("older observation must not borrow another case's equal version: want IGNORED/v%d got %s/v%d", newIgnored.Version, after.Status, after.Version)
	}
	if !reflect.DeepEqual(beforeRecovery, entitlementFinancialFactsSnapshot(t, ctx, r)) {
		t.Fatal("replaying persisted input changed financial or existing outbox facts")
	}
	var priorEvents []struct {
		ID uuid.UUID `json:"event_id"`
	}
	if err := json.Unmarshal([]byte(beforeRecovery["outbox_events"]), &priorEvents); err != nil {
		t.Fatal(err)
	}
	priorIDs := make([]uuid.UUID, 0, len(priorEvents))
	for _, event := range priorEvents {
		priorIDs = append(priorIDs, event.ID)
	}
	fresh, err := observe(ctx, 3)
	if err != nil || fresh.Status != "COMPLETED" {
		t.Fatal("fresh observation after preserved manual disposition", fresh.Status, err)
	}
	reopened := read(fresh.ID)
	if reopened.ID != newCase.ID || reopened.Status != "OPEN" || reopened.Version <= newIgnored.Version {
		t.Fatal("a genuinely fresh changed observation must reopen the same canonical case", reopened)
	}
	var legacyAfter string
	if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM reconciliation_cases c WHERE id=$1`, legacy.ID).Scan(&legacyAfter); err != nil || legacyAfter != legacyBefore {
		t.Fatal("recovery changed the retained legacy investigation", err)
	}
	// A fresh Bill call legitimately emits one health event. Permit precisely
	// that actual successful attempt; freeze every old outbox row and all money
	// tables rather than excluding diagnostic history from the assertion.
	var healthEvents int
	var healthID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT count(*),min(event.event_id::text)::uuid
FROM outbox_events event
JOIN payment_provider_attempts attempt ON attempt.id=event.aggregate_id
JOIN payment_provider_health_observations health ON health.provider_attempt_id=attempt.provider_attempt_id
WHERE NOT(event.event_id=ANY($1::uuid[]))
AND event.event_type='billing.payment-provider.health-observed.v1' AND event.aggregate_version=1
AND attempt.provider_id=$2 AND attempt.provider_version=$3 AND attempt.channel=$4
AND attempt.capability='BILL_QUERY' AND attempt.status='RESULT_RECORDED' AND health.outcome='SUCCESS'
AND event.payload->>'provider_attempt_id'=attempt.provider_attempt_id
AND event.payload->>'provider_id'=attempt.provider_id AND event.payload->>'provider_version'=attempt.provider_version
AND event.payload->>'channel'=attempt.channel AND event.payload->>'route'=attempt.route
AND event.payload->>'capability'='BILL_QUERY' AND event.payload->>'outcome'='SUCCESS'`, priorIDs, legacyRun.ProviderID, legacyRun.ProviderVersion, order.Channel).Scan(&healthEvents, &healthID); err != nil || healthEvents != 1 || healthID == uuid.Nil {
		t.Fatal("fresh observation must append exactly its successful Bill health event", healthEvents, err)
	}
	afterFinancial := entitlementFinancialFactsSnapshot(t, ctx, r)
	var priorOutboxAfter string
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(event) AS row FROM outbox_events event WHERE event_id<>$1) facts`, healthID).Scan(&priorOutboxAfter); err != nil {
		t.Fatal(err)
	}
	afterFinancial["outbox_events"] = priorOutboxAfter
	if !reflect.DeepEqual(beforeRecovery, afterFinancial) {
		var changed []string
		for table, original := range beforeRecovery {
			if afterFinancial[table] != original {
				changed = append(changed, table)
			}
		}
		t.Fatal("fresh observation changed financial or prior outbox facts", changed)
	}
}
