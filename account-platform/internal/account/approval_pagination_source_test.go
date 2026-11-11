package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

// Only persistence is replaced: exercise the registered authenticated route and
// the real ingress source resolver, rather than manufacturing context values.
type paginationSourceRepository struct {
	personalBoundaryRepository
	source, cursor string
	limit, calls   int
	filter         ApprovalFilter
	principal      identity.Principal
	err            error
}

func (r *paginationSourceRepository) ListApprovalRequestsV2(_ context.Context, p identity.Principal, f ApprovalFilter, cursor string, limit int, source string) (ApprovalV2Page, error) {
	r.calls++
	r.source, r.cursor, r.limit, r.filter, r.principal = source, cursor, limit, f, p
	return ApprovalV2Page{Items: []ApprovalV2Summary{}, Limit: limit}, r.err
}

func TestApprovalPaginationRegisteredSource(t *testing.T) {
	for _, tc := range []struct {
		name, peer, hint, second, want string
		withoutIngress                 bool
	}{
		{name: "trusted-portal", peer: "192.0.2.10:1234", hint: "198.51.100.11", want: "198.51.100.11"},
		{name: "direct-header-ignored", peer: "198.51.100.12:1234", hint: "203.0.113.88", want: "198.51.100.12"},
		{name: "trusted-missing-hint", peer: "192.0.2.10:1234", want: "192.0.2.10"},
		{name: "trusted-invalid-hint", peer: "192.0.2.10:1234", hint: "not-an-address", want: "192.0.2.10"},
		{name: "trusted-duplicate-hint", peer: "192.0.2.10:1234", hint: "198.51.100.11", second: "198.51.100.12", want: "192.0.2.10"},
		{name: "mapped-address-canonical", peer: "192.0.2.10:1234", hint: "::ffff:198.51.100.11", want: "198.51.100.11"},
		{name: "direct-ipv6", peer: "[2001:db8::12]:1234", want: "2001:db8::12"},
		{name: "legacy-handler-ignores-hint", peer: "192.0.2.10:1234", hint: "198.51.100.11", want: "192.0.2.10", withoutIngress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &paginationSourceRepository{}
			p := identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session", AuthenticatedAt: time.Now()}
			app := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) { return p, nil })
			handler := app
			if !tc.withoutIngress {
				handler = httpserver.HandlerWithApplication(config.Config{ServiceName: "account-source-test", Environment: "test", RateLimitRPS: 100, RateLimitBurst: 200, HTTPTrustedProxyIPs: []netip.Addr{netip.MustParseAddr("192.0.2.10")}}, app, nil)
			}
			request := httptest.NewRequest(http.MethodGet, "/admin/v2/approval-requests?limit=17&cursor=original-cursor&status=APPROVED", nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set(httpserver.RateLimitSourceHeader, tc.hint)
			if tc.second != "" {
				request.Header.Add(httpserver.RateLimitSourceHeader, tc.second)
			}
			request.Header.Set("X-Forwarded-For", "203.0.113.99")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || repo.calls != 1 || repo.source != tc.want || repo.limit != 17 || repo.cursor != "original-cursor" || repo.filter.Status != "APPROVED" || repo.principal.Subject != p.Subject || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("code=%d source=%q calls=%d limit=%d cursor=%q filter=%+v", response.Code, repo.source, repo.calls, repo.limit, repo.cursor, repo.filter)
			}
		})
	}
}

func TestApprovalPaginationSourcePreservesRejection(t *testing.T) {
	for _, tc := range []struct {
		name, query, peer, code string
		authErr, repoErr        error
		status, calls           int
	}{
		{name: "invalid-peer", peer: "invalid-peer", status: 503, code: "PAGINATION_SNAPSHOT_UNAVAILABLE"},
		{name: "invalid-query", peer: "192.0.2.10:1234", query: "?unsupported=1", status: 400, code: "INVALID_FILTER"},
		{name: "no-session", peer: "192.0.2.10:1234", authErr: errors.New("controlled verification unavailable"), status: 401},
		{name: "persistent-rate-limit", peer: "192.0.2.10:1234", repoErr: approvalPaginationRateError{cause: ErrApprovalPaginationRateLimited, retryAfter: 27}, status: 429, calls: 1, code: "RATE_LIMITED"},
		{name: "snapshot-unavailable", peer: "192.0.2.10:1234", repoErr: ErrApprovalPaginationSnapshotUnavailable, status: 503, calls: 1, code: "PAGINATION_SNAPSHOT_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &paginationSourceRepository{err: tc.repoErr}
			handler := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session", AuthenticatedAt: time.Now()}, tc.authErr
			})
			request := httptest.NewRequest(http.MethodGet, "/admin/v2/approval-requests"+tc.query, nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set(httpserver.RateLimitSourceHeader, "198.51.100.11")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || repo.calls != tc.calls || tc.code != "" && !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("code=%d calls=%d body=%s", response.Code, repo.calls, response.Body.String())
			}
			if tc.status == 429 && response.Header().Get("Retry-After") != "27" {
				t.Fatal("original retry advice changed")
			}
		})
	}
}
