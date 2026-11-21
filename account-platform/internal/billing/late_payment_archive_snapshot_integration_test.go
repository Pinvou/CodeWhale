package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Force the actual ReadCommitted Prepare to retain its uncommitted marker and
// case locks while the Serializable archive acquires an older snapshot. The
// production no-op case UPDATE must make that archive retry into the marker.
func archivePendingSnapshotBarrier(t *testing.T, r *PostgresRepository, ctx context.Context, item LatePaymentReviewCase, actor uuid.UUID, facts func(*testing.T, uuid.UUID) string) {
	blocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(96112511)`); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION hold_archive_query_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(96112511); RETURN NEW; END $$; CREATE TRIGGER hold_archive_query_fixture AFTER INSERT ON late_payment_query_commands FOR EACH ROW EXECUTE FUNCTION hold_archive_query_fixture()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER hold_archive_query_fixture ON late_payment_query_commands; DROP FUNCTION hold_archive_query_fixture()`); err != nil {
			t.Error("owned query barrier removal failed", err)
		}
	}()
	waiting := func(pattern string) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var blocked bool
			if err := r.pool.QueryRow(waitCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND wait_event_type='Lock' AND query LIKE $1)`, pattern).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				return
			}
			select {
			case <-waitCtx.Done():
				t.Fatal("actual query missed its owned SQL barrier", waitCtx.Err())
			case <-ticker.C:
			}
		}
	}
	before, financial := facts(t, item.ID), instructionCommandFactsSnapshot(t, r, ctx)
	var originalVersion string
	if err := r.pool.QueryRow(ctx, `SELECT xmin::text FROM late_payment_review_cases WHERE id=$1`, item.ID).Scan(&originalVersion); err != nil {
		t.Fatal(err)
	}
	input := LatePaymentQueryInput{1, item.PaymentOrderID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
	key := "query-" + uuid.NewString()
	type prepared struct {
		receipt LatePaymentQueryReceipt
		err     error
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	preparedResult, prepareDone := make(chan prepared, 1), make(chan struct{})
	go func() {
		defer close(prepareDone)
		receipt, err := r.PrepareLatePaymentQuery(prepareCtx, item.ID, actor, key, input)
		preparedResult <- prepared{receipt, err}
	}()
	defer func() {
		cancelPrepare()
		_ = blocker.Rollback(ctx)
		select {
		case <-prepareDone:
		case <-ctx.Done():
			t.Error("owned Prepare did not acknowledge exit")
		}
	}()
	waiting("INSERT INTO late_payment_query_commands%")
	archiveCtx, cancelArchive := context.WithCancel(ctx)
	defer cancelArchive()
	archiveResult, archiveDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(archiveDone)
		_, err := r.ArchiveLatePaymentReview(archiveCtx, item.ID, actor, "archive-"+uuid.NewString())
		archiveResult <- err
	}()
	defer func() {
		cancelArchive()
		select {
		case <-archiveDone:
		case <-ctx.Done():
			t.Error("owned archive did not acknowledge exit")
		}
	}()
	waiting("SELECT late.id,%FOR UPDATE OF late,manual")
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var accepted LatePaymentQueryReceipt
	select {
	case result := <-preparedResult:
		if result.err != nil || !result.receipt.dispatch || result.receipt.State != "UNKNOWN" {
			t.Fatal("real Prepare did not commit its original accepted marker", result.err)
		}
		accepted = result.receipt
	case <-ctx.Done():
		t.Fatal("Prepare did not pass the released barrier")
	}
	select {
	case err := <-archiveResult:
		if !errors.Is(err, ErrConflict) {
			t.Fatal("old snapshot did not retry into the pending marker guard", err)
		}
	case <-ctx.Done():
		t.Fatal("archive did not finish its original bounded retries")
	}
	// Only this real Prepare seam changes the two private current-query facts.
	// Keep every other complete JSON value, including exact numeric text.
	decodeFacts := func(raw string) map[string]any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value map[string]any
		if err := decoder.Decode(&value); err != nil || decoder.Decode(new(any)) != io.EOF {
			t.Fatal("invalid complete query-fence snapshot", err)
		}
		return value
	}
	expected := decodeFacts(before)
	order, ok := expected["order"].(map[string]any)
	if !ok || accepted.generation == uuid.Nil || order["archive_query_generation"] == accepted.generation.String() {
		t.Fatal("Prepare did not create a distinct bound generation")
	}
	if _, present := order["archive_query_generation"]; !present {
		t.Fatal("old snapshot omitted its full generation column")
	}
	if _, present := order["archive_query_authoritative_unpaid"]; !present {
		t.Fatal("old snapshot omitted its full authority column")
	}
	order["archive_query_generation"], order["archive_query_authoritative_unpaid"] = accepted.generation.String(), nil
	if !reflect.DeepEqual(expected, decodeFacts(facts(t, item.ID))) || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
		t.Fatal("query admission changed facts beyond its exact private UUID/UNKNOWN projection")
	}
	var markerVersion string
	if err := r.pool.QueryRow(ctx, `SELECT xmin::text FROM late_payment_review_cases WHERE id=$1`, item.ID).Scan(&markerVersion); err != nil || markerVersion == originalVersion {
		t.Fatal("accepted Prepare failed to create the required physical version", err)
	}
	for _, read := range []bool{true, false} {
		var receipt LatePaymentQueryReceipt
		if read {
			receipt, err = r.ReadLatePaymentQuery(ctx, item.ID, actor, key, input)
		} else {
			receipt, err = r.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input)
		}
		if err != nil || receipt.State != "UNKNOWN" || receipt.dispatch || receipt.Reference != key || receipt.generation != accepted.generation {
			t.Fatal("unknown read/replay renewed dispatch or altered the invocation", err)
		}
	}
	var replayVersion string
	if err := r.pool.QueryRow(ctx, `SELECT xmin::text FROM late_payment_review_cases WHERE id=$1`, item.ID).Scan(&replayVersion); err != nil || replayVersion != markerVersion {
		t.Fatal("read/replay performed a fresh physical query admission", err)
	}
	var markers int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM late_payment_query_commands WHERE case_id=$1 AND idempotency_key=$2 AND completed_at IS NULL AND result_snapshot IS NULL`, item.ID, key).Scan(&markers); err != nil || markers != 1 {
		t.Fatal("original pending identity is not exactly once", err)
	}
	var bound uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT generation FROM late_payment_query_generations WHERE idempotency_key=$1`, key).Scan(&bound); err != nil || bound != accepted.generation {
		t.Fatal("original command generation is not immutable and exactly bound", err)
	}
}
