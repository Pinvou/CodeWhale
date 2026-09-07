package workload

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
)

// Run only in the network namespace of the disposable fixture managed by
// scripts/account-platform-workload-keycloak-fixture.py. It cannot target a
// configurable remote realm or import credentials from a production service.
func TestKeycloakWorkloadIntegration(t *testing.T) {
	if os.Getenv("PINVOU_TEST_KEYCLOAK_ISOLATED") != "1" {
		t.Skip("explicit isolated Keycloak fixture opt-in required")
	}
	const origin = "https://localhost:8443"
	const realm = "pinvou-fixture-workload"
	const issuer = origin + "/realms/" + realm
	const audience = "pinvou-account-platform-internal"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	adminSecret, err := os.ReadFile("/fixture/admin-secret")
	if err != nil {
		t.Fatal("isolated bootstrap file unavailable")
	}
	defer clear(adminSecret)
	var adminToken string
	for attempt := 0; attempt < 90; attempt++ {
		form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"fixture-admin"}, "password": {string(adminSecret)}}
		request, _ := http.NewRequestWithContext(ctx, "POST", origin+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, requestErr := client.Do(request)
		if requestErr == nil {
			var result struct {
				AccessToken string `json:"access_token"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, maximumResponse)).Decode(&result)
			response.Body.Close()
			if response.StatusCode == 200 && decodeErr == nil && result.AccessToken != "" {
				adminToken = result.AccessToken
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("isolated issuer readiness timed out")
		case <-time.After(time.Second):
		}
	}
	if adminToken == "" {
		t.Fatal("isolated issuer did not become ready")
	}
	admin := func(method, path string, body any) map[string]any {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded, _ = json.Marshal(body)
		}
		request, _ := http.NewRequestWithContext(ctx, method, origin+"/admin/realms/"+realm+path, bytes.NewReader(encoded))
		request.Header.Set("Authorization", "Bearer "+adminToken)
		request.Header.Set("Content-Type", "application/json")
		response, callErr := client.Do(request)
		if callErr != nil {
			t.Fatal("isolated admin request failed")
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("isolated admin status %d", response.StatusCode)
		}
		var result map[string]any
		if response.StatusCode != 204 {
			if json.NewDecoder(io.LimitReader(response.Body, maximumResponse)).Decode(&result) != nil {
				t.Fatal("invalid isolated admin response")
			}
		}
		return result
	}
	// Resolve IDs through the actual Admin API, never from unverified JWT claims.
	lookup := func(clientID string) (string, string, string) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, "GET", origin+"/admin/realms/"+realm+"/clients?clientId="+url.QueryEscape(clientID), nil)
		request.Header.Set("Authorization", "Bearer "+adminToken)
		response, callErr := client.Do(request)
		if callErr != nil {
			t.Fatal("isolated client lookup failed")
		}
		defer response.Body.Close()
		var clients []struct {
			ID string `json:"id"`
		}
		if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, maximumResponse)).Decode(&clients) != nil || len(clients) != 1 {
			t.Fatal("isolated client missing or ambiguous")
		}
		id := clients[0].ID
		subject, _ := admin("GET", "/clients/"+id+"/service-account-user", nil)["id"].(string)
		secret, _ := admin("GET", "/clients/"+id+"/client-secret", nil)["value"].(string)
		if subject == "" || len(secret) < 16 {
			t.Fatal("isolated service account or secret missing")
		}
		return id, subject, secret
	}
	id, subject, secret := lookup("account-service")
	for _, clientID := range []string{"account-service", "identity-service", "billing-service", "token-gateway", "token-command-executor", "token-credential-portal", "keycloak-mobile-authenticator"} {
		clientIDValue, _, _ := lookup(clientID)
		actual := admin("GET", "/clients/"+clientIDValue, nil)
		if actual["serviceAccountsEnabled"] != true || actual["publicClient"] != false || actual["standardFlowEnabled"] != false || actual["implicitFlowEnabled"] != false || actual["directAccessGrantsEnabled"] != false || actual["fullScopeAllowed"] != false {
			t.Fatal("imported client authorization mode differs from workload policy")
		}
		for _, field := range []string{"defaultClientScopes", "optionalClientScopes", "redirectUris", "webOrigins"} {
			if values, ok := actual[field].([]any); ok && len(values) != 0 {
				t.Fatalf("imported fixture client gained unexpected %s (count %d)", field, len(values))
			}
		}
	}
	// Verify the real issuer does not return refresh tokens or accept offline
	// access/password/device grants for these clients; metadata alone is not proof.
	grant := func(endpoint string, extras url.Values) (int, map[string]any) {
		t.Helper()
		form := url.Values{"client_id": {"account-service"}, "client_secret": {secret}}
		for key, value := range extras {
			form[key] = value
		}
		request, _ := http.NewRequestWithContext(ctx, "POST", issuer+"/protocol/openid-connect/"+endpoint, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, callErr := client.Do(request)
		if callErr != nil {
			t.Fatal("isolated grant probe failed")
		}
		defer response.Body.Close()
		var result map[string]any
		if json.NewDecoder(io.LimitReader(response.Body, maximumResponse)).Decode(&result) != nil {
			t.Fatal("invalid isolated grant response")
		}
		return response.StatusCode, result
	}
	status, tokenResponse := grant("token", url.Values{"grant_type": {"client_credentials"}})
	if status != 200 || tokenResponse["refresh_token"] != nil {
		t.Fatal("workload grant failed or returned refresh token")
	}
	for _, form := range []url.Values{
		{"grant_type": {"password"}, "username": {"fixture-user"}, "password": {"fixture-unused-password"}},
		{"grant_type": {"client_credentials"}, "scope": {"offline_access"}},
		{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {tokenResponse["access_token"].(string)}, "subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}},
	} {
		status, _ := grant("token", form)
		if status != 400 && status != 403 {
			t.Fatal("disabled workload grant was not rejected")
		}
	}
	if status, _ := grant("auth/device", nil); status != 400 && status != 403 {
		t.Fatal("device authorization was not rejected")
	}
	root := t.TempDir()
	for _, name := range []string{"bootstrap", "projected", "other"} {
		if os.Mkdir(filepath.Join(root, name), 0o700) != nil {
			t.Fatal("fixture directory failed")
		}
	}
	config := Config{Issuer: issuer, Audience: audience, ClientID: "account-service", Subject: subject, ClientSecretFile: filepath.Join(root, "bootstrap", "secret"), TokenFile: filepath.Join(root, "projected", "token")}
	if os.WriteFile(config.ClientSecretFile, []byte(secret), 0o600) != nil {
		t.Fatal("fixture secret write failed")
	}
	agent, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := agent.Refresh(ctx)
	if err != nil {
		t.Fatal("real Keycloak refresh failed", err)
	}
	if expiry.Sub(time.Now()) < 4*time.Minute || expiry.Sub(time.Now()) > 5*time.Minute {
		t.Fatal("real Keycloak lifetime differs from 300 second policy")
	}
	first, _ := os.ReadFile(config.TokenFile)
	if err := agent.Check(ctx); err != nil {
		t.Fatal("real Keycloak token health failed")
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer+"/protocol/openid-connect/certs")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	rules, _ := json.Marshal([]map[string]any{{"subject": subject, "service": "account-service", "permissions": []string{"internal.fixture.allowed"}}})
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
	router := gin.New()
	router.Use(serviceauth.GinMiddleware(""))
	for _, route := range []string{"allowed", "denied"} {
		router.GET("/"+route, serviceauth.Require("internal.fixture."+route), func(c *gin.Context) { c.Status(204) })
	}
	checkRoute := func(raw []byte, path string, want int) {
		t.Helper()
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", "Bearer "+string(raw))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("real workload route status = %d, want %d", response.Code, want)
		}
	}
	checkRoute(first, "/allowed", 204)
	checkRoute(first, "/denied", 403)
	for _, mutate := range []func(*Config){func(c *Config) { c.Subject = "wrong-subject" }, func(c *Config) { c.Audience = "wrong-audience" }} {
		bad := config
		mutate(&bad)
		other, err := New(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = other.Refresh(ctx); err == nil {
			t.Fatal("real mismatched identity accepted")
		}
	}
	_, otherSubject, otherSecret := lookup("billing-service")
	otherConfig := config
	otherConfig.ClientID, otherConfig.Subject = "billing-service", otherSubject
	otherConfig.ClientSecretFile = filepath.Join(root, "bootstrap", "other-secret")
	otherConfig.TokenFile = filepath.Join(root, "other", "token")
	if os.WriteFile(otherConfig.ClientSecretFile, []byte(otherSecret), 0o600) != nil {
		t.Fatal("other fixture secret write failed")
	}
	otherAgent, err := New(otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = otherAgent.Refresh(ctx); err != nil {
		t.Fatal("second real service refresh failed")
	}
	otherRaw, _ := os.ReadFile(otherConfig.TokenFile)
	checkRoute(otherRaw, "/allowed", 403)
	// Rotate the actual client secret and prove the agent reloads its file.
	rotated, _ := admin("POST", "/clients/"+id+"/client-secret", nil)["value"].(string)
	if rotated == "" || rotated == secret {
		t.Fatal("real client secret rotation failed")
	}
	if _, err = agent.Refresh(ctx); err == nil {
		t.Fatal("retired client secret still accepted")
	}
	if os.WriteFile(config.ClientSecretFile, []byte(rotated), 0o600) != nil {
		t.Fatal("rotated fixture secret write failed")
	}
	if _, err = agent.Refresh(ctx); err != nil {
		t.Fatal("rotated client credential did not reload")
	}
	second, _ := os.ReadFile(config.TokenFile)
	if bytes.Equal(first, second) {
		t.Fatal("real token was not renewed")
	}
	checkRoute(second, "/allowed", 204)
	// A workload identity cannot administer its issuer.
	request, _ := http.NewRequestWithContext(ctx, "GET", origin+"/admin/realms/"+realm, nil)
	request.Header.Set("Authorization", "Bearer "+string(second))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("issuer permission probe failed")
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("workload token unexpectedly accessed issuer administration")
	}
	// Only this disposable client is shortened, to exercise actual wall-clock
	// expiry without fabricating or re-signing a Keycloak token.
	clientConfig := admin("GET", "/clients/"+id, nil)
	attributes := clientConfig["attributes"].(map[string]any)
	attributes["access.token.lifespan"] = "40"
	admin("PUT", "/clients/"+id, clientConfig)
	shortExpiry, err := agent.Refresh(ctx)
	if err != nil || time.Until(shortExpiry) > 40*time.Second {
		t.Fatal("short isolated expiry fixture failed")
	}
	shortRaw, _ := os.ReadFile(config.TokenFile)
	clientConfig["enabled"] = false
	admin("PUT", "/clients/"+id, clientConfig)
	if _, err = agent.Refresh(ctx); err == nil {
		t.Fatal("disabled real client still issued token")
	}
	retained, _ := os.ReadFile(config.TokenFile)
	if !bytes.Equal(shortRaw, retained) {
		t.Fatal("failed renewal replaced existing projection")
	}
	checkRoute(retained, "/allowed", 204)
	t.Log("real issuance, UUID binding, permission refusal and rotation passed; awaiting actual expiry")
	select {
	case <-ctx.Done():
		t.Fatal("expiry fixture deadline exceeded")
	case <-time.After(time.Until(shortExpiry) + time.Second):
	}
	if agent.Check(ctx) == nil {
		t.Fatal("expired real token remained healthy")
	}
	checkRoute(retained, "/allowed", 401)
}
