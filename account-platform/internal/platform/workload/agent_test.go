package workload

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type fixture struct {
	agent        *Agent
	server       *httptest.Server
	key          *rsa.PrivateKey
	now          time.Time
	claims       map[string]any
	secret       string
	status       int
	body         string
	badSignature bool
	requests     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{key: key, now: time.Now().Truncate(time.Second), secret: "fixture-secret-never-production", status: http.StatusOK}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/certs") {
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
			return
		}
		f.requests++
		if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "fixture-client" || r.Form.Get("client_secret") != f.secret || len(r.Form) != 3 {
			http.Error(w, "fixture-rejected-credential", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", f.server.URL+"/must-not-follow")
		w.WriteHeader(f.status)
		if f.body != "" {
			_, _ = w.Write([]byte(f.body))
			return
		}
		claims := make(map[string]any)
		for k, v := range f.claims {
			claims[k] = v
		}
		if f.badSignature {
			claims["iss"] = f.server.URL + "/realms/fixture"
		}
		signer, signErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
		if signErr != nil {
			t.Error("fixture signer failed")
			return
		}
		payload, _ := json.Marshal(claims)
		signed, signErr := signer.Sign(payload)
		if signErr != nil {
			t.Error("fixture signing failed")
			return
		}
		raw, _ := signed.CompactSerialize()
		if f.badSignature {
			parts := strings.Split(raw, ".")
			parts[2] = "invalid-signature"
			raw = strings.Join(parts, ".")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": raw, "token_type": "Bearer", "expires_in": 999999})
	}))
	t.Cleanup(f.server.Close)
	root := t.TempDir()
	for _, name := range []string{"bootstrap", "projected"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{Issuer: f.server.URL + "/realms/fixture", Audience: "fixture-api", ClientID: "fixture-client", Subject: "fixture-service-uuid", ClientSecretFile: filepath.Join(root, "bootstrap", "secret"), TokenFile: filepath.Join(root, "projected", "token")}
	if err := os.WriteFile(config.ClientSecretFile, []byte(f.secret), 0o600); err != nil {
		t.Fatal(err)
	}
	f.claims = map[string]any{"iss": config.Issuer, "aud": config.Audience, "sub": config.Subject, "azp": config.ClientID, "iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix()}
	f.agent, err = newAgent(config, f.server.Client().Transport, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRefreshVerifiesAndRotates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	expiry, err := f.agent.Refresh(ctx)
	if err != nil || !expiry.Equal(f.now.Add(5*time.Minute)) {
		t.Fatal("valid token was not projected with signed expiry", err)
	}
	first, err := os.ReadFile(f.agent.config.TokenFile)
	if err != nil || len(first) == 0 {
		t.Fatal("missing projected token")
	}
	if err := f.agent.Check(ctx); err != nil {
		t.Fatal(err)
	}
	f.secret = "rotated-fixture-secret-not-production"
	if err := os.WriteFile(f.agent.config.ClientSecretFile, []byte(f.secret), 0o600); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.claims["iat"], f.claims["exp"] = f.now.Unix(), f.now.Add(5*time.Minute).Unix()
	if _, err := f.agent.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(f.agent.config.TokenFile)
	if string(first) == string(second) || f.requests != 2 {
		t.Fatal("token or client secret rotation failed")
	}
	files, _ := os.ReadDir(filepath.Dir(f.agent.config.TokenFile))
	if len(files) != 1 {
		t.Fatal("temporary projection files leaked")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(f.agent.config.TokenFile)
		if info.Mode().Perm() != 0o600 {
			t.Fatal("projected file is not private")
		}
	}
	f.status, f.body = http.StatusServiceUnavailable, "private-upstream-detail"
	if _, err := f.agent.Refresh(ctx); err == nil || strings.Contains(err.Error(), f.body) || strings.Contains(err.Error(), f.secret) {
		t.Fatal("failure leaked upstream data or was accepted")
	}
	after, _ := os.ReadFile(f.agent.config.TokenFile)
	if string(after) != string(second) {
		t.Fatal("failure replaced valid projection")
	}
	f.now = f.now.Add(6 * time.Minute)
	if err := f.agent.Check(ctx); err == nil {
		t.Fatal("expired retained token accepted")
	}
	if err := os.Remove(f.agent.config.ClientSecretFile); err != nil {
		t.Fatal(err)
	}
	before := f.requests
	if _, err := f.agent.Refresh(ctx); err == nil || f.requests != before {
		t.Fatal("missing secret reused cached credentials")
	}
}

func TestRejectsInvalidIdentityAndTime(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		name, claim string
		value       any
	}{
		{"subject", "sub", "other-service"}, {"audience", "aud", "other-api"}, {"issuer", "iss", "https://invalid.example/realms/fixture"},
		{"client", "azp", "other-client"}, {"missing-issued", "iat", nil}, {"future-issued", "iat", f.now.Add(time.Second).Unix()},
		{"future-not-before", "nbf", f.now.Add(time.Second).Unix()}, {"expired", "exp", f.now.Add(-time.Second).Unix()},
		{"long-life", "exp", f.now.Add(16 * time.Minute).Unix()}, {"nearly-expired", "exp", f.now.Add(29 * time.Second).Unix()},
		{"zero-life", "exp", f.now.Unix()},
	} {
		t.Run(test.name, func(t *testing.T) {
			original, exists := f.claims[test.claim]
			f.claims[test.claim] = test.value
			defer func() {
				if exists {
					f.claims[test.claim] = original
				} else {
					delete(f.claims, test.claim)
				}
			}()
			if _, err := f.agent.Refresh(context.Background()); err == nil {
				t.Fatal("invalid claims accepted")
			}
			if _, err := os.Stat(f.agent.config.TokenFile); !os.IsNotExist(err) {
				t.Fatal("invalid token published")
			}
		})
	}
	f.badSignature = true
	if _, err := f.agent.Refresh(context.Background()); err == nil {
		t.Fatal("invalid signature accepted")
	}
}

func TestRejectsTransportAndResponseFailures(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 307, "private-detail"}, {"denied", 401, "private-detail"}, {"sealed-upstream", 503, "private-detail"},
		{"invalid-json", 200, "private-detail"}, {"oversize", 200, strings.Repeat("x", maximumResponse+1)},
		{"trailing-json", 200, `{"access_token":"private-token","token_type":"Bearer"}{}`},
		{"wrong-type", 200, `{"access_token":"private-token","token_type":"Basic"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.status, f.body = test.status, test.body
			before := f.requests
			if _, err := f.agent.Refresh(context.Background()); err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("invalid response accepted or leaked")
			}
			if f.requests != before+1 {
				t.Fatal("redirect followed")
			}
		})
	}
	untrusted, err := New(f.agent.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Refresh(context.Background()); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestConfigAndFilesystemFailClosed(t *testing.T) {
	f := newFixture(t)
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Issuer = "http://example.test/realms/workload" },
		func(c *Config) { c.Issuer = "https://user:pass@example.test/realms/workload" },
		func(c *Config) { c.Issuer += "?leak=true" }, func(c *Config) { c.Issuer += "/" },
		func(c *Config) { c.Subject = "" }, func(c *Config) { c.ClientSecretFile = "relative" },
		func(c *Config) { c.ClientSecretFile = filepath.Join(filepath.Dir(c.TokenFile), "secret") },
		func(c *Config) { c.ClientSecretFile = filepath.Join(filepath.Dir(c.TokenFile), "nested", "secret") },
	} {
		config := f.agent.config
		mutate(&config)
		if _, err := New(config); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(f.agent.config)
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(body, []byte("{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(configPath); err == nil {
		t.Fatal("trailing config accepted")
	}
	if err := os.Mkdir(f.agent.config.TokenFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agent.Refresh(context.Background()); err == nil {
		t.Fatal("directory destination accepted")
	}
}

func TestRunCancellationAndSafeEvents(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.status, f.body = 401, "private-denial-data"
	var events []string
	err := f.agent.Run(ctx, func(message string) { events = append(events, message); cancel() })
	if err != context.Canceled || len(events) != 1 || events[0] != "workload refresh failed; retry scheduled" {
		t.Fatal("unsafe event or cancellation failure")
	}
}
