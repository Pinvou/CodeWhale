package portal

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagedPricePageProxyPreservesCursor(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "GET" || r.URL.Path != "/admin/v1/pricing" || r.URL.RawQuery != "limit=100&cursor=public_seek_cursor" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" {
			t.Error("page query or server session changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[],"has_more":false,"next_cursor":""}`))
	}))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	req := financeBrowserRequest(service, session, "GET", "/api/admin/billing/admin/v1/pricing?limit=100&cursor=public_seek_cursor", "")
	out := httptest.NewRecorder()
	service.Handler(http.NotFoundHandler()).ServeHTTP(out, req)
	if out.Code != 200 || !called || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pricing page proxy failed")
	}
}
