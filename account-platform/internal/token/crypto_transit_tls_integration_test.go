package token

import (
	"bytes"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The infrastructure fixture owns this disposable TLS/Raft instance and both
// Agents. This test has no initialization/root material and creates no keys.
func TestTransitProviderOpenBaoTLSIntegration(t *testing.T) {
	if os.Getenv("PINVOU_TEST_OPENBAO_TRANSIT_TLS_ISOLATED") != "1" {
		t.Skip("explicit isolated TLS/Agent fixture required")
	}
	root := os.Getenv("PINVOU_TEST_OPENBAO_FIXTURE_ROOT")
	endpoint := os.Getenv("PINVOU_TEST_OPENBAO_ENDPOINT")
	u, err := url.Parse(endpoint)
	if runtime.GOOS != "linux" || err != nil || u.Scheme != "https" || u.Port() != "8200" || !net.ParseIP(u.Hostname()).IsPrivate() || filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "pinvou-openbao-fixture-") {
		t.Fatal("unsafe TLS fixture target")
	}
	config := SecretProviderConfig{Environment: "production", Mode: "vault-transit", KeyID: "pinvou-token-data", RemoteURL: endpoint, RemoteTokenFile: filepath.Join(root, "gateway-token", "token"), TransitCAFile: filepath.Join(root, "trust", "ca.crt")}
	provider, err := NewSecretProvider(config)
	if err != nil {
		t.Fatal("native TLS provider configuration rejected")
	}
	p := provider.(*TransitProvider)
	defer p.client.CloseIdleConnections()
	plaintext, aad := []byte("isolated-native-tls-value"), []byte("isolated-native-tls-purpose")
	first, err := p.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatal("actual native TLS encryption failed")
	}
	second, err := p.Encrypt(plaintext, aad)
	if err != nil || bytes.Equal(first, second) {
		t.Fatal("native TLS nonce behavior failed")
	}
	result, err := p.Decrypt(first, aad)
	if err != nil || !bytes.Equal(result, plaintext) {
		t.Fatal("actual native TLS roundtrip failed")
	}
	if _, err := p.Decrypt(first, []byte("wrong-purpose")); err == nil {
		t.Fatal("actual native TLS AAD mismatch accepted")
	}
	if _, err := p.DecryptWithKeyID("outside-key", first, aad); err == nil {
		t.Fatal("actual native TLS ACL mismatch accepted")
	}
	// An explicit correct CA still requires the original endpoint certificate name.
	p.client.CloseIdleConnections()
	p.client.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.fixture.invalid"
	if _, err := p.Encrypt(plaintext, aad); err == nil {
		t.Fatal("native TLS hostname verification bypassed")
	}
	t.Log("ACTUAL_OPENBAO_TRANSIT_TLS_PASS")
}
