package initialize

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
)

const rootCanary = "test-only-initializer-root-canary"

func material() []byte {
	b, _ := json.Marshal(map[string]any{"keys_base64": []string{base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))}, "root_token": rootCanary})
	return b
}

func setup(t *testing.T, post http.HandlerFunc) (Config, *age.X25519Identity, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/sys/init" {
			io.WriteString(w, `{"initialized":false}`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/sys/seal-status" {
			io.WriteString(w, `{"initialized":false,"sealed":true,"type":"shamir"}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/sys/init" {
			t.Error("unexpected operation")
			w.WriteHeader(400)
			return
		}
		count.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"secret_shares":1,"secret_threshold":1}` {
			t.Error("unexpected shares or action")
		}
		post(w, r)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.Recipient().String()
	cfg := Config{Endpoint: server.URL, CAFile: filepath.Join(dir, "ca.crt"), RecipientFile: filepath.Join(dir, "recipient.txt"), RecipientSHA256: sum([]byte(pub)), OutputDirectory: filepath.Join(dir, "capture"), InstanceID: "isolated-fixture-20260907"}
	if err := os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RecipientFile, []byte(pub+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, id, count
}

func openEnvelope(t *testing.T, cfg Config, id age.Identity) Envelope {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.OutputDirectory, "initialization.age"))
	if err != nil {
		t.Fatal("encrypted response missing")
	}
	if bytes.Contains(b, []byte(rootCanary)) {
		t.Fatal("raw root persisted")
	}
	r, err := age.Decrypt(bytes.NewReader(b), id)
	if err != nil {
		t.Fatal("envelope cannot be decrypted")
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatal("envelope authentication failed")
	}
	defer clear(plain)
	var e Envelope
	if json.Unmarshal(plain, &e) != nil {
		t.Fatal("invalid envelope")
	}
	return e
}

func TestInitializationEncryptedCaptureAndNoOverwrite(t *testing.T) {
	cfg, id, count := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(material()) })
	if err := Run(context.Background(), cfg, true, true); err != nil {
		t.Fatal(err)
	}
	e := openEnvelope(t, cfg, id)
	ca, _ := os.ReadFile(cfg.CAFile)
	if e.Version != 1 || e.InstanceID != cfg.InstanceID || e.Endpoint != cfg.Endpoint || e.CASHA256 != sum(ca) || e.RecipientSHA256 != cfg.RecipientSHA256 || !e.Complete || e.HTTPStatus != 200 || !bytes.Equal(e.Response, material()) {
		t.Fatal("material or instance binding mismatch")
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDirectory, "INIT_CAPTURED")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"initialization.age", "INIT_ATTEMPTED", "INIT_CAPTURED"} {
		path := filepath.Join(cfg.OutputDirectory, name)
		b, _ := os.ReadFile(path)
		if bytes.Contains(b, []byte(rootCanary)) {
			t.Fatal("plaintext artifact")
		}
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("public output")
			}
		}
	}
	if err := Run(context.Background(), cfg, true, true); err != ErrOutput || count.Load() != 1 {
		t.Fatal("reinitialized or overwrote prior output")
	}
	other, _ := age.GenerateX25519Identity()
	b, _ := os.ReadFile(filepath.Join(cfg.OutputDirectory, "initialization.age"))
	if _, err := age.Decrypt(bytes.NewReader(b), other); err == nil {
		t.Fatal("wrong recipient accepted")
	}
	b[len(b)-1] ^= 1
	r, err := age.Decrypt(bytes.NewReader(b), id)
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("damaged package accepted")
	}
}

func TestInitializationUncertainNeverRetries(t *testing.T) {
	for _, kind := range []string{"error", "redirect", "truncated", "oversized", "missing-material", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			cfg, id, count := setup(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "error":
					w.WriteHeader(500)
					io.WriteString(w, rootCanary)
				case "redirect":
					w.Header().Set("Location", "/v1/sys/init")
					w.WriteHeader(307)
					io.WriteString(w, rootCanary)
				case "truncated":
					w.Header().Set("Content-Length", "10000")
					io.WriteString(w, rootCanary)
				case "oversized":
					io.WriteString(w, strings.Repeat("x", MaxResponse+2))
				case "missing-material":
					io.WriteString(w, `{"keys_base64":[],"root_token":"`+rootCanary+`"}`)
				case "disconnect":
					c, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						c.Close()
					}
				}
			})
			err := Run(context.Background(), cfg, true, true)
			if err != ErrUncertain || strings.Contains(err.Error(), rootCanary) || count.Load() != 1 {
				t.Fatal("uncertain result retried or misreported")
			}
			if _, err := os.Stat(filepath.Join(cfg.OutputDirectory, "INIT_CAPTURED")); !os.IsNotExist(err) {
				t.Fatal("uncertain result marked complete")
			}
			if _, err := os.Stat(filepath.Join(cfg.OutputDirectory, "INIT_ATTEMPTED")); err != nil {
				t.Fatal("attempt evidence lost")
			}
			if kind != "disconnect" {
				e := openEnvelope(t, cfg, id)
				if len(e.Response) == 0 {
					t.Fatal("received diagnostic not captured")
				}
			}
			if Run(context.Background(), cfg, true, true) != ErrOutput || count.Load() != 1 {
				t.Fatal("uncertain attempt repeated")
			}
		})
	}
}

func TestInitializationRejectsConfigurationBeforeMutation(t *testing.T) {
	for _, kind := range []string{"http", "path", "query", "fragment", "userinfo", "recipient-hash", "recipient-invalid", "trust", "output-parent", "custody", "fresh", "instance"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _, count := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(material()) })
			custody, fresh := true, true
			switch kind {
			case "http":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "https:", "http:", 1)
			case "path":
				cfg.Endpoint += "/"
			case "query":
				cfg.Endpoint += "?secret=canary"
			case "fragment":
				cfg.Endpoint += "#canary"
			case "userinfo":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "https://", "https://canary@", 1)
			case "recipient-hash":
				cfg.RecipientSHA256 = strings.Repeat("0", 64)
			case "recipient-invalid":
				os.WriteFile(cfg.RecipientFile, []byte("invalid"), 0600)
			case "trust":
				os.WriteFile(cfg.CAFile, []byte("invalid"), 0600)
			case "output-parent":
				cfg.OutputDirectory = filepath.Join(cfg.OutputDirectory, "missing", "child")
			case "custody":
				custody = false
			case "fresh":
				fresh = false
			case "instance":
				cfg.InstanceID = "../wrong"
			}
			if Run(context.Background(), cfg, custody, fresh) == nil || count.Load() != 0 {
				t.Fatal("invalid input mutated remote")
			}
		})
	}
}

func TestInitializationFailsClosedOnUnknownOrInitializedStatus(t *testing.T) {
	for _, response := range []string{`{}`, `{"initialized":null}`, `{"initialized":true}`, `{"initialized":"false"}`} {
		t.Run(response, func(t *testing.T) {
			cfg, _, count := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(material()) })
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					count.Add(1)
				}
				io.WriteString(w, response)
			}))
			defer s.Close()
			cfg.Endpoint = s.URL
			os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
			if Run(context.Background(), cfg, true, true) != ErrPreflight || count.Load() != 0 {
				t.Fatal("non-fresh or unknown instance initialized")
			}
		})
	}
}

func TestCaptureWriteFailureReturnsUncertain(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	f, err := os.CreateTemp(t.TempDir(), "closed-")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if capture(f, id.Recipient(), Envelope{Response: material()}) != ErrUncertain {
		t.Fatal("write failure not uncertain")
	}
}

func TestInitializationRejectsUntrustedTLS(t *testing.T) {
	cfg, _, count := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(material()) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(7), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600) != nil {
		t.Fatal("test CA write failed")
	}
	if Run(context.Background(), cfg, true, true) != ErrPreflight || count.Load() != 0 {
		t.Fatal("untrusted TLS initialized instance")
	}
}

func TestInitializerConfigFileAndPrivatePaths(t *testing.T) {
	cfg, _, count := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(material()) })
	path := filepath.Join(filepath.Dir(cfg.OutputDirectory), "config.json")
	b, _ := json.Marshal(cfg)
	if os.WriteFile(path, b, 0600) != nil {
		t.Fatal("config write failed")
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded != cfg {
		t.Fatal("config roundtrip failed")
	}
	for _, content := range [][]byte{append(append([]byte{}, b...), []byte(` {}`)...), []byte(`{"unexpected":"canary"}`), []byte(`{"endpoint":`), bytes.Repeat([]byte{'x'}, (16<<10)+1)} {
		if os.WriteFile(path, content, 0600) != nil {
			t.Fatal("config write failed")
		}
		if _, err := LoadConfig(path); err != ErrConfig {
			t.Fatal("invalid configuration accepted")
		}
	}
	if _, err := LoadConfig("relative.json"); err != ErrConfig {
		t.Fatal("relative configuration accepted")
	}
	if count.Load() != 0 {
		t.Fatal("config parsing had network effect")
	}
	if runtime.GOOS == "windows" {
		return
	} // POSIX assertions run in Linux fixture.
	if os.WriteFile(path, b, 0666) != nil || os.Chmod(path, 0666) != nil {
		t.Fatal("fixture permissions failed")
	}
	if _, err := LoadConfig(path); err != ErrConfig {
		t.Fatal("writable configuration accepted")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("fixture permissions failed")
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if os.Symlink(path, link) != nil {
		t.Fatal("fixture link failed")
	}
	if _, err := LoadConfig(link); err != ErrConfig {
		t.Fatal("symlink configuration accepted")
	}
	if os.Chmod(filepath.Dir(cfg.OutputDirectory), 0755) != nil {
		t.Fatal("fixture permissions failed")
	}
	if Run(context.Background(), cfg, true, true) != ErrOutput || count.Load() != 0 {
		t.Fatal("public output parent accepted")
	}
}
