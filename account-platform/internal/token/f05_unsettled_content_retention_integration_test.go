package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This is the existing isolated public-migration Token fixture. The clock is
// controlled; no deployed issuer/provider or production database is exercised.
func TestF05UnsettledContentRetention(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	clock := base
	r.now = func() time.Time { return clock }
	newContent := func(t *testing.T, legacyNull bool) (uuid.UUID, ReplayResult) {
		t.Helper()
		clock = base
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		for _, payload := range []string{`{"text":"first"}`, `{"text":"second"}`} {
			if _, err := r.AppendReplayEvent(ctx, id, "DELTA", []byte(payload)); err != nil {
				t.Fatal(err)
			}
		}
		result := ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"text":"retained response"}`), LastSequence: 2}
		if err := r.SaveReplayResult(ctx, id, result); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkReview(ctx, id, "usage_missing", map[string]any{"usage_pending": true}); err != nil {
			t.Fatal(err)
		}
		if legacyNull {
			// Model an existing NULL-expiry history without changing its genuine
			// first persistence clock or ciphertext/digest/financial facts.
			if _, err := r.pool.Exec(ctx, `UPDATE usage_stream_events SET expires_at=NULL WHERE usage_request_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE usage_replay_results SET expires_at=NULL WHERE usage_request_id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return id, result
	}
	assertNoContent := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		for _, after := range []int64{0, 1, 2} {
			snapshot, err := r.LoadReplay(ctx, id, after)
			if err != nil || snapshot.Result != nil || len(snapshot.Events) != 0 || snapshot.Usage.ExecutionStatus != "REVIEW" {
				t.Errorf("expired content replayable: after=%d result=%v events=%d expired=%t execution=%s err=%v", after, snapshot.Result != nil, len(snapshot.Events), snapshot.ResultExpired, snapshot.Usage.ExecutionStatus, err)
			}
		}
	}
	assertOriginalResultIdentity := func(t *testing.T, id uuid.UUID, result ReplayResult) {
		t.Helper()
		var created time.Time
		var digest string
		var sequence int64
		if err := r.pool.QueryRow(ctx, `SELECT created_at,evidence_payload_digest,last_sequence FROM usage_replay_results WHERE usage_request_id=$1`, id).Scan(&created, &digest, &sequence); err != nil || !created.Equal(base) || digest != replayDigest(result.Payload) || sequence != result.LastSequence {
			t.Error("first persisted result metadata lost or renewed", created, digest, sequence, err)
		}
	}

	t.Run("second-case-copies-only-actual-ciphertext", func(t *testing.T) {
		id, _ := newContent(t, true)
		clock = base.Add(24 * time.Hour)
		first, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "first controlled case", "original content", clock.Add(time.Hour), clock.Add(10*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Hour)
		second, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "second controlled case", "remaining actual content only", clock.Add(time.Hour), clock.Add(10*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var events int
		var absent bool
		if err = r.pool.QueryRow(ctx, `SELECT jsonb_array_length(encrypted_events),encrypted_result IS NULL FROM usage_replay_case_payloads WHERE retention_case_id=$1`, second).Scan(&events, &absent); err != nil || events != 0 || !absent {
			t.Fatal("second case manufactured an encrypted copy from metadata stubs", events, absent, err)
		}
		if err = r.pool.QueryRow(ctx, `SELECT jsonb_array_length(encrypted_events),encrypted_result->>'ciphertext' IS NOT NULL AND encrypted_result->>'ciphertext'<>'' FROM usage_replay_case_payloads WHERE retention_case_id=$1`, first).Scan(&events, &absent); err != nil || events != 2 || !absent {
			t.Fatal("original controlled ciphertext copy changed", events, absent, err)
		}
	})

	t.Run("empty-stub-and-unproven-save-do-not-assert-content-persistence", func(t *testing.T) {
		id, result := newContent(t, true)
		clock = base.Add(24 * time.Hour)
		if _, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "controlled case", "private copy", clock.Add(time.Hour), clock.Add(10*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		for _, days := range []int{2, 3} {
			clock = base.Add(time.Duration(days) * 24 * time.Hour)
			if err := r.SaveReplayResult(ctx, id, result); err != nil {
				t.Fatal(err)
			}
			var falseRecords int
			var first time.Time
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE sanitized_snapshot->>'content_persisted'='false'),min(observed_at) FILTER(WHERE sanitized_snapshot->>'content_persisted' IS DISTINCT FROM 'false') FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='RESULT_OBSERVED' AND decision='RECORDED'`, id).Scan(&falseRecords, &first); err != nil || falseRecords != 1 || !first.Equal(base) {
				t.Error("empty stub claimed new body persistence or moved first clock", falseRecords, first, err)
			}
			assertOriginalResultIdentity(t, id, result)
		}
		// A legacy REVIEW with no result row or immutable first-body fact must
		// never turn an observation-only retry into a new thirty-day lifetime.
		unknown := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		if err := r.MarkReview(ctx, unknown, "usage_missing", nil); err != nil {
			t.Fatal(err)
		}
		for _, days := range []int{4, 5} {
			clock = base.Add(time.Duration(days) * 24 * time.Hour)
			if err := r.SaveReplayResult(ctx, unknown, result); err != nil {
				t.Fatal(err)
			}
			var rows, firsts int
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_replay_results WHERE usage_request_id=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='RESULT_OBSERVED' AND sanitized_snapshot->>'content_persisted' IS DISTINCT FROM 'false')`, unknown).Scan(&rows, &firsts); err != nil || rows != 0 || firsts != 0 {
				t.Fatal("unproven history manufactured a first-body clock", rows, firsts, err)
			}
		}
	})

	t.Run("each-content-has-its-own-first-clock", func(t *testing.T) {
		clock = base
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		clock = base.Add(time.Hour)
		if _, err := r.AppendReplayEvent(ctx, id, "DELTA", []byte(`{"text":"early frame"}`)); err != nil {
			t.Fatal(err)
		}
		clock = base.Add(2 * time.Hour)
		if _, err := r.AppendReplayEvent(ctx, id, "DELTA", []byte(`{"text":"later frame"}`)); err != nil {
			t.Fatal(err)
		}
		clock = base.Add(3 * time.Hour)
		result := ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"text":"later response"}`), LastSequence: 2}
		if err := r.SaveReplayResult(ctx, id, result); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkReview(ctx, id, "usage_missing", map[string]any{"usage_pending": true}); err != nil {
			t.Fatal(err)
		}
		for _, step := range []struct {
			hours, events int
			result        bool
		}{{1, 1, true}, {2, 0, true}, {3, 0, false}} {
			clock = base.Add(30*24*time.Hour + time.Duration(step.hours)*time.Hour)
			if err := r.CleanupReplayRetention(ctx); err != nil {
				t.Fatal(err)
			}
			snapshot, err := r.LoadReplay(ctx, id, 0)
			if err != nil || len(snapshot.Events) != step.events || (snapshot.Result != nil) != step.result {
				t.Errorf("individual content deadline at hour%d: events=%d result=%t err=%v", step.hours, len(snapshot.Events), snapshot.Result != nil, err)
			}
			if step.events == 1 && (len(snapshot.Events) != 1 || snapshot.Events[0].Sequence != 2) {
				t.Error("early frame clock affected later frame identity")
			}
		}
	})

	t.Run("legacy-null-review-read-boundary-and-cleanup", func(t *testing.T) {
		id, _ := newContent(t, true)
		before := f05CancellationRecoveryFacts(t, r, ctx, id)
		clock = base.Add(30*24*time.Hour - time.Microsecond)
		snapshot, err := r.LoadReplay(ctx, id, 1)
		if err != nil || snapshot.Result == nil || len(snapshot.Events) != 1 || snapshot.Events[0].Sequence != 2 || snapshot.ResultExpired {
			t.Fatal("content unavailable before precise boundary", snapshot, err)
		}
		clock = base.Add(30 * 24 * time.Hour)
		assertNoContent(t, id) // Expired reads must close even before the worker runs.
		if err = r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoContent(t, id)
		var bytes int64
		if err = r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT sum(octet_length(encrypted_payload)) FROM usage_stream_events WHERE usage_request_id=$1),0)+COALESCE((SELECT sum(octet_length(encrypted_payload)) FROM usage_replay_results WHERE usage_request_id=$1),0)`, id).Scan(&bytes); err != nil || bytes != 0 {
			t.Error("cleanup retained expired encrypted content", bytes, err)
		}
		if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
			t.Error("content cleanup changed financial/identity/audit facts")
		}
	})

	t.Run("same-result-persist-does-not-renew-or-recreate", func(t *testing.T) {
		id, result := newContent(t, false)
		clock = base.Add(29 * 24 * time.Hour)
		if err := r.SaveReplayResult(ctx, id, result); err != nil {
			t.Fatal(err)
		}
		var created time.Time
		if err := r.pool.QueryRow(ctx, `SELECT created_at FROM usage_replay_results WHERE usage_request_id=$1`, id).Scan(&created); err != nil || !created.Equal(base) {
			t.Fatal("duplicate result changed first clock", created, err)
		}
		clock = base.Add(31 * 24 * time.Hour)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		// An exact repeat after cleanup must not resurrect the expired response.
		if err := r.SaveReplayResult(ctx, id, result); err != nil {
			t.Fatal("same original result changed persistence acknowledgement", err)
		}
		assertNoContent(t, id)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoContent(t, id)
	})

	t.Run("expired-review-same-key-read-never-dispatches", func(t *testing.T) {
		id, _ := newContent(t, true)
		clock = base.Add(31 * 24 * time.Hour)
		if err := r.CleanupReplayRetention(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoContent(t, id)
		usage, err := r.GetUsage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		key := usage.IdempotencyKeyDigest
		request := usage
		request.ID = uuid.New()
		existing, created, err := r.BeginUsage(ctx, request, map[string]any{"context_type": "PERSONAL"})
		if err != nil || created || existing.ID != id || existing.IdempotencyKeyDigest != key || existing.ExecutionStatus != "REVIEW" {
			t.Fatal("content expiry recycled unresolved invocation identity", existing, created, err)
		}
		service := &Service{repository: r}
		response := httptest.NewRecorder()
		service.replayUsage(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)), existing, []byte(`{}`))
		if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), "USAGE_REVIEW") || !strings.Contains(response.Body.String(), "/v1/usage-requests/"+id.String()) {
			t.Error("expired REVIEW response did not preserve status-query recovery", response.Code, response.Body.String())
		}
		var attempts, intents int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_attempts WHERE usage_request_id=$1),(SELECT count(*) FROM usage_dispatch_intents WHERE usage_request_id=$1)`, id).Scan(&attempts, &intents); err != nil || attempts != 0 || intents != 0 {
			t.Fatal("expired replay created dispatch/attempt", attempts, intents, err)
		}
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != "REVIEW" || current.IdempotencyKeyDigest != key {
			t.Fatal("financial status/key no longer queryable", current, err)
		}
	})

	for _, release := range []bool{false, true} {
		name := "controlled-case-expires-with-copy"
		if release {
			name = "controlled-case-release-clears-copy-after-30d"
		}
		t.Run(name, func(t *testing.T) {
			id, result := newContent(t, true)
			clock = base.Add(29 * 24 * time.Hour)
			caseID, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "controlled approved retention", "original encrypted content", clock.Add(time.Hour), base.Add(90*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			// Exact persistence re-entry cannot republish the original response
			// that the controlled case has moved out of ordinary replay.
			if err = r.SaveReplayResult(ctx, id, result); err != nil {
				t.Fatal(err)
			}
			assertOriginalResultIdentity(t, id, result)
			caseReplay, err := r.LoadReplay(ctx, id, 0)
			if err != nil || caseReplay.Result != nil || len(caseReplay.Events) != 0 {
				t.Error("same result republished controlled case content", caseReplay.Result != nil, len(caseReplay.Events), err)
			}
			clock = base.Add(31 * 24 * time.Hour)
			if err = r.CleanupReplayRetention(ctx); err != nil {
				t.Fatal(err)
			}
			var copied int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_case_payloads WHERE retention_case_id=$1 AND jsonb_array_length(encrypted_events)=2 AND encrypted_result IS NOT NULL`, caseID).Scan(&copied); err != nil || copied != 1 {
				t.Fatal("controlled case lost original encrypted copy", copied, err)
			}
			// The copied controlled evidence is never reintroduced into public replay.
			snapshot, err := r.LoadReplay(ctx, id, 0)
			if err != nil || snapshot.Result != nil || len(snapshot.Events) != 0 {
				t.Fatal("controlled copy became ordinary response content", snapshot, err)
			}
			deadline := base.Add(90 * 24 * time.Hour)
			if release {
				if err = r.ReleaseReplayRetentionCase(ctx, caseID); err != nil {
					t.Fatal(err)
				}
				deadline = clock.Add(30 * 24 * time.Hour)
			}
			clock = deadline.Add(-time.Microsecond)
			if err = r.CleanupReplayRetention(ctx); err != nil {
				t.Fatal(err)
			}
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_case_payloads WHERE retention_case_id=$1`, caseID).Scan(&copied); err != nil || copied != 1 {
				t.Fatal("controlled copy cleared before existing deadline", copied, err)
			}
			clock = deadline
			if err = r.CleanupReplayRetention(ctx); err != nil {
				t.Fatal(err)
			}
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_case_payloads WHERE retention_case_id=$1`, caseID).Scan(&copied); err != nil || copied != 0 {
				t.Fatal("case deadline did not clear encrypted copy", copied, err)
			}
			if err = r.SaveReplayResult(ctx, id, result); err != nil {
				t.Fatal(err)
			}
			assertNoContent(t, id)
			assertOriginalResultIdentity(t, id, result)
		})
	}

	t.Run("settled-stream24h-result30d-still-exact", func(t *testing.T) {
		id, _ := newContent(t, true)
		clock = base.Add(time.Hour)
		if err := r.MarkReleased(ctx, id, "controlled existing release receipt"); err != nil {
			t.Fatal(err)
		}
		terminal := clock
		clock = terminal.Add(24*time.Hour - time.Microsecond)
		snapshot, err := r.LoadReplay(ctx, id, 0)
		if err != nil || len(snapshot.Events) != 2 || snapshot.Result == nil {
			t.Fatal("settled content expired before old boundary", snapshot, err)
		}
		clock = terminal.Add(24 * time.Hour)
		snapshot, err = r.LoadReplay(ctx, id, 0)
		if err != nil || len(snapshot.Events) != 0 || snapshot.Result == nil {
			t.Fatal("settled stream24h/result30d changed", snapshot, err)
		}
		clock = terminal.Add(30 * 24 * time.Hour)
		snapshot, err = r.LoadReplay(ctx, id, 0)
		if err != nil || len(snapshot.Events) != 0 || snapshot.Result != nil || !snapshot.ResultExpired {
			t.Fatal("settled result30d changed", snapshot, err)
		}
	})
}
