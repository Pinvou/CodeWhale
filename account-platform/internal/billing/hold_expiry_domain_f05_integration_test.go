package billing

import (
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedFinanceRepository(t)
	w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	// Fund the controlled package before the skewed clock used below, so its
	// normal availability checks do not mask the database renewal fence.
	r.now = func() time.Time { return time.Now().UTC().Add(-3 * time.Minute) }
	if _, err = r.Credit(ctx, w.ID, "f05-expiry-"+uuid.NewString(), 1000, false); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Now().UTC() }
	usage := uuid.New()
	h, err := r.Reserve(ctx, usage, w.ID, 100, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ExtendHold(ctx, usage, h.ID, time.Now().Add(time.Hour), 50); !errors.Is(err, ErrConflict) {
		t.Fatal("expired domain renewal not conflict", err)
	}
	// A skewed application clock must not hide the database expiry fence.
	// The rejected transaction includes component and wallet changes, all of
	// which must roll back, and the real HTTP response must remain a domain 409.
	r.now = func() time.Time { return time.Now().UTC().Add(-2 * time.Minute) }
	s := NewService(r)
	s.now = r.now
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/holds/extend", strings.NewReader(`{"usage_request_id":"`+usage.String()+`","hold_id":"`+h.ID.String()+`","lease_seconds":900,"additional_microcredits":50}`))
	req.Header.Set("Authorization", "Bearer controlled-expiry-service")
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	s.Handler("controlled-expiry-service").ServeHTTP(out, req)
	if out.Code != 409 || !strings.Contains(out.Body.String(), "BILLING_CONFLICT") {
		t.Fatal("database fence leaked as internal error", out.Code, out.Body.String())
	}
	r.now = func() time.Time { return time.Now().UTC() }
	if count, err := r.ExpireHolds(ctx, 100); count != 0 || !errors.Is(err, ErrHoldExpiryProofRequired) {
		t.Fatal("legacy expiry not fail closed", count, err)
	}
	if err = NewExpiredHoldRecoveryWorker(r, nil, time.Minute).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var reserved, maximum int64
	var status, state string
	var events int
	if err = r.pool.QueryRow(ctx, `SELECT w.reserved_microcredits,h.max_microcredits,h.status,x.state,(SELECT count(*) FROM usage_hold_lease_events e WHERE e.hold_id=h.id AND e.event_type='EXTENDED') FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id WHERE h.id=$1`, h.ID).Scan(&reserved, &maximum, &status, &state, &events); err != nil || reserved != 100 || maximum != 100 || status != "ACTIVE" || state != "MANUAL_REVIEW" || events != 0 {
		t.Fatal("unproven expiry changed funds or lease", reserved, maximum, status, state, events, err)
	}
	var componentReserved int64
	if err = r.pool.QueryRow(ctx, `SELECT COALESCE(sum(c.reserved_microcredits),0) FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=$1`, w.ID).Scan(&componentReserved); err != nil || componentReserved != 100 {
		t.Fatal("failed renewal changed component reservation", componentReserved, err)
	}
}
