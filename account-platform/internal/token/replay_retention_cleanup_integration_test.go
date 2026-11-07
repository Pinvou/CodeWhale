package token

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReplayRetentionBoundedCleanupPublicPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	clock := base
	r.now = func() time.Time { return clock }
	newStream := func() uuid.UUID {
		t.Helper()
		clock = base
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		for range replayCleanupBatchSize + 5 {
			if _, err := r.AppendReplayEvent(ctx, id, "DELTA", []byte(`{"text":"controlled retention data"}`)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.MarkReview(ctx, id, "usage_missing", nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	count := func(id uuid.UUID, live bool) int {
		t.Helper()
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_stream_events WHERE usage_request_id=$1 AND (NOT $2 OR octet_length(encrypted_payload)>0)`, id, live).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("one-large-unsettled-stream-is-row-bounded-and-retryable", func(t *testing.T) {
		id := newStream()
		before := f05CancellationRecoveryFacts(t, r, ctx, id)
		clock = base.Add(31 * 24 * time.Hour)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(id, true); n != 5 {
			t.Fatal("cleanup was not bounded by content rows", n)
		}
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(id, true); n != 0 || count(id, false) != replayCleanupBatchSize+5 {
			t.Fatal("next cycle failed or removed unresolved identity stubs", n)
		}
		if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
			t.Fatal("cleanup changed financial/identity/audit facts")
		}
	})
	t.Run("locked-oldest-is-skipped-and-next-cycle-resumes", func(t *testing.T) {
		locked, healthy := newStream(), newStream()
		clock = base.Add(31 * 24 * time.Hour)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, locked); err != nil {
			t.Fatal(err)
		}
		if err = r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		if count(locked, true) != replayCleanupBatchSize+5 || count(healthy, true) != 5 {
			t.Fatal("locked usage blocked other due content or was mutated")
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err = r.CleanupReplayRetention(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if count(locked, true) != 0 || count(healthy, true) != 0 {
			t.Fatal("unlocked backlog did not make subsequent progress")
		}
	})
	t.Run("settled-stream-delete-is-bounded", func(t *testing.T) {
		id := newStream()
		clock = base.Add(time.Hour)
		if err := r.MarkReleased(ctx, id, "controlled original release"); err != nil {
			t.Fatal(err)
		}
		before := f05CancellationRecoveryFacts(t, r, ctx, id)
		clock = clock.Add(25 * time.Hour)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(id, false); n != 5 {
			t.Fatal("terminal content deletion exceeded batch", n)
		}
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		if count(id, false) != 0 || f05CancellationRecoveryFacts(t, r, ctx, id) != before {
			t.Fatal("terminal cleanup retry changed financial facts or retained content")
		}
	})
	t.Run("active-case-parent-lock-serializes-copy-with-cleanup", func(t *testing.T) {
		id := newStream()
		clock = base.Add(29 * 24 * time.Hour)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		type result struct {
			id  uuid.UUID
			err error
		}
		done := make(chan result, 1)
		caseCtx, cancel := context.WithCancel(ctx)
		finished := make(chan struct{})
		defer func() {
			cancel()
			_ = tx.Rollback(ctx)
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("retention case fixture worker did not retire")
			}
		}()
		go func() {
			defer close(finished)
			caseID, err := r.CreateReplayRetentionCase(caseCtx, id, uuid.New(), "controlled approved case", "original replay", clock.Add(time.Hour), base.Add(90*24*time.Hour))
			done <- result{caseID, err}
		}()
		// Observe the actual row-lock wait rather than assuming a goroutine has
		// reached the copy. The caller's original context bounds this barrier.
		for {
			var waiting bool
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var got result
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		clock = base.Add(31 * 24 * time.Hour)
		if err = r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		var copied int
		if err = r.pool.QueryRow(ctx, `SELECT jsonb_array_length(encrypted_events) FROM usage_replay_case_payloads WHERE retention_case_id=$1`, got.id).Scan(&copied); err != nil || copied != replayCleanupBatchSize+5 {
			t.Fatal("controlled copy lost content during serialized cleanup", copied, err)
		}
	})
	t.Run("tombstone-key-clear-is-atomic-bounded-and-protected-rows-do-not-starve", func(t *testing.T) {
		clock = base
		protected := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		if err := r.MarkReview(ctx, protected, "usage_missing", nil); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkReleased(ctx, protected, "controlled original release"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.CreateReplayRetentionCase(ctx, protected, uuid.New(), "controlled hold", "idempotency", base.Add(time.Hour), base.Add(200*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, 0, replayCleanupBatchSize+5)
		for range replayCleanupBatchSize + 5 {
			id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
			if err := r.MarkReview(ctx, id, "usage_missing", nil); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkReleased(ctx, id, "controlled original release"); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		clock = base.Add(91 * 24 * time.Hour)
		check := func(want int) {
			t.Helper()
			var tombstones, keys, mismatch, protectedKeys int
			if err := r.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM usage_idempotency_tombstones WHERE usage_request_id=ANY($1::uuid[])),
 (SELECT count(*) FROM usage_requests WHERE id=ANY($1::uuid[]) AND idempotency_key_digest IS NOT NULL),
 (SELECT count(*) FROM usage_requests request WHERE id=ANY($1::uuid[]) AND (idempotency_key_digest IS NOT NULL) IS DISTINCT FROM EXISTS(SELECT 1 FROM usage_idempotency_tombstones tombstone WHERE tombstone.usage_request_id=request.id)),
 (SELECT count(*) FROM usage_requests WHERE id=$2 AND idempotency_key_digest IS NOT NULL)`, ids, protected).Scan(&tombstones, &keys, &mismatch, &protectedKeys); err != nil || tombstones != want || keys != want || mismatch != 0 || protectedKeys != 1 {
				t.Fatal("bounded atomic idempotency retirement/protected progress mismatch", tombstones, keys, mismatch, protectedKeys, err)
			}
		}
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		check(5)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		check(0)
	})
	t.Run("case-delete-and-payload-cascade-are-bounded", func(t *testing.T) {
		clock = base
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		ids := make([]uuid.UUID, 0, replayCleanupBatchSize+5)
		for range replayCleanupBatchSize + 5 {
			caseID, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "controlled short case", "replay", base.Add(time.Hour), base.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, caseID)
		}
		clock = base.Add(3 * time.Hour)
		check := func(want int) {
			t.Helper()
			var cases, payloads int
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_replay_retention_cases WHERE id=ANY($1::uuid[])),(SELECT count(*) FROM usage_replay_case_payloads WHERE retention_case_id=ANY($1::uuid[]))`, ids).Scan(&cases, &payloads); err != nil || cases != want || payloads != want {
				t.Fatal("case/payload batch mismatch", cases, payloads, err)
			}
		}
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		check(5)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		check(0)
	})
	t.Run("capacity-sample-10000-live-frames-plan-and-single-batch", func(t *testing.T) {
		clock = base
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		const frames = 10000
		payload := []byte(`{"text":"controlled replay capacity sample; no customer data"}`)
		for range frames {
			if _, err := r.AppendReplayEvent(ctx, id, "DELTA", payload); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.MarkReview(ctx, id, "usage_missing", nil); err != nil {
			t.Fatal(err)
		}
		clock = base.Add(31 * 24 * time.Hour)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Same selection as the unsettled-event worker, natural planner and real
		// row locks. This small single-user sample is not a production SLA.
		var plan string
		if err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON)
 SELECT content.usage_request_id,request.user_id,content.sequence FROM usage_stream_events content JOIN usage_requests request ON request.id=content.usage_request_id
 WHERE octet_length(content.encrypted_payload)>0 AND (content.expires_at<=$1 OR content.created_at<=$2) AND request.status NOT IN ('COMMITTED','RELEASED') AND `+replayCleanupCaseGuard+`
 ORDER BY content.expires_at NULLS LAST,content.created_at,content.usage_request_id,content.sequence LIMIT $3 FOR UPDATE OF request SKIP LOCKED`, clock, clock.Add(-720*time.Hour), replayCleanupBatchSize).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("capacity sample frames=%d payload_bytes=%d natural PG plan=%s", frames, len(payload), plan)
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err = r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		t.Log("capacity sample single cycle elapsed", time.Since(started))
		if n := count(id, true); n != frames-replayCleanupBatchSize {
			t.Fatal("sample cleanup exceeded/failed row bound", n)
		}
	})
	t.Run("content-lock-timeout-rolls-back-and-next-call-recovers", func(t *testing.T) {
		// Existing sample content is due too; lock every live event row so the
		// first selected update must wait even if the planner chooses another ID.
		clock = base.Add(31 * 24 * time.Hour)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT usage_request_id FROM usage_stream_events WHERE octet_length(encrypted_payload)>0 FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		var before, after int
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_stream_events WHERE octet_length(encrypted_payload)>0`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err = r.CleanupReplayRetention(ctx); err == nil {
			t.Fatal("blocked content update did not exhaust bounded transaction")
		}
		t.Log("controlled content-lock timeout elapsed", time.Since(started))
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_stream_events WHERE octet_length(encrypted_payload)>0`).Scan(&after); err != nil || after != before {
			t.Fatal("failed transaction partially cleared content", before, after, err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err = r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal("next call did not recover after lock release", err)
		}
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_stream_events WHERE octet_length(encrypted_payload)>0`).Scan(&after); err != nil || after != before-replayCleanupBatchSize {
			t.Fatal("recovered call failed bounded progress", before, after, err)
		}
	})
}
