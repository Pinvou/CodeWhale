package identityadapter

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
)

const fixtureProvisionerSecret = "isolated-provisioner-secret-canary"

func keycloakTestFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal("fixture file write failed")
	}
	return path
}

func keycloakTestTLS(t *testing.T, wrongHost bool, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"fixture.invalid"}}
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
	return server, keycloakTestFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func keycloakTestConfig(t *testing.T, origin, caFile string) KeycloakConnectionConfig {
	return KeycloakConnectionConfig{AdminURL: origin, TokenURL: origin + "/realms/pinvou/protocol/openid-connect/token",
		Realm: "pinvou", ClientID: "pinvou-account-provisioner", Production: true, CAFile: caFile,
		SecretFile: keycloakTestFile(t, "client-secret", []byte(fixtureProvisionerSecret+"\n"))}
}

func TestKeycloakProductionFileAndTLS(t *testing.T) {
	var adminCalls, userCalls atomic.Int32
	server, caFile := keycloakTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/pinvou/protocol/openid-connect/token" {
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
			return
		}
		if r.FormValue("grant_type") == "client_credentials" {
			if r.FormValue("client_secret") != fixtureProvisionerSecret {
				t.Error("secret not delivered through body")
			}
			adminCalls.Add(1)
		} else {
			if r.FormValue("client_secret") != "" || r.FormValue("client_id") != "pinvou-desktop" {
				t.Error("proxy credential boundary changed")
			}
			userCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fixture-token","expires_in":300}`)
	}))
	// Loopback requests alone cannot prove proxy isolation: Go's ambient proxy
	// also bypasses loopback. Check the actual scoped transport configuration.
	pool := x509.NewCertPool()
	transport := keycloakTransport(pool)
	if transport.Proxy != nil || transport.TLSClientConfig.RootCAs != pool || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("scoped transport permits proxy or changes explicit trust")
	}
	original := http.DefaultTransport
	cfg := keycloakTestConfig(t, server.URL, caFile)
	directory, err := NewKeycloakDirectoryWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = directory.Ping(context.Background()); err != nil {
		t.Fatal("trusted directory request failed")
	}
	proxy, err := NewTokenProxyWithTrust(cfg.TokenURL, &admissionFake{}, caFile, true)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0
	request := httptest.NewRequest("POST", "/token", strings.NewReader("grant_type=password&client_id=pinvou-desktop&username=fixture&password=fixture-only"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 200 || adminCalls.Load() != 1 || userCalls.Load() != 1 || http.DefaultTransport != original {
		t.Fatal("actual directory/proxy TLS or global isolation failed")
	}
	if directory.client.Timeout != 10*time.Second || proxy.client.Timeout != 15*time.Second {
		t.Fatal("timeouts changed")
	}
}

func TestKeycloakProductionWrongTrustAndRedirectRejected(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) })
	server, _ := keycloakTestTLS(t, false, handler)
	wrongHost, wrongHostCA := keycloakTestTLS(t, true, handler)
	for _, item := range []struct{ name, endpoint, ca string }{
		{"wrong-ca", server.URL, wrongHostCA}, {"wrong-host", wrongHost.URL, wrongHostCA},
	} {
		t.Run(item.name, func(t *testing.T) {
			cfg := keycloakTestConfig(t, item.endpoint, item.ca)
			directory, err := NewKeycloakDirectoryWithConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = directory.Ping(context.Background()); err == nil || strings.Contains(err.Error(), fixtureProvisionerSecret) {
				t.Fatal("TLS failure missing or leaked secret")
			}
			assertKeycloakTLSFailure(t, item.name, err)
			proxy, err := NewTokenProxyWithTrust(cfg.TokenURL, &admissionFake{}, item.ca, true)
			if err != nil {
				t.Fatal(err)
			}
			response, err := proxy.client.Get(cfg.TokenURL)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("proxy accepted wrong trust")
			}
			assertKeycloakTLSFailure(t, item.name, err)
		})
	}
	if calls.Load() != 0 {
		t.Fatal("TLS negatives reached application")
	}
	redirect, redirectCA := keycloakTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	cfg := keycloakTestConfig(t, redirect.URL, redirectCA)
	directory, err := NewKeycloakDirectoryWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if directory.Ping(context.Background()) == nil {
		t.Fatal("directory accepted redirect")
	}
	proxy, err := NewTokenProxyWithTrust(cfg.TokenURL, &admissionFake{}, redirectCA, true)
	if err != nil {
		t.Fatal(err)
	}
	response, err := proxy.client.Get(cfg.TokenURL)
	if err != nil {
		t.Fatal("redirect response unavailable")
	}
	response.Body.Close()
	if response.StatusCode != 307 || calls.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func assertKeycloakTLSFailure(t *testing.T, kind string, err error) {
	t.Helper()
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if (kind == "wrong-ca" && !errors.As(err, &authority)) || (kind == "wrong-host" && !errors.As(err, &hostname)) {
		t.Fatal("failure was not the expected TLS certificate rejection")
	}
}

func TestKeycloakConfigurationFailsClosedAndPreservesDevelopment(t *testing.T) {
	server, caFile := keycloakTestTLS(t, false, http.NotFoundHandler())
	base := keycloakTestConfig(t, server.URL, caFile)
	for _, change := range []func(*KeycloakConnectionConfig){
		func(c *KeycloakConnectionConfig) { c.SecretFile = "" },
		func(c *KeycloakConnectionConfig) { c.ClientSecret = "private-canary" },
		func(c *KeycloakConnectionConfig) { c.CAFile = "" },
		func(c *KeycloakConnectionConfig) { c.AdminURL = "http://127.0.0.1" },
		func(c *KeycloakConnectionConfig) { c.TokenURL += "?private-canary" },
		func(c *KeycloakConnectionConfig) { c.AdminURL += "#private-canary" },
		func(c *KeycloakConnectionConfig) { c.AdminURL = "https://private-canary@localhost" },
		func(c *KeycloakConnectionConfig) {
			c.TokenURL = "https://elsewhere.invalid/realms/pinvou/protocol/openid-connect/token"
		},
		func(c *KeycloakConnectionConfig) { c.Realm = "different-realm" },
		func(c *KeycloakConnectionConfig) { c.AdminURL += "/other" },
		func(c *KeycloakConnectionConfig) { c.SecretFile = "private-canary-relative" },
	} {
		cfg := base
		change(&cfg)
		if _, err := NewKeycloakDirectoryWithConfig(cfg); err != ErrKeycloakConfiguration {
			t.Fatal("configuration was accepted or error was not redacted")
		}
	}
	if _, err := NewTokenProxyWithTrust(base.TokenURL, &admissionFake{}, "", true); err != ErrKeycloakConfiguration {
		t.Fatal("production proxy accepted absent CA")
	}
	development := KeycloakConnectionConfig{AdminURL: "http://localhost", TokenURL: "http://localhost/token", Realm: "dev", ClientID: "dev", ClientSecret: "dev-secret"}
	if _, err := NewKeycloakDirectoryWithConfig(development); err != nil {
		t.Fatal("development compatibility failed")
	}
	development.SecretFile = base.SecretFile
	if _, err := NewKeycloakDirectoryWithConfig(development); err != ErrKeycloakConfiguration {
		t.Fatal("ambiguous development source accepted")
	}
	development.ClientSecret = ""
	if _, err := NewKeycloakDirectoryWithConfig(development); err != nil {
		t.Fatal("development file source failed")
	}
}

func TestKeycloakFilesAndCABoundaries(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(" \n"), []byte("secret\nembedded"), bytesOfSize(16<<10 + 1)} {
		cfg := KeycloakConnectionConfig{AdminURL: "http://localhost", TokenURL: "http://localhost/token", Realm: "dev", ClientID: "dev",
			SecretFile: keycloakTestFile(t, "secret", body)}
		if _, err := NewKeycloakDirectoryWithConfig(cfg); !errors.Is(err, ErrKeycloakConfiguration) {
			t.Fatal("invalid secret file accepted")
		}
	}
	_, caFile := keycloakTestTLS(t, false, http.NotFoundHandler())
	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{[]byte("not a certificate"), append(append([]byte{}, ca...), []byte("-----BEGIN PRIVATE KEY-----\ninvalid\n-----END PRIVATE KEY-----")...), bytesOfSize(1<<20 + 1)} {
		if _, err := keycloakRoots(keycloakTestFile(t, "invalid.pem", body)); err != ErrKeycloakConfiguration {
			t.Fatal("invalid CA bundle accepted")
		}
	}
	for _, kind := range []string{"expired", "future", "not-ca"} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(3), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		switch kind {
		case "expired":
			cert.NotAfter = time.Now().Add(-time.Minute)
		case "future":
			cert.NotBefore = time.Now().Add(time.Minute)
		case "not-ca":
			cert.IsCA = false
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		file := keycloakTestFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		if _, err := keycloakRoots(file); err != ErrKeycloakConfiguration {
			t.Fatal("invalid CA constraints accepted")
		}
	}
	t.Run("permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX modes require Linux verification")
		}
		file := keycloakTestFile(t, "secret", []byte("isolated"))
		if err := os.Chmod(file, 0400); err != nil {
			t.Fatal(err)
		}
		if _, err := readKeycloakFile(file, 1024, true); err != nil {
			t.Fatal("read-only private secret rejected")
		}
		for _, mode := range []os.FileMode{0644, 0640, 0660, 0700} {
			if err := os.Chmod(file, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readKeycloakFile(file, 1024, true); err != ErrKeycloakConfiguration {
				t.Fatal("unsafe secret mode accepted")
			}
		}
		if err := os.Chmod(caFile, 0664); err != nil {
			t.Fatal(err)
		}
		if _, err := keycloakRoots(caFile); err != ErrKeycloakConfiguration {
			t.Fatal("writable CA accepted")
		}
	})
	t.Run("links", func(t *testing.T) {
		file := keycloakTestFile(t, "secret", []byte("isolated"))
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(file, link); err != nil {
			if runtime.GOOS == "windows" {
				t.Skip("Windows symlink privilege unavailable")
			}
			t.Fatal(err)
		}
		if _, err := readKeycloakFile(link, 1024, true); err != ErrKeycloakConfiguration {
			t.Fatal("leaf link accepted")
		}
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.Symlink(filepath.Dir(file), parent); err != nil {
			t.Fatal(err)
		}
		if _, err := readKeycloakFile(filepath.Join(parent, "secret"), 1024, true); err != ErrKeycloakConfiguration {
			t.Fatal("parent link accepted")
		}
	})
}

func bytesOfSize(size int) []byte { return []byte(strings.Repeat("x", size)) }

func TestKeycloakCABundleRejectsSkippedMalformedBlocks(t *testing.T) {
	_, firstPath := keycloakTestTLS(t, false, http.NotFoundHandler())
	_, secondPath := keycloakTestTLS(t, false, http.NotFoundHandler())
	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	malformed := []byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n")
	for _, body := range [][]byte{append(append([]byte{}, malformed...), first...), append(append([]byte{}, first...), malformed...),
		append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n"), first...)} {
		if _, err := keycloakRoots(keycloakTestFile(t, "bundle.pem", body)); err != ErrKeycloakConfiguration {
			t.Fatal("malformed PEM block was silently skipped")
		}
	}
	if pool, err := keycloakRoots(keycloakTestFile(t, "bundle.pem", append(first, second...))); err != nil || len(pool.Subjects()) != 2 {
		t.Fatal("valid two-CA bundle rejected or lost a CA")
	}
}
