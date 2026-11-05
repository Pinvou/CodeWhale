package identityadapter

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshFailureDoesNotLockPasswordAliasUser(t *testing.T) {
	ctx := context.Background()
	store := &identifierRiskStore{values: map[string]string{}}
	a, err := NewRedisTokenAdmission("redis://127.0.0.1:1/0", base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("r", 32))))
	if err != nil {
		t.Fatal(err)
	}
	a.client.AddHook(store)
	t.Cleanup(func() { _ = a.client.Close() })
	// This is an allowed registration username, not a reserved internal name.
	password := tokenRiskDimensions{action: "password", identifier: "refresh-session"}
	if _, err := a.Complete(ctx, password, true, "independent-password-user"); err != nil {
		t.Fatal(err)
	}
	aliasKey := "pinvou:auth-alias:" + a.digest(password.identifier)
	stableKey := "pinvou:auth-failure:user:" + a.digest("independent-password-user")
	alias := store.values[aliasKey]
	if alias == "" {
		t.Fatal("password success did not establish the prerequisite alias")
	}

	// Obtain the dimensions from the actual HTTP producer. Only its admission
	// sink is controlled here to avoid spending real failed-response delays;
	// below, the real RedisTokenAdmission consumes those produced dimensions.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "independent-session" {
			t.Error("refresh input was not forwarded unchanged")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer upstream.Close()
	capture := &admissionFake{}
	proxy, err := NewTokenProxy(upstream.URL, capture)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0
	r := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=refresh_token&refresh_token=independent-session"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	refresh := capture.dimensions
	if w.Code != http.StatusUnauthorized || capture.completions != 1 || refresh.action != "refresh" ||
		refresh.identifier != password.identifier || refresh.session != "independent-session" {
		t.Fatal("actual refresh failure did not reach the expected risk boundary")
	}
	for range 10 {
		if _, err := a.Complete(ctx, refresh, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	if store.values["pinvou:auth-failure:refresh:"+a.digest(refresh.session)] != "10" {
		t.Fatal("refresh must retain its own session failure history")
	}
	if store.values[stableKey] != "" || store.values["pinvou:auth-failure:password:"+a.digest(password.identifier)] != "" {
		t.Fatal("unrelated refresh failures changed password-user risk")
	}
	if locked, err := a.RecheckStableLock(ctx, password); err != nil || locked != 0 {
		t.Fatal("unrelated refresh failures locked the password user", locked, err)
	}
	if _, err := a.Complete(ctx, refresh, true, "different-refresh-user"); err != nil || store.values[aliasKey] != alias {
		t.Fatal("refresh success changed the password alias", err)
	}
	// Preserve the chosen policy: actual password failures still increment the
	// learned stable user; success does not erase history; the tenth is locked.
	store.values[stableKey] = "9"
	if _, err := a.Complete(ctx, password, true, "independent-password-user"); err != nil || store.values[stableKey] != "9" {
		t.Fatal("successful login cleared existing risk", err)
	}
	if _, err := a.Complete(ctx, password, false, ""); err != nil || store.values[stableKey] != "10" {
		t.Fatal("password failure no longer counts against its stable user", err)
	}
	if locked, err := a.RecheckStableLock(ctx, password); err != nil || locked != 15*time.Minute {
		t.Fatal("existing stable-user lock policy changed", locked, err)
	}
}
