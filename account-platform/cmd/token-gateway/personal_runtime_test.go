package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

func TestF04PersonalGatewayConstructors(t *testing.T) {
	for _, tc := range []struct {
		name, account, billing, credential string
		valid                              bool
	}{
		{"valid", "https://account.invalid", "https://billing.invalid", "controlled", true},
		{"missing-account", "", "https://billing.invalid", "controlled", false},
		{"missing-billing", "https://account.invalid", "", "controlled", false},
		{"missing-workload", "https://account.invalid", "https://billing.invalid", "", false},
		{"credentialed-url", "https://private:secret@account.invalid", "https://billing.invalid", "controlled", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newPersonalGatewayService(tc.account, tc.billing, tc.credential)
			if (err == nil) != tc.valid || (err == nil && s == nil) {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestF04TokenProfileFailClosed(t *testing.T) {
	for _, profile := range []string{"", "models", "unknown"} {
		t.Run("profile-"+profile, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestF04PersonalGatewayProcess$", "-test.count=1")
			command.Env = append(personalProcessEnvironment(), "F04_PERSONAL_GATEWAY_TEST_PROCESS=1", "APP_ENV=test", "ACCOUNT_PLATFORM_PROFILE="+profile, "TOKEN_SECRET_PROVIDER=invalid-model-provider", "TOKEN_LOCAL_KEK_BASE64=", "DATABASE_URL=")
			output, err := command.CombinedOutput()
			if err == nil || ctx.Err() != nil || !bytes.Contains(output, []byte(`"level":"ERROR"`)) {
				t.Fatalf("profile=%q failed to reject before model DB/startup: err=%v", profile, err)
			}
		})
	}
}

func personalProcessEnvironment() []string {
	env := []string{}
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		if strings.HasPrefix(name, "TOKEN_") || strings.HasPrefix(name, "OIDC_") || strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "PINVOU_TEST_") || strings.HasPrefix(name, "F17_TEST_") || name == "DATABASE_URL" || name == "WORKLOAD_IDENTITY_TOKEN_FILE" || name == "INTERNAL_SERVICE_TOKEN" || name == "APP_ENV" || name == "PORT" || name == "METRICS_ADDRESS" || name == "ACCOUNT_PLATFORM_PROFILE" || name == "ACCOUNT_HTTP_URL" || name == "BILLING_HTTP_URL" || name == "F04_PERSONAL_GATEWAY_TEST_PROCESS" {
			continue
		}
		env = append(env, value)
	}
	return env
}

// This reexecutes the real main/profile/OIDC/HTTP assembly, not an injected
// balance handler. Only Account, Billing and JWKS are controlled loopback peers.
func TestF04PersonalGatewayProcess(t *testing.T) {
	if os.Getenv("F04_PERSONAL_GATEWAY_TEST_PROCESS") == "1" {
		main()
		return
	}
	user, wallet := uuid.New(), uuid.New()
	var denied, accountDown, billingDown atomic.Bool
	var contextCalls, billingCalls atomic.Int32
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accountDown.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/authorization/check":
			if r.Header.Get("Authorization") != "Bearer controlled-workload" {
				t.Error("authorization workload credential missing")
			}
			var body struct {
				Permission string `json:"permission"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Permission != "personal.billing.read" {
				t.Error("personal permission changed")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": !denied.Load(), "user_id": user})
		case "/v1/me/authorization-context":
			contextCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"user_id": user, "session_id": "controlled", "context_type": "PERSONAL", "context_version": 1, "authorization_version": 1})
		default:
			t.Errorf("unexpected Account path=%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer accountServer.Close()
	balance := fmt.Sprintf(`{"wallet_id":%q,"owner_type":"PERSONAL","owner_id":%q,"status":"ACTIVE","version":1,"available_microcredits":9007199254740993}`, wallet.String(), user.String())
	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		billingCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer controlled-workload" {
			t.Error("Billing workload credential missing")
		}
		if billingDown.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/wallets/ensure":
			var body struct {
				OwnerType string    `json:"owner_type"`
				OwnerID   uuid.UUID `json:"owner_id"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.OwnerType != "PERSONAL" || body.OwnerID != user {
				t.Error("wallet owner changed")
			}
			w.WriteHeader(201)
			_, _ = io.WriteString(w, balance)
		case "/internal/v1/wallets/" + wallet.String() + "/balance":
			_, _ = io.WriteString(w, balance)
		default:
			t.Errorf("unexpected Billing path=%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer billingServer.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "profile-fixture", Use: "sig", Algorithm: "RS256"}}})
	}))
	defer jwks.Close()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "profile-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"iss": jwks.URL, "aud": "personal-fixture", "sub": user.String(), "sid": "controlled", "typ": "Bearer", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
	signed, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	port := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return p
	}
	appPort, metricsPort := port(), port()
	command := exec.Command(os.Args[0], "-test.run=^TestF04PersonalGatewayProcess$", "-test.count=1")
	env := personalProcessEnvironment()
	command.Env = append(env, "F04_PERSONAL_GATEWAY_TEST_PROCESS=1", "APP_ENV=test", "ACCOUNT_PLATFORM_PROFILE=personal", "ACCOUNT_HTTP_URL="+accountServer.URL, "BILLING_HTTP_URL="+billingServer.URL, "INTERNAL_SERVICE_TOKEN=controlled-workload", "OIDC_ISSUER="+jwks.URL, "OIDC_JWKS_URL="+jwks.URL, "OIDC_AUDIENCE=personal-fixture", "PORT="+strconv.Itoa(appPort), "METRICS_ADDRESS=127.0.0.1:"+strconv.Itoa(metricsPort), "TOKEN_SECRET_PROVIDER=invalid-model-provider", "DATABASE_URL=invalid-model-database")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	completed := make(chan struct{})
	var processErr error
	go func() { processErr = command.Wait(); close(completed) }()
	t.Cleanup(func() { _ = command.Process.Kill(); <-completed })
	base := "http://127.0.0.1:" + strconv.Itoa(appPort)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(base + "/health/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case <-completed:
			t.Fatalf("personal real main exited=%v output=%s", processErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("personal main did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	request := func(path string, authenticated bool) (int, string, string) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(payload), resp.Header.Get("Cache-Control")
	}
	t.Run("balance-real-oidc-lossless", func(t *testing.T) {
		code, body, cache := request("/v1/me/balance", true)
		if code != 200 || body != balance || cache != "no-store" {
			t.Fatalf("status=%d cache=%s body=%s", code, cache, body)
		}
	})
	t.Run("oidc-required", func(t *testing.T) {
		n := billingCalls.Load()
		code, _, _ := request("/v1/me/balance", false)
		if code != 401 || billingCalls.Load() != n {
			t.Fatal("unauthenticated balance reached Billing")
		}
	})
	t.Run("permission-denial-before-wallet", func(t *testing.T) {
		denied.Store(true)
		defer denied.Store(false)
		n := billingCalls.Load()
		c := contextCalls.Load()
		code, _, _ := request("/v1/me/balance", true)
		if code != 403 || billingCalls.Load() != n || contextCalls.Load() != c {
			t.Fatal("denied permission reached context/wallet")
		}
	})
	t.Run("account-unavailable", func(t *testing.T) {
		accountDown.Store(true)
		defer accountDown.Store(false)
		n := billingCalls.Load()
		code, _, _ := request("/v1/me/balance", true)
		if code != 503 || billingCalls.Load() != n {
			t.Fatal("failed authority reached Billing")
		}
	})
	t.Run("billing-unavailable", func(t *testing.T) {
		billingDown.Store(true)
		defer billingDown.Store(false)
		code, _, _ := request("/v1/me/balance", true)
		if code != 503 {
			t.Fatalf("Billing failure status=%d", code)
		}
	})
	t.Run("management-routes-absent", func(t *testing.T) {
		for _, path := range []string{"/admin/v1/models", "/admin/v1/overview", "/internal/v1/token-governance/activation"} {
			code, _, _ := request(path, true)
			if code != 404 {
				t.Fatalf("personal management exposed=%s status=%d", path, code)
			}
		}
	})
}
