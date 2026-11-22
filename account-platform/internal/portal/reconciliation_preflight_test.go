package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type importCountingBody struct{ reads int }

func (b *importCountingBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*importCountingBody) Close() error               { return nil }

func TestImportPreflightRejectsBeforeReadingBrowserBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		want   int
	}{
		{"usage-only", 200, `{"user_id":"11111111-1111-4111-8111-111111111111","status":"ACTIVE","platform_permissions":["platform.usage.read","platform.usage.reconcile.retry"]}`, 403},
		{"unavailable", 503, `private failure`, 503},
		{"invalid", 200, `{`, 503},
		{"missing-principal", 200, `{"platform_permissions":["platform.billing.reconciliation.run"]}`, 503},
		{"redirect", 302, ``, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirect := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("Account redirect leaked a session request") }))
			defer redirect.Close()
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
					t.Error("untrusted identity reached Account")
				}
				if tc.status == http.StatusFound {
					w.Header().Set("Location", redirect.URL)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer account.Close()
			service.config.AccountURL = parseTestURL(t, account.URL)
			billing := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("denied request reached Billing") }))
			defer billing.Close()
			service.config.BillingURL = parseTestURL(t, billing.URL)
			r := financeBrowserRequest(service, session, http.MethodPost, "/api/admin/billing/admin/v1/reconciliation-imports", "")
			body := &importCountingBody{}
			r.Body = body
			r.Header.Set("Authorization", "Bearer browser-forged")
			r.Header.Set("X-Platform-Permissions", "platform.billing.reconciliation.run")
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != tc.want || body.reads != 0 || strings.Contains(w.Body.String(), "private failure") {
				t.Fatalf("status=%d reads=%d", w.Code, body.reads)
			}
		})
	}
}

func TestOnlyAuthorizedImportReceivesLargerPortalBudget(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"user_id":"11111111-1111-4111-8111-111111111111","status":"ACTIVE","platform_permissions":["platform.billing.reconciliation.run"]}`)
	}))
	defer account.Close()
	service.config.AccountURL = parseTestURL(t, account.URL)
	body := strings.Repeat(" ", 4<<20) + `{}`
	calls := 0
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != body || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("large import authority or bytes changed")
		}
		w.WriteHeader(201)
	}))
	defer billing.Close()
	service.config.BillingURL = parseTestURL(t, billing.URL)
	for _, path := range []string{"reconciliation-imports", "reconciliation-runs"} {
		r := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/"+path, body)
		w := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(w, r)
		want := 201
		if path != "reconciliation-imports" {
			want = 413
		}
		if w.Code != want {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d", calls)
	}
}
