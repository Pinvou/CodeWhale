package identityadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real loopback consumers prove that redirect bodies are never replayed. Neither
// receiver nor assertions print credential values or request bodies.
func TestKeycloakDevelopmentDirectoryRedirectBoundary(t *testing.T) {
	for _, constructor := range []string{"legacy", "config-no-ca"} {
		for _, operation := range []string{"client-credentials", "password-reset"} {
			for _, status := range []int{307, 308, 200} {
				t.Run(fmt.Sprintf("%s/%s/%d", constructor, operation, status), func(t *testing.T) {
					var originCalls, receiverCalls atomic.Int32
					respond := func(w http.ResponseWriter) {
						if operation == "client-credentials" {
							_, _ = io.WriteString(w, `{"access_token":"fixture-token","expires_in":300}`)
						} else {
							w.WriteHeader(204)
						}
					}
					receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						receiverCalls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						respond(w)
					}))
					defer receiver.Close()
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/realms/pinvou/protocol/openid-connect/token" {
							if r.Method != http.MethodPost || r.FormValue("grant_type") != "client_credentials" || r.FormValue("client_secret") != fixtureProvisionerSecret {
								t.Error("unexpected provisioner request shape")
							}
							if operation == "password-reset" {
								_, _ = io.WriteString(w, `{"access_token":"fixture-token","expires_in":300}`)
								return
							}
						} else {
							var body struct {
								Value string `json:"value"`
							}
							if r.URL.Path != "/admin/realms/pinvou/users/fixture-user/reset-password" || r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer fixture-token" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Value != "isolated-reset-password" {
								t.Error("unexpected password reset request shape")
							}
						}
						originCalls.Add(1)
						if status != 200 {
							w.Header().Set("Location", receiver.URL)
							w.WriteHeader(status)
							return
						}
						respond(w)
					}))
					defer origin.Close()
					tokenURL := origin.URL + "/realms/pinvou/protocol/openid-connect/token"
					var directory *KeycloakDirectory
					var err error
					if constructor == "legacy" {
						directory, err = NewKeycloakDirectory(origin.URL, tokenURL, "pinvou", "fixture-client", fixtureProvisionerSecret)
					} else {
						directory, err = NewKeycloakDirectoryWithConfig(KeycloakConnectionConfig{AdminURL: origin.URL, TokenURL: tokenURL, Realm: "pinvou", ClientID: "fixture-client", ClientSecret: fixtureProvisionerSecret})
					}
					if err != nil {
						t.Fatal("directory construction failed")
					}
					if directory.client.Timeout != 10*time.Second {
						t.Fatal("directory timeout changed")
					}
					if operation == "client-credentials" {
						err = directory.Ping(context.Background())
					} else {
						err = directory.SetPassword(context.Background(), "fixture-user", "isolated-reset-password")
					}
					if status == 200 && err != nil {
						t.Error("direct credential request failed")
					}
					if status != 200 && err == nil {
						t.Error("redirect accepted as successful credential request")
					}
					if originCalls.Load() != 1 {
						t.Error("credential producer did not reach configured endpoint exactly once")
					}
					if receiverCalls.Load() != 0 {
						t.Error("credential request reached redirect receiver")
					}
				})
			}
		}
	}
}

func TestKeycloakDevelopmentTokenProxyRedirectBoundary(t *testing.T) {
	for _, constructor := range []string{"legacy", "trust-no-ca"} {
		for _, status := range []int{307, 308, 200} {
			t.Run(fmt.Sprintf("%s/%d", constructor, status), func(t *testing.T) {
				var originCalls, receiverCalls atomic.Int32
				respond := func(w http.ResponseWriter) {
					_, _ = io.WriteString(w, `{"access_token":"fixture-user-token","expires_in":300}`)
				}
				receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					receiverCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					respond(w)
				}))
				defer receiver.Close()
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					originCalls.Add(1)
					if r.Method != http.MethodPost || r.FormValue("grant_type") != "password" || r.FormValue("password") != "isolated-login-password" || r.FormValue("client_secret") != "" {
						t.Error("unexpected user credential request shape")
					}
					if status != 200 {
						w.Header().Set("Location", receiver.URL)
						w.WriteHeader(status)
						return
					}
					respond(w)
				}))
				defer origin.Close()
				admission := &admissionFake{}
				var proxy *TokenProxy
				var err error
				if constructor == "legacy" {
					proxy, err = NewTokenProxy(origin.URL, admission)
				} else {
					proxy, err = NewTokenProxyWithTrust(origin.URL, admission, "", false)
				}
				if err != nil {
					t.Fatal("proxy construction failed")
				}
				if proxy.client.Timeout != 15*time.Second {
					t.Fatal("proxy timeout changed")
				}
				proxy.floor = 0
				form := url.Values{"grant_type": {"password"}, "client_id": {"pinvou-desktop"}, "username": {"fixture"}, "password": {"isolated-login-password"}}
				request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				proxy.ServeHTTP(response, request)
				if status == 200 {
					if response.Code != 200 || !admission.success {
						t.Error("direct password request failed")
					}
				} else if response.Code != 401 || admission.success {
					t.Error("redirect did not retain failed OAuth result")
				}
				if admission.completions != 1 || originCalls.Load() != 1 {
					t.Error("proxy credential or admission path not exercised exactly once")
				}
				if receiverCalls.Load() != 0 {
					t.Error("password request reached redirect receiver")
				}
			})
		}
	}
}
