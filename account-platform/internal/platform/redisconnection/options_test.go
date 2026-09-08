package redisconnection

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testCertificate(t *testing.T, ca bool, expiry time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: ca, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry}
	der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestOptionsScopedCA(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(filename, testCertificate(t, true, time.Now().Add(time.Hour)), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := Options("rediss://portal:test-only@redis:6379/0", filename)
	if err != nil || o.TLSConfig == nil || o.TLSConfig.MinVersion != tls.VersionTLS12 || o.TLSConfig.ServerName != "redis" || o.TLSConfig.RootCAs == nil || o.TLSConfig.InsecureSkipVerify {
		t.Fatal("scoped TLS not configured")
	}
	for _, raw := range []string{"redis://portal:secret@redis/0", "rediss://portal:secret@redis/0?skip_verify=true", "rediss://portal:secret@/0", "rediss://redis/0#fragment", "rediss://redis/no-secret", "rediss://redis/0?unknown=secret"} {
		if _, err := Options(raw, filename); err != ErrConfiguration {
			t.Fatal("unsafe URL did not fail with redacted error")
		}
	}
	plain, err := Options("redis://localhost:6379/1", "")
	if err != nil || plain.DB != 1 || plain.TLSConfig != nil {
		t.Fatal("development compatibility changed")
	}
	secure, err := Options("rediss://redis:6379/0", "")
	if err != nil || secure.TLSConfig.RootCAs != nil {
		t.Fatal("global trust modified")
	}
}

func TestOptionsCAFileValidation(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "ca.crt")
	valid := testCertificate(t, true, time.Now().Add(time.Hour))
	for _, content := range [][]byte{nil, []byte("not a certificate"), append(append([]byte{}, valid...), []byte("private trailing data")...), testCertificate(t, false, time.Now().Add(time.Hour)), testCertificate(t, true, time.Now().Add(-time.Minute)), make([]byte, (1<<20)+1)} {
		if err := os.WriteFile(filename, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Options("rediss://redis/0", filename); err != ErrConfiguration {
			t.Fatal("invalid CA accepted")
		}
	}
	if err := os.WriteFile(filename, valid, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative.crt", dir, filepath.Join(dir, "missing.crt"), dir + string(os.PathSeparator) + "." + string(os.PathSeparator) + "ca.crt"} {
		if _, err := Options("rediss://redis/0", path); err != ErrConfiguration {
			t.Fatal("unsafe path accepted")
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.crt")
		if err := os.Symlink(filename, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Options("rediss://redis/0", link); err != ErrConfiguration {
			t.Fatal("symlink accepted")
		}
		if err := os.Chmod(filename, 0660); err != nil {
			t.Fatal(err)
		}
		if _, err := Options("rediss://redis/0", filename); err != ErrConfiguration {
			t.Fatal("writable CA accepted")
		}
	}
}
