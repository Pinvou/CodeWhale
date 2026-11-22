package serviceauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
)

func trustTestFile(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func trustTestCertificate(t *testing.T, change func(*x509.Certificate)) ([]byte, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated workload CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if change != nil {
		change(cert)
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key, cert
}

func trustTestTLS(t *testing.T, wrongHost bool, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	caDER, caKey, ca := trustTestCertificate(t, nil)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"fixture.invalid"}}
	if !wrongHost {
		leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, trustTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func trustTestEnvironment(t *testing.T, origin, ca string) {
	t.Helper()
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", origin+"/realms/workload")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", origin+"/certs")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "fixture-api")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"fixture-subject","service":"fixture-service","permissions":["internal.authorization.check"]}]`)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
}

func TestWorkloadPrivateCARealSignedAuthentication(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fetched, redirected atomic.Int32
	server, ca := trustTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/certs":
			fetched.Add(1)
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/redirect":
			http.Redirect(w, r, "/must-not-follow", http.StatusTemporaryRedirect)
		case "/must-not-follow":
			redirected.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	trustTestEnvironment(t, server.URL, ca)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(subject string) string {
		payload, _ := json.Marshal(map[string]any{"iss": server.URL + "/realms/workload", "aud": "fixture-api",
			"sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
		value, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := value.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := func(raw string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/internal/check", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		return r
	}
	raw := sign("fixture-subject")
	var served bool
	response := httptest.NewRecorder()
	Middleware("legacy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := FromContext(r.Context())
		if !ok || identity.Subject != "fixture-subject" || len(identity.permissions) != 1 || identity.permissions[0] != "internal.authorization.check" {
			t.Error("signed identity or exact permission changed")
		}
		served = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request(raw))
	if !served || response.Code != http.StatusNoContent || fetched.Load() == 0 {
		t.Fatal("actual middleware did not fetch private TLS JWKS and authenticate")
	}
	if _, status := newAuthenticator("legacy").authenticate(request(sign("unknown-subject"))); status != http.StatusForbidden {
		t.Fatal("unmapped signed subject was not denied")
	}
	if _, status := newAuthenticator("legacy").authenticate(request("legacy")); status != http.StatusUnauthorized {
		t.Fatal("static token bypassed workload authentication")
	}
	router := gin.New()
	router.Use(GinMiddleware("legacy"))
	router.GET("/allowed", Require("internal.authorization.check"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/denied", Require("internal.authorization.delegate"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for path, expected := range map[string]int{"/allowed": http.StatusNoContent, "/denied": http.StatusForbidden} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != expected {
			t.Fatal("actual signed workload permission decision was incorrect")
		}
	}
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", server.URL+"/redirect")
	if _, status := newAuthenticator("legacy").authenticate(request(raw)); status != http.StatusUnauthorized || redirected.Load() != 0 {
		t.Fatal("JWKS redirect was followed or accepted")
	}
}

func TestWorkloadTrustTransportIsolation(t *testing.T) {
	server, ca := trustTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:8999")
	before := http.DefaultTransport.(*http.Transport)
	oldTLS := before.TLSClientConfig
	client, err := workloadHTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if transport == before || transport.Proxy != nil || transport.TLSClientConfig.RootCAs == nil ||
		transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || client.Timeout != 10*time.Second {
		t.Fatal("scoped transport trust, proxy or timeout contract changed")
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("trusted private TLS request failed")
	}
	response.Body.Close()
	if http.DefaultTransport != before || before.TLSClientConfig != oldTLS {
		t.Fatal("global transport changed")
	}
	public, err := workloadHTTPClient("")
	if err != nil || public.Transport.(*http.Transport).TLSClientConfig.RootCAs != nil {
		t.Fatal("system root compatibility changed")
	}
	_, err = public.Get(server.URL)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatal("private root leaked into another client")
	}
	wrong, wrongCA := trustTestTLS(t, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("wrong host reached application") }))
	client, err = workloadHTTPClient(wrongCA)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(wrong.URL)
	var hostname x509.HostnameError
	if !errors.As(err, &hostname) {
		t.Fatal("wrong host did not fail certificate name verification")
	}
	client, err = workloadHTTPClient(wrongCA)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(server.URL)
	if !errors.As(err, &unknown) {
		t.Fatal("wrong CA did not fail certificate chain verification")
	}
}

func TestWorkloadInvalidCAFailsClosed(t *testing.T) {
	validDER, _, _ := trustTestCertificate(t, nil)
	valid := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: validDER})
	bodies := map[string][]byte{"empty": nil, "garbage": []byte("invalid"), "oversize": []byte(strings.Repeat("x", (1<<20)+1)),
		"private-key":   append(append([]byte(nil), valid...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("fixture")})...),
		"partial-valid": append(append([]byte(nil), valid...), []byte("garbage")...)}
	malformed := []byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n")
	bodies["malformed-before-valid"] = append(append([]byte(nil), malformed...), valid...)
	bodies["malformed-after-valid"] = append(append([]byte(nil), valid...), malformed...)
	bodies["nested-malformed"] = append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n"), valid...)
	for name, change := range map[string]func(*x509.Certificate){
		"expired":     func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Second) },
		"future":      func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) },
		"non-ca":      func(c *x509.Certificate) { c.IsCA = false },
		"wrong-usage": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature },
	} {
		der, _, _ := trustTestCertificate(t, change)
		bodies[name] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			path := trustTestFile(t, body)
			trustTestEnvironment(t, "https://fixture.invalid", path)
			auth := newAuthenticator("legacy")
			if !errors.Is(auth.invalid, errWorkloadTrust) || auth.legacyToken != "" {
				t.Fatal("invalid CA did not fail closed without fallback")
			}
		})
	}
	for _, path := range []string{"relative.pem", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if _, err := workloadRoots(path); !errors.Is(err, errWorkloadTrust) {
			t.Fatal("invalid path accepted")
		}
	}
	trustTestEnvironment(t, "https://fixture.invalid", trustTestFile(t, valid))
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("APP_ENV", "development")
	if auth := newAuthenticator("legacy"); auth.invalid == nil || auth.legacyToken != "" {
		t.Fatal("explicit unused CA allowed legacy fallback")
	}
}

func TestWorkloadCARejectsUnsafePOSIXFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires real POSIX permissions and symlinks")
	}
	der, _, _ := trustTestCertificate(t, nil)
	path := trustTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := workloadRoots(path); !errors.Is(err, errWorkloadTrust) {
		t.Fatal("writable CA accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked.pem")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := workloadRoots(link); !errors.Is(err, errWorkloadTrust) {
		t.Fatal("symlink CA accepted")
	}
}

func TestWorkloadExplicitCARejectsNoncanonicalURLs(t *testing.T) {
	der, _, _ := trustTestCertificate(t, nil)
	ca := trustTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	for _, variable := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL"} {
		for _, value := range []string{"http://fixture.invalid", "https://name:secret@fixture.invalid", "https://fixture.invalid?x=1", "https://fixture.invalid?", "https://fixture.invalid#fragment", "https://fixture.invalid/%63erts"} {
			t.Run(variable+"/"+strings.Split(value, ":")[0], func(t *testing.T) {
				trustTestEnvironment(t, "https://fixture.invalid", ca)
				t.Setenv(variable, value)
				if newAuthenticator("legacy").invalid == nil {
					t.Fatal("noncanonical explicit-trust URL accepted")
				}
			})
		}
	}
}
