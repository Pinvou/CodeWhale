package operator

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

func newSnapshotFixture(t *testing.T) (*fixture, SnapshotConfig, []byte) {
	t.Helper()
	f := newFixture(t)
	parent := t.TempDir()
	os.Chmod(parent, 0700)
	for _, name := range []string{"recipient", "token", "output"} {
		if os.Mkdir(filepath.Join(parent, name), 0700) != nil {
			t.Fatal("test directory failed")
		}
	}
	cfg := SnapshotConfig{Config: f.cfg, RecipientFile: filepath.Join(parent, "recipient", "recipient.txt"), TokenFile: filepath.Join(parent, "token", "token"), OutputDirectory: filepath.Join(parent, "output")}
	os.WriteFile(cfg.RecipientFile, []byte(f.id.Recipient().String()), 0600)
	os.WriteFile(cfg.TokenFile, []byte("test-only-backup-token-not-real"), 0600)
	data := make([]byte, 512)
	rand.Read(data)
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Close()
	raw := b.Bytes()
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != "GET" || r.Header.Get("X-Vault-Token") != "test-only-backup-token-not-real" {
			t.Error("unexpected snapshot operation or identity")
		}
		if r.URL.Path == "/v1/auth/token/lookup-self" {
			io.WriteString(w, `{"data":{"policies":["pinvou-kms-backup"],"ttl":180,"explicit_max_ttl":900,"type":"service"}}`)
		} else if r.URL.Path == "/v1/sys/storage/raft/snapshot" {
			w.Write(raw)
		} else {
			t.Error("unapproved snapshot endpoint")
			w.WriteHeader(404)
		}
	})
	return f, cfg, raw
}

func TestSnapshotTLSCaptureAndOfflineVerify(t *testing.T) {
	f, cfg, raw := newSnapshotFixture(t)
	out, err := CaptureSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := readSnapshotFile(filepath.Join(out, "snapshot.age"))
	if err != nil || bytes.Contains(encrypted, raw) || bytes.Contains(encrypted, []byte("test-only-backup-token-not-real")) {
		t.Fatal("snapshot is not encrypted")
	}
	manifest, _ := os.ReadFile(filepath.Join(out, "SNAPSHOT_READY"))
	var meta map[string]any
	if json.Unmarshal(manifest, &meta) != nil || meta["ciphertext_sha256"] != hash(encrypted) {
		t.Fatal("snapshot completion manifest invalid")
	}
	second, err := CaptureSnapshot(context.Background(), cfg)
	if err != nil || second == out || f.requests.Load() != 4 {
		t.Fatal("repeated capture overwrote prior backup")
	}
	f.server.Close()
	verified, err := VerifySnapshot(cfg.Config, encrypted, f.id)
	if err != nil || !bytes.Equal(verified, raw) || f.requests.Load() != 4 {
		t.Fatal("offline verification failed or contacted server")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := VerifySnapshot(cfg.Config, encrypted, other); err == nil {
		t.Fatal("wrong recipient accepted")
	}
	broken := append([]byte(nil), encrypted...)
	broken[len(broken)-1] ^= 1
	for _, bad := range [][]byte{broken, encrypted[:len(encrypted)-1]} {
		if _, err := VerifySnapshot(cfg.Config, bad, f.id); err == nil {
			t.Fatal("corrupt or truncated snapshot accepted")
		}
	}
	wrong := cfg.Config
	wrong.InstanceID = "wrong-instance-must-reject"
	if _, err := VerifySnapshot(wrong, encrypted, f.id); err == nil {
		t.Fatal("wrong instance accepted")
	}
	if err := writeSnapshotFile(filepath.Join(out, "snapshot.age"), []byte("overwrite")); err == nil {
		t.Fatal("existing snapshot overwritten")
	}
}

func TestSnapshotRejectsUnsafeIdentityAndResponses(t *testing.T) {
	for _, mode := range []string{"wrong-role", "expired", "extra-policy", "unbounded", "denied", "redirect", "truncated", "invalid-gzip", "bad-crc", "bound"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, raw := newSnapshotFixture(t)
			old := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "lookup-self") {
					switch mode {
					case "wrong-role":
						io.WriteString(w, `{"data":{"policies":["root"],"ttl":180,"explicit_max_ttl":900,"type":"service"}}`)
						return
					case "expired", "extra-policy", "unbounded":
						ttl, maximum, policies := 180, 900, []string{"pinvou-kms-backup"}
						if mode == "expired" {
							ttl = 0
						}
						if mode == "unbounded" {
							maximum = 0
						}
						if mode == "extra-policy" {
							policies = append(policies, "default")
						}
						json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"policies": policies, "ttl": ttl, "explicit_max_ttl": maximum, "type": "service"}})
						return
					}
					old.ServeHTTP(w, r)
					return
				}
				switch mode {
				case "denied":
					w.WriteHeader(403)
				case "redirect":
					w.Header().Set("Location", "/v1/sys/storage/raft/snapshot")
					w.WriteHeader(307)
				case "truncated":
					w.Header().Set("Content-Length", "99999")
					w.Write(raw)
				case "invalid-gzip":
					w.Write(bytes.Repeat([]byte("secret-error-canary"), 100))
				case "bad-crc":
					raw[len(raw)-1] ^= 1
					w.Write(raw)
				case "bound":
					w.Write(raw)
				default:
					t.Error("snapshot fetched despite invalid identity")
				}
			})
			if mode == "bound" {
				c, _, _ := client(cfg.Config)
				defer c.CloseIdleConnections()
				if _, err := snapshotRead(context.Background(), c, cfg, "test-only-backup-token-not-real", "sys/storage/raft/snapshot", 32); err == nil {
					t.Fatal("response limit ignored")
				}
			} else if _, err := CaptureSnapshot(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("invalid backup accepted or error leaked")
			}
			entries, _ := filepath.Glob(filepath.Join(cfg.OutputDirectory, "*", "SNAPSHOT_READY"))
			if len(entries) != 0 {
				t.Fatal("invalid backup marked complete")
			}
		})
	}
}

func TestSnapshotRejectsConfigurationAndPermissions(t *testing.T) {
	for _, mode := range []string{"recipient", "http", "ca", "relative-output", "overlap", "token", "mode"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, _ := newSnapshotFixture(t)
			switch mode {
			case "recipient":
				cfg.RecipientSHA256 = strings.Repeat("0", 64)
			case "http":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "https:", "http:", 1)
			case "ca":
				os.WriteFile(cfg.CAFile, []byte("invalid"), 0600)
			case "relative-output":
				cfg.OutputDirectory = "relative"
			case "overlap":
				cfg.OutputDirectory = filepath.Dir(cfg.TokenFile)
			case "token":
				os.WriteFile(cfg.TokenFile, []byte("invalid\nvalue"), 0600)
			case "mode":
				if runtime.GOOS == "windows" {
					return
				}
				os.Chmod(cfg.TokenFile, 0644)
			}
			if _, err := CaptureSnapshot(context.Background(), cfg); err == nil || f.requests.Load() != 0 {
				t.Fatal("unsafe snapshot input contacted service")
			}
		})
	}
}
