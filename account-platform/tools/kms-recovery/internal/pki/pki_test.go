package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

func fixture(t *testing.T) (Config, *age.X25519Identity) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.Recipient().String()
	h := sha256.Sum256([]byte(pub))
	c := Config{RecipientFile: filepath.Join(dir, "recipient.txt"), RecipientSHA256: hex.EncodeToString(h[:]), OutputDirectory: filepath.Join(dir, "pki"), ServerIPs: []string{"192.168.50.2"}}
	if os.WriteFile(c.RecipientFile, []byte(pub), 0600) != nil {
		t.Fatal("fixture setup failed")
	}
	return c, id
}

func TestPKIEncryptedRootAndVerifiedServer(t *testing.T) {
	c, id := fixture(t)
	if err := Generate(c); err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(c.OutputDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	encrypted := read("ca-private.age")
	plain, err := custody.Decrypt(encrypted, id)
	if err != nil {
		t.Fatal("CA recovery failed")
	}
	defer clear(plain)
	var envelope RootEnvelope
	if json.Unmarshal(plain, &envelope) != nil || envelope.Version != 1 || envelope.Purpose != "pinvou-kms-ca" {
		t.Fatal("CA envelope invalid")
	}
	defer clear(envelope.PrivatePKCS8)
	rootKey, err := x509.ParsePKCS8PrivateKey(envelope.PrivatePKCS8)
	if err != nil {
		t.Fatal("CA private key cannot be recovered")
	}
	block, _ := pem.Decode(read("ca.crt"))
	if block == nil {
		t.Fatal("CA PEM invalid")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.MaxPathLenZero || ca.MaxPathLen != 0 || !ca.PublicKey.(*ecdsa.PublicKey).Equal(rootKey.(*ecdsa.PrivateKey).Public()) {
		t.Fatal("CA constraints or recovered key mismatch")
	}
	if !bytes.Equal(envelope.CAPEM, read("ca.crt")) {
		t.Fatal("CA recovery binding mismatch")
	}
	pair, err := tls.X509KeyPair(read("server.crt"), read("server.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.NotAfter.After(time.Now().Add(91*24*time.Hour)) || leaf.VerifyHostname("openbao") != nil || leaf.VerifyHostname(c.ServerIPs[0]) != nil || leaf.PublicKey.(*ecdsa.PublicKey).Equal(ca.PublicKey) {
		t.Fatal("leaf identity or key separation failed")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "openbao"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca-private.age", "ca.crt", "server.crt", "server.key", "PKI_READY"} {
		b := read(name)
		if bytes.Contains(b, envelope.PrivatePKCS8) || bytes.Contains(b, []byte(id.String())) {
			t.Fatal("root private material persisted in plaintext")
		}
		if runtime.GOOS != "windows" {
			s, _ := os.Stat(filepath.Join(c.OutputDirectory, name))
			if s.Mode().Perm() != 0600 {
				t.Fatal("public output permissions")
			}
		}
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	defer s.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "openbao", MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get(s.URL)
	if err != nil {
		t.Fatal("generated certificate TLS failed")
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("TLS probe failed")
	}
	before := read("ca-private.age")
	if Generate(c) != ErrPKI || !bytes.Equal(before, read("ca-private.age")) {
		t.Fatal("existing CA overwritten")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := custody.Decrypt(encrypted, other); err == nil {
		t.Fatal("wrong recovery identity accepted")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := custody.Decrypt(encrypted, id); err == nil {
		t.Fatal("corrupt CA recovery accepted")
	}
}

func TestPKIRejectsUntrustedInputs(t *testing.T) {
	for _, kind := range []string{"bad-recipient", "bad-hash", "no-ip", "public-ip", "loopback", "ipv6", "duplicate", "relative-output", "missing-parent"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := fixture(t)
			switch kind {
			case "bad-recipient":
				os.WriteFile(c.RecipientFile, []byte("invalid"), 0600)
			case "bad-hash":
				c.RecipientSHA256 = strings.Repeat("0", 64)
			case "no-ip":
				c.ServerIPs = nil
			case "public-ip":
				c.ServerIPs = []string{"192.0.2.1"}
			case "loopback":
				c.ServerIPs = []string{"127.0.0.1"}
			case "ipv6":
				c.ServerIPs = []string{"fd00::1"}
			case "duplicate":
				c.ServerIPs = append(c.ServerIPs, c.ServerIPs[0])
			case "relative-output":
				c.OutputDirectory = "relative"
			case "missing-parent":
				c.OutputDirectory = filepath.Join(c.OutputDirectory, "missing", "child")
			}
			if Generate(c) != ErrPKI {
				t.Fatal("invalid PKI configuration accepted")
			}
		})
	}
}

func TestPKIConfigParsingAndPermissions(t *testing.T) {
	c, _ := fixture(t)
	p := filepath.Join(filepath.Dir(c.OutputDirectory), "config.json")
	b, _ := json.Marshal(c)
	os.WriteFile(p, b, 0600)
	loaded, err := LoadConfig(p)
	if err != nil || loaded.OutputDirectory != c.OutputDirectory {
		t.Fatal("config roundtrip failed")
	}
	for _, bad := range []string{`{"unknown":true}`, string(b) + `{}`, `{`} {
		os.WriteFile(p, []byte(bad), 0600)
		if _, err := LoadConfig(p); err != ErrPKI {
			t.Fatal("invalid config accepted")
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.Chmod(filepath.Dir(c.OutputDirectory), 0755)
	if Generate(c) != ErrPKI {
		t.Fatal("public parent accepted")
	}
	os.Chmod(filepath.Dir(c.OutputDirectory), 0700)
	os.Chmod(c.RecipientFile, 0666)
	if Generate(c) != ErrPKI {
		t.Fatal("replaceable recipient accepted")
	}
}
