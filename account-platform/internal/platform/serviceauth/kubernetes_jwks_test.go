package serviceauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
)

const kubeTestIssuer = "https://issuer.fixture.invalid"
const kubeTestAPI = "fixture-kubernetes-api"
const kubeTestInternal = "pinvou-account-platform-internal"
const kubeTestReceiver = "system:serviceaccount:fixture:account-service"
const kubeTestCaller = "system:serviceaccount:fixture:billing-service"

type kubeJWKSFixture struct {
	issuer          string
	t               *testing.T
	apiKey          *rsa.PrivateKey
	apiFile, caFile string
	server          *httptest.Server
	mu              sync.Mutex
	apiToken        string
	keys            jose.JSONWebKeySet
	headers         []string
	redirect        string
	status          int
}

func kubeTestSign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(claims)
	object, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	value, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func kubeTestClaims(subject, audience string) map[string]any {
	return map[string]any{"iss": kubeTestIssuer, "sub": subject, "aud": []string{audience},
		"iat": time.Now().Add(-10 * time.Second).Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
}

func newKubeJWKSFixture(t *testing.T, wrongHost ...bool) *kubeJWKSFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &kubeJWKSFixture{t: t, apiKey: key, issuer: kubeTestIssuer}
	f.keys = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "first", Algorithm: "RS256", Use: "sig"}}}
	f.apiFile = filepath.Join(t.TempDir(), "token")
	f.rotateAPIToken("first")
	caDER, caKey, ca := trustTestCertificate(t, nil)
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		DNSNames: []string{"kubernetes.default.svc"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if len(wrongHost) > 0 && wrongHost[0] {
		leaf.DNSNames = []string{"wrong.fixture.invalid"}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.caFile = trustTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Get("Authorization"))
		if r.Method != http.MethodGet || r.URL.Path != "/openid/v1/jwks" || r.Header.Get("Authorization") != "Bearer "+f.apiToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		object, err := jose.ParseSigned(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		payload, err := object.Verify(&f.apiKey.PublicKey)
		var claims struct {
			Audience []string `json:"aud"`
			Issuer   string   `json:"iss"`
			Subject  string   `json:"sub"`
			Expiry   int64    `json:"exp"`
		}
		if err != nil || json.Unmarshal(payload, &claims) != nil || len(claims.Audience) != 1 || claims.Audience[0] != kubeTestAPI || claims.Issuer != f.issuer || claims.Subject != kubeTestReceiver || claims.Expiry <= time.Now().Unix() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusTemporaryRedirect)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(f.keys)
	}))
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	// Resolve only the fixed production hostname to this owned loopback server.
	// No real DNS, cluster endpoint, kubeconfig or credential is accessed.
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "kubernetes.default.svc:443" {
			return nil, errors.New("unexpected fixture destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, f.server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { transport.CloseIdleConnections(); http.DefaultTransport = previous })
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", kubeTestIssuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", kubernetesJWKSURL)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", kubeTestInternal)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", f.caFile)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_AUTH_MODE", "kubernetes")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE", f.apiFile)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_API_SUBJECT", kubeTestReceiver)
	t.Setenv("WORKLOAD_IDENTITY_TOKEN_FILE", filepath.Join(t.TempDir(), "business-token"))
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"`+kubeTestCaller+`","service":"billing-service","permissions":["internal.authorization.check"]}]`)
	return f
}

func (f *kubeJWKSFixture) rotateAPIToken(id string) {
	f.t.Helper()
	claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
	claims["jti"] = id
	raw := kubeTestSign(f.t, f.apiKey, "api", claims)
	tmp := f.apiFile + ".next"
	if err := os.WriteFile(tmp, []byte(raw), 0600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, f.apiFile); err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.apiToken = raw
	f.mu.Unlock()
}

func kubeTestRequest(raw string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/internal/check", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	return r
}

func TestKubernetesJWKSRealTLSAPIAuthenticationAndExactPermission(t *testing.T) {
	f := newKubeJWKSFixture(t)
	raw := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
	router := gin.New()
	router.Use(GinMiddleware("unused-legacy"))
	router.GET("/internal/check", Require("internal.authorization.check"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/internal/denied", Require("internal.billing.wallet.read"), func(c *gin.Context) { t.Fatal("ungranted permission reached handler") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, kubeTestRequest(raw))
	if w.Code != 204 {
		t.Fatalf("signed business request = %d", w.Code)
	}
	r := kubeTestRequest(raw)
	r.URL.Path = "/internal/denied"
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("ungranted permission = %d", w.Code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) != 1 || f.headers[0] != "Bearer "+f.apiToken || f.headers[0] == "Bearer "+raw {
		t.Fatal("JWKS used anonymous or business-audience credential")
	}
}

func TestKubernetesJWKSAtomicAPITokenAndSigningKeyRotation(t *testing.T) {
	f := newKubeJWKSFixture(t)
	auth := newAuthenticator("")
	oldAPI := f.apiToken
	if _, status := auth.authenticate(kubeTestRequest(kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal)))); status != 200 {
		t.Fatal("initial authentication failed")
	}
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.rotateAPIToken("second")
	f.mu.Lock()
	f.keys = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &second.PublicKey, KeyID: "second", Algorithm: "RS256", Use: "sig"}}}
	f.mu.Unlock()
	if _, status := auth.authenticate(kubeTestRequest(kubeTestSign(t, second, "second", kubeTestClaims(kubeTestCaller, kubeTestInternal)))); status != 200 {
		t.Fatal("rotation did not fetch with current API token")
	}
	f.mu.Lock()
	if len(f.headers) != 2 || f.headers[0] != "Bearer "+oldAPI || f.headers[1] != "Bearer "+f.apiToken {
		t.Fatal("old API credential was pinned")
	}
	f.mu.Unlock()
	// An expired current credential cannot fetch a new signing key. Cached,
	// already trusted keys keep go-oidc's existing independent cache semantics.
	expired := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
	expired["iat"] = time.Now().Add(-6 * time.Minute).Unix()
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	if err := os.WriteFile(f.apiFile, []byte(kubeTestSign(t, f.apiKey, "api", expired)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, status := auth.authenticate(kubeTestRequest(kubeTestSign(t, second, "third", kubeTestClaims(kubeTestCaller, kubeTestInternal)))); status != 401 {
		t.Fatal("expired API token did not fail closed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) != 2 {
		t.Fatal("expired credential left the process")
	}
}

func TestKubernetesJWKSRejectsUnsafeRequestsAndRedirects(t *testing.T) {
	f := newKubeJWKSFixture(t)
	client, err := workloadJWKSHTTPClient(f.caFile, kubeTestIssuer, kubernetesJWKSURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"https://other.invalid/openid/v1/jwks", "https://kubernetes.default.svc/.well-known/openid-configuration", kubernetesJWKSURL + "?other=1", kubernetesJWKSURL + "#fragment"} {
		r, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := client.Do(r); err == nil {
			t.Fatal("non-fixed endpoint accepted")
		}
	}
	r, _ := http.NewRequest(http.MethodPost, kubernetesJWKSURL, nil)
	if _, err := client.Do(r); err == nil {
		t.Fatal("non-GET accepted")
	}
	f.mu.Lock()
	if len(f.headers) != 0 {
		t.Fatal("unsafe request reached network")
	}
	f.mu.Unlock()
	var leaked int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked++; w.WriteHeader(500) }))
	defer other.Close()
	f.mu.Lock()
	f.redirect = other.URL
	f.mu.Unlock()
	r, _ = http.NewRequest(http.MethodGet, kubernetesJWKSURL, nil)
	r.Header.Set("Authorization", "Bearer caller-secret")
	r.Header.Set("Cookie", "caller-cookie")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 307 || leaked != 0 || r.Header.Get("Authorization") != "Bearer caller-secret" {
		t.Fatal("redirect followed or original request changed")
	}
}

func TestKubernetesJWKSRejectsMissingBadExpiredAndMismatchedConfiguration(t *testing.T) {
	f := newKubeJWKSFixture(t)
	rawBusiness := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
	for _, tc := range []struct{ name, key, value string }{
		{"missing-ca", "WORKLOAD_IDENTITY_CA_FILE", ""}, {"missing-token", "WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE", ""},
		{"missing-subject", "WORKLOAD_IDENTITY_JWKS_API_SUBJECT", ""},
		{"relative-path", "WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE", "relative/token"},
		{"external-url", "WORKLOAD_IDENTITY_JWKS_URL", "https://external.invalid/jwks"}, {"discovery-url", "WORKLOAD_IDENTITY_JWKS_URL", "https://kubernetes.default.svc/.well-known/openid-configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if newAuthenticator("").invalid == nil {
				t.Fatal("bad Kubernetes configuration accepted")
			}
		})
	}
	for _, tc := range []struct{ name, key, value string }{
		{"wrong-issuer", "WORKLOAD_IDENTITY_ISSUER", "https://unrelated.invalid"},
		{"wrong-subject", "WORKLOAD_IDENTITY_JWKS_API_SUBJECT", "system:serviceaccount:fixture:other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			client, err := workloadJWKSHTTPClient(f.caFile, os.Getenv("WORKLOAD_IDENTITY_ISSUER"), kubernetesJWKSURL)
			if err != nil {
				t.Fatal("well-formed immutable configuration rejected")
			}
			request, _ := http.NewRequest(http.MethodGet, kubernetesJWKSURL, nil)
			if _, err := client.Do(request); err == nil {
				t.Fatal("mismatched API credential was not rejected before transport")
			}
			if _, status := newAuthenticator("").authenticate(kubeTestRequest(rawBusiness)); status != http.StatusUnauthorized {
				t.Fatal("mismatched API credential accepted")
			}
		})
	}
	for _, raw := range []string{"not-a-jwt", "a.b.c", strings.Repeat("a", 32769), ""} {
		if err := os.WriteFile(f.apiFile, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, status := newAuthenticator("").authenticate(kubeTestRequest(rawBusiness)); status != http.StatusUnauthorized {
			t.Fatal("bad API token accepted")
		}
	}
	for _, change := range []func(map[string]any){func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, func(c map[string]any) { c["iat"] = time.Now().Add(time.Minute).Unix() }, func(c map[string]any) { c["exp"] = time.Now().Add(time.Hour).Unix() }, func(c map[string]any) { c["aud"] = []string{} }, func(c map[string]any) { c["nbf"] = time.Now().Add(time.Minute).Unix() }} {
		claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
		change(claims)
		if err := os.WriteFile(f.apiFile, []byte(kubeTestSign(t, f.apiKey, "api", claims)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, status := newAuthenticator("").authenticate(kubeTestRequest(rawBusiness)); status != http.StatusUnauthorized {
			t.Fatal("bad token validity accepted")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) != 0 {
		t.Fatal("invalid local API credentials were sent to the server")
	}
}

func TestKubernetesJWKSInitialCredentialFailureRecoversWithoutReconstructingMiddleware(t *testing.T) {
	for _, family := range []string{"http", "gin"} {
		for _, failure := range []string{"missing", "malformed", "expired"} {
			t.Run(family+"/"+failure, func(t *testing.T) {
				f := newKubeJWKSFixture(t)
				switch failure {
				case "missing":
					if err := os.Remove(f.apiFile); err != nil {
						t.Fatal(err)
					}
				case "malformed":
					if err := os.WriteFile(f.apiFile, []byte("invalid-projected-token"), 0600); err != nil {
						t.Fatal(err)
					}
				case "expired":
					claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
					claims["iat"] = time.Now().Add(-6 * time.Minute).Unix()
					claims["exp"] = time.Now().Add(-time.Minute).Unix()
					if err := os.WriteFile(f.apiFile, []byte(kubeTestSign(t, f.apiKey, "api", claims)), 0600); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				var handler http.Handler
				if family == "gin" {
					router := gin.New()
					router.Use(GinMiddleware("unused-legacy"))
					router.GET("/internal/check", Require("internal.authorization.check"), func(c *gin.Context) { calls++; c.Status(http.StatusNoContent) })
					handler = router
				} else {
					handler = Middleware("unused-legacy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
				}
				raw := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, kubeTestRequest(raw))
				if response.Code != http.StatusUnauthorized || calls != 0 {
					t.Fatalf("initial failure = %d, calls=%d", response.Code, calls)
				}
				f.mu.Lock()
				sent := len(f.headers)
				f.mu.Unlock()
				if sent != 0 {
					t.Fatal("invalid API credential left the process")
				}
				f.rotateAPIToken("recovered")
				for attempt := 1; attempt <= 2; attempt++ {
					response = httptest.NewRecorder()
					handler.ServeHTTP(response, kubeTestRequest(raw))
					if response.Code != http.StatusNoContent || calls != attempt {
						t.Fatalf("recovered request %d = %d, calls=%d", attempt, response.Code, calls)
					}
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.headers) != 1 || f.headers[0] != "Bearer "+f.apiToken {
					t.Fatal("recovery did not fetch with the current API credential")
				}
			})
		}
	}
}

func TestKubernetesJWKSAPIRejectsBusinessAudienceAndForbidden(t *testing.T) {
	f := newKubeJWKSFixture(t)
	// Shape-valid credentials still require the API server's signature, identity
	// and API audience authentication; business audience is never sufficient.
	wrong := kubeTestSign(t, f.apiKey, "api", kubeTestClaims(kubeTestReceiver, kubeTestInternal))
	if err := os.WriteFile(f.apiFile, []byte(wrong), 0600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.apiToken = wrong
	f.mu.Unlock()
	auth := newAuthenticator("")
	raw := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
	if _, status := auth.authenticate(kubeTestRequest(raw)); status != 401 {
		t.Fatal("API rejected business-audience token was bypassed")
	}
	f.rotateAPIToken("restored")
	f.mu.Lock()
	f.status = 403
	f.mu.Unlock()
	if _, status := newAuthenticator("").authenticate(kubeTestRequest(raw)); status != 401 {
		t.Fatal("403 JWKS did not fail closed")
	}
}

func TestKubernetesJWKSContextCancellationAndTimeout(t *testing.T) {
	f := newKubeJWKSFixture(t)
	client, err := workloadJWKSHTTPClient(f.caFile, kubeTestIssuer, kubernetesJWKSURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, kubernetesJWKSURL, nil)
	if _, err := client.Do(r); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled context was not respected")
	}
	f.mu.Lock()
	if len(f.headers) != 0 {
		t.Fatal("cancelled request sent credential")
	}
	f.mu.Unlock()
	if client.Timeout != 10*time.Second {
		t.Fatal("production fetch timeout changed")
	}
	client.Timeout = 30 * time.Millisecond
	entered := make(chan struct{})
	done := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(done) })
	r, _ = http.NewRequest(http.MethodGet, kubernetesJWKSURL, nil)
	start := time.Now()
	if _, err := client.Do(r); err == nil {
		t.Fatal("bounded request did not time out")
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout was not bounded")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request never reached fixture")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outgoing context did not cancel")
	}
}

func TestKubernetesJWKSRejectsTLSCAAndHostnameMismatch(t *testing.T) {
	for _, badHost := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-ca", true: "wrong-host"}[badHost], func(t *testing.T) {
			f := newKubeJWKSFixture(t, badHost)
			if !badHost {
				der, _, _ := trustTestCertificate(t, nil)
				t.Setenv("WORKLOAD_IDENTITY_CA_FILE", trustTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
			}
			raw := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
			if _, status := newAuthenticator("").authenticate(kubeTestRequest(raw)); status != 401 {
				t.Fatal("untrusted TLS accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.headers) != 0 {
				t.Fatal("TLS failure sent HTTP credential")
			}
		})
	}
}

func TestKubernetesJWKSProjectedSymlinkRotationStaysInsideReadOnlyRoot(t *testing.T) {
	f := newKubeJWKSFixture(t)
	root := filepath.Dir(f.apiFile)
	first := filepath.Join(root, "version-1")
	if err := os.Mkdir(first, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "token"), []byte(f.apiToken), 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "..data")
	if err := os.Symlink("version-1", data); err != nil {
		t.Skip("symlink projection unavailable on this host")
	}
	if err := os.Remove(f.apiFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), f.apiFile); err != nil {
		t.Fatal(err)
	}
	if token, err := readKubernetesAPIToken(f.apiFile, kubeTestIssuer, kubeTestReceiver, time.Now()); err != nil || token != f.apiToken {
		t.Fatal("current projected token unreadable")
	}
	second := filepath.Join(root, "version-2")
	if err := os.Mkdir(second, 0700); err != nil {
		t.Fatal(err)
	}
	claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
	claims["jti"] = "rotated-projection"
	next := kubeTestSign(t, f.apiKey, "api", claims)
	if err := os.WriteFile(filepath.Join(second, "token"), []byte(next), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("version-2", filepath.Join(root, "..next")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "..next"), data); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("atomic directory symlink replacement unsupported by Windows host")
		}
		t.Fatal(err)
	}
	if token, err := readKubernetesAPIToken(f.apiFile, kubeTestIssuer, kubeTestReceiver, time.Now()); err != nil || token != next {
		t.Fatal("atomic projection rotation pinned old data")
	}
	outside := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outside, []byte(next), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.apiFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.apiFile); err != nil {
		t.Fatal(err)
	}
	if _, err := readKubernetesAPIToken(f.apiFile, kubeTestIssuer, kubeTestReceiver, time.Now()); err == nil {
		t.Fatal("projection escape accepted")
	}
}

func TestKubernetesJWKSOptInCannotFallBackToDevelopmentStaticAuthentication(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_AUTH_MODE", "kubernetes")
	if newAuthenticator("legacy").invalid == nil {
		t.Fatal("incomplete Kubernetes configuration fell back to a legacy token")
	}
}

func TestKubernetesJWKSExternalPublicModeDoesNotSendAPICredential(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var headers []string
	server, ca := trustTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "external", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Setenv("WORKLOAD_IDENTITY_JWKS_AUTH_MODE", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_API_SUBJECT", "")
	trustTestEnvironment(t, server.URL, ca)
	claims := kubeTestClaims("fixture-subject", "fixture-api")
	claims["iss"] = server.URL + "/realms/workload"
	if _, status := newAuthenticator("").authenticate(kubeTestRequest(kubeTestSign(t, key, "external", claims))); status != 200 {
		t.Fatal("existing external JWKS mode changed")
	}
	if len(headers) != 1 || headers[0] != "" {
		t.Fatal("Kubernetes or business credential reached external JWKS")
	}
}

func TestKubernetesJWKSActualVerifierCancellationKeepsSharedFetchBounded(t *testing.T) {
	f := newKubeJWKSFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			_ = json.NewEncoder(w).Encode(f.keys)
		case <-r.Context().Done():
		}
	})
	auth := newAuthenticator("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := kubeTestRequest(kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))).WithContext(ctx)
	done := make(chan int, 1)
	go func() { _, status := auth.authenticate(r); done <- status }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("verifier did not fetch")
	}
	cancel()
	select {
	case status := <-done:
		if status != 401 {
			t.Fatal("cancelled verification succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("verification ignored request cancellation")
	}
	close(release)
}

func TestKubernetesJWKSDiagnosticsDoNotLogCredential(t *testing.T) {
	f := newKubeJWKSFixture(t)
	claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
	claims["jti"] = "isolated-private-credential-marker"
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	raw := kubeTestSign(t, f.apiKey, "api", claims)
	if err := os.WriteFile(f.apiFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	rawBusiness := kubeTestSign(t, f.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal))
	if _, status := newAuthenticator("").authenticate(kubeTestRequest(rawBusiness)); status != http.StatusUnauthorized {
		t.Fatal("invalid credential accepted")
	}
	if strings.Contains(output.String(), raw) || strings.Contains(output.String(), "isolated-private-credential-marker") {
		t.Fatal("credential entered diagnostics")
	}
}

func TestKubernetesJWKSCAChangeRequiresReconstructedClient(t *testing.T) {
	first := newKubeJWKSFixture(t)
	old := newAuthenticator("")
	if _, status := old.authenticate(kubeTestRequest(kubeTestSign(t, first.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal)))); status != 200 {
		t.Fatal("initial trust failed")
	}
	second := newKubeJWKSFixture(t)
	// The old Pod/client keeps its CA snapshot even when the endpoint presents
	// a new serving chain. A fresh Pod/client receives the new CA and credential.
	first.server.CloseClientConnections()
	first.server.TLS.Certificates = second.server.TLS.Certificates
	first.mu.Lock()
	first.keys = second.keys
	first.mu.Unlock()
	request := kubeTestRequest(kubeTestSign(t, second.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal)))
	if _, status := old.authenticate(request); status != 401 {
		t.Fatal("old CA unexpectedly trusted replacement")
	}
	if _, status := newAuthenticator("").authenticate(kubeTestRequest(kubeTestSign(t, second.apiKey, "first", kubeTestClaims(kubeTestCaller, kubeTestInternal)))); status != 200 {
		t.Fatal("reconstructed CA trust did not work")
	}
}

func TestKubernetesJWKSMatchesActualIssuerWithoutTrailingSlashNormalization(t *testing.T) {
	f := newKubeJWKSFixture(t)
	issuer := kubeTestIssuer + "/"
	claims := kubeTestClaims(kubeTestReceiver, kubeTestAPI)
	claims["iss"] = issuer
	api := kubeTestSign(t, f.apiKey, "api", claims)
	if err := os.WriteFile(f.apiFile, []byte(api), 0600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.apiToken = api
	f.issuer = issuer
	f.mu.Unlock()
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	business := kubeTestClaims(kubeTestCaller, kubeTestInternal)
	business["iss"] = issuer
	if _, status := newAuthenticator("").authenticate(kubeTestRequest(kubeTestSign(t, f.apiKey, "first", business))); status != 200 {
		t.Fatal("exact actual issuer was normalized away")
	}
}
