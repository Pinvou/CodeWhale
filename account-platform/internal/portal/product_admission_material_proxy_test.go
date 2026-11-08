package portal

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type materialProxyRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *materialProxyRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

type materialProxyTransport func(*http.Request) (*http.Response, error)

func (transport materialProxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestProductAdmissionMaterialPortalBoundary(t *testing.T) {
	base := "/admin/v1/recharge-products/" + uuid.NewString()
	material := uuid.NewString()
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"POST", base + "/admission-materials", true},
		{"GET", base + "/admission-materials/" + material, true},
		{"HEAD", base + "/admission-materials/" + material + "/content", true},
		{"POST", base + "/admission-materials/" + material + "/legal-hold", true},
		{"GET", base + "/admission-material-receipts/" + uuid.NewString(), true},
		{"DELETE", base + "/admission-materials/" + material, false},
		{"POST", base + "/admission-materials/" + material + "/content", false},
		{"GET", base + "/admission-materials/not-a-uuid/content", false},
		{"GET", base + "/admission-material-receipts/short", false},
		{"POST", "/admin/v1/wallets/" + material + "/admission-materials", false},
	} {
		if productAdmissionMaterialPathAllowed(tc.method, tc.path) != tc.allowed {
			t.Fatalf("%s %s", tc.method, tc.path)
		}
	}
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	allowed := false
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("browser identity was forwarded")
		}
		permission := "platform.products.read"
		if allowed {
			permission = "platform.products.publish"
		}
		_, _ = io.WriteString(w, `{"user_id":"11111111-1111-4111-8111-111111111111","status":"ACTIVE","platform_permissions":["`+permission+`"]}`)
	}))
	defer account.Close()
	service.config.AccountURL = parseTestURL(t, account.URL)
	calls := 0
	data := bytes.Repeat([]byte{0, 255, 128, 1}, (10<<20)/4)
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == base+"/publish" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(raw, data) || r.Header.Get("Content-Type") != "image/png" {
			t.Error("binary body changed")
		}
		w.WriteHeader(200)
	}))
	defer billing.Close()
	service.config.BillingURL = parseTestURL(t, billing.URL)
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "token-portal", Environment: "test"}, service.Handler(nil), nil)
	transport := service.client.Transport
	service.client.Transport = materialProxyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == service.config.BillingURL.Host {
			deadline, bounded := r.Context().Deadline()
			want := 15 * time.Second
			if productAdmissionMaterialUpload(r.Method, r.URL.Path) {
				want = 70 * time.Second
			}
			if remaining := time.Until(deadline); !bounded || remaining < want-2*time.Second || remaining > want {
				t.Errorf("unexpected Billing request budget for %s: %v", r.URL.Path, remaining)
			}
		}
		return transport.RoundTrip(r)
	})
	r := financeBrowserRequest(service, session, "POST", "/api/admin/billing"+base+"/admission-materials", "")
	body := &importCountingBody{}
	r.Body = body
	w := &materialProxyRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(w, r)
	if w.Code != 403 || body.reads != 0 || calls != 0 || !w.deadline.IsZero() {
		t.Fatalf("preflight status=%d reads=%d calls=%d", w.Code, body.reads, calls)
	}
	allowed = true
	for _, tc := range []struct {
		suffix string
		body   []byte
		want   int
	}{
		{"/admission-materials", data, 200},
		{"/publish", data, 413},
		{"/publish", []byte(`{}`), 204},
		{"/admission-materials", make([]byte, (10<<20)+1), 413},
	} {
		r := financeBrowserRequest(service, session, "POST", "/api/admin/billing"+base+tc.suffix, "")
		r.Body = io.NopCloser(bytes.NewReader(tc.body))
		r.Header.Set("Content-Type", "image/png")
		w := &materialProxyRecorder{ResponseRecorder: httptest.NewRecorder()}
		started := time.Now()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s status=%d", tc.suffix, w.Code)
		}
		if tc.suffix == "/admission-materials" {
			if w.deadline.Before(started.Add(105*time.Second)) || w.deadline.After(time.Now().Add(105*time.Second)) {
				t.Fatalf("upload response deadline = %v", w.deadline)
			}
		} else if !w.deadline.IsZero() {
			t.Fatal("ordinary response deadline changed")
		}
	}
	if calls != 2 || service.client.Timeout != 15*time.Second {
		t.Fatal("unexpected Billing calls or altered shared timeout", calls, service.client.Timeout)
	}
}

func TestProductAdmissionOriginalProxyRejectsPartialResponse(t *testing.T) {
	for _, tc := range []struct {
		name, media, body string
		length            int64
		want              int
	}{
		{"whole", "application/pdf", "original", 8, 200},
		{"short", "application/pdf", "secret", 8, 502},
		{"long", "image/png", "private", 3, 502},
		{"html", "text/html", "private", 7, 502},
		{"unknown-length", "image/jpeg", "private", -1, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/original", nil)
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.media}}, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.body))}
			w := httptest.NewRecorder()
			forwardProductAdmissionOriginal(w, r, response)
			if w.Code != tc.want {
				t.Fatal(w.Code)
			}
			if tc.want != 200 && strings.Contains(w.Body.String(), tc.body) {
				t.Fatal("partial original disclosed")
			}
			if tc.want == 200 && (w.Body.String() != tc.body || w.Header().Get("Content-Length") != strconv.FormatInt(tc.length, 10) || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;")) {
				t.Fatal("attachment contract")
			}
		})
	}
}
