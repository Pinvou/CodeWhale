package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Neither endpoint exercised here sends SMS or provisions an identity. Embedding
// unconfigured interfaces deliberately panics if that boundary changes.
type sourceFixtureSMS struct{ account.SMSProvider }
type sourceFixtureIdentity struct{ account.IdentityProvisioner }

func TestPortalAccountPersistentPublicSourceAdmission(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture connection failed")
	}
	defer connection.Close(context.Background())
	schema := "portal_source_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := connection.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("fixture cleanup failed")
		}
	}()
	if _, err = connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"000089_public_mobile_admission.up.sql", "000009_slider_captcha.up.sql"} {
		body, err := os.ReadFile("../../migrations/account/" + filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = connection.Exec(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	fixtureURL, err := url.Parse(raw)
	if err != nil {
		t.Fatal("fixture URL invalid")
	}
	query := fixtureURL.Query()
	query.Set("search_path", schema)
	fixtureURL.RawQuery = query.Encode()
	repository, err := account.NewPostgresRepository(ctx, fixtureURL.String())
	if err != nil {
		t.Fatal("fixture repository failed")
	}
	defer repository.Close()
	signer, err := account.NewRegistrationTokenSigner([]byte(strings.Repeat("controlled-fixture-", 3)))
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := account.NewMobileAuthService(repository, sourceFixtureSMS{}, sourceFixtureIdentity{}, "https://identity.example", signer)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := account.NewTrustedSourceHandler(account.NewService(nil, nil).WithMobileAuth(mobile, "controlled-fixture-service-token-123456").Handler(nil), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"X-Device-ID", "X-Registration-Query-Token", "Authorization", "Cookie", "X-Forwarded-For", "X-Internal-Service-Token"} {
			if r.Header.Get(name) != "" {
				t.Errorf("unexpected browser authority header %s", name)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	newPortal := func() http.Handler {
		cfg := testService(t, newMemoryStore()).config
		cfg.AccountURL = parseTestURL(t, upstream.URL)
		cfg.TrustedProxyIPs = []netip.Addr{netip.MustParseAddr("192.0.2.45")}
		service, err := NewService(cfg, newMemoryStore())
		if err != nil {
			t.Fatal(err)
		}
		return service.Handler(nil)
	}
	portal := newPortal()
	request := func(portal http.Handler, index int, remote, claimedSource string, captcha bool, want int) {
		route := "/v1/auth/registration/precheck"
		// Invalid username intentionally proves admission precedes validation,
		// without contacting an unrelated identity service. Captcha uses the real
		// image generator and repository and must successfully return201.
		body := fmt.Sprintf(`{"username":"!","mobile":"139%08d"}`, index)
		if captcha {
			route = "/v1/auth/captcha/challenges"
			body = fmt.Sprintf(`{"mobile":"139%08d","purpose":"LOGIN"}`, index)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/account"+route, strings.NewReader(body)).WithContext(ctx)
		r.RemoteAddr = remote
		r.Header.Set("Origin", testService(t, newMemoryStore()).config.PublicURL.String())
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Pinvou-Client-Source", claimedSource)
		for _, name := range []string{"X-Device-ID", "X-Registration-Query-Token", "Authorization", "Cookie", "X-Forwarded-For", "X-Internal-Service-Token"} {
			r.Header.Set(name, "controlled-untrusted-value")
		}
		w := httptest.NewRecorder()
		portal.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("request%d captcha=%v status=%d want=%d", index, captcha, w.Code, want)
		}
		if want == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Fatal("rate-limit cooldown lost in Portal")
		}
	}
	for i := range 30 {
		captcha := i%2 == 0
		want := http.StatusBadRequest
		if captcha {
			want = http.StatusCreated
		}
		request(portal, i, "192.0.2.45:443", "203.0.113.10", captcha, want)
	}
	request(portal, 30, "192.0.2.45:443", "203.0.113.10", false, http.StatusTooManyRequests)
	request(portal, 31, "192.0.2.45:443", "203.0.113.11", true, http.StatusCreated)
	// Replacement Portal instance and endpoint changes cannot reset Account quota.
	request(newPortal(), 32, "192.0.2.45:443", "203.0.113.10", true, http.StatusTooManyRequests)
	// An untrusted immediate peer cannot rotate buckets through browser headers.
	for i := range 30 {
		request(portal, 100+i, "198.51.100.45:443", fmt.Sprintf("203.0.113.%d", 20+i), false, http.StatusBadRequest)
	}
	request(newPortal(), 130, "198.51.100.45:443", "203.0.113.90", true, http.StatusTooManyRequests)
}
