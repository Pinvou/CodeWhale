package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type f04LegacyOverviewStub struct {
	*PostgresRepository
	rateCalls, queryCalls int
	rateErr, queryErr     error
	subject               uuid.UUID
	endpoint, source      string
	deadline              time.Time
}

func (s *f04LegacyOverviewStub) reserveAdminOverviewRate(ctx context.Context, subject uuid.UUID, endpoint, source string) error {
	s.rateCalls++
	s.subject = subject
	s.endpoint = endpoint
	s.source = source
	s.deadline, _ = ctx.Deadline()
	return s.rateErr
}
func (s *f04LegacyOverviewStub) AdminOverview(ctx context.Context) (AdminOverview, error) {
	s.queryCalls++
	if s.deadline.IsZero() {
		s.deadline, _ = ctx.Deadline()
	}
	return AdminOverview{1, 2, 3, 4, 5, 6, 7, 8, 9}, s.queryErr
}
func f04LegacyOverviewHandler(t *testing.T, repo Repository, grants []string) (http.Handler, uuid.UUID) {
	t.Helper()
	actor := uuid.New()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": len(grants) > 0, "user_id": actor, "granted_permissions": grants})
	}))
	t.Cleanup(auth.Close)
	a, err := authorization.NewClient(auth.URL, "controlled-service-auth")
	if err != nil {
		t.Fatal(err)
	}
	s := NewManagementService(repo, nil, nil, nil, ProviderURLPolicy{}, nil, nil).WithAuthorizer(a).WithAdminProxyHMACKey(pageOverviewProxyKey)
	return s.ManagementHandler("", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: actor.String(), SessionID: "controlled"}, nil
	}), actor
}
func f04LegacyOverviewRequest(h http.Handler, ctx context.Context, signed bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/admin/v1/overview", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer controlled")
	r.RemoteAddr = "10.0.0.8:4321"
	r.Header.Set("X-Pinvou-Client-Source", "203.0.113.200")
	r.Header.Set("X-Pinvou-Client-Source-Signature", "forged")
	if signed {
		signAdminQuerySource(r, pageOverviewProxyKey, "203.0.113.9")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type f04LegacyOverviewUnsupported struct{ Repository }

func TestF04LegacyOverviewAdmissionHTTP(t *testing.T) {
	t.Run("unsupported-fail-closed", func(t *testing.T) {
		h, _ := f04LegacyOverviewHandler(t, &f04LegacyOverviewUnsupported{}, []string{"platform.tokens.read"})
		w := f04LegacyOverviewRequest(h, context.Background(), false)
		if w.Code != 503 {
			t.Fatalf("unsupported admission status=%d body=%s", w.Code, w.Body.String())
		}
	})

	for _, tc := range []struct {
		name   string
		grants []string
		fields int
	}{
		{"both", []string{"platform.tokens.read", "platform.usage.read"}, 9},
		{"token-only", []string{"platform.tokens.read"}, 5},
		{"usage-only", []string{"platform.usage.read"}, 4},
		{"system-only", []string{"platform.system.status.read"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &f04LegacyOverviewStub{}
			h, actor := f04LegacyOverviewHandler(t, s, tc.grants)
			start := time.Now()
			w := f04LegacyOverviewRequest(h, context.Background(), false)
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var got map[string]int64
			if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != tc.fields {
				t.Fatalf("redaction=%s", w.Body.String())
			}
			if len(tc.grants) == 1 && tc.grants[0] != "platform.tokens.read" {
				if _, ok := got["token_accounts"]; ok {
					t.Fatal("token fields leaked")
				}
			}
			if tc.name == "token-only" {
				if _, ok := got["usage_requests"]; ok {
					t.Fatal("usage fields leaked")
				}
			}
			if s.rateCalls != 1 || s.queryCalls != 1 || s.subject != actor || s.source != "10.0.0.8" || s.endpoint != "legacy-overview" {
				t.Fatalf("admission=%+v", s)
			}
			if s.deadline.IsZero() || s.deadline.After(start.Add(AdminSnapshotBuildBudget+time.Second)) {
				t.Fatal("unbounded legacy read")
			}
		})
	}
	t.Run("quota-before-count", func(t *testing.T) {
		s := &f04LegacyOverviewStub{rateErr: adminQueryRateLimitError{RetryAfter: 17}}
		h, _ := f04LegacyOverviewHandler(t, s, []string{"platform.tokens.read"})
		w := f04LegacyOverviewRequest(h, context.Background(), false)
		if w.Code != 429 || w.Header().Get("Retry-After") != "17" || s.queryCalls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, s.queryCalls, w.Body.String())
		}
	})
	t.Run("dependency-before-count", func(t *testing.T) {
		s := &f04LegacyOverviewStub{rateErr: errors.New("controlled storage failure")}
		h, _ := f04LegacyOverviewHandler(t, s, []string{"platform.tokens.read"})
		w := f04LegacyOverviewRequest(h, context.Background(), false)
		if w.Code != 503 || s.queryCalls != 0 {
			t.Fatalf("status=%d calls=%d", w.Code, s.queryCalls)
		}
	})
	t.Run("permission-before-owner", func(t *testing.T) {
		s := &f04LegacyOverviewStub{}
		h, _ := f04LegacyOverviewHandler(t, s, nil)
		w := f04LegacyOverviewRequest(h, context.Background(), false)
		if w.Code != 403 || s.rateCalls+s.queryCalls != 0 {
			t.Fatalf("status=%d calls=%d/%d", w.Code, s.rateCalls, s.queryCalls)
		}
	})
	t.Run("signed-source", func(t *testing.T) {
		s := &f04LegacyOverviewStub{}
		h, _ := f04LegacyOverviewHandler(t, s, []string{"platform.tokens.read"})
		w := f04LegacyOverviewRequest(h, context.Background(), true)
		if w.Code != 200 || s.source != "203.0.113.9" {
			t.Fatalf("status=%d source=%s", w.Code, s.source)
		}
	})
	t.Run("short-parent", func(t *testing.T) {
		s := &f04LegacyOverviewStub{}
		h, _ := f04LegacyOverviewHandler(t, s, []string{"platform.tokens.read"})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		expected, _ := ctx.Deadline()
		w := f04LegacyOverviewRequest(h, ctx, false)
		if w.Code != 200 || !s.deadline.Equal(expected) {
			t.Fatalf("status=%d changed parent deadline", w.Code)
		}
	})
	t.Run("expired-before-owner", func(t *testing.T) {
		s := &f04LegacyOverviewStub{}
		h, _ := f04LegacyOverviewHandler(t, s, []string{"platform.tokens.read"})
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		w := f04LegacyOverviewRequest(h, ctx, false)
		if w.Code < 400 || s.rateCalls+s.queryCalls != 0 {
			t.Fatalf("status=%d calls=%d/%d", w.Code, s.rateCalls, s.queryCalls)
		}
	})
}
