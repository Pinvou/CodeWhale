package token

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Opt-in genuine public-migration/token_gateway test. This invokes the actual
// repository and error presenter, not the full public HTTP authentication chain
// or a downstream Billing/supplier lifecycle. The existing fixture owns cleanup.
func TestF05CancellationKeyScopePublicPostgres(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	user := uuid.New()
	first := retainedCancellationUsage(t, repository, ctx, user, uuid.New(), nil, "STREAMING")
	// The existing seed helper uses one fixed inference key. Give its first row
	// a distinct controlled invocation identity before creating a second row for
	// the same user; cancellation keys are a separate existing namespace.
	if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET idempotency_key_digest=$2,cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, first, digestString([]byte(first.String()))); err != nil {
		t.Fatal("first controlled invocation identity setup failed")
	}
	second := retainedCancellationUsage(t, repository, ctx, user, uuid.New(), nil, "STREAMING")
	if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, second); err != nil {
		t.Fatal("second controlled cancellation state setup failed")
	}
	key := digestString([]byte("controlled-cancellation-key-" + user.String()))
	initial, preDispatch, err := repository.RequestCancellation(ctx, first, user, key, cancellationFingerprint(first), "controlled-adapter-v1")
	if err != nil || preDispatch || initial.CancelStatus != "UNSUPPORTED" {
		t.Fatal("first in-flight cancellation did not retain the existing unsupported policy")
	}
	persistedUsage, err := repository.GetUsage(ctx, first)
	if err != nil {
		t.Fatal("first durable cancellation receipt unavailable")
	}
	// Compare replays to the real durable receipt; PostgreSQL timestamp
	// precision need not preserve the initial in-memory clock's nanoseconds.
	originalReceipt := cancellationSnapshot(persistedUsage)
	snapshot := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var value string
		err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'usage',to_jsonb(u),
			'commands',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM usage_cancel_commands c WHERE c.usage_request_id=u.id),
			'cancellation_evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM usage_cancellation_evidence e WHERE e.usage_request_id=u.id),
			'convergence_evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM usage_convergence_evidence e WHERE e.usage_request_id=u.id)
		)::text FROM usage_requests u WHERE u.id=$1`, id).Scan(&value)
		if err != nil {
			t.Fatal("controlled cancellation snapshot unavailable")
		}
		return value
	}
	firstFacts, secondFacts := snapshot(t, first), snapshot(t, second)
	assertUnchanged := func(t *testing.T) {
		t.Helper()
		if snapshot(t, first) != firstFacts || snapshot(t, second) != secondFacts {
			t.Error("duplicate or rejected cancellation changed complete usage facts or durable command/evidence")
		}
	}
	t.Run("cross-usage-same-user-key-conflicts-without-effect", func(t *testing.T) {
		_, _, err := repository.RequestCancellation(ctx, second, user, key, cancellationFingerprint(second), "controlled-adapter-v1")
		if !errors.Is(err, ErrIdempotencyConflict) {
			var databaseError *pgconn.PgError
			if errors.As(err, &databaseError) {
				// Safe catalog identifiers from this synthetic fixture identify the
				// exact collision. Do not print database messages/details or DSNs.
				t.Logf("actual collision SQLSTATE=%s table=%s constraint=%s", databaseError.Code, databaseError.TableName, databaseError.ConstraintName)
			}
			t.Error("cross-resource cancellation key was not classified as the existing idempotency conflict")
		}
		out := httptest.NewRecorder()
		(&Service{}).writeError(out, httptest.NewRequest(http.MethodPost, "/v1/usage-requests/"+second.String()+"/cancel", nil), err)
		var receipt struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(out.Body.Bytes(), &receipt) != nil || out.Code != http.StatusConflict || receipt.Code != "IDEMPOTENCY_CONFLICT" {
			t.Errorf("actual error presenter returned status=%d instead of409 IDEMPOTENCY_CONFLICT", out.Code)
		}
		assertUnchanged(t)
	})
	t.Run("same-resource-original-key-replays", func(t *testing.T) {
		result, local, err := repository.RequestCancellation(ctx, first, user, key, cancellationFingerprint(first), "controlled-adapter-v1")
		if err != nil || local || !reflect.DeepEqual(result, originalReceipt) {
			t.Error("original cancellation receipt did not replay")
		}
		assertUnchanged(t)
	})
	t.Run("same-resource-new-key-retains-existing-duplicate-semantics", func(t *testing.T) {
		result, local, err := repository.RequestCancellation(ctx, first, user, digestString([]byte("another-controlled-key-"+user.String())), cancellationFingerprint(first), "controlled-adapter-v1")
		if err != nil || local || !reflect.DeepEqual(result, originalReceipt) {
			t.Error("same resource duplicate behavior was changed to a new key policy")
		}
		assertUnchanged(t)
	})
	t.Run("foreign-owner-still-rejected", func(t *testing.T) {
		if _, _, err := repository.RequestCancellation(ctx, second, uuid.New(), key, cancellationFingerprint(second), "controlled-adapter-v1"); !errors.Is(err, ErrNotFound) {
			t.Error("foreign owner cancellation was not rejected before receipt lookup/write")
		}
		assertUnchanged(t)
	})
	t.Run("another-user-may-use-the-same-key", func(t *testing.T) {
		otherUser := uuid.New()
		other := retainedCancellationUsage(t, repository, ctx, otherUser, uuid.New(), nil, "STREAMING")
		if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, other); err != nil {
			t.Fatal("other user controlled state setup failed")
		}
		result, local, err := repository.RequestCancellation(ctx, other, otherUser, key, cancellationFingerprint(other), "controlled-adapter-v1")
		if err != nil || local || result.CancelStatus != "UNSUPPORTED" {
			t.Error("cancellation key namespace was incorrectly extended across users")
		}
		var count int
		if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1 AND user_id=$2 AND idempotency_key_digest=$3`, other, otherUser, key).Scan(&count); err != nil || count != 1 {
			t.Error("other user did not retain one actual cancellation command")
		}
		assertUnchanged(t)
	})
}
