package identityadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestIndependentBootstrapNullUserMustReturnErrorNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"access_token":"fixture-token","expires_in":300}`))
			return
		}
		if r.Method == "GET" {
			w.Write([]byte(`null`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	a, _ := url.Parse(server.URL)
	u, _ := url.Parse(server.URL + "/token")
	d := &KeycloakDirectory{adminURL: a, tokenURL: u, realm: "pinvou", clientID: "fixture", clientSecret: "fixture", client: server.Client()}
	defer func() {
		if recover() != nil {
			t.Fatal("invalid user authority caused panic after successful password step")
		}
	}()
	if err := d.ActivateBootstrapAdmin(context.Background(), "fixture", "fixture-user", "R3coverySafe!2026"); err == nil {
		t.Fatal("invalid user authority accepted")
	}
}
