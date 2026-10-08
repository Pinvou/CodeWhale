package newapitls

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func tlsFixture(t *testing.T, upstream http.Handler, expired ...bool) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("fixture CA key generation failed")
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal("fixture CA generation failed")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("fixture server key generation failed")
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "newapi"}, DNSNames: []string{"newapi"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	if len(expired) > 0 && expired[0] {
		leaf.NotAfter = now.Add(-time.Minute)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal("fixture server certificate generation failed")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("fixture key encoding failed")
	}
	directory := t.TempDir()
	certificatePath, keyPath := filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key")
	if os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0600) != nil || os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600) != nil {
		t.Fatal("fixture certificate files failed")
	}
	server, err := NewServer("", certificatePath, keyPath)
	if err != nil {
		t.Fatal("mounted certificate did not configure the production TLS server")
	}
	if server.WriteTimeout != 0 {
		t.Fatal("TLS server added a stream response deadline")
	}
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	server.Handler = newHandler(target) // Controlled same-pod HTTP fixture, not live New API.
	front := httptest.NewUnstartedServer(server.Handler)
	front.Config = server
	front.TLS = server.TLSConfig.Clone()
	front.StartTLS()
	t.Cleanup(front.Close)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) {
		t.Fatal("fixture CA trust failed")
	}
	return front, roots
}

func TestTLSCertificateMissingOrInvalidCannotStart(t *testing.T) {
	for _, paths := range [][2]string{{"", ""}, {filepath.Join(t.TempDir(), "missing.crt"), filepath.Join(t.TempDir(), "missing.key")}} {
		if server, err := NewServer(":8443", paths[0], paths[1]); err == nil || server != nil {
			t.Fatal("missing TLS certificate did not fail closed")
		}
	}
	directory := t.TempDir()
	certificatePath, keyPath := filepath.Join(directory, "invalid.crt"), filepath.Join(directory, "invalid.key")
	if os.WriteFile(certificatePath, []byte("invalid PEM"), 0600) != nil || os.WriteFile(keyPath, []byte("invalid PEM"), 0600) != nil {
		t.Fatal("invalid certificate fixture failed")
	}
	if server, err := NewServer(":8443", certificatePath, keyPath); err == nil || server != nil {
		t.Fatal("invalid certificate did not fail closed")
	}
}

func TestTLSProxyExactRoutesQueryAndHeaderBoundary(t *testing.T) {
	var calls atomic.Int64
	server, roots := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("private TLS proxy forwarded browser or proxy headers")
		}
		if r.Header.Get("Authorization") != "Bearer fixture-auth" || r.Header.Get("X-Request-ID") != "fixture-correlation" {
			t.Error("TLS hop lost existing provider authentication or correlation")
		}
		if r.URL.Path == "/api/log/" && (r.URL.Query().Get("request_id") != "fixture-request" || r.URL.Query().Get("type") != "2" || r.URL.Query().Get("p") != "1" || r.URL.Query().Get("page_size") != "2" || r.Header.Get("New-Api-User") != "1") {
			t.Error("TLS hop changed the real reconciliation query contract")
		}
		w.Header().Set("X-Oneapi-Request-Id", "fixture-request")
		w.Header().Set("Set-Cookie", "fixture-cookie=blocked")
		w.Header().Set("Location", "/api/user")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true}`)
	}))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	t.Cleanup(client.CloseIdleConnections)
	for _, route := range []struct{ method, path string }{{"POST", "/v1/chat/completions"}, {"GET", "/api/log/?request_id=fixture-request&type=2&p=1&page_size=2"}, {"GET", "/v1/models"}, {"GET", "/api/status"}} {
		request, _ := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(`{"model":"fixture"}`))
		request.Header.Set("Authorization", "Bearer fixture-auth")
		request.Header.Set("X-Request-ID", "fixture-correlation")
		request.Header.Set("New-Api-User", "1")
		for _, name := range []string{"Cookie", "X-CSRF-Token", "X-Forwarded-Host", "Proxy-Authorization"} {
			request.Header.Set(name, "fixture-only")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("trusted TLS route failed")
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || string(payload) != `{"success":true}` || response.Header.Get("X-Oneapi-Request-Id") != "fixture-request" || response.Header.Get("Set-Cookie") != "" || response.Header.Get("Location") != "" {
			t.Fatal("TLS hop changed permitted response or exposed browser state")
		}
	}
	for _, route := range []struct{ method, path string }{{"HEAD", "/api/status"}, {"POST", "/api/status"}, {"GET", "/api/status/extra"}, {"GET", "/api%2fstatus"}, {"GET", "/api/setup"}, {"GET", "/api/user"}, {"GET", "/api/log/self"}, {"GET", "/api/log"}, {"GET", "/api/log%2f"}, {"POST", "/api/log/"}, {"GET", "/v1/chat/completions"}, {"POST", "/v1/chat/completions/other"}} {
		request, _ := http.NewRequest(route.method, server.URL+route.path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("route rejection could not be read")
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("TLS hop exposed a non-Gateway management route")
		}
	}
	for _, path := range []string{"/health/live", "/health/ready"} {
		for _, method := range []string{"GET", "HEAD"} {
			request, _ := http.NewRequest(method, server.URL+path, nil)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("TLS static health failed")
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("TLS health route was not healthy")
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatal("rejected or static health routes reached the upstream")
	}
}

func TestGatewayTLSClientsRequireCAAndCorrectServerName(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer fixture-auth" {
			t.Error("management TLS client lost its actual discovery contract")
		}
		io.WriteString(w, `{"data":[{"id":"fixture"}]}`)
	})
	server, roots := tlsFixture(t, upstream)
	expiredServer, expiredRoots := tlsFixture(t, upstream, true)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, scenario := range []struct {
		name       string
		roots      *x509.CertPool
		serverName string
		accepted   bool
		baseURL    string
	}{{"trusted", roots, "", true, server.URL}, {"untrusted", x509.NewCertPool(), "", false, server.URL}, {"wrong-SAN", roots, "other.invalid", false, server.URL}, {"expired", expiredRoots, "", false, expiredServer.URL}} {
		t.Run(scenario.name, func(t *testing.T) {
			transport := original.(*http.Transport).Clone()
			transport.TLSClientConfig = &tls.Config{RootCAs: scenario.roots, ServerName: scenario.serverName}
			http.DefaultTransport = transport
			defer transport.CloseIdleConnections()
			models, err := token.NewNewAPIGatewayAdapter().DiscoverModels(context.Background(), scenario.baseURL, []byte("fixture-auth"))
			if (err == nil) != scenario.accepted || (scenario.accepted && (len(models) != 1 || models[0].UpstreamModelRef != "fixture")) {
				t.Fatal("production discovery transport did not enforce CA/SAN/expiry or preserve the catalog")
			}
			policy, err := token.NewProviderURLPolicy("production-new", []string{"127.0.0.1"})
			if err != nil {
				t.Fatal("fixture provider policy failed")
			}
			err = token.NewHTTPModelValidator(policy).Validate(context.Background(), scenario.baseURL, []byte("fixture-auth"), []string{"fixture"})
			if (err == nil) != scenario.accepted {
				t.Fatal("production credential validation did not enforce CA/SAN/expiry")
			}
		})
	}
}

func TestTLSStreamFlushAndClientCancellationReachUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	server, roots := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: fixture-first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"stream":true}`))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("TLS stream did not flush before upstream EOF")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: fixture-first\n" {
		t.Fatal("TLS hop buffered or changed the SSE event")
	}
	response.Body.Close()
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS client cancellation did not close upstream")
	}
}

func TestTLSUpstreamFailureIsStatic(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(backend.URL)
	backend.Close()
	request := httptest.NewRequest(http.MethodGet, "/v1/models?canary=private-fixture", nil)
	request.Header.Set("Authorization", "Bearer private-fixture")
	response := httptest.NewRecorder()
	newHandler(target).ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || response.Body.String() != "New API upstream is unavailable\n" {
		t.Fatal("upstream connection error did not remain a static 502")
	}
}

func TestTLSMinimumVersion(t *testing.T) {
	server, roots := tlsFixture(t, http.NotFoundHandler())
	for _, version := range []uint16{tls.VersionTLS11, tls.VersionTLS12} {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: version, MaxVersion: version}}}
		response, err := client.Get(server.URL + "/health/live")
		if response != nil {
			response.Body.Close()
		}
		client.CloseIdleConnections()
		if (err == nil) != (version == tls.VersionTLS12) {
			t.Fatal("production TLS minimum version was not enforced")
		}
	}
}
