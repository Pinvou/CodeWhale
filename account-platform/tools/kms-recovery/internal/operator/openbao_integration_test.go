package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/initialize"
)

func TestOperatorOpenBaoIntegration(t *testing.T) {
	if os.Getenv("PINVOU_TEST_OPENBAO_OPERATOR_ISOLATED") != "1" {
		t.Skip("explicit isolated operator fixture required")
	}
	endpoint := os.Getenv("PINVOU_TEST_OPENBAO_ENDPOINT")
	root := os.Getenv("PINVOU_TEST_OPENBAO_FIXTURE_ROOT")
	u, err := url.Parse(endpoint)
	if runtime.GOOS != "linux" || err != nil || u.Scheme != "https" || u.Port() != "8200" || !net.ParseIP(u.Hostname()).IsPrivate() || filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "pinvou-openbao-fixture-") {
		t.Fatal("unsafe fixture target")
	}
	parent := t.TempDir()
	os.Chmod(parent, 0700)
	dir := filepath.Join(parent, "custody")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture custody failed")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("fixture identity failed")
	}
	pass := []byte("fixture-only-operator-password-not-production")
	defer clear(pass)
	password, err := age.NewScryptRecipient(string(pass))
	if err != nil {
		t.Fatal("fixture wrapper failed")
	}
	identityBytes := []byte(id.String())
	wrapped, err := custody.Encrypt(identityBytes, password)
	clear(identityBytes)
	if err != nil {
		t.Fatal("fixture identity encryption failed")
	}
	for name, data := range map[string][]byte{"identity.age": wrapped, "recipient.txt": []byte(id.Recipient().String()), "CUSTODY_READY": []byte("TEST_FIXTURE_ONLY")} {
		if os.WriteFile(filepath.Join(dir, name), data, 0600) != nil {
			t.Fatal("fixture private files failed")
		}
	}
	initCfg := initialize.Config{Endpoint: endpoint, CAFile: filepath.Join(root, "trust", "ca.crt"), RecipientFile: filepath.Join(dir, "recipient.txt"), RecipientSHA256: hash([]byte(id.Recipient().String())), OutputDirectory: filepath.Join(parent, "capture"), InstanceID: "isolated-operator-real-bao"}
	if err := initialize.Run(context.Background(), initCfg, true, true); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(initCfg.OutputDirectory, "initialization.age")
	cfg := Config{Endpoint: endpoint, CAFile: initCfg.CAFile, RecipientSHA256: initCfg.RecipientSHA256, InstanceID: initCfg.InstanceID}
	// A transparent loopback relay emulates an SSH tunnel; it cannot terminate
	// TLS and never sees decrypted key material. It targets only this fixture.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture tunnel failed")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				up, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{})
				go func() {
					io.Copy(up, conn)
					if c, ok := up.(*net.TCPConn); ok {
						c.CloseWrite()
					}
					close(done)
				}()
				io.Copy(conn, up)
				conn.Close()
				<-done
			}()
		}
	}()
	cfg.ConnectAddress = listener.Addr().String()
	if _, err := Run(context.Background(), cfg, dir, bundle, []byte("wrong-fixture-password")); err != ErrBinding {
		t.Fatal("wrong custody password accepted")
	}
	changed, err := Run(context.Background(), cfg, dir, bundle, pass)
	if err != nil || !changed {
		t.Fatal("real tunnel unseal failed", err)
	}
	cfg.ConnectAddress = ""
	changed, err = Run(context.Background(), cfg, dir, bundle, pass)
	if err != nil || changed {
		t.Fatal("already-unsealed actual instance repeated write")
	}
	b, _ := os.ReadFile(bundle)
	plain, err := custody.Decrypt(b, id)
	if err != nil {
		t.Fatal("test envelope failed")
	}
	defer clear(plain)
	var envelope initialize.Envelope
	var m struct {
		Root string `json:"root_token"`
	}
	if json.Unmarshal(plain, &envelope) != nil || json.Unmarshal(envelope.Response, &m) != nil {
		t.Fatal("test material failed")
	}
	defer clear(envelope.Response)
	c, _, err := client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	call := func(path, method string, token string) int {
		req, err := http.NewRequest(method, endpoint+"/v1/"+path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal("test request failed")
		}
		req.Header.Set("X-Vault-Token", token)
		req.Header.Set("Content-Type", "application/json")
		r, err := c.Do(req)
		if err != nil {
			t.Fatal("test transport failed")
		}
		r.Body.Close()
		return r.StatusCode
	}
	healthy := false
	for range 40 {
		if call("sys/health", "GET", "") == 200 {
			healthy = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("actual unsealed instance did not become ready")
	}
	if call("sys/seal", "POST", m.Root) != 204 {
		t.Fatal("fixture reseal failed")
	}
	changed, err = Run(context.Background(), cfg, dir, bundle, pass)
	if err != nil || !changed {
		t.Fatal("second actual unseal failed", err)
	}
	healthy = false
	for range 40 {
		if call("sys/health", "GET", "") == 200 {
			healthy = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("resealed instance not ready")
	}
	if restoreEndpoint := os.Getenv("PINVOU_TEST_OPENBAO_RESTORE_ENDPOINT"); restoreEndpoint != "" {
		snapshotRestoreProof(t, cfg, m.Root, id, b, bundle, restoreEndpoint)
	} else {
		if call("auth/token/revoke-self", "POST", m.Root) != 204 || call("auth/token/lookup-self", "GET", m.Root) != 403 {
			t.Fatal("fixture root revocation failed")
		}
	}
	t.Log("ACTUAL_OPENBAO_OPERATOR_UNSEAL_PASS")
}
