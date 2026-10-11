package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/initialize"
)

type fixture struct {
	cfg       Config
	id        *age.X25519Identity
	envelope  initialize.Envelope
	requests  atomic.Int32
	writes    atomic.Int32
	sealed    atomic.Bool
	postMode  string
	stateBody string
	wrongHost bool
	server    *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.sealed.Store(true)
	f.id, _ = age.GenerateX25519Identity()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Host != strings.TrimPrefix(f.cfg.Endpoint, "https://") {
			t.Error("tunnel changed logical Host")
		}
		if r.Header.Get("X-Vault-Token") != "" {
			t.Error("unseal sent root or other token")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/sys/seal-status" {
			if f.stateBody != "" {
				io.WriteString(w, f.stateBody)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"initialized": true, "sealed": f.sealed.Load(), "type": "shamir", "n": 1, "t": 1})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/sys/unseal" {
			t.Error("unexpected API operation")
			w.WriteHeader(400)
			return
		}
		f.writes.Add(1)
		body, _ := io.ReadAll(r.Body)
		defer clear(body)
		var payload map[string]string
		if json.Unmarshal(body, &payload) != nil || len(payload) != 1 || payload["key"] != key {
			t.Error("invalid unseal payload or extra operation")
		}
		switch f.postMode {
		case "error":
			w.WriteHeader(500)
			io.WriteString(w, "secret-error-canary")
			return
		case "redirect":
			w.Header().Set("Location", "/v1/sys/unseal")
			w.WriteHeader(307)
			return
		case "disconnect":
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
			return
		case "truncated":
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, "secret-error-canary")
			return
		case "still-sealed":
			io.WriteString(w, `{}`)
			return
		}
		f.sealed.Store(false)
		io.WriteString(w, `{"sealed":false}`)
	}))
	t.Cleanup(f.server.Close)
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	f.cfg = Config{Endpoint: f.server.URL, CAFile: filepath.Join(dir, "ca.crt"), RecipientSHA256: hash([]byte(f.id.Recipient().String())), InstanceID: "isolated-operator-fixture"}
	if err := os.WriteFile(f.cfg.CAFile, ca, 0600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"keys_base64": []string{key}, "root_token": "root-canary-must-not-be-sent"})
	f.envelope = initialize.Envelope{Version: 1, Endpoint: f.cfg.Endpoint, CASHA256: hash(ca), RecipientSHA256: f.cfg.RecipientSHA256, InstanceID: f.cfg.InstanceID, CapturedAt: time.Now().UTC().Format(time.RFC3339), Complete: true, HTTPStatus: 200, Response: body}
	return f
}

func (f *fixture) bundle(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := custody.Encrypt(b, f.id.Recipient())
	clear(b)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func TestUnsealAndAlreadyUnsealed(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(t)
	changed, err := Unseal(context.Background(), f.cfg, b, f.id)
	if err != nil || !changed || f.writes.Load() != 1 {
		t.Fatal("unseal failed", err)
	}
	changed, err = Unseal(context.Background(), f.cfg, b, f.id)
	if err != nil || changed || f.writes.Load() != 1 {
		t.Fatal("already-unsealed target received another key")
	}
}

func TestUnsealBindingRejectedBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"version", "incomplete", "status", "instance", "origin", "ca", "recipient", "future", "timestamp", "key", "wrong-identity", "damaged"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			id := f.id
			switch kind {
			case "version":
				f.envelope.Version = 2
			case "incomplete":
				f.envelope.Complete = false
			case "status":
				f.envelope.HTTPStatus = 500
			case "instance":
				f.envelope.InstanceID = "other-instance-id"
			case "origin":
				f.envelope.Endpoint = "https://other.example.com"
			case "ca":
				f.envelope.CASHA256 = strings.Repeat("0", 64)
			case "recipient":
				f.envelope.RecipientSHA256 = strings.Repeat("0", 64)
			case "future":
				f.envelope.CapturedAt = time.Now().Add(time.Hour).Format(time.RFC3339)
			case "timestamp":
				f.envelope.CapturedAt = "invalid"
			case "key":
				f.envelope.Response = []byte(`{"keys_base64":["invalid"]}`)
			case "wrong-identity":
				id, _ = age.GenerateX25519Identity()
			}
			b := f.bundle(t)
			if kind == "damaged" {
				b[len(b)-1] ^= 1
			}
			_, err := Unseal(context.Background(), f.cfg, b, id)
			if err != ErrBinding || f.requests.Load() != 0 {
				t.Fatal("invalid binding caused a network request")
			}
		})
	}
}

func TestUnsealTransportPreservesTLSAndOrigin(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		name := "verified"
		if wrong {
			name = "wrong-hostname"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, port, _ := net.SplitHostPort(f.server.Listener.Addr().String())
			host := "example.com"
			if wrong {
				host = "wrong.example.invalid"
			}
			f.cfg.Endpoint = "https://" + net.JoinHostPort(host, port)
			f.envelope.Endpoint = f.cfg.Endpoint
			f.cfg.ConnectAddress = f.server.Listener.Addr().String()
			_, err := Unseal(context.Background(), f.cfg, f.bundle(t), f.id)
			if wrong {
				if err != ErrPreflight || f.writes.Load() != 0 {
					t.Fatal("tunnel bypassed TLS hostname")
				}
			} else if err != nil || f.writes.Load() != 1 {
				t.Fatal("verified tunnel failed", err)
			}
		})
	}
}

func TestUnsealWriteFailureDoesNotRetryOrLeak(t *testing.T) {
	for _, kind := range []string{"error", "redirect", "disconnect", "truncated", "still-sealed"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.postMode = kind
			_, err := Unseal(context.Background(), f.cfg, f.bundle(t), f.id)
			if err != ErrUncertain || f.writes.Load() != 1 || strings.Contains(err.Error(), "canary") {
				t.Fatal("unsafe unseal retry/result")
			}
		})
	}
}

func TestUnsealPreflightRejectsUnknownState(t *testing.T) {
	for _, body := range []string{`{}`, `{"initialized":false,"sealed":true,"type":"shamir","n":1,"t":1}`, `{"initialized":true,"sealed":true,"type":"shamir","n":3,"t":2}`, `{"initialized":true,"sealed":true,"type":"transit","n":1,"t":1}`} {
		t.Run(body, func(t *testing.T) {
			f := newFixture(t)
			f.stateBody = body
			_, err := Unseal(context.Background(), f.cfg, f.bundle(t), f.id)
			if err != ErrPreflight || f.writes.Load() != 0 {
				t.Fatal("unknown state accepted")
			}
		})
	}
}

func TestUnsealConfigRejectsUnsafeDestinations(t *testing.T) {
	for _, kind := range []string{"http", "path", "query", "userinfo", "remote-dial", "dns-dial", "missing-port", "bad-port", "zero-port"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "http":
				f.cfg.Endpoint = strings.Replace(f.cfg.Endpoint, "https:", "http:", 1)
			case "path":
				f.cfg.Endpoint += "/"
			case "query":
				f.cfg.Endpoint += "?canary"
			case "userinfo":
				f.cfg.Endpoint = strings.Replace(f.cfg.Endpoint, "https://", "https://canary@", 1)
			case "remote-dial":
				f.cfg.ConnectAddress = "192.0.2.1:8200"
			case "dns-dial":
				f.cfg.ConnectAddress = "localhost:8200"
			case "missing-port":
				f.cfg.ConnectAddress = "127.0.0.1"
			case "bad-port":
				f.cfg.ConnectAddress = "127.0.0.1:65536"
			case "zero-port":
				f.cfg.ConnectAddress = "127.0.0.1:0"
			}
			_, err := Unseal(context.Background(), f.cfg, f.bundle(t), f.id)
			if err != ErrConfig || f.requests.Load() != 0 {
				t.Fatal("unsafe connection accepted")
			}
		})
	}
}
