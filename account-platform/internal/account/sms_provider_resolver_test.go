package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

type smsConfigurationStoreFake struct {
	active   SMSConfiguration
	found    bool
	versions map[int64]SMSConfiguration
}

func (f *smsConfigurationStoreFake) ActiveSMSConfiguration(context.Context) (SMSConfiguration, bool, error) {
	return f.active, f.found, nil
}

func (f *smsConfigurationStoreFake) SMSConfigurationVersion(_ context.Context, revision int64) (SMSConfiguration, error) {
	configuration, ok := f.versions[revision]
	if !ok {
		return SMSConfiguration{}, ErrSMSUnavailable
	}
	return configuration, nil
}

func (*smsConfigurationStoreFake) UpdateSMSConfiguration(context.Context, identity.Principal, SMSConfiguration, int64, string, string) (SMSConfiguration, bool, error) {
	panic("not used")
}

func testSMSConfiguration(revision int64, enabled bool, providerID string) SMSConfiguration {
	configuration := SMSConfiguration{Revision: revision, Enabled: enabled, ProviderID: providerID, AdapterVersion: "v1"}
	if providerID == "aliyun-managed" {
		configuration.CredentialProfile = "aliyun-default"
		configuration.SignName = "Pinvou"
		configuration.Templates = SMSPurposeTemplates{Register: "register", Login: "login", ProfileMobile: "profile", PasswordReset: "reset"}
	} else {
		configuration.CredentialProfile = "http-default"
	}
	configuration.ConfigurationFingerprint = smsConfigurationFingerprint(configuration)
	return configuration
}

func TestVersionedSMSProviderResolverKeepsHistoricalChallengeRoute(t *testing.T) {
	defaultConfiguration := testSMSConfiguration(0, true, "http-generated")
	legacy, err := NewHTTPGeneratedSMSProvider(HTTPGeneratedSMSConfig{
		Endpoint:        "https://sms.example.test/send",
		VerificationKey: []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := testSMSConfiguration(1, true, "http-generated")
	second := testSMSConfiguration(2, true, "http-generated")
	store := &smsConfigurationStoreFake{active: second, found: true, versions: map[int64]SMSConfiguration{1: first, 2: second}}
	resolver, err := NewVersionedSMSProviderResolver(store, legacy, defaultConfiguration, []SMSProviderProfile{{
		ID: "http-default", ProviderID: "http-generated", Configured: true,
		HTTPGenerated: HTTPGeneratedSMSConfig{Endpoint: "https://sms.example.test/send", VerificationKey: []byte("01234567890123456789012345678901")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	active, err := resolver.ActiveProvider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := active.Descriptor(), second.descriptor(); got != want {
		t.Fatalf("active descriptor = %#v, want %#v", got, want)
	}
	historical, err := resolver.ProviderForVerification(context.Background(), first.descriptor())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := historical.Descriptor(), first.descriptor(); got != want {
		t.Fatalf("historical descriptor = %#v, want %#v", got, want)
	}
}

func TestVersionedSMSProviderResolverDisablesNewSendsButKeepsVerification(t *testing.T) {
	defaultConfiguration := testSMSConfiguration(0, true, "http-generated")
	legacy, err := NewHTTPGeneratedSMSProvider(HTTPGeneratedSMSConfig{
		Endpoint:        "https://sms.example.test/send",
		VerificationKey: []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := testSMSConfiguration(1, true, "http-generated")
	disabled := testSMSConfiguration(2, false, "http-generated")
	store := &smsConfigurationStoreFake{active: disabled, found: true, versions: map[int64]SMSConfiguration{1: first, 2: disabled}}
	resolver, err := NewVersionedSMSProviderResolver(store, legacy, defaultConfiguration, []SMSProviderProfile{{
		ID: "http-default", ProviderID: "http-generated", Configured: true,
		HTTPGenerated: HTTPGeneratedSMSConfig{Endpoint: "https://sms.example.test/send", VerificationKey: []byte("01234567890123456789012345678901")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = resolver.ActiveProvider(context.Background()); err != ErrSMSUnavailable {
		t.Fatalf("ActiveProvider error = %v, want %v", err, ErrSMSUnavailable)
	}
	if _, err = resolver.ProviderForSend(context.Background(), first.descriptor()); err != ErrSMSUnavailable {
		t.Fatalf("ProviderForSend error = %v, want %v", err, ErrSMSUnavailable)
	}
	if _, err = resolver.ProviderForVerification(context.Background(), first.descriptor()); err != nil {
		t.Fatalf("historical verification must remain available: %v", err)
	}
}

func TestNormalizeSMSConfigurationRejectsProviderProfileMismatch(t *testing.T) {
	configuration := testSMSConfiguration(0, true, "http-generated")
	configuration.CredentialProfile = "aliyun-default"
	if _, err := normalizeSMSConfiguration(configuration); err != ErrSMSConfigurationInvalid {
		t.Fatalf("normalize error = %v, want %v", err, ErrSMSConfigurationInvalid)
	}
}

func TestAdminSMSConfigurationRequiresPermissionAndReturnsCompleteTemplateShape(t *testing.T) {
	configuration := testSMSConfiguration(0, true, "http-generated")
	provider, err := NewHTTPGeneratedSMSProvider(HTTPGeneratedSMSConfig{
		Endpoint:        "https://sms.example.test/send",
		VerificationKey: []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &smsConfigurationStoreFake{active: configuration, found: false}
	resolver, err := NewVersionedSMSProviderResolver(store, provider, configuration, []SMSProviderProfile{{
		ID: "http-default", ProviderID: "http-generated", Configured: true,
		HTTPGenerated: HTTPGeneratedSMSConfig{Endpoint: "https://sms.example.test/send", VerificationKey: []byte("01234567890123456789012345678901")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{Issuer: "https://issuer.example", Subject: "operator", SessionID: "session", AuthenticatedAt: time.Now().UTC()}
	verify := func(context.Context, string) (identity.Principal, error) { return principal, nil }

	request := httptest.NewRequest(http.MethodGet, "/admin/v1/sms-configuration", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	NewService(&personalBoundaryRepository{userID: uuid.New(), deny: true}, &fakeWalletProvisioner{}).WithSMSConfiguration(resolver).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/v1/sms-configuration", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	NewService(&personalBoundaryRepository{userID: uuid.New()}, &fakeWalletProvisioner{}).WithSMSConfiguration(resolver).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%s", response.Code, response.Body.String())
	}
	for _, field := range []string{`"REGISTER":""`, `"LOGIN":""`, `"PROFILE_MOBILE":""`, `"PASSWORD_RESET":""`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("response is missing template field %s: %s", field, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "VerificationKey") || strings.Contains(response.Body.String(), "01234567890123456789012345678901") {
		t.Fatal("deployment credentials escaped the SMS projection")
	}
	request = httptest.NewRequest(http.MethodPut, "/admin/v1/sms-configuration", strings.NewReader(`{"broken"`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	NewService(&personalBoundaryRepository{userID: uuid.New()}, &fakeWalletProvisioner{}).WithSMSConfiguration(resolver).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
		t.Fatal("malformed SMS configuration did not fail before storage")
	}
	if response.Header().Get("X-Command-Type") != "ACCOUNT_SMS_CONFIGURATION_UPDATE" {
		t.Fatal("SMS mutation lost its audit-only policy binding")
	}
}
