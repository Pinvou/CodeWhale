package token

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: only an isolated, disposable OpenBao fixture initialized by
// the test operator. This creates/rotates fixture keys; never use production.
func TestTransitProviderOpenBaoIntegration(t *testing.T) {
	if os.Getenv("PINVOU_TEST_OPENBAO_ISOLATED") != "1" {
		t.Skip("requires isolated OpenBao fixture")
	}
	endpoint := os.Getenv("PINVOU_TEST_OPENBAO_URL")
	if endpoint != "http://127.0.0.1:8200" {
		t.Fatal("fixture must use its own loopback network namespace")
	}
	root := os.Getenv("PINVOU_TEST_OPENBAO_ROOT_TOKEN")
	if len(root) < 16 {
		t.Fatal("fixture token required")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(path string, body any) map[string]any {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, endpoint+"/v1/"+path, bytes.NewReader(data))
		req.Header.Set("X-Vault-Token", root)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("fixture request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("fixture status %d for %s", resp.StatusCode, path)
		}
		var result map[string]any
		if resp.StatusCode != 204 {
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatal("fixture response invalid")
			}
		}
		return result
	}
	post("sys/mounts/transit", map[string]string{"type": "transit"})
	post("transit/keys/fixture-key", map[string]any{"type": "aes256-gcm96", "exportable": false, "allow_plaintext_backup": false})
	policy := `path "transit/encrypt/fixture-key" { capabilities = ["update"] }
path "transit/decrypt/fixture-key" { capabilities = ["update"] }`
	post("sys/policies/acl/fixture-runtime", map[string]string{"policy": policy})
	runtimeToken := post("auth/token/create", map[string]any{"policies": []string{"fixture-runtime"}, "no_default_policy": true, "ttl": "5m"})["auth"].(map[string]any)["client_token"].(string)
	p, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "fixture-key", RemoteURL: endpoint, RemoteToken: runtimeToken})
	if err != nil {
		t.Fatal(err)
	}
	plain, aad := []byte("isolated-fixture-secret"), []byte("fixture-purpose")
	one, err := p.Encrypt(plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	two, err := p.Encrypt(plain, aad)
	if err != nil || bytes.Equal(one, two) {
		t.Fatal("encryption must use fresh nonce")
	}
	decoded, err := p.Decrypt(one, aad)
	if err != nil || !bytes.Equal(decoded, plain) {
		t.Fatal("real Transit roundtrip failed")
	}
	if _, err := p.Decrypt(one, []byte("wrong-aad")); err == nil {
		t.Fatal("AAD mismatch accepted")
	}
	if _, err := p.DecryptWithKeyID("other-key", one, aad); err == nil {
		t.Fatal("wrong key/ACL accepted")
	}
	post("transit/keys/fixture-key/rotate", map[string]any{})
	newCipher, err := p.Encrypt(plain, aad)
	if err != nil || !strings.HasPrefix(string(newCipher), "vault:v2:") {
		t.Fatal("new key version not used")
	}
	decoded, err = p.Decrypt(one, aad)
	if err != nil || !bytes.Equal(decoded, plain) {
		t.Fatal("rotation lost historical ciphertext")
	}
	post("auth/token/revoke", map[string]string{"token": runtimeToken})
	if _, err := p.Decrypt(one, aad); err == nil {
		t.Fatal("revoked token accepted")
	}
}
