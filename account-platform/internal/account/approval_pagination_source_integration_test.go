package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestApprovalPaginationSourcePostgres(t *testing.T) {
	r, f, _ := approvalListPoolFixture(t)
	ctx := f.ctx
	pageConfig := ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "source-test", CurrentKey: []byte(strings.Repeat("s", 32))}
	if err := r.ConfigureApprovalPagination(ctx, pageConfig); err != nil {
		t.Fatal(err)
	}
	if err := r.SetApprovalPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	// A second genuine repository and pool share the same published database.
	replica, err := NewPostgresRepository(ctx, r.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)
	replica.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
	if err := replica.ConfigureApprovalPagination(ctx, pageConfig); err != nil {
		t.Fatal(err)
	}
	first := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "source-first")
	second := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "source-second")
	principals := map[string]identity.Principal{"first": entitlementApprovalPrincipal(first), "second": entitlementApprovalPrincipal(second)}
	verify := func(_ context.Context, token string) (identity.Principal, error) {
		p, ok := principals[token]
		if !ok {
			return identity.Principal{}, errors.New("controlled missing identity")
		}
		return p, nil
	}
	cfg := config.Config{ServiceName: "account-source-pg", Environment: "test", RateLimitRPS: 100000, RateLimitBurst: 1000000, HTTPTrustedProxyIPs: []netip.Addr{netip.MustParseAddr("192.0.2.10")}}
	handlers := []http.Handler{
		httpserver.HandlerWithApplication(cfg, NewService(r, nil).Handler(verify), nil),
		httpserver.HandlerWithApplication(cfg, NewService(replica, nil).Handler(verify), nil),
	}
	request := func(instance int, actor, source string, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/admin/v2/approval-requests?limit=1", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("Authorization", "Bearer "+actor)
		req.Header.Set(httpserver.RateLimitSourceHeader, source)
		req.Header.Set("X-Forwarded-For", "203.0.113.99")
		response := httptest.NewRecorder()
		handlers[instance].ServeHTTP(response, req)
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("instance=%d actor=%s code=%d body=%s", instance, actor, response.Code, response.Body.String())
		}
		if want == 200 {
			var page ApprovalV2Page
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Limit != 1 || len(page.Items) != 0 || page.HasMore {
				t.Fatal("registered empty page projection changed")
			}
		} else {
			retry, err := strconv.Atoi(response.Header().Get("Retry-After"))
			if err != nil || retry < 1 || retry > 60 || !strings.Contains(response.Body.String(), `"code":"RATE_LIMITED"`) {
				t.Fatal("persistent rate rejection contract changed")
			}
		}
	}
	count := func(key string) int {
		t.Helper()
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(request_count),0) FROM approval_pagination_rate_windows WHERE rate_key=$1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seedFull := func(key string, n int) {
		t.Helper()
		// Seed this and the next two windows as already exhausted. This keeps the
		// rejection boundary deterministic across a wall-clock minute rollover;
		// production transaction_timestamp and all quota code remain untouched.
		if _, err := r.pool.Exec(ctx, `INSERT INTO approval_pagination_rate_windows(rate_key,window_start,request_count)
SELECT $1,point,$2 FROM generate_series(date_trunc('minute',transaction_timestamp()),date_trunc('minute',transaction_timestamp())+interval '2 minutes',interval '1 minute') point
ON CONFLICT(rate_key,window_start) DO UPDATE SET request_count=EXCLUDED.request_count`, key, n); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("registered-replicas-use-distinct-admitted-client-keys", func(t *testing.T) {
		request(0, "first", "198.51.100.11", 200)
		request(1, "second", "198.51.100.12", 200)
		if count("read:ip:198.51.100.11") != 1 || count("read:ip:198.51.100.12") != 1 || count("read:ip:192.0.2.10") != 0 || count("read:ip:203.0.113.99") != 0 {
			t.Fatal("different clients were coalesced into the Portal peer or a raw header")
		}
	})
	t.Run("exhausted-client-is-shared-across-replicas-with-rollback", func(t *testing.T) {
		seedFull("read:ip:198.51.100.11", 120) // Existing published IP read budget.
		fullIPBefore := count("read:ip:198.51.100.11")
		firstKey := "read:subject:" + first.ID.String() + ":approval_requests"
		secondKey := "read:subject:" + second.ID.String() + ":approval_requests"
		beforeFirst, beforeSecond := count(firstKey), count(secondKey)
		request(1, "first", "198.51.100.11", 429)
		request(0, "second", "198.51.100.11", 429)
		if count(firstKey) != beforeFirst || count(secondKey) != beforeSecond || count("read:ip:198.51.100.11") != fullIPBefore {
			t.Fatal("rejected request committed a partial quota reservation")
		}
		otherBefore := count("read:ip:198.51.100.12")
		request(1, "first", "198.51.100.12", 200)
		if count("read:ip:198.51.100.12") != otherBefore+1 {
			t.Fatal("an exhausted unrelated client blocked the next admitted request")
		}
	})
	t.Run("subject-budget-survives-source-change", func(t *testing.T) {
		key := "read:subject:" + second.ID.String() + ":approval_requests"
		seedFull(key, 60) // Existing published per-subject read budget.
		fullSubjectBefore := count(key)
		request(0, "second", "198.51.100.13", 429)
		if count("read:ip:198.51.100.13") != 0 || count(key) != fullSubjectBefore {
			t.Fatal("source rotation bypassed subject quota or committed a failed reservation")
		}
		request(1, "first", "198.51.100.13", 200)
		if count("read:ip:198.51.100.13") != 1 {
			t.Fatal("subject rejection blocked a different authorized subject")
		}
	})
}
