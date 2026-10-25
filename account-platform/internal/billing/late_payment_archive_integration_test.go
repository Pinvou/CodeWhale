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
	"github.com/jackc/pgx/v5/pgconn"
)

// Authority facts and identity producers are controlled fixtures. Archive's
// public route, guards, SQL, receipt and financial non-effects are production.
// Existing real SyncOrder/authority scenarios run separately without weakening.
func TestLatePaymentArchiveCommandReplayPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.now = func() time.Time { return now }
	actor := uuid.New()
	fixture := func(t *testing.T) LatePaymentReviewCase {
		t.Helper()
		_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		item, err := r.EscalateLatePaymentReview(ctx, order.ID, "CLOSED")
		if err != nil {
			t.Fatal(err)
		}
		item, err = r.ClaimLatePaymentReview(ctx, item.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		// Controlled historical authority facts, exactly the original archive
		// predicate. Supplier-query producer contracts are covered by old roots.
		if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET created_at=$2::timestamptz-interval '10 days',expires_at=$2::timestamptz-interval '9 days',provider_payable_until=$2::timestamptz-interval '9 days',late_review_boundary_at=$2::timestamptz-interval '8 days',final_archive_eligible_at=$2::timestamptz-interval '1 hour',last_successful_query_at=$2,last_authoritative_unpaid_at=$2 WHERE id=$1`, order.ID, now); err != nil {
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
			t.Error("invalid authority call")
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
	call := func(t *testing.T, item LatePaymentReviewCase, token, key string, want int) LatePaymentReviewCase {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+item.ID.String()+"/archive-command", strings.NewReader("{}"))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("archive got %d want %d: %s", response.Code, want, response.Body.String())
		}
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
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('order',to_jsonb(payment),'late',to_jsonb(late),'manual',to_jsonb(manual),'events',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM manual_review_case_events e WHERE e.case_id=late.id),'commands',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM late_payment_review_archive_commands c WHERE c.case_id=late.id))::text FROM late_payment_review_cases late JOIN manual_review_cases manual ON manual.id=late.id JOIN payment_orders payment ON payment.id=late.payment_order_id WHERE late.id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("lost-ack-recovers-exact-original-terminal-receipt", func(t *testing.T) {
		item := fixture(t)
		key := "archive-" + uuid.NewString()
		financial := instructionCommandFactsSnapshot(t, r, ctx)
		now = now.Add(123 * time.Nanosecond)
		defer func() { now = now.Truncate(time.Microsecond) }()
		original := call(t, item, actor.String(), key, 200) // caller loses this ACK
		now = now.Add(time.Hour)
		defer func() { now = now.Add(-time.Hour) }()
		// Controlled later stored observation distinguishes an exact invocation's
		// receipt from a freshly reconstructed view of the current terminal case.
		if _, err := r.pool.Exec(ctx, `UPDATE late_payment_review_cases SET evidence_summary='{"controlled_later_observation":true}',updated_at=$2 WHERE id=$1`, item.ID, now); err != nil {
			t.Fatal(err)
		}
		before := facts(t, item.ID)
		replay := call(t, item, actor.String(), key, 200)
		if !reflect.DeepEqual(replay, original) {
			t.Fatal("lost archive acknowledgement did not recover its exact original receipt")
		}
		if original.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" || original.ResolvedAt == nil || !original.ResolvedAt.Equal(now.Add(-time.Hour).Truncate(time.Microsecond)) || !original.UpdatedAt.Equal(now.Add(-time.Hour).Truncate(time.Microsecond)) {
			t.Fatal("archive receipt must reflect stored precision and original terminal outcome")
		}
		call(t, item, actor.String(), "archive-"+uuid.NewString(), 409)
		if before != facts(t, item.ID) || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
			t.Fatal("archive recovery repeated state/event/financial effects")
		}
		var commands, events int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM late_payment_review_archive_commands WHERE case_id=$1),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='ARCHIVED_CONFIRMED_UNPAID')`, item.ID).Scan(&commands, &events); err != nil || commands != 1 || events != 1 {
			t.Fatal(commands, events, err)
		}
	})
	t.Run("current-auth-owner-and-changed-case-identity", func(t *testing.T) {
		defer allowed.Store(true)
		item := fixture(t)
		key := "archive-" + uuid.NewString()
		original := call(t, item, actor.String(), key, 200)
		before := facts(t, item.ID)
		allowed.Store(false)
		call(t, item, actor.String(), key, 403)
		allowed.Store(true)
		call(t, item, "", key, 401)
		call(t, item, uuid.NewString(), key, 409)
		if before != facts(t, item.ID) {
			t.Fatal("denied replay mutated facts")
		}
		otherOwner := uuid.New()
		for _, owner := range []*uuid.UUID{nil, &otherOwner} {
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=CASE WHEN $2::uuid IS NULL THEN NULL ELSE claimed_at END WHERE id=$1`, item.ID, owner); err != nil {
				t.Fatal(err)
			}
			before = facts(t, item.ID)
			call(t, item, actor.String(), key, 409)
			if owner != nil {
				call(t, item, owner.String(), key, 409)
			}
			if before != facts(t, item.ID) {
				t.Fatal("ownership denial changed receipt/current state")
			}
			if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2,claimed_at=$3 WHERE id=$1`, item.ID, actor, item.ClaimedAt); err != nil {
				t.Fatal(err)
			}
		}
		if replay := call(t, item, actor.String(), key, 200); !reflect.DeepEqual(replay, original) {
			t.Fatal("restored current authority cannot confirm own receipt")
		}
		other := fixture(t)
		otherBefore := facts(t, other.ID)
		call(t, other, actor.String(), key, 409)
		if otherBefore != facts(t, other.ID) {
			t.Fatal("same key changed case")
		}
	})
	t.Run("no-receipt-terminal-and-provider-blind-are-not-success", func(t *testing.T) {
		for _, status := range []string{"ARCHIVED_CONFIRMED_UNPAID", "RESOLVED_PAID", "PROVIDER_BLIND"} {
			item := fixture(t)
			if _, err := r.pool.Exec(ctx, `UPDATE late_payment_review_cases SET review_status=$2 WHERE id=$1`, item.ID, status); err != nil {
				t.Fatal(err)
			}
			before := facts(t, item.ID)
			call(t, item, actor.String(), "archive-"+uuid.NewString(), 409)
			if before != facts(t, item.ID) {
				t.Fatal("unproven current state treated as original receipt")
			}
		}
	})
	t.Run("new-command-retains-original-authority-and-claim-admission", func(t *testing.T) {
		for _, guard := range []string{"before-boundary", "stale-query", "absent-authority", "unreviewed-claim"} {
			item := fixture(t)
			switch guard {
			case "before-boundary":
				_, err = r.pool.Exec(ctx, `UPDATE payment_orders SET final_archive_eligible_at=$2 WHERE id=$1`, item.PaymentOrderID, now.Add(time.Hour))
			case "stale-query":
				_, err = r.pool.Exec(ctx, `UPDATE payment_orders SET last_successful_query_at=final_archive_eligible_at-interval '1 microsecond' WHERE id=$1`, item.PaymentOrderID)
			case "absent-authority":
				_, err = r.pool.Exec(ctx, `UPDATE payment_orders SET last_authoritative_unpaid_at=NULL WHERE id=$1`, item.PaymentOrderID)
			case "unreviewed-claim":
				_, err = r.pool.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'USER_PAYMENT_CLAIM',repeat('b',64),'{}',$3)`, uuid.New(), item.ID, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := facts(t, item.ID)
			call(t, item, actor.String(), "archive-"+uuid.NewString(), 409)
			if before != facts(t, item.ID) {
				t.Fatal("failed original archive admission wrote facts", guard)
			}
		}
	})
	t.Run("receipt-insert-failure-rolls-back-original-archive", func(t *testing.T) {
		item := fixture(t)
		key := "archive-" + uuid.NewString()
		before := facts(t, item.ID)
		financial := instructionCommandFactsSnapshot(t, r, ctx)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION reject_archive_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled archive receipt failure'; END $$; CREATE TRIGGER reject_archive_fixture BEFORE INSERT ON late_payment_review_archive_commands FOR EACH ROW EXECUTE FUNCTION reject_archive_fixture()`); err != nil {
			t.Fatal(err)
		}
		call(t, item, actor.String(), key, 500)
		if before != facts(t, item.ID) || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
			t.Fatal("receipt failure left partial archive effects")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER reject_archive_fixture ON late_payment_review_archive_commands; DROP FUNCTION reject_archive_fixture()`); err != nil {
			t.Fatal(err)
		}
		call(t, item, actor.String(), key, 200)
	})
	t.Run("concurrent-same-key-archives-once", func(t *testing.T) {
		item := fixture(t)
		key := "archive-" + uuid.NewString()
		type result struct {
			item LatePaymentReviewCase
			err  error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for range 2 {
			go func() {
				<-start
				value, err := r.ArchiveLatePaymentReview(ctx, item.ID, actor, key)
				results <- result{value, err}
			}()
		}
		close(start)
		a, b := <-results, <-results
		if a.err != nil || b.err != nil || !reflect.DeepEqual(a.item, b.item) {
			t.Fatal("concurrent archive has no common original receipt", a.err, b.err)
		}
		var commands, events int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM late_payment_review_archive_commands WHERE case_id=$1),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='ARCHIVED_CONFIRMED_UNPAID')`, item.ID).Scan(&commands, &events); err != nil || commands != 1 || events != 1 {
			t.Fatal(commands, events, err)
		}
	})
	t.Run("key-admission-and-permanent-immutable-receipt", func(t *testing.T) {
		item := fixture(t)
		before := facts(t, item.ID)
		legacyRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+item.ID.String()+"/archive", strings.NewReader("{}"))
		legacyRequest.Header.Set("Authorization", "Bearer "+actor.String())
		legacyRequest.Header.Set("Idempotency-Key", "archive-"+uuid.NewString())
		legacyResponse := httptest.NewRecorder()
		handler.ServeHTTP(legacyResponse, legacyRequest)
		if legacyResponse.Code != 404 {
			t.Fatal("legacy archive path remains a write fallback", legacyResponse.Code, legacyResponse.Body.String())
		}
		for _, key := range []string{"", "short", strings.Repeat("k", 121)} {
			call(t, item, actor.String(), key, 400)
		}
		if before != facts(t, item.ID) {
			t.Fatal("invalid key dispatched archive")
		}
		key := "archive-" + uuid.NewString()
		call(t, item, actor.String(), key, 200)
		before = facts(t, item.ID)
		for _, sql := range []string{`UPDATE late_payment_review_archive_commands SET request_hash=repeat('0',64) WHERE idempotency_key=$1`, `DELETE FROM late_payment_review_archive_commands WHERE idempotency_key=$1`} {
			if _, err := r.pool.Exec(ctx, sql, key); err == nil {
				t.Fatal("immutable archive receipt changed")
			}
		}
		if _, err := r.pool.Exec(ctx, `TRUNCATE late_payment_review_archive_commands`); err == nil {
			t.Fatal("archive receipt truncated")
		}
		migrationFacts := func() string {
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text) FROM schema_migrations item),'marker',to_regclass('public.late_payment_review_archive_commands')::text)::text`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		beforeMigration := migrationFacts()
		down, err := os.ReadFile("../../migrations/billing/900029_late_payment_archive_commands.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.pool.Exec(ctx, string(down))
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "P0001" || rejected.Message != "late payment archive commands and receipts are forward-only" {
			t.Fatal("archive direct down did not hit its exact retained SQL refusal", err)
		}
		if beforeMigration != migrationFacts() {
			t.Fatal("archive rejected down changed ledger or marker")
		}
		if before != facts(t, item.ID) {
			t.Fatal("archive receipt/history changed")
		}
	})
}
