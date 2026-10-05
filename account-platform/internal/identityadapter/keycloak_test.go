package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestKeycloakRegistrationStrictIntentAndLostACK(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			op := uuid.New()
			var user map[string]any
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"access_token":"test-service-token","expires_in":120}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-service-token" {
					t.Error("missing service auth")
					w.WriteHeader(401)
					return
				}
				if r.Method == http.MethodGet {
					if r.URL.Query().Get("q") != registrationMarker(legacy)+":"+op.String() {
						t.Error("lookup must use exactly the typed opaque intent namespace")
					}
					if user == nil {
						_, _ = w.Write([]byte(`[]`))
					} else {
						_ = json.NewEncoder(w).Encode([]any{user})
					}
					return
				}
				if r.Method != http.MethodPost {
					t.Error("unexpected mutation")
					w.WriteHeader(405)
					return
				}
				creates++
				if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				user["id"] = "test-owned-subject"
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close() // accepted create, no response
			}))
			defer server.Close()
			directory, err := NewKeycloakDirectory("test", server.URL, server.URL+"/token", "test", "client", "test-secret")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := directory.CreateMobileUser(context.Background(), op, "test-user", "+8613800138000", "FirstPassword123", legacy)
			if err != nil || !identity.matches(op, "test-user", "+8613800138000") || !identity.PasswordPreserved {
				t.Fatalf("lost ACK recovery=%+v err=%v", identity, err)
			}
			identity, err = directory.CreateMobileUser(context.Background(), op, "test-user", "+8613800138000", "DifferentPassword456", legacy)
			if err != nil || !identity.PasswordPreserved || creates != 1 {
				t.Fatalf("retry=%+v creates=%d err=%v", identity, creates, err)
			}
			if _, err := directory.CreateMobileUser(context.Background(), op, "other-user", "+8613800138000", "DifferentPassword456", legacy); !errors.Is(err, ErrReferenceConflict) {
				t.Fatalf("changed facts adopted: %v", err)
			}
		})
	}
}

func TestKeycloakRegistrationLookupRejectsMalformedAuthority(t *testing.T) {
	op := uuid.New()
	valid := fmt.Sprintf(`[{"id":"subject","username":"user","enabled":true,"attributes":{"pinvou_registration_operation_id":["%s"],"phone_number":["+8613800138000"],"phone_number_verified":["true"]}}]`, op)
	for name, payload := range map[string]string{
		"null": "null", "two JSON values": valid + `[]`, "trailing garbage": valid + `broken`, "oversize": valid + strings.Repeat(" ", 256<<10),
		"disabled": strings.Replace(valid, `"enabled":true`, `"enabled":false`, 1), "foreign marker": strings.Replace(valid, op.String(), uuid.New().String(), 1),
		"unverified": strings.Replace(valid, `"phone_number_verified":["true"]`, `"phone_number_verified":["false"]`, 1),
		"multiple":   "[" + strings.Trim(valid, "[]") + "," + strings.Trim(valid, "[]") + "]",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"access_token":"token","expires_in":120}`))
				} else {
					_, _ = w.Write([]byte(payload))
				}
			}))
			defer server.Close()
			directory, err := NewKeycloakDirectory("test", server.URL, server.URL+"/token", "realm", "client", "secret")
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := directory.FindMobileUserByRegistrationOperation(context.Background(), op, false); err == nil || found {
				t.Fatalf("invalid authority adopted: found=%v err=%v", found, err)
			}
		})
	}
}

func TestKeycloakDirectoryPreservesEnvironmentHTTPSGate(t *testing.T) {
	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) })
	plain := httptest.NewServer(handler)
	defer plain.Close()
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	for _, environment := range []string{"", "development", "test", "production", "staging"} {
		for _, adminHTTPS := range []bool{false, true} {
			for _, tokenHTTPS := range []bool{false, true} {
				t.Run(fmt.Sprintf("env=%s/adminHTTPS=%t/tokenHTTPS=%t", environment, adminHTTPS, tokenHTTPS), func(t *testing.T) {
					adminURL, tokenURL := plain.URL, plain.URL+"/token"
					if adminHTTPS {
						adminURL = secure.URL
					}
					if tokenHTTPS {
						tokenURL = secure.URL + "/token"
					}
					allowed := environment == "" || environment == "development" || environment == "test" || adminHTTPS && tokenHTTPS
					directory, err := NewKeycloakDirectory(environment, adminURL, tokenURL, "realm", "controlled-client", "controlled-secret")
					if allowed && (err != nil || directory == nil) || !allowed && (err == nil || directory != nil) {
						t.Fatal("Keycloak construction did not preserve the existing environment transport boundary")
					}
					if hits.Load() != 0 {
						t.Fatal("Keycloak construction performed network I/O before its transport check")
					}
				})
			}
		}
	}
}
