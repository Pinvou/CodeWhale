package newapitls

import (
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
)

func statusTLSFixture(t *testing.T, upstream http.Handler, expired ...bool) (*httptest.Server, *x509.CertPool) {
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
	server, err := NewStatusServer("", certificatePath, keyPath)
	if err != nil {
		t.Fatal("mounted certificate did not configure the production TLS server")
	}
	if server.WriteTimeout != 0 {
		t.Fatal("TLS server added a stream response deadline")
	}
	if server.TLSConfig.MinVersion != tls.VersionTLS12 || server.ReadHeaderTimeout != 5*time.Second || server.IdleTimeout != 90*time.Second {
		t.Fatal("status listener changed the inherited TLS or HTTP limits")
	}
	denied := httptest.NewRecorder()
	server.Handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if denied.Code != http.StatusNotFound {
		t.Fatal("production constructor admitted a provider route")
	}
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	server.Handler = newStatusHandler(target) // Controlled same-pod HTTP fixture, not live New API.
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

func TestStatusTLSExactRequestAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int64
	front, roots := statusTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/status" || !strings.HasPrefix(r.Host, "127.0.0.1:") {
			t.Error("status request path, method or same-pod host changed")
		}
		for _, h := range []string{"Authorization", "Cookie", "New-Api-User", "X-Forwarded-For", "X-Forwarded-Host", "X-Request-ID", "Traceparent"} {
			if r.Header.Get(h) != "" {
				t.Errorf("caller header forwarded: %s", h)
			}
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("status accept header changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "forbidden=1")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest(http.MethodGet, front.URL+"/api/status", nil)
	request.Host = "caller.invalid"
	for _, h := range []string{"Authorization", "Cookie", "New-Api-User", "X-Forwarded-For", "X-Forwarded-Host", "X-Request-ID", "Traceparent"} {
		request.Header.Set(h, "controlled-caller-value")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != `{"success":true}` || response.Header.Get("Set-Cookie") != "" || calls.Load() != 1 {
		t.Fatal("real backend status/body or response isolation failed")
	}
	for _, c := range []struct{ name, method, path, body string }{
		{"query", "GET", "/api/status?debug=1", ""},
		{"empty_query", "GET", "/api/status?", ""},
		{"escaped_alias", "GET", "/api/%73tatus", ""},
		{"head", "HEAD", "/api/status", ""},
		{"post", "POST", "/api/status", ""},
		{"provider_models", "GET", "/v1/models", ""},
		{"provider_chat", "POST", "/v1/chat/completions", "{}"},
		{"static_health", "GET", "/health/ready", ""},
		{"body", "GET", "/api/status", "body"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, _ := http.NewRequest(c.method, front.URL+c.path, strings.NewReader(c.body))
			r, e := client.Do(q)
			if e != nil {
				t.Fatal(e)
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != 404 || calls.Load() != 1 {
				t.Fatal("invalid request reached provider")
			}
		})
	}
}

func TestStatusTLSNonSuccessIsNotListenerReadiness(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int64
			front, roots := statusTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/v1/models")
				w.Header().Set("Set-Cookie", "bad=1")
				w.WriteHeader(code)
				io.WriteString(w, "provider-state")
			}))
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			r, e := client.Get(front.URL + "/api/status")
			if e != nil {
				t.Fatal(e)
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode != code || string(b) != "provider-state" || r.Header.Get("Location") != "" || r.Header.Get("Set-Cookie") != "" || calls.Load() != 1 {
				t.Fatal("upstream failure or redirect was converted/followed")
			}
		})
	}
}

func TestStatusTLSRequiresCAAndServerName(t *testing.T) {
	front, roots := statusTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"success":true}`) }))
	for _, c := range []struct {
		name string
		tls  *tls.Config
	}{
		{"untrusted", &tls.Config{MinVersion: tls.VersionTLS12}},
		{"wrong_name", &tls.Config{RootCAs: roots, ServerName: "wrong.invalid", MinVersion: tls.VersionTLS12}},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: c.tls}}
			if r, e := client.Get(front.URL + "/api/status"); e == nil {
				r.Body.Close()
				t.Fatal("invalid TLS trust accepted")
			}
		})
	}
}

func TestStatusTLSRejectsTransferEncodingAndMissingCertificate(t *testing.T) {
	if _, e := NewStatusServer(":8444", "missing.crt", "missing.key"); e == nil {
		t.Fatal("missing cert accepted")
	}
	h := newStatusHandler(&url.URL{Scheme: "http", Host: "127.0.0.1:1"})
	for _, c := range []struct {
		name     string
		length   int64
		transfer []string
		body     io.ReadCloser
	}{
		{"unknown_body", -1, nil, io.NopCloser(strings.NewReader(""))},
		{"chunked", 0, []string{"chunked"}, http.NoBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://newapi/api/status", nil)
			r.ContentLength = c.length
			r.TransferEncoding = c.transfer
			r.Body = c.body
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatal("ambiguous body admitted")
			}
		})
	}
}
