package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func transitTestCertificate(t *testing.T, ca bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated TLS test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTransitProviderDedicatedCAIsolation(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"data":{"ciphertext":"vault:v1:YWJjZA=="}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	sink := filepath.Join(dir, "token")
	if os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600) != nil || os.WriteFile(sink, []byte("fixture-only-agent-token"), 0600) != nil {
		t.Fatal("fixture setup failed")
	}
	cfg := SecretProviderConfig{Environment: "production", Mode: "vault-transit", KeyID: "key", RemoteURL: server.URL, RemoteTokenFile: sink, TransitCAFile: caPath}
	proxyCalls := 0
	previous := http.DefaultTransport
	trap := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { proxyCalls++; return url.Parse("http://proxy.invalid:8080") }}
	http.DefaultTransport = trap
	defer func() { http.DefaultTransport = previous; trap.CloseIdleConnections() }()
	p, err := NewTransitProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	transport := p.client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.RootCAs == nil || http.DefaultTransport != trap {
		t.Fatal("global transport or private trust isolation failed")
	}
	if _, err := p.Encrypt([]byte("fixture-plaintext"), []byte("aad")); err != nil || calls != 1 || proxyCalls != 0 {
		t.Fatal("dedicated CA used default proxy or did not authenticate TLS", err)
	}
	// Omission retains system-root semantics, not the prior provider's private CA.
	omitted := cfg
	omitted.TransitCAFile = ""
	public, err := NewTransitProvider(omitted)
	if err != nil {
		t.Fatal(err)
	}
	defer public.client.CloseIdleConnections()
	if public.client.Transport.(*http.Transport).TLSClientConfig.RootCAs != nil {
		t.Fatal("private trust contaminated another client")
	}
	if _, err := public.Encrypt([]byte("x"), []byte("aad")); err == nil || calls != 1 {
		t.Fatal("private CA became globally trusted")
	}
	// A different valid CA must not silently fall back to a previously trusted root.
	if os.WriteFile(caPath, transitTestCertificate(t, true), 0600) != nil {
		t.Fatal("fixture CA update failed")
	}
	wrong, err := NewTransitProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.client.CloseIdleConnections()
	if _, err := wrong.Encrypt([]byte("x"), []byte("aad")); err == nil || calls != 1 {
		t.Fatal("wrong CA accepted")
	}
	// A correct trust root never disables endpoint hostname validation.
	u, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	p.baseURL = "https://localhost:" + port
	if _, err := p.Encrypt([]byte("x"), []byte("aad")); err == nil || calls != 1 {
		t.Fatal("wrong TLS hostname accepted")
	}
}

func TestTransitProviderDedicatedCARejectsInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	ca := transitTestCertificate(t, true)
	path := filepath.Join(dir, "ca.crt")
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"empty", nil}, {"garbage", []byte("secret-canary")}, {"trailing", append(append([]byte{}, ca...), []byte("garbage")...)},
		{"prefix", append([]byte("garbage\n"), ca...)}, {"private-key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret-canary")})},
		{"non-ca", transitTestCertificate(t, false)}, {"malformed-cert", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")})},
		{"oversize", []byte(strings.Repeat("x", (64<<10)+1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if os.WriteFile(path, test.data, 0600) != nil {
				t.Fatal("fixture write failed")
			}
			if _, err := newTransitTransport(path); err != errTransitCA || strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("invalid CA accepted or leaked")
			}
		})
	}
	for _, rejected := range []string{"relative-ca", filepath.Join(dir, "missing"), dir} {
		if _, err := newTransitTransport(rejected); err != errTransitCA {
			t.Fatal("invalid CA path accepted")
		}
	}
	if os.WriteFile(path, ca, 0600) != nil {
		t.Fatal("fixture restore failed")
	}
	for _, mode := range []string{"local", "remote", ""} {
		if _, err := NewSecretProvider(SecretProviderConfig{Environment: "test", Mode: mode, TransitCAFile: path}); err == nil {
			t.Fatal("ignored explicit trust configuration")
		}
	}
	if _, err := NewTransitProvider(SecretProviderConfig{Environment: "test", RemoteURL: "http://kms.example", KeyID: "key", RemoteToken: "test-only-credential", TransitCAFile: path}); err == nil {
		t.Fatal("CA configured on plaintext transport")
	}
	if runtime.GOOS != "windows" {
		if os.Chmod(path, 0664) != nil {
			t.Fatal("fixture chmod failed")
		}
		if _, err := newTransitTransport(path); err != errTransitCA {
			t.Fatal("writable CA accepted")
		}
		os.Chmod(path, 0600)
		link := filepath.Join(dir, "projected-ca.crt")
		if os.Symlink(path, link) != nil {
			t.Fatal("fixture symlink failed")
		}
		transport, err := newTransitTransport(link)
		if err != nil {
			t.Fatal("read-only projected CA rejected", err)
		}
		transport.CloseIdleConnections()
	}
}
