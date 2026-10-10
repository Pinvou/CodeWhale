package account

import (
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"testing"
	"time"
)

type auditAuthorityRepository struct {
	Repository
	allowed       bool
	err           error
	denied, reads int
}

func (*auditAuthorityRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return Me{}, errors.New("controlled context header unavailable")
}

type auditMissingAuthorityRepository struct{ Repository }

func (*auditMissingAuthorityRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return Me{}, errors.New("controlled context header unavailable")
}

func (r *auditAuthorityRepository) Authorize(context.Context, identity.Principal, string, *uuid.UUID, *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: r.allowed, ContextVersion: 7, AuthorizationVersion: 11, PlatformAuthorizationVersion: 11}, r.err
}
func (r *auditAuthorityRepository) RecordPermissionDenial(context.Context, identity.Principal, string, string) error {
	r.denied++
	return nil
}
func (r *auditAuthorityRepository) AdminAuditLogs(context.Context, AdminAuditFilter) ([]AdminAuditLog, error) {
	r.reads++
	return []AdminAuditLog{}, nil
}
func (r *auditAuthorityRepository) AdminAuditLog(context.Context, uuid.UUID) (AdminAuditLog, error) {
	panic("not this route")
}
func TestAuditExportActualRouterAuthorizationFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		allowed               bool
		err                   error
		status, denied, reads int
	}{
		{"infra", false, errors.New("controlled database outage"), 503, 0, 0},
		{"typed-forbidden", false, ErrForbidden, 403, 1, 0},
		{"wrapped-forbidden", false, fmt.Errorf("wrapped: %w", ErrForbidden), 403, 1, 0},
		{"denied", false, nil, 403, 1, 0},
		{"allowed", true, nil, 200, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &auditAuthorityRepository{allowed: tc.allowed, err: tc.err}
			h := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://owned.invalid", Subject: "audit-test", SessionID: "owned-session"}, nil
			})
			req := httptest.NewRequest("GET", "/admin/v1/audit-logs", nil)
			req.Header.Set("Authorization", "Bearer owned")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status || repo.denied != tc.denied || repo.reads != tc.reads {
				t.Fatalf("status%d denied%d reads%d", w.Code, repo.denied, repo.reads)
			}
			if tc.allowed && w.Header().Get("X-Platform-Authorization-Version") != "11" {
				t.Fatal("version header lost")
			}
		})
	}
	t.Run("missing-authority", func(t *testing.T) {
		h := NewService(&auditMissingAuthorityRepository{}, nil).Handler(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "https://owned.invalid", Subject: "audit-test", SessionID: "owned-session"}, nil
		})
		req := httptest.NewRequest("GET", "/admin/v1/audit-logs", nil)
		req.Header.Set("Authorization", "Bearer owned")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 503 {
			t.Fatalf("missing authority status%d", w.Code)
		}
	})
}

// The pool is closed before use and BeforeConnect forbids any network attempt.
// This reaches the concrete PII authorizer's real error path without PostgreSQL.
func TestAuditExportPIIAuthorityOutageIsNotForbidden(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://controlled@127.0.0.1:1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal("controlled config")
	}
	cfg.MinConns = 0
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("network forbidden in this unit") }
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal("controlled pool")
	}
	pool.Close()
	now := time.Now().UTC()
	owner := &AuditExportService{repository: &PostgresRepository{pool: pool}, now: func() time.Time { return now }}
	_, err = owner.Create(context.Background(), identity.Principal{AuthenticatedAt: now}, AuditExportRequest{IncludePII: true, Reason: "controlled PII reason", IdempotencyKey: "controlled-key", Filter: AuditExportFilter{From: now.Add(-time.Minute), To: now}})
	if err == nil {
		t.Fatal("closed authority accepted PII")
	}
	w := httptest.NewRecorder()
	(&Service{}).writeError(w, httptest.NewRequest("POST", "/admin/v1/audit-exports", nil), err)
	if w.Code != 503 {
		t.Fatalf("authority outage status%d", w.Code)
	}
}
