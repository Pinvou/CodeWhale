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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSystemProbePrivateCAIsolation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "controlled probe CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("probe sent authority")
		}
		io.WriteString(w, `{"success":true}`)
	}))
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	backend.StartTLS()
	defer backend.Close()
	base := testService(t, newMemoryStore()).config
	t.Run("probe_only_trust", func(t *testing.T) {
		cfg := base
		cfg.SystemProbeCAFile = ca
		service, err := NewService(cfg, newMemoryStore())
		if err != nil {
			t.Fatal(err)
		}
		if service.client == service.probeClient {
			t.Fatal("probe trust escaped isolation")
		}
		target := parseTestURL(t, backend.URL)
		if got := service.probe(context.Background(), SystemProbe{Target: target, JSONBooleanField: "success"}); got.Status != "HEALTHY" {
			t.Fatalf("probe=%+v", got)
		}
		response, err := service.client.Get(backend.URL)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("session/OIDC client acquired probe CA")
		}
		wrong := parseTestURL(t, strings.Replace(backend.URL, "127.0.0.1", "localhost", 1))
		if got := service.probe(context.Background(), SystemProbe{Target: wrong, JSONBooleanField: "success"}); got.Status != "UNHEALTHY" {
			t.Fatal("hostname validation bypassed")
		}
	})
	t.Run("empty_preserves_client", func(t *testing.T) {
		service, err := NewService(base, newMemoryStore())
		if err != nil {
			t.Fatal(err)
		}
		if service.client != service.probeClient {
			t.Fatal("empty trust changed default client")
		}
	})
	for _, c := range []struct{ name, filename, content string }{
		{"missing", filepath.Join(t.TempDir(), "absent.pem"), ""},
		{"malformed", filepath.Join(t.TempDir(), "invalid.pem"), "not a certificate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.content != "" {
				if err := os.WriteFile(c.filename, []byte(c.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := base
			cfg.SystemProbeCAFile = c.filename
			if _, err := NewService(cfg, newMemoryStore()); err == nil {
				t.Fatal("invalid CA did not reject startup")
			}
		})
	}
	t.Run("OIDC_guard_unchanged", func(t *testing.T) {
		cfg := base
		cfg.TokenURL = parseTestURL(t, "http://identity.example/token")
		cfg.SystemProbeCAFile = ca
		if _, err := NewService(cfg, newMemoryStore()); err != nil {
			t.Fatal("test environment probe trust incorrectly bound to token scheme", err)
		}
		cfg.PrivateCAFile = ca
		if _, err := NewService(cfg, newMemoryStore()); err == nil {
			t.Fatal("original private CA guard relaxed")
		}
		cfg.PrivateCAFile = ""
		cfg.Environment = "production"
		if _, err := NewService(cfg, newMemoryStore()); err == nil {
			t.Fatal("original production OIDC guard relaxed")
		}
	})
}

func TestSystemStatusControlledAccountAuthority(t *testing.T) {
	for _, c := range []struct {
		name                 string
		accountStatus        int
		payload              string
		session, validSource bool
		want                 int
	}{
		{"unauthenticated", 200, `{"platform_permissions":["platform.system.status.read"]}`, false, true, 401},
		{"allowed", 200, `{"platform_permissions":["platform.system.status.read"]}`, true, true, 200},
		{"missing_permission", 200, `{"platform_permissions":[]}`, true, true, 403},
		{"account_401", 401, `{}`, true, true, 403},
		{"account_403", 403, `{}`, true, true, 403},
		{"account_unavailable", 503, `{}`, true, true, 503},
		{"invalid_peer", 200, `{"platform_permissions":["platform.system.status.read"]}`, true, false, 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if c.session {
				if err := store.SaveSession(context.Background(), session); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/account/bootstrap" {
					calls++
					body, _ := io.ReadAll(r.Body)
					if r.Method != "POST" || string(body) != "{}" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-Pinvou-Client-Source") != "192.0.2.20" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" {
						t.Error("bootstrap authority/source contract changed")
					}
					w.WriteHeader(c.accountStatus)
					io.WriteString(w, c.payload)
					return
				}
				w.WriteHeader(503)
			}))
			defer account.Close()
			service.config.AccountURL = parseTestURL(t, account.URL)
			request := financeBrowserRequest(service, session, "GET", "/api/admin/system/status", "")
			request.RemoteAddr = "192.0.2.20:1234"
			if !c.validSource {
				request.RemoteAddr = "invalid"
			}
			request.Header.Set("X-Forwarded-For", "198.51.100.1")
			request.Header.Set("X-Pinvou-Client-Source", "198.51.100.1")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != c.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, c.want, response.Body.String())
			}
			wantCalls := 0
			if c.session && c.validSource {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("bootstrap calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}
