package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type acceptingCredentialValidator struct{}

func (acceptingCredentialValidator) Validate(context.Context, string, []byte, []string) error {
	return nil
}

func TestCanonicalCredentialLaunchIsClosedAndStable(t *testing.T) {
	accountID := uuid.MustParse("0199a200-0001-7000-8000-000000000201")
	actorID := uuid.MustParse("0199a200-0001-7000-8000-000000000202")
	input := CredentialLaunchRequest{Action: " rotate ", ExpectedAccountVersion: 4, ExpectedCredentialVersion: 2, Slot: " primary ", Reason: " rotate the active credential "}
	canonical, digest, err := canonicalCredentialLaunch(accountID, actorID, input)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Action != "ROTATE" || canonical.Slot != "PRIMARY" || canonical.Reason != "rotate the active credential" || len(digest) != 64 {
		t.Fatalf("unexpected canonical launch: %#v digest=%q", canonical, digest)
	}
	for _, invalid := range []CredentialLaunchRequest{
		{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 1, Slot: "PRIMARY", Reason: strings.Repeat("x", 10)},
		{Action: "ROTATE", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: strings.Repeat("x", 10)},
		{Action: "ROTATE", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 1, Slot: "PRIMARY", Reason: strings.Repeat("x", 2001)},
	} {
		if _, _, err = canonicalCredentialLaunch(accountID, actorID, invalid); err == nil {
			t.Fatalf("invalid launch was accepted: %#v", invalid)
		}
	}
}

func TestCredentialIsolationAuthorityGuardFailsClosed(t *testing.T) {
	snapshot := CredentialIsolationAuthoritySnapshot{ActorID: uuid.New(), Issuer: "https://id.example/realms/pinvou", Subject: "subject", AuthorizationVersion: 7, GateEpoch: 11}
	repository := &PostgresRepository{}
	if err := repository.recheckCredentialIsolationAuthority(context.Background(), nil, snapshot); !errors.Is(err, ErrTokenRouteActivationChanged) {
		t.Fatalf("missing authority guard err=%v", err)
	}
	called := 0
	repository.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, observed CredentialIsolationAuthoritySnapshot) error {
		called++
		if observed != snapshot {
			t.Fatalf("authority snapshot changed: %#v", observed)
		}
		return nil
	}))
	if err := repository.recheckCredentialIsolationAuthority(context.Background(), nil, snapshot); err != nil || called != 1 {
		t.Fatalf("authority guard err=%v calls=%d", err, called)
	}
	for _, invalid := range []CredentialIsolationAuthoritySnapshot{{}, {ActorID: snapshot.ActorID, Issuer: snapshot.Issuer, Subject: snapshot.Subject, AuthorizationVersion: 0, GateEpoch: 11}, {ActorID: snapshot.ActorID, Issuer: snapshot.Issuer, Subject: snapshot.Subject, AuthorizationVersion: 7, GateEpoch: 0}} {
		if err := repository.recheckCredentialIsolationAuthority(context.Background(), nil, invalid); !errors.Is(err, ErrTokenRouteActivationChanged) {
			t.Fatalf("invalid frozen authority accepted: %#v err=%v", invalid, err)
		}
	}
}

func TestCredentialOriginRequiresFixedSecureOrigin(t *testing.T) {
	repository := &PostgresRepository{}
	parse := func(raw string) *url.URL {
		value, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := CredentialOriginConfig{
		PublicURL: parse("https://credentials.pinvou.com"), PortalOrigin: parse("https://account.pinvou.com"), IssuerURL: parse("https://id.example/realms/pinvou"), AuthorizationURL: parse("https://id.example/authorize"),
		TokenURL: parse("https://id.example/token"), JWKSURL: parse("https://id.example/certs"), ClientID: "credential-client", SecureCookies: true,
		Verify: func(context.Context, string, string) (identity.Principal, error) {
			return identity.Principal{AuthenticatedAt: time.Now()}, nil
		},
	}
	if _, err := NewCredentialOriginService(base, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err != nil {
		t.Fatalf("fixed production origin rejected: %v", err)
	}
	for _, pair := range [][2]string{{"https://account.pinvou.com", "https://credentials.pinvou.com"}, {"https://account-uat.pinvou.com", "https://credentials-uat.pinvou.com"}} {
		candidate := base
		candidate.PortalOrigin, candidate.PublicURL = parse(pair[0]), parse(pair[1])
		if _, err := NewCredentialOriginService(candidate, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err != nil {
			t.Fatalf("fixed pair rejected: %v", err)
		}
		for _, rejected := range []string{"https://credentials.account.pinvou.com", "https://credentials-uat.account.pinvou.com", "https://" + candidate.PublicURL.Hostname() + ":443", pair[1] + ".", pair[1] + "?x=1", pair[1] + "#x", "https://user@" + candidate.PublicURL.Hostname(), "http://" + candidate.PublicURL.Hostname()} {
			candidate.PublicURL = parse(rejected)
			if _, err := NewCredentialOriginService(candidate, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err == nil {
				t.Fatalf("retired or invalid origin accepted: %s", rejected)
			}
		}
		candidate.PublicURL = parse(pair[1])
		otherPortal := "https://account.pinvou.com"
		if pair[0] == otherPortal {
			otherPortal = "https://account-uat.pinvou.com"
		}
		candidate.PortalOrigin = parse(otherPortal)
		if _, err := NewCredentialOriginService(candidate, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err == nil {
			t.Fatal("cross-environment origin pair accepted")
		}
	}
	for _, raw := range []string{"https://credentials.pinvou.com.evil.example", "https://credentials.pinvou.com:8443", "https://credentials.pinvou.com/path", "https://arbitrary.example"} {
		candidate := base
		candidate.PublicURL = parse(raw)
		if _, err := NewCredentialOriginService(candidate, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err == nil {
			t.Fatalf("unsafe credential origin accepted: %s", raw)
		}
	}
	development := base
	development.PublicURL = parse("http://127.0.0.1:8099")
	development.PortalOrigin = parse("http://127.0.0.1:8080")
	development.IssuerURL = parse("http://127.0.0.1:8081/issuer")
	development.AuthorizationURL = parse("http://127.0.0.1:8081/authorize")
	development.TokenURL = parse("http://127.0.0.1:8081/token")
	development.JWKSURL = parse("http://127.0.0.1:8081/certs")
	development.SecureCookies = false
	service, err := NewCredentialOriginService(development, repository, acceptingCredentialValidator{}, ProviderURLPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(service.credentialSessionCookieName(), "__Host-") || strings.HasPrefix(service.credentialBindingCookieName(), "__Host-") {
		t.Fatal("insecure loopback development cookies cannot use the __Host prefix")
	}
	redirectRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8082/token", nil)
	if redirectErr := service.client.CheckRedirect(redirectRequest, []*http.Request{httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/token", nil)}); !errors.Is(redirectErr, http.ErrUseLastResponse) {
		t.Fatalf("credential token client followed a redirect: %v", redirectErr)
	}
	for language, expected := range map[string]string{"zh-CN": "凭据安全接入", "en-US": "Secure credential enrollment", "ja-JP": "認証情報の安全な登録"} {
		if actual := credentialPageCopyFor(language).Title; actual != expected {
			t.Fatalf("language=%s title=%q", language, actual)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/assets/credential.css", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(response.Body.String(), "@media(max-width:360px)") {
		t.Fatalf("credential stylesheet response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	unsafeEndpoints := []func(*CredentialOriginConfig){
		func(item *CredentialOriginConfig) { item.TokenURL = parse("https://evil.example/token") },
		func(item *CredentialOriginConfig) { item.AuthorizationURL = parse("http://id.example/authorize") },
		func(item *CredentialOriginConfig) { item.JWKSURL = parse("https://id.example:8443/certs") },
		func(item *CredentialOriginConfig) { item.TokenURL = parse("https://id.example/userinfo") },
		func(item *CredentialOriginConfig) { item.AuthorizationURL = parse("https://user@id.example/authorize") },
		func(item *CredentialOriginConfig) { item.JWKSURL = parse("https://id.example/certs#fragment") },
	}
	for index, mutate := range unsafeEndpoints {
		candidate := base
		mutate(&candidate)
		if _, err := NewCredentialOriginService(candidate, repository, acceptingCredentialValidator{}, ProviderURLPolicy{}); err == nil {
			t.Fatalf("unsafe OIDC endpoint case %d was accepted", index)
		}
	}
	for _, origin := range []string{"", "null", "https://evil.example", "http://127.0.0.1:8080/path", "http://localhost:8080"} {
		launchRequest := httptest.NewRequest(http.MethodPost, "/launch", strings.NewReader("launch_id="+uuid.New().String()))
		launchRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			launchRequest.Header.Set("Origin", origin)
		}
		launchResponse := httptest.NewRecorder()
		service.Handler().ServeHTTP(launchResponse, launchRequest)
		if launchResponse.Code != http.StatusForbidden {
			t.Fatalf("untrusted Portal launch origin %q status=%d", origin, launchResponse.Code)
		}
	}
}
