package initialize

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
)

// The owning Docker fixture supplies only a new private-network endpoint and
// test CA path. No bootstrap secret is provided through arguments/environment.
func TestInitializerOpenBaoIntegration(t *testing.T) {
	if os.Getenv("PINVOU_TEST_OPENBAO_INITIALIZER_ISOLATED") != "1" {
		t.Skip("explicit isolated OpenBao fixture required")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("actual initializer fixture requires Linux")
	}
	endpoint := os.Getenv("PINVOU_TEST_OPENBAO_ENDPOINT")
	root := os.Getenv("PINVOU_TEST_OPENBAO_FIXTURE_ROOT")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Port() != "8200" || !net.ParseIP(u.Hostname()).IsPrivate() || filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "pinvou-openbao-fixture-") {
		t.Fatal("unsafe fixture endpoint or root")
	}
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private output unavailable")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("fixture identity failed")
	}
	pub := id.Recipient().String()
	cfg := Config{Endpoint: endpoint, CAFile: filepath.Join(root, "trust", "ca.crt"), RecipientFile: filepath.Join(parent, "recipient.txt"), RecipientSHA256: sum([]byte(pub)), OutputDirectory: filepath.Join(parent, "capture"), InstanceID: "isolated-openbao-initializer"}
	if os.WriteFile(cfg.RecipientFile, []byte(pub), 0600) != nil {
		t.Fatal("fixture public recipient failed")
	}
	if err := Run(context.Background(), cfg, true, true); err != nil {
		t.Fatal(err)
	}
	e := openEnvelope(t, cfg, id)
	defer clear(e.Response)
	if !e.Complete || e.HTTPStatus != 200 || e.InstanceID != cfg.InstanceID || e.Endpoint != cfg.Endpoint {
		t.Fatal("actual response binding failed")
	}
	var m struct {
		Keys []string `json:"keys_base64"`
		Root string   `json:"root_token"`
	}
	if json.Unmarshal(e.Response, &m) != nil || len(m.Keys) != 1 {
		t.Fatal("actual encrypted material missing")
	}
	c, _, _, err := clientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	call := func(path string, body []byte, token string) (int, []byte) {
		method := "GET"
		if body != nil {
			method = "POST"
		}
		r, err := http.NewRequest(method, endpoint+"/v1/"+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal("fixture request failed")
		}
		r.Header.Set("X-Vault-Token", token)
		r.Header.Set("Content-Type", "application/json")
		response, err := c.Do(r)
		if err != nil {
			t.Fatal("fixture transport failed")
		}
		defer response.Body.Close()
		b, err := io.ReadAll(io.LimitReader(response.Body, MaxResponse+1))
		if err != nil || len(b) > MaxResponse {
			t.Fatal("fixture response failed")
		}
		return response.StatusCode, b
	}
	status, b := call("sys/seal-status", nil, "")
	var seal struct {
		Initialized bool `json:"initialized"`
		Sealed      bool `json:"sealed"`
	}
	if status != 200 || json.Unmarshal(b, &seal) != nil || !seal.Initialized || !seal.Sealed {
		t.Fatal("initializer unexpectedly unsealed or failed to initialize")
	}
	keyBody, _ := json.Marshal(map[string]string{"key": m.Keys[0]})
	status, _ = call("sys/unseal", keyBody, "")
	clear(keyBody)
	if status != 200 {
		t.Fatal("captured key did not unseal actual server")
	}
	healthy := false
	for range 40 {
		status, _ = call("sys/health", nil, "")
		if status == 200 {
			healthy = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("actual instance did not become ready")
	}
	status, _ = call("auth/token/lookup-self", nil, m.Root)
	if status != 200 {
		t.Fatal("captured root failed actual authentication")
	}
	for _, name := range []string{"initialization.age", "INIT_ATTEMPTED", "INIT_CAPTURED"} {
		b, err := os.ReadFile(filepath.Join(cfg.OutputDirectory, name))
		if err != nil || bytes.Contains(b, []byte(m.Root)) || bytes.Contains(b, []byte(m.Keys[0])) {
			t.Fatal("actual material persisted in plaintext")
		}
	}
	if Run(context.Background(), cfg, true, true) != ErrOutput {
		t.Fatal("existing encrypted capture overwritten")
	}
	cfg.OutputDirectory = filepath.Join(parent, "second-capture")
	if Run(context.Background(), cfg, true, true) != ErrPreflight {
		t.Fatal("initialized instance accepted again")
	}
	status, _ = call("auth/token/revoke-self", []byte(`{}`), m.Root)
	if status != 204 && status != 200 {
		t.Fatal("fixture root revocation failed")
	}
	status, _ = call("auth/token/lookup-self", nil, m.Root)
	if status != 403 {
		t.Fatal("revoked fixture root still accepted")
	}
	t.Log("ACTUAL_OPENBAO_ENCRYPTED_INITIALIZATION_PASS")
}
