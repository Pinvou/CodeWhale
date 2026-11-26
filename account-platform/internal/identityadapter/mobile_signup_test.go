package identityadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestSMSOnlyIdentityCreatesNoPasswordAndNeverRedispatches(t *testing.T) {
	created := make(chan map[string]json.RawMessage, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"test-fixture"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/fixture/users":
			var user map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			created <- user
			w.Header().Set("Location", "/admin/realms/fixture/users/sms-user")
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	directory, err := NewKeycloakDirectory(server.URL, server.URL+"/token", "fixture", "fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := uuid.New()
	const username, mobile = "user-0123456789abcdef01234567", "+8613800138000"
	mode := identity.MobileSignupCredentialMode
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "password-handle", mode, username, mobile, true); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatal("password reference was accepted by SMS-only creation", err)
	}
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, false); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal("nonfirst request could create an identity", err)
	}
	if subject, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, true); err != nil || subject != "sms-user" {
		t.Fatal("failed to create SMS identity", subject, err)
	}
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, true); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal("a stale first grant was allowed to repeat remote mutation", err)
	}
	if len(created) != 1 {
		t.Fatal("unexpected identity create count", len(created))
	}
	user := <-created
	if _, hasCredentials := user["credentials"]; hasCredentials {
		t.Fatal("the SMS-only account acquired a password credential")
	}
	var attrs map[string][]string
	if err := json.Unmarshal(user["attributes"], &attrs); err != nil || strings.Join(attrs["phone_number_verified"], "") != "true" ||
		strings.Join(attrs["phone_number"], "") != mobile || strings.Join(attrs["pinvou_registration_operation_id"], "") != id.String() {
		t.Fatal("identity lacks the verified mobile and exact operation correlation", err)
	}
}

func TestSMSOnlyIdentityHTTPDispatchContract(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, key := range []string{
		"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL",
		"WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE",
		"WORKLOAD_IDENTITY_JWKS_AUTH_MODE", "WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE", "WORKLOAD_IDENTITY_JWKS_API_SUBJECT",
	} {
		t.Setenv(key, "")
	}

	const smsRoute = "/internal/v1/users/mobile/sms-only"
	const passwordRoute = "/internal/v1/users/mobile"
	const smsMode = "sms-only-v1"
	const passwordMode = "user-v1"
	const usersPath = "/admin/realms/fixture/users"
	const serviceCredential = "controlled-sms-http-fixture"

	for _, tc := range []struct {
		name, route, mode                          string
		first, failToken, cancelWarm, rebindMobile bool
		usernameConflict                           bool
		correlatedSubject                          string
		status                                     int
		code, subject                              string
		tokens, lookups, creates                   int32
	}{
		{
			name: "token failure proves no dispatch", route: smsRoute, mode: smsMode,
			first: true, failToken: true, status: http.StatusGone,
			code: "IDENTITY_CREATE_NOT_DISPATCHED", tokens: 1,
		},
		{
			name: "canceled request with cached token proves no dispatch", route: smsRoute, mode: smsMode,
			first: true, cancelWarm: true, status: http.StatusGone,
			code: "IDENTITY_CREATE_NOT_DISPATCHED", tokens: 1,
		},
		{
			name: "password mode cannot use SMS route", route: smsRoute, mode: passwordMode,
			first: true, status: http.StatusBadRequest, code: "INVALID_REQUEST",
		},
		{
			name: "SMS mode cannot use password route", route: passwordRoute, mode: smsMode,
			first: true, status: http.StatusBadRequest, code: "INVALID_REQUEST",
		},
		{
			name: "unmatched probe does not create", route: smsRoute, mode: smsMode,
			status: http.StatusServiceUnavailable, code: "IDENTITY_CREATE_UNCONFIRMED", tokens: 1, lookups: 1,
		},
		{
			name: "matched probe returns original subject", route: smsRoute, mode: smsMode,
			correlatedSubject: "existing-sms-user", status: http.StatusCreated, subject: "existing-sms-user", tokens: 1, lookups: 1,
		},
		{
			name: "first attempt creates", route: smsRoute, mode: smsMode,
			first: true, status: http.StatusCreated, subject: "new-sms-user", tokens: 1, creates: 1,
		},
		{
			name: "directory username conflict remains a conflict", route: smsRoute, mode: smsMode,
			first: true, usernameConflict: true, status: http.StatusConflict,
			code: "USERNAME_ALREADY_REGISTERED", tokens: 1, creates: 1,
		},
		{
			name: "existing dispatch cannot bind another mobile", route: smsRoute, mode: smsMode,
			first: true, rebindMobile: true, status: http.StatusConflict,
			code: "PASSWORD_REFERENCE_CONFLICT", tokens: 1, creates: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			var tokens, lookups, creates atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/token" && r.Method == http.MethodPost:
					tokens.Add(1)
					if tc.failToken {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write([]byte(`{"access_token":"fixture-only","expires_in":300}`))
				case r.URL.Path == usersPath && r.Method == http.MethodGet:
					lookups.Add(1)
					if got := r.URL.Query().Get("q"); got != "pinvou_registration_operation_id:"+id.String() {
						t.Errorf("registration correlation = %q", got)
					}
					if tc.correlatedSubject == "" {
						_, _ = w.Write([]byte(`[]`))
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{{
						"id":         tc.correlatedSubject,
						"attributes": map[string][]string{"pinvou_registration_operation_id": {id.String()}},
					}})
				case r.URL.Path == usersPath && r.Method == http.MethodPost:
					creates.Add(1)
					if tc.usernameConflict {
						w.WriteHeader(http.StatusConflict)
						return
					}
					w.Header().Set("Location", usersPath+"/new-sms-user")
					w.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			directory, err := NewKeycloakDirectory(upstream.URL, upstream.URL+"/token", "fixture", "fixture", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(vault, serviceCredential)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.cancelWarm {
				if err := directory.Ping(ctx); err != nil {
					t.Fatalf("prime the provisioner token cache: %v", err)
				}
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}

			handler := service.Handler()
			type responseBody struct {
				Code    string `json:"code"`
				Subject string `json:"subject"`
			}
			requestCreate := func(mobile string) (int, responseBody) {
				body, err := json.Marshal(map[string]any{
					"registration_operation_id": id, "username": "user-0123456789abcdef01234567",
					"mobile": mobile, "password_policy_version": tc.mode, "first_attempt": tc.first,
				})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequestWithContext(ctx, http.MethodPost, tc.route, bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+serviceCredential)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)

				var result responseBody
				if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
					t.Fatalf("decode response (HTTP %d): %v", response.Code, err)
				}
				return response.Code, result
			}

			mobile := "+8613800138000"
			if tc.rebindMobile {
				status, result := requestCreate(mobile)
				if status != http.StatusCreated || result.Subject != "new-sms-user" || result.Code != "" {
					t.Fatalf("first attempt = HTTP %d %+v, want new SMS subject", status, result)
				}
				mobile = "+8613800138001"
			}
			status, result := requestCreate(mobile)
			if status != tc.status || result.Code != tc.code || result.Subject != tc.subject {
				t.Errorf("response = HTTP %d %+v, want HTTP %d code %q subject %q", status, result, tc.status, tc.code, tc.subject)
			}
			if tokens.Load() != tc.tokens || lookups.Load() != tc.lookups || creates.Load() != tc.creates {
				t.Errorf("upstream calls = token %d lookup %d create %d, want %d/%d/%d", tokens.Load(), lookups.Load(), creates.Load(), tc.tokens, tc.lookups, tc.creates)
			}
		})
	}
}

func TestPasswordIdentityCreateDoesNotIssueSMSDispatchProof(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{name: "provisioner token unavailable"},
		{name: "canceled request with cached token", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokens, creates atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/token":
					tokens.Add(1)
					if !tc.canceled {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write([]byte(`{"access_token":"fixture-only","expires_in":300}`))
				case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/fixture/users":
					creates.Add(1)
					w.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			directory, err := NewKeycloakDirectory(upstream.URL, upstream.URL+"/token", "fixture", "fixture", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.canceled {
				if err := directory.Ping(ctx); err != nil {
					t.Fatalf("prime the provisioner token cache: %v", err)
				}
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			subject, err := directory.CreateMobileUser(ctx, uuid.New(), "controlled", "+8613800138000", "Controlled2026")
			if subject != "" || err == nil || errors.Is(err, ErrCreateNotDispatched) {
				t.Fatalf("password-based creation = %q, %v; want the original error without SMS dispatch proof", subject, err)
			}
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("password-based cancellation lost its context error: %v", err)
			}
			if tokens.Load() != 1 || creates.Load() != 0 {
				t.Fatalf("upstream calls = token %d, create %d; want 1/0", tokens.Load(), creates.Load())
			}
		})
	}
}
