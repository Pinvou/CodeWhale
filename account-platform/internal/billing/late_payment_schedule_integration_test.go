package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Identity and Account authority are controlled producers. Registered middleware,
// repository, migration, transaction and receipt reads use production code/SQL.
func TestLatePaymentScheduleCommandReplayPostgres(t *testing.T) {
	// PostgreSQL scans use time.Local, while JSON Z timestamps use time.UTC.
	// Keep this same-instant representation boundary covered on every host.
	local := time.Local
	time.Local = time.FixedZone("receipt-test-UTC", 0)
	t.Cleanup(func() { time.Local = local })
	r, ctx := retainedFinanceRepository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.now = func() time.Time { return now }
	actor := uuid.New()
	fixture := func(t *testing.T) LatePaymentReviewCase {
		t.Helper()
		_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		item, err := r.EscalateLatePaymentReview(ctx, order.ID, "PENDING")
		if err != nil {
			t.Fatal(err)
		}
		item, err = r.ClaimLatePaymentReview(ctx, item.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	var allowed atomic.Bool
	allowed.Store(true)
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Subject    string `json:"subject"`
			Permission string `json:"permission"`
		}
		if request.Method != "POST" || request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Error("invalid authority request")
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed.Load() && body.Permission == "platform.billing.orders.sync", "user_id": body.Subject})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewService(r).WithAuthorizer(authorizer).Handler("controlled-service", func(_ context.Context, token string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: r.now()}, nil
	})
	var lastCallJSON []byte
	call := func(t *testing.T, item LatePaymentReviewCase, token, key string, next time.Time, reason string, want int) LatePaymentReviewCase {
		t.Helper()
		body, err := json.Marshal(map[string]any{"command_protocol": 1, "next_review_at": next, "reason": reason})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+item.ID.String()+"/schedule", strings.NewReader(string(body)))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("schedule got %d want %d: %s", response.Code, want, response.Body.String())
		}
		lastCallJSON = append([]byte(nil), response.Body.Bytes()...)
		var result LatePaymentReviewCase
		if want == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	facts := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var value string
		err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('late',to_jsonb(late),'manual',to_jsonb(manual),'events',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM manual_review_case_events e WHERE e.case_id=late.id),'commands',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM late_payment_review_schedule_commands c WHERE c.case_id=late.id))::text FROM late_payment_review_cases late JOIN manual_review_cases manual ON manual.id=late.id WHERE late.id=$1`, id).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	const reason = "controlled next payment review"
	t.Run("lost-ack-new-session-plan-original-receipt", func(t *testing.T) {
		item := fixture(t)
		keyA, keyB := "schedule-"+uuid.NewString(), "schedule-"+uuid.NewString()
		a, b := now.Add(time.Hour+123*time.Nanosecond), now.Add(2*time.Hour)
		// The first reply is captured for comparison but treated as lost by the caller.
		original := call(t, item, actor.String(), keyA, a, reason, 200)
		later := call(t, item, actor.String(), keyB, b, "another session chose a later review", 200)
		before := facts(t, item.ID)
		replayed := call(t, item, actor.String(), keyA, a, reason, 200)
		if !reflect.DeepEqual(replayed, original) || before != facts(t, item.ID) {
			t.Fatal("original replay changed the later plan or immutable receipt")
		}
		if !original.NextReviewAt.Equal(a.Truncate(time.Microsecond)) || !later.NextReviewAt.Equal(b) {
			t.Fatal("receipt does not reflect stored PostgreSQL precision")
		}
		var eventCount, commandCount int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='REVIEW_SCHEDULED'),(SELECT count(*) FROM late_payment_review_schedule_commands WHERE case_id=$1)`, item.ID).Scan(&eventCount, &commandCount); err != nil || eventCount != 2 || commandCount != 2 {
			t.Fatal(eventCount, commandCount, err)
		}
		var protocol int
		if err := r.pool.QueryRow(ctx, `SELECT (request_snapshot->>'command_protocol')::int FROM late_payment_review_schedule_commands WHERE idempotency_key=$1`, keyA).Scan(&protocol); err != nil || protocol != 1 {
			t.Fatal("schedule request identity lacks explicit protocol", protocol, err)
		}
		// Same instant in another zone is the same command; a sub-microsecond
		// changed instant is not, even though PostgreSQL stores equal microseconds.
		zone := time.FixedZone("controlled", 9*60*60)
		if zoned := call(t, item, actor.String(), keyA, a.In(zone), "  "+reason+"  ", 200); !reflect.DeepEqual(zoned, original) {
			t.Fatal("canonical instant/reason changed receipt")
		}
		call(t, item, actor.String(), keyA, a.Add(time.Nanosecond), reason, 409)
		call(t, item, actor.String(), keyA, a, reason+" changed", 409)
		call(t, item, uuid.NewString(), keyA, a, reason, 409)
		other := fixture(t)
		otherBefore := facts(t, other.ID)
		call(t, other, actor.String(), keyA, a, reason, 409)
		if before != facts(t, item.ID) || otherBefore != facts(t, other.ID) {
			t.Fatal("changed facts mutated a case")
		}
		now = now.Add(3 * time.Hour)
		defer func() { now = now.Add(-3 * time.Hour) }()
		if expired := call(t, item, actor.String(), keyA, a, reason, 200); !reflect.DeepEqual(expired, original) {
			t.Fatal("past original time prevented recovery")
		}
		call(t, item, actor.String(), "schedule-"+uuid.NewString(), a, reason, 409)
		if before != facts(t, item.ID) {
			t.Fatal("past-time recovery wrote a new plan")
		}
	})
	t.Run("current-auth-owner-and-terminal-receipt-recovery", func(t *testing.T) {
		defer allowed.Store(true)
		item := fixture(t)
		key := "schedule-" + uuid.NewString()
		next := now.Add(time.Hour)
		original := call(t, item, actor.String(), key, next, reason, 200)
		before := facts(t, item.ID)
		allowed.Store(false)
		call(t, item, actor.String(), key, next, reason, 403)
		allowed.Store(true)
		call(t, item, "", key, next, reason, 401)
		if before != facts(t, item.ID) {
			t.Fatal("authorization denial wrote facts")
		}
		otherOwner := uuid.New()
		for _, newOwner := range []*uuid.UUID{nil, &otherOwner} {
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=CASE WHEN $2::uuid IS NULL THEN NULL ELSE claimed_at END WHERE id=$1`, item.ID, newOwner); err != nil {
				t.Fatal(err)
			}
			before = facts(t, item.ID)
			call(t, item, actor.String(), key, next, reason, 409)
			if newOwner != nil {
				call(t, item, newOwner.String(), key, next, reason, 409)
			}
			if before != facts(t, item.ID) {
				t.Fatal("current guard denial changed facts")
			}
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=$3 WHERE id=$1`, item.ID, actor, item.ClaimedAt); err != nil {
				t.Fatal(err)
			}
		}
		// A committed response is recovered after automatic resolution/archive;
		// terminal admission blocks only new writes, never the exact original ACK.
		for _, status := range []string{"RESOLVED_PAID", "ARCHIVED_CONFIRMED_UNPAID"} {
			if _, err := r.pool.Exec(ctx, `UPDATE late_payment_review_cases SET review_status=$2,resolved_at=$3 WHERE id=$1`, item.ID, status, now); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2 WHERE id=$1`, item.ID, now); err != nil {
				t.Fatal(err)
			}
			before = facts(t, item.ID)
			if recovered := call(t, item, actor.String(), key, next, reason, 200); !reflect.DeepEqual(recovered, original) {
				t.Fatal("terminal receipt lost original response")
			}
			call(t, item, actor.String(), "schedule-"+uuid.NewString(), next, reason, 409)
			allowed.Store(false)
			call(t, item, actor.String(), key, next, reason, 403)
			allowed.Store(true)
			if before != facts(t, item.ID) {
				t.Fatal("terminal recovery changed terminal facts")
			}
		}
		var receipt []byte
		if err := r.pool.QueryRow(ctx, `SELECT after_snapshot FROM late_payment_review_schedule_commands WHERE idempotency_key=$1`, key).Scan(&receipt); err != nil {
			t.Fatal(err)
		}
		var stored, expected any
		originalJSON, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(receipt, &stored); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(originalJSON, &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored, expected) {
			t.Fatal("stored receipt changed")
		}
	})
	t.Run("key-admission-and-new-time-bounds", func(t *testing.T) {
		item := fixture(t)
		before := facts(t, item.ID)
		for _, protocol := range []any{nil, 0, 2, "1", true} {
			body := map[string]any{"next_review_at": now.Add(time.Hour), "reason": reason}
			if protocol != nil {
				body["command_protocol"] = protocol
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+item.ID.String()+"/schedule", strings.NewReader(string(raw)))
			request.Header.Set("Authorization", "Bearer "+actor.String())
			request.Header.Set("Idempotency-Key", "schedule-"+uuid.NewString())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 400 {
				t.Fatal("invalid/missing schedule protocol dispatched", protocol, response.Code, response.Body.String())
			}
		}
		for _, key := range []string{"", "short", strings.Repeat("k", 121)} {
			call(t, item, actor.String(), key, now.Add(time.Hour), reason, 400)
		}
		for _, next := range []time.Time{now, now.Add(-time.Hour), now.Add(8*24*time.Hour + time.Microsecond)} {
			call(t, item, actor.String(), "schedule-"+uuid.NewString(), next, reason, 409)
		}
		if before != facts(t, item.ID) {
			t.Fatal("invalid admission wrote facts")
		}
	})
	t.Run("receipt-write-failure-rolls-back-all-effects", func(t *testing.T) {
		item := fixture(t)
		before := facts(t, item.ID)
		key := "schedule-" + uuid.NewString()
		next := now.Add(time.Hour)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION reject_schedule_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled schedule receipt failure'; END $$; CREATE TRIGGER reject_schedule_fixture BEFORE INSERT ON late_payment_review_schedule_commands FOR EACH ROW EXECUTE FUNCTION reject_schedule_fixture()`); err != nil {
			t.Fatal(err)
		}
		call(t, item, actor.String(), key, next, reason, 500)
		if before != facts(t, item.ID) {
			t.Fatal("receipt failure committed partial schedule")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER reject_schedule_fixture ON late_payment_review_schedule_commands; DROP FUNCTION reject_schedule_fixture()`); err != nil {
			t.Fatal(err)
		}
		call(t, item, actor.String(), key, next, reason, 200)
	})
	t.Run("concurrent-original-invocations-share-one-receipt", func(t *testing.T) {
		item := fixture(t)
		key, next := "schedule-"+uuid.NewString(), now.Add(time.Hour)
		type result struct {
			item LatePaymentReviewCase
			err  error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for range 2 {
			go func() {
				<-start
				value, err := r.ScheduleLatePaymentReview(ctx, item.ID, actor, next, reason, key)
				results <- result{value, err}
			}()
		}
		close(start)
		a, b := <-results, <-results
		aJSON, aJSONErr := json.Marshal(a.item)
		bJSON, bJSONErr := json.Marshal(b.item)
		if a.err != nil || b.err != nil || aJSONErr != nil || bJSONErr != nil || string(aJSON) != string(bJSON) {
			t.Fatal("concurrent schedule receipts differ", a.err, b.err)
		}

		// Both concurrent values must be the complete persisted original receipt,
		// and later repository/HTTP recovery must return that same invocation.
		var storedJSON []byte
		if err := r.pool.QueryRow(ctx, `SELECT after_snapshot FROM late_payment_review_schedule_commands WHERE case_id=$1 AND idempotency_key=$2`, item.ID, key).Scan(&storedJSON); err != nil {
			t.Fatal(err)
		}
		var stored LatePaymentReviewCase
		if err := json.Unmarshal(storedJSON, &stored); err != nil {
			t.Fatal(err)
		}
		storedWire, err := json.Marshal(stored)
		if err != nil || string(storedWire) != string(aJSON) {
			t.Fatal("concurrent receipt differs from persisted original", err)
		}
		replayed, err := r.ScheduleLatePaymentReview(ctx, item.ID, actor, next, reason, key)
		if err != nil {
			t.Fatal(err)
		}
		replayedWire, err := json.Marshal(replayed)
		if err != nil || string(replayedWire) != string(aJSON) {
			t.Fatal("later same-key repository recovery changed original receipt", err)
		}
		call(t, item, actor.String(), key, next, reason, 200)
		// Admin HTTP intentionally stringifies exact financial integers. Compare
		// its whole body with that production wire projection, without float64.
		wantHTTP, err := adminFinancialSnapshot(aJSON)
		if err != nil || strings.TrimSpace(string(lastCallJSON)) != string(wantHTTP) {
			t.Fatal("HTTP recovery changed the complete original receipt wire", err)
		}

		var commands, events int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM late_payment_review_schedule_commands WHERE case_id=$1),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='REVIEW_SCHEDULED')`, item.ID).Scan(&commands, &events); err != nil || commands != 1 || events != 1 {
			t.Fatal(commands, events, err)
		}
	})
	t.Run("receipts-have-no-mutation-or-truncate-path", func(t *testing.T) {
		item := fixture(t)
		key := "schedule-" + uuid.NewString()
		call(t, item, actor.String(), key, now.Add(time.Hour), reason, 200)
		before := facts(t, item.ID)
		for _, sql := range []string{`UPDATE late_payment_review_schedule_commands SET request_hash=repeat('0',64) WHERE idempotency_key=$1`, `DELETE FROM late_payment_review_schedule_commands WHERE idempotency_key=$1`} {
			if _, err := r.pool.Exec(ctx, sql, key); err == nil {
				t.Fatal("receipt mutation accepted")
			}
		}
		if _, err := r.pool.Exec(ctx, `TRUNCATE late_payment_review_schedule_commands`); err == nil {
			t.Fatal("receipt truncate accepted")
		}
		down, err := os.ReadFile("../../migrations/billing/900027_late_payment_schedule_commands.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, string(down)); err == nil {
			t.Fatal("receipt-bearing migration downgrade accepted")
		}
		if before != facts(t, item.ID) {
			t.Fatal("immutable receipt changed")
		}
	})
	t.Run("repository-rejects-missing-identity-before-sql", func(t *testing.T) {
		for _, ids := range [][2]uuid.UUID{{uuid.Nil, actor}, {uuid.New(), uuid.Nil}} {
			if _, err := r.ScheduleLatePaymentReview(ctx, ids[0], ids[1], now.Add(time.Hour), reason, "schedule-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		}
	})
}
