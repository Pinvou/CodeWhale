package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcallback"
	"github.com/google/uuid"
)

type unreadAdmissionBody struct{ reads int }

func (b *unreadAdmissionBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*unreadAdmissionBody) Close() error               { return nil }

func TestCallbackAdmissionDoesNotConsumeBodyOrTrustClaims(t *testing.T) {
	service, auth := admissionFixture(t, nil)
	token, err := auth.Issue("client-a", strings.Repeat("a", 32), paymentcallback.Scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/payment-callback/token", "/api/v1/payment-callbacks"} {
		body := &unreadAdmissionBody{}
		r := httptest.NewRequest("POST", path, body)
		if strings.HasSuffix(path, "/token") {
			r.SetBasicAuth("client-a", strings.Repeat("a", 32))
		} else {
			r.Header.Set("Authorization", "Bearer "+token.AccessToken)
		}
		class, principal := service.callbackAdmission(r)
		if principal != "client-a" || (class != httpserver.VerifiedCallbackAdmission && class != httpserver.VerifiedCallbackTokenAdmission) || body.reads != 0 {
			t.Fatal("pure verified callback admission changed body or principal", class, body.reads)
		}
		r.SetBasicAuth("unconfigured-client", strings.Repeat("a", 32))
		class, principal = service.callbackAdmission(r)
		if class != httpserver.InvalidCallbackAdmission || principal != "" || body.reads != 0 {
			t.Fatal("unverified claim selected a principal or consumed body")
		}
	}
}

func alternateSignedAdmissionToken(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	// A valid representation of the same claims, independently signed with the
	// controlled fixture key; different JWT bytes must retain one principal bucket.
	unsigned := parts[0] + "." + base64.RawURLEncoding.EncodeToString(append([]byte(" "), claims...))
	mac := hmac.New(sha256.New, []byte(strings.Repeat("fixture", 6)))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type admissionFixtureProvider struct{ payment.Provider }

func (*admissionFixtureProvider) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{ProviderID: "controlled", Version: "v1", Channels: []string{"ALIPAY"}, NotificationSupported: true, TestOnly: true}
}

type admissionFixtureProfile struct{}

func (admissionFixtureProfile) ProviderID() string      { return "controlled" }
func (admissionFixtureProfile) ProviderVersion() string { return "v1" }
func (admissionFixtureProfile) ParseNotification(body []byte, now time.Time) (paymentcallback.Notification, error) {
	var input struct {
		ID uuid.UUID `json:"id"`
	}
	if json.Unmarshal(body, &input) != nil || input.ID == uuid.Nil {
		return paymentcallback.Notification{}, errors.New("controlled fixture requires notification ID")
	}
	return paymentcallback.Notification{ID: input.ID, SentAt: now, Provider: ProviderOrder{MerchantOrderNo: "fixture-" + input.ID.String(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", Status: "PENDING"}}, nil
}
func admissionFixture(t *testing.T, repository *PostgresRepository) (*Service, *paymentcallback.Authenticator) {
	t.Helper()
	if repository == nil {
		repository = &PostgresRepository{}
	}
	coordinator, err := NewPaymentCoordinatorWithProvider(repository, &admissionFixtureProvider{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.ConfigureNotifications("controlled-merchant", "https://provider.invalid/callback", map[string]string{"ALIPAY": strings.Repeat("fixture", 6)}); err != nil {
		t.Fatal(err)
	}
	auth, err := paymentcallback.NewAuthenticator(paymentcallback.AuthConfig{KeyID: "controlled-key", SigningKey: []byte(strings.Repeat("fixture", 6)), Clients: []paymentcallback.ClientConfig{
		{ClientID: "client-a", ClientSecret: strings.Repeat("a", 32), ProviderID: "controlled", ProviderVersion: "v1", MerchantID: "controlled-merchant"},
		{ClientID: "client-b", ClientSecret: strings.Repeat("b", 32), ProviderID: "controlled", ProviderVersion: "v1", MerchantID: "controlled-merchant"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := paymentcallback.NewProfiles(admissionFixtureProfile{})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(repository).WithPayments(coordinator).WithPaymentCallbacks(auth, profiles), auth
}
func admissionHTTP(t *testing.T, service *Service) http.Handler {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("RATE_LIMIT_RPS", "0.001")
	t.Setenv("RATE_LIMIT_BURST", "1")
	t.Setenv("HTTP_TRUSTED_PROXY_IPS", "192.0.2.1")
	cfg, err := config.Load("billing-service")
	if err != nil {
		t.Fatal(err)
	}
	return httpserver.HandlerWithApplication(cfg, service.Handler("controlled-unused-service"), nil)
}
func admissionRequest(h http.Handler, path, authorization, source, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:40000"
	r.Header.Set("X-Pinvou-RateLimit-Source", source)
	r.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
	r.Header.Set("X-Forwarded-For", "203.0.113.202")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", authorization)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func admissionToken(t *testing.T, h http.Handler, id, secret, source string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/payment-callback/token", strings.NewReader("grant_type=client_credentials"))
	r.RemoteAddr = "192.0.2.1:40000"
	r.SetBasicAuth(id, secret)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Pinvou-RateLimit-Source", source)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("actual token endpoint=%d", w.Code)
	}
	var token paymentcallback.Token
	if json.Unmarshal(w.Body.Bytes(), &token) != nil || token.AccessToken == "" {
		t.Fatal("actual token response invalid")
	}
	return token.AccessToken
}
func TestCallbackHTTPAdmissionBudgetIsolation(t *testing.T) {
	service, _ := admissionFixture(t, nil)
	h := admissionHTTP(t, service)
	a := admissionToken(t, h, "client-a", strings.Repeat("a", 32), "198.51.100.1")
	b := admissionToken(t, h, "client-b", strings.Repeat("b", 32), "198.51.100.1")
	// Spend ordinary and invalid budgets without spending authenticated callbacks.
	for i, want := range []int{404, 429} {
		if got := admissionRequest(h, "/ordinary", "", "198.51.100.1", "").Code; got != want {
			t.Fatalf("ordinary %d=%d", i, got)
		}
	}
	for i, want := range []int{401, 429} {
		if got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer forged", "198.51.100.1", "{}").Code; got != want {
			t.Fatalf("invalid %d=%d", i, got)
		}
	}
	// Parse rejection happens after real HMAC verification but before persistence;
	// the unit fixture never opens a database or changes financial facts.
	for i := 0; i < 200; i++ {
		if got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+a, "198.51.100.1", "{}").Code; got != 400 {
			t.Fatalf("authenticated callback below200 burst=%d at%d", got, i)
		}
	}
	limited := false
	for i := 0; i < 800; i++ {
		got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+a, "198.51.100.1", "{}").Code
		if got == 429 {
			limited = true
			break
		}
		if got != 400 {
			t.Fatal("unexpected callback status", got)
		}
	}
	if !limited {
		t.Fatal("authenticated callback bucket never limits")
	}
	rotated, stillLimited := a, false
	for i := 0; i < 20; i++ {
		rotated = alternateSignedAdmissionToken(t, rotated)
		got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+rotated, "198.51.100.1", "{}").Code
		if got == 429 {
			stillLimited = true
			break
		}
		if got != 400 {
			t.Fatal("alternate signed token rejected before callback parse", got)
		}
	}
	if !stillLimited {
		t.Fatal("different signed JWT bytes opened fresh principal buckets")
	}
	for _, tc := range []struct{ token, source string }{{b, "198.51.100.1"}, {a, "198.51.100.2"}} {
		if got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+tc.token, tc.source, "{}").Code; got != 400 {
			t.Fatal("verified principal/source coupled", got)
		}
	}
	// Token issuance retains its own authenticated budget after callback exhaustion.
	_ = admissionToken(t, h, "client-a", strings.Repeat("a", 32), "198.51.100.1")
}
func TestCallbackHTTPAdmissionPersistsPostgres(t *testing.T) {
	repository, ctx := retainedFinanceRepository(t)
	var restricted bool
	if err := repository.pool.QueryRow(ctx, `SELECT current_user='billing_service' AND session_user='billing_service' AND NOT (SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user) AND NOT pg_has_role(current_user,'billing_schema_owner','MEMBER') AND NOT has_schema_privilege(current_user,'public','CREATE')`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("actual callback runtime actor is not restricted", err)
	}
	service, _ := admissionFixture(t, repository)
	h := admissionHTTP(t, service)
	a := admissionToken(t, h, "client-a", strings.Repeat("a", 32), "198.51.100.1")
	for _, path := range []string{"/ordinary", "/ordinary"} {
		_ = admissionRequest(h, path, "", "198.51.100.1", "")
	}
	for i := 0; i < 3; i++ {
		_ = admissionRequest(h, "/api/v1/payment-callbacks", "Bearer forged", "198.51.100.1", "{}")
	}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	for i, id := range ids {
		body := fmt.Sprintf(`{"id":%q}`, id.String())
		source := fmt.Sprintf("198.51.100.%d", i+1)
		w := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+a, source, body)
		if w.Code != 202 {
			t.Fatalf("actual authenticated persistence status=%d", w.Code)
		}
	}
	var received, risk int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM payment_notifications WHERE result='RECEIVED'`).Scan(&received); err != nil || received != 2 {
		t.Fatal("actual callback notifications not durably received", received, err)
	}
	for _, source := range []string{"198.51.100.1", "198.51.100.2"} {
		digest := sha256.Sum256([]byte(source))
		if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='CALLBACK' AND source_hash=$1`, digest[:]).Scan(&risk); err != nil || risk != 1 {
			t.Fatal("callback audit did not use admitted source", risk, err)
		}
	}
	// Retained financial idempotency: replay leaves one notification and no new risk fact.
	if got := admissionRequest(h, "/api/v1/payment-callbacks", "Bearer "+a, "198.51.100.1", fmt.Sprintf(`{"id":%q}`, ids[0].String())).Code; got != 202 {
		t.Fatal("retained callback replay failed", got)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM payment_notifications`).Scan(&received); err != nil || received != 2 {
		t.Fatal("callback replay duplicated facts", received, err)
	}
}
