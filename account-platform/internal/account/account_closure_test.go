package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type closureCreateErrorRepository struct {
	*closureAssemblyRepository
	cause     error
	principal identity.Principal
}

func (f *closureCreateErrorRepository) CreateAccountClosure(_ context.Context, principal identity.Principal, key, confirmation string) (AccountClosureRequest, error) {
	f.calls++
	f.principal, f.key, f.confirmation = principal, key, confirmation
	return AccountClosureRequest{}, f.cause
}

func TestAccountClosureCreateRoutePreservesErrorClassification(t *testing.T) {
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "closure-actor", SessionID: "closure-session", AuthenticatedAt: time.Now()}
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"serialization", &pgconn.PgError{Code: "40001", Message: "private database detail"}, http.StatusConflict, "ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT"},
		{"wrapped serialization", errors.Join(errors.New("private context"), &pgconn.PgError{Code: "40001"}), http.StatusConflict, "ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT"},
		{"domain conflict", ErrConflict, http.StatusConflict, "ACCOUNT_CONFLICT"},
		{"unique violation", &pgconn.PgError{Code: "23505", Message: "private database detail"}, http.StatusInternalServerError, "INTERNAL_ERROR"},
		{"deadlock", &pgconn.PgError{Code: "40P01", Message: "private database detail"}, http.StatusInternalServerError, "INTERNAL_ERROR"},
		{"unavailable", errors.New("private database detail"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &closureCreateErrorRepository{closureAssemblyRepository: &closureAssemblyRepository{allow: true}, cause: tc.cause}
			handler := NewService(store, nil).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			request := httptest.NewRequest(http.MethodPost, "/v1/me/account-closure-requests", strings.NewReader(`{"confirmation":"CLOSE_MY_ACCOUNT"}`))
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set("Idempotency-Key", "original-closure-key")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			var payload struct{ Code, Message string }
			if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if out.Code != tc.status || payload.Code != tc.code || strings.Contains(out.Body.String(), "private") {
				t.Fatalf("status=%d payload=%#v", out.Code, payload)
			}
			if store.calls != 1 || store.permission != PermissionPersonalAccountClosureRequest || store.key != "original-closure-key" || store.confirmation != accountClosureConfirmation || store.principal.Issuer != principal.Issuer || store.principal.Subject != principal.Subject || store.principal.SessionID != principal.SessionID {
				t.Fatalf("original authority or request facts lost: calls=%d permission=%s principal=%#v key=%q confirmation=%q", store.calls, store.permission, store.principal, store.key, store.confirmation)
			}
			if got, want := strings.Contains(payload.Message, "retry with the same idempotency key"), tc.code == "ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT"; got != want {
				t.Fatalf("wrong recovery semantics for %s: %q", tc.code, payload.Message)
			}
		})
	}
}

func TestAccountClosureRecentAuthenticationHTTPErrorIsStable(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/account-closure", nil), ErrRecentAuthenticationRequired)
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusUnauthorized || payload.Code != "RECENT_AUTHENTICATION_REQUIRED" {
		t.Fatalf("unexpected response: status=%d code=%q body=%s", recorder.Code, payload.Code, recorder.Body.String())
	}
}

func TestAccountClosureSerializationConflictHasStableHTTPClassification(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/me/account-closure-requests", nil), &pgconn.PgError{Code: "40001", Message: "private serialization detail"})
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusConflict || payload.Code != "ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT" || strings.Contains(payload.Message, "private") {
		t.Fatalf("unexpected serialization response: status=%d payload=%#v", recorder.Code, payload)
	}
	if !strings.Contains(payload.Message, "retry with the same idempotency key") {
		t.Fatalf("original-key recovery instruction missing: %q", payload.Message)
	}
}

func TestAccountClosureSerializationClassificationIsRouteScoped(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/me/account-closure-request"},
		{http.MethodPost, "/admin/v1/account-closure-requests/00000000-0000-7000-8000-000000000001/actions/retry"},
	} {
		recorder := httptest.NewRecorder()
		(&Service{}).writeError(recorder, httptest.NewRequest(route.method, route.path, nil), &pgconn.PgError{Code: "40001"})
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("out-of-scope account route %s status=%d, want 500", route.path, recorder.Code)
		}
	}
}

func TestAccountClosureSerializationClassificationRejectsOtherDatabaseErrors(t *testing.T) {
	for _, cause := range []error{
		&pgconn.PgError{Code: "23505", Message: "private unique constraint detail"},
		errors.New("private unavailable database detail"),
	} {
		recorder := httptest.NewRecorder()
		(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/me/account-closure-requests", nil), cause)
		var payload struct{ Code, Message string }
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusInternalServerError || payload.Code != "INTERNAL_ERROR" || strings.Contains(payload.Message, "private") {
			t.Fatalf("unrelated database error gained serialization recovery: status=%d payload=%#v", recorder.Code, payload)
		}
	}
}

func TestAccountClosureDomainConflictKeepsDistinctHTTPCode(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/me/account-closure-requests", nil), ErrConflict)
	var payload struct{ Code, Message string }
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusConflict || payload.Code != "ACCOUNT_CONFLICT" || strings.Contains(payload.Message, "retry with the same idempotency key") {
		t.Fatalf("domain conflict gained serialization recovery: status=%d payload=%#v", recorder.Code, payload)
	}
}

func TestAccountClosureValidationClassifiesRecentAuthentication(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	valid := identity.Principal{AuthenticatedAt: now.Add(-time.Minute)}
	tests := []struct {
		name         string
		principal    identity.Principal
		key          string
		confirmation string
		want         error
	}{
		{name: "valid", principal: valid, key: "account-closure-key", confirmation: accountClosureConfirmation},
		{name: "stale authentication", principal: identity.Principal{AuthenticatedAt: now.Add(-accountClosureRecentAuth - time.Second)}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "missing authentication time", principal: identity.Principal{}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "future authentication time", principal: identity.Principal{AuthenticatedAt: now.Add(time.Minute + time.Second)}, key: "account-closure-key", confirmation: accountClosureConfirmation, want: ErrRecentAuthenticationRequired},
		{name: "invalid confirmation", principal: valid, key: "account-closure-key", confirmation: "CLOSE_MY_ORGANIZATION", want: ErrInvalidAccountClosureRequest},
		{name: "invalid idempotency key", principal: valid, key: "short", confirmation: accountClosureConfirmation, want: ErrInvalidAccountClosureRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAccountClosureRequest(now, test.principal, test.key, test.confirmation)
			if (test.want == nil && err != nil) || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("validation error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestAccountClosureRoleBlockersAreActionable(t *testing.T) {
	tests := []struct {
		err    error
		code   string
		action string
	}{
		{ErrLastOwner, "ORGANIZATION_OWNERSHIP_REQUIRED", "TRANSFER_OWNERSHIP"},
		{ErrLastActiveSuperAdmin, "LAST_ACTIVE_SUPER_ADMIN", "HANDOFF_SUPER_ADMIN"},
	}
	for _, test := range tests {
		blockers, err := accountClosureRoleBlockers(test.err)
		if err != nil || len(blockers) != 1 || blockers[0].Code != test.code || blockers[0].UserAction != test.action {
			t.Fatalf("unexpected blocker for %v: %#v, %v", test.err, blockers, err)
		}
	}
	if _, err := accountClosureRoleBlockers(errors.New("database unavailable")); err == nil {
		t.Fatal("unexpected infrastructure error must not be converted to a business blocker")
	}
}

func TestUserVisibleClosureRemovesInternalIdentifiers(t *testing.T) {
	now := time.Now().UTC()
	item := AccountClosureRequest{
		ID: uuid.New(), UserID: uuid.New(), Status: "BLOCKED", Stage: "FINANCIAL_CLEARANCE",
		PolicyVersion: 1, Attempts: 1, ReauthenticatedAt: now, SLAExpiresAt: now.Add(time.Hour),
		Version: 4, CreatedAt: now, UpdatedAt: now,
		Blockers: []AccountClosureBlocker{{Code: "PAYMENT_PROCESSING", Domain: "BILLING", ResourceType: "PAYMENT_ORDER", ResourceID: uuid.NewString(), UserAction: "FINISH_PAYMENT_ORDER"}},
	}
	visible := userVisibleClosure(item)
	if visible.ID != item.ID || visible.Blockers[0].ResourceID != "" {
		t.Fatalf("unexpected public closure: %#v", visible)
	}
}

func TestAccountClosureCursorIsOpaqueAndRoundTrips(t *testing.T) {
	item := AccountClosureRequest{ID: uuid.New(), UpdatedAt: time.Date(2026, 10, 31, 5, 4, 3, 123456000, time.FixedZone("CST", 8*60*60))}
	cursor, err := encodeAccountClosureCursor(item)
	if err != nil || cursor == "" {
		t.Fatalf("encode cursor: %v %q", err, cursor)
	}
	updatedAt, id, err := decodeAccountClosureCursor(cursor)
	if err != nil || id == nil || *id != item.ID || updatedAt == nil || !updatedAt.Equal(item.UpdatedAt.UTC()) {
		t.Fatalf("round trip mismatch: updated_at=%v id=%v err=%v", updatedAt, id, err)
	}
	if _, _, err := decodeAccountClosureCursor("not-a-cursor"); !errors.Is(err, ErrInvalidAccountClosureRequest) {
		t.Fatalf("invalid cursor error=%v", err)
	}
}

func TestAccountClosureSelfServiceRequiresKnownFinancialAction(t *testing.T) {
	for _, test := range []struct{ code, action string }{
		{"PERSONAL_BALANCE_NONZERO", "CLEAR_PERSONAL_BALANCE"},
		{"RECEIVABLE_OUTSTANDING", "SETTLE_RECEIVABLE"},
		{"PAYMENT_PROCESSING", "FINISH_PAYMENT_ORDER"},
	} {
		if !accountClosureNeedsSelfService([]AccountClosureBlocker{{Domain: "BILLING", Code: test.code, UserAction: test.action}}) {
			t.Fatal("existing user financial action omitted", test)
		}
		for _, invalid := range []AccountClosureBlocker{
			{Domain: "TOKEN", Code: test.code, UserAction: test.action},
			{Domain: "BILLING", Code: "UNKNOWN", UserAction: test.action},
			{Domain: "BILLING", Code: test.code, UserAction: "CONTACT_SUPPORT"},
		} {
			if accountClosureNeedsSelfService([]AccountClosureBlocker{invalid}) {
				t.Fatal("unproven action restored access", invalid)
			}
		}
	}
	for _, blocker := range []AccountClosureBlocker{
		{Domain: "BILLING", Code: "UNSETTLED_HOLD", UserAction: "WAIT_FOR_USAGE_SETTLEMENT"},
		{Domain: "BILLING", Code: "REFUND_UNRESOLVED", UserAction: "FINISH_REFUND"},
		{Domain: "ACCOUNT", Code: "ORGANIZATION_OWNERSHIP_REQUIRED", UserAction: "TRANSFER_OWNERSHIP"},
	} {
		if accountClosureNeedsSelfService([]AccountClosureBlocker{blocker}) {
			t.Fatal("non-user financial blocker restored access", blocker)
		}
	}
}
