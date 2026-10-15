package token

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// The real Billing handler commits the first extension before the controlled
// transport loses its ACK. This is not a mock financial repository or a crash
// injector: the second invocation exercises the same persisted operation tuple.
func TestF05AdditionalReservationProjectionPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	var clock atomic.Int64
	var loseACK atomic.Bool
	var calls atomic.Int64
	var operationIDs []uuid.UUID
	var mu sync.Mutex
	handler := billing.NewService(funds).Handler("controlled-extension-money")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path != "/internal/v1/holds/extend" {
			handler.ServeHTTP(w, q)
			return
		}
		calls.Add(1)
		// Read the real request without replacing its wire facts.
		raw, err := io.ReadAll(q.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var id uuid.UUID
		if err := json.Unmarshal(body["operation_id"], &id); err != nil || id == uuid.Nil {
			t.Error("missing durable operation", err)
		}
		mu.Lock()
		operationIDs = append(operationIDs, id)
		mu.Unlock()
		q.Body = io.NopCloser(bytes.NewReader(raw))
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, q)
		if loseACK.Swap(false) {
			if result.Code != http.StatusOK {
				t.Errorf("actual extension did not commit: HTTP%d %s", result.Code, result.Body.String())
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"BILLING_UNAVAILABLE"}`))
			return
		}
		for name, values := range result.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-extension-money")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, now, now, true, false)
	usage := Usage{InputTokens: 100, OutputTokens: 100}
	actual, err := Cost(c.price, usage)
	if err != nil || actual <= c.usage.MaximumMicrocredits || actual > 1000 {
		t.Fatal("fixture requires a backed positive extension", actual, err)
	}
	authorize := func() {
		t.Helper()
		maximum, notice, ok := c.service.authorizeAdditional(ctx, c.usage.ID, *c.usage.HoldID, c.price, usage, c.usage.MaximumMicrocredits)
		if !ok || maximum != actual || notice != "" {
			t.Fatal("unexpected final usage authorization", maximum, notice, ok)
		}
	}
	loseACK.Store(true)
	authorize()
	afterLost, err := r.GetUsage(ctx, c.usage.ID)
	if err != nil || afterLost.MaximumMicrocredits != c.usage.MaximumMicrocredits {
		t.Fatal("unknown ACK was projected", afterLost, err)
	}
	var holdMaximum int64
	if err := pool.QueryRow(ctx, `SELECT max_microcredits FROM usage_holds WHERE id=$1`, *c.usage.HoldID).Scan(&holdMaximum); err != nil || holdMaximum != actual {
		t.Fatal("lost ACK did not follow a real committed extension", holdMaximum, err)
	}
	money := f05DeadlineBillingFacts(t, ctx, pool)
	authorize()
	confirmed, err := r.GetUsage(ctx, c.usage.ID)
	if err != nil || confirmed.MaximumMicrocredits != actual {
		t.Fatal("confirmed target not projected", confirmed, err)
	}
	authorize()
	replay, err := r.GetUsage(ctx, c.usage.ID)
	if err != nil || replay.MaximumMicrocredits != actual || !replay.UpdatedAt.Equal(confirmed.UpdatedAt) {
		t.Fatal("replayed ACK added or rewrote local projection", replay, err)
	}
	mu.Lock()
	ids := append([]uuid.UUID(nil), operationIDs...)
	mu.Unlock()
	if calls.Load() != 3 || len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatal("logical operation identity changed", calls.Load(), ids)
	}
	f05DeadlineNoBillingChange(t, ctx, pool, money)
	if err := r.EnsureMaximumAtLeast(ctx, c.usage.ID, actual-1); err != nil {
		t.Fatal(err)
	}
	lower, err := r.GetUsage(ctx, c.usage.ID)
	if err != nil || lower.MaximumMicrocredits != actual || !lower.UpdatedAt.Equal(confirmed.UpdatedAt) {
		t.Fatal("lower confirmed target regressed or rewrote projection", lower, err)
	}
	f05DeadlineNoBillingChange(t, ctx, pool, money)
}
