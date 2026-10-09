package portal

import (
	"context"
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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPrivateCAExtendsOnlyPortalClientAndRejectsRedirect(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "portal test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "loopback test server"}, NotBefore: caTemplate.NotBefore, NotAfter: caTemplate.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer destination.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	defer server.Close()
	certFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newPortalHTTPClient(certFile)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil || response.StatusCode != 200 {
		t.Fatal("configured private CA was not trusted")
	}
	response.Body.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-only-server-token")
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusTemporaryRedirect || reached.Load() != 0 {
		t.Fatal("credential-bearing client followed redirect")
	}
	response.Body.Close()
	ordinary, err := newPortalHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.CloseIdleConnections()
	if response, err := ordinary.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("private trust changed the default client")
	}
	for _, body := range [][]byte{nil, []byte("not-a-certificate"), append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), []byte("garbage")...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("test-only-invalid")})} {
		path := filepath.Join(t.TempDir(), "invalid-ca.pem")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newPortalHTTPClient(path); err == nil {
			t.Fatal("invalid/private/non-CA material accepted")
		}
	}
}

func TestRefreshErrorBodyCannotDeleteFromPrefixOrUnavailableStatus(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
	}{
		{503, `{"error":"invalid_grant"}`}, {400, `{"error":"invalid_grant"} {}`}, {400, `{"error":"invalid_grant"}` + strings.Repeat(" ", 1<<20)},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			store := newMemoryStore()
			service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			session := validSession(uuid.NewString())
			session.AccessExpiresAt = time.Now().Add(-time.Minute)
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 503 || response.Header().Get("Set-Cookie") != "" {
				t.Fatal("malformed/unavailable response classified as logout")
			}
			if current, err := store.GetSession(context.Background(), session.ID); err != nil || current != session {
				t.Fatal("failed refresh mutated current session")
			}
		})
	}
}

func TestHandoffAccountIdentityRequiresCompleteNonzeroUUID(t *testing.T) {
	for _, body := range []string{`{"user_id":"00000000-0000-0000-0000-000000000000"}`, `{"user_id":"11111111-1111-4111-8111-111111111111"} {}`, `{"user_id":"11111111-1111-4111-8111-111111111111"}` + strings.Repeat(" ", 64<<10)} {
		t.Run("invalid identity", func(t *testing.T) {
			service, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
			dependency := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer dependency.Close()
			service.config.AccountURL = parseTestURL(t, dependency.URL)
			if _, err := service.accountUserID(context.Background(), "test-only"); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
