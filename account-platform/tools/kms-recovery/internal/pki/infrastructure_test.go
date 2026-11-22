package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
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

func infrastructureFixture(t *testing.T) (InfrastructureConfig, *age.X25519Identity) {
	t.Helper()
	c, id := fixture(t)
	return InfrastructureConfig{RecipientFile: c.RecipientFile, RecipientSHA256: c.RecipientSHA256, OutputDirectory: c.OutputDirectory}, id
}

func infrastructureRead(t *testing.T, directory, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal("fixture file unavailable:", name)
	}
	return b
}

func infrastructureCertificate(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("invalid certificate PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal("invalid certificate")
	}
	return c
}

func checkInfrastructureBundle(t *testing.T, cfg InfrastructureConfig, id *age.X25519Identity) (*x509.Certificate, map[string]tls.Certificate) {
	t.Helper()
	if strings.Join(infrastructureServices(), ",") != "postgres,redis,nats,keycloak,newapi,newapi-redis" {
		t.Fatal("production v2 requires exactly six fixed service identities")
	}
	read := func(name string) []byte { return infrastructureRead(t, cfg.OutputDirectory, name) }
	b := read("manifest.json")
	var manifest InfrastructureManifest
	if json.Unmarshal(b, &manifest) != nil || manifest.Version != 2 || manifest.Purpose != "pinvou-platform-internal-pki" || manifest.RecipientSHA256 != cfg.RecipientSHA256 || len(manifest.Files) != 14 {
		t.Fatal("invalid manifest")
	}
	if string(read("READY")) != "INFRASTRUCTURE_PKI_READY_V2\n"+infrastructureHash(b)+"\n" {
		t.Fatal("unbound READY marker")
	}
	expected := map[string]bool{"ca-private.age": true, "ca.crt": true, "manifest.json": true, "READY": true}
	for _, name := range infrastructureServices() {
		expected[name+"/server.crt"] = true
		expected[name+"/server.key"] = true
	}
	for name, hash := range manifest.Files {
		if !expected[name] || name == "manifest.json" || name == "READY" || infrastructureHash(read(name)) != hash {
			t.Fatal("manifest file/hash mismatch")
		}
	}
	count := 0
	if err := filepath.WalkDir(cfg.OutputDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		wantMode := os.FileMode(0600)
		if entry.IsDir() {
			wantMode = 0700
		} else {
			rel, _ := filepath.Rel(cfg.OutputDirectory, path)
			if !info.Mode().IsRegular() || !expected[filepath.ToSlash(rel)] {
				t.Fatal("unexpected output file")
			}
			count++
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != wantMode {
			t.Fatal("unsafe output permissions")
		}
		return nil
	}); err != nil || count != 16 {
		t.Fatal("incomplete output")
	}
	ca := infrastructureCertificate(t, read("ca.crt"))
	if !ca.IsCA || !ca.BasicConstraintsValid || !ca.MaxPathLenZero || ca.MaxPathLen != 0 || ca.CheckSignatureFrom(ca) != nil || ca.NotAfter.After(time.Now().AddDate(5, 0, 1)) {
		t.Fatal("root constraints")
	}
	encrypted := read("ca-private.age")
	plain, err := custody.Decrypt(encrypted, id)
	if err != nil {
		t.Fatal("test CA recovery failed")
	}
	defer clear(plain)
	var envelope RootEnvelope
	if json.Unmarshal(plain, &envelope) != nil || envelope.Version != 1 || envelope.Purpose != "pinvou-platform-internal-ca" || !bytes.Equal(envelope.CAPEM, read("ca.crt")) {
		t.Fatal("CA purpose/binding")
	}
	defer clear(envelope.PrivatePKCS8)
	key, err := x509.ParsePKCS8PrivateKey(envelope.PrivatePKCS8)
	if err != nil || !ca.PublicKey.(*ecdsa.PublicKey).Equal(key.(*ecdsa.PrivateKey).Public()) {
		t.Fatal("recovered CA key mismatch")
	}
	for name := range expected {
		data := read(name)
		if bytes.Contains(data, envelope.PrivatePKCS8) || bytes.Contains(data, []byte(id.String())) {
			t.Fatal("plaintext custody material persisted")
		}
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := custody.Decrypt(encrypted, other); err == nil {
		t.Fatal("wrong recovery identity accepted")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := custody.Decrypt(encrypted, id); err == nil {
		t.Fatal("corrupt recovery accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	keys := map[string]bool{infrastructureHash(ca.RawSubjectPublicKeyInfo): true}
	pairs := map[string]tls.Certificate{}
	for _, name := range infrastructureServices() {
		pair, err := tls.X509KeyPair(read(name+"/server.crt"), read(name+"/server.key"))
		if err != nil {
			t.Fatal("leaf key mismatch")
		}
		leaf := infrastructureCertificate(t, read(name+"/server.crt"))
		if leaf.IsCA || !leaf.BasicConstraintsValid || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != name || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 || len(leaf.EmailAddresses) != 0 || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.KeyUsage != x509.KeyUsageDigitalSignature || leaf.NotAfter.After(time.Now().Add(91*24*time.Hour)) {
			t.Fatal("leaf identity constraints")
		}
		fingerprint := infrastructureHash(leaf.RawSubjectPublicKeyInfo)
		if keys[fingerprint] {
			t.Fatal("shared certificate key")
		}
		keys[fingerprint] = true
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Fatal("server chain failed")
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			t.Fatal("server identity accepted as client")
		}
		pairs[name] = pair
	}
	return ca, pairs
}

func TestInfrastructurePKITwoBundlesAndActualTLSRotation(t *testing.T) {
	oldCfg, oldID := infrastructureFixture(t)
	newCfg, newID := infrastructureFixture(t)
	if GenerateInfrastructure(oldCfg) != nil || GenerateInfrastructure(newCfg) != nil {
		t.Fatal("generation failed")
	}
	oldCA, oldPairs := checkInfrastructureBundle(t, oldCfg, oldID)
	newCA, newPairs := checkInfrastructureBundle(t, newCfg, newID)
	if bytes.Equal(oldCA.RawSubjectPublicKeyInfo, newCA.RawSubjectPublicKeyInfo) {
		t.Fatal("renewal reused root")
	}
	oldTrust := x509.NewCertPool()
	oldTrust.AddCert(oldCA)
	newTrust := x509.NewCertPool()
	newTrust.AddCert(newCA)
	dualTrust := oldTrust.Clone()
	dualTrust.AddCert(newCA)
	probe := func(pair tls.Certificate, trust *x509.CertPool, name string, want bool) {
		t.Helper()
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
		server.StartTLS()
		defer server.Close()
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust, ServerName: name}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}
		if want && (err != nil || response.StatusCode != 204) {
			t.Fatal("verified TLS probe failed")
		}
		if !want && err == nil {
			t.Fatal("untrusted TLS accepted")
		}
	}
	for _, name := range infrastructureServices() {
		if bytes.Equal(oldPairs[name].Certificate[0], newPairs[name].Certificate[0]) {
			t.Fatal("leaf not replaced")
		}
		probe(oldPairs[name], oldTrust, name, true)
		probe(newPairs[name], oldTrust, name, false)
		probe(oldPairs[name], dualTrust, name, true)
		probe(newPairs[name], dualTrust, name, true)
		probe(newPairs[name], newTrust, name, true)
		probe(oldPairs[name], newTrust, name, false)
		probe(newPairs[name], newTrust, "wrong-service", false)
	}
	// Force a server-only leaf into the client-certificate callback. The TLS
	// server must reject its purpose, not just rely on client-side selection.
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{newPairs["nats"]}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: newTrust}
	server.StartTLS()
	defer server.Close()
	for _, name := range infrastructureServices() {
		clientPair := newPairs[name]
		sent := false
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: newTrust, ServerName: "nats", GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { sent = true; return &clientPair, nil }}}
		response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get(server.URL)
		if response != nil {
			response.Body.Close()
		}
		transport.CloseIdleConnections()
		if err == nil || !sent {
			t.Fatal("server-only leaf TLS client rejection not proven")
		}
	}
	t.Log("INFRASTRUCTURE_PKI_TWO_BUNDLES_TLS_PASS")
}

func TestInfrastructurePKIRejectsInvalidAndPartialOutputs(t *testing.T) {
	for _, kind := range []string{"recipient", "hash", "relative", "missing-parent", "existing-empty", "existing-partial", "existing-complete"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := infrastructureFixture(t)
			switch kind {
			case "recipient":
				os.WriteFile(cfg.RecipientFile, []byte("invalid"), 0600)
			case "hash":
				cfg.RecipientSHA256 = strings.Repeat("0", 64)
			case "relative":
				cfg.OutputDirectory = "relative"
			case "missing-parent":
				cfg.OutputDirectory = filepath.Join(cfg.OutputDirectory, "child")
			case "existing-empty", "existing-partial":
				if os.Mkdir(cfg.OutputDirectory, 0700) != nil {
					t.Fatal("fixture")
				}
				if kind == "existing-partial" {
					os.WriteFile(filepath.Join(cfg.OutputDirectory, "ca-private.age"), []byte("preserve-partial"), 0600)
				}
			case "existing-complete":
				if GenerateInfrastructure(cfg) != nil {
					t.Fatal("fixture generation")
				}
			}
			before, beforeErr := os.ReadFile(filepath.Join(cfg.OutputDirectory, "ca-private.age"))
			if GenerateInfrastructure(cfg) != ErrInfrastructurePKI {
				t.Fatal("unsafe generation accepted")
			}
			after, afterErr := os.ReadFile(filepath.Join(cfg.OutputDirectory, "ca-private.age"))
			if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
				t.Fatal("existing partial/root changed")
			}
			if kind != "existing-complete" {
				if _, err := os.Stat(filepath.Join(cfg.OutputDirectory, "READY")); !os.IsNotExist(err) {
					t.Fatal("failed generation became ready")
				}
			}
		})
	}
}

func TestInfrastructurePKIConfigAndInputPermissions(t *testing.T) {
	cfg, _ := infrastructureFixture(t)
	path := filepath.Join(filepath.Dir(cfg.OutputDirectory), "config.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(path, b, 0600)
	loaded, err := LoadInfrastructureConfig(path)
	if err != nil || loaded != cfg {
		t.Fatal("configuration roundtrip")
	}
	for _, invalid := range []string{`{"server_ips":["127.0.0.1"]}`, `{"server_names":["custom"]}`, string(b) + `{}`, `{`} {
		os.WriteFile(path, []byte(invalid), 0600)
		if _, err := LoadInfrastructureConfig(path); err != ErrInfrastructurePKI {
			t.Fatal("unapproved configuration accepted")
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, mode := range []os.FileMode{0750, 0755, 0777} {
		os.Chmod(filepath.Dir(cfg.OutputDirectory), mode)
		if GenerateInfrastructure(cfg) != ErrInfrastructurePKI {
			t.Fatal("non-private output parent accepted")
		}
	}
	os.Chmod(filepath.Dir(cfg.OutputDirectory), 0700)
	os.Chmod(cfg.RecipientFile, 0660)
	if GenerateInfrastructure(cfg) != ErrInfrastructurePKI {
		t.Fatal("replaceable recipient accepted")
	}
	os.Chmod(cfg.RecipientFile, 0600)
	link := cfg.RecipientFile + ".link"
	if os.Symlink(cfg.RecipientFile, link) != nil {
		t.Fatal("fixture symlink")
	}
	cfg.RecipientFile = link
	if GenerateInfrastructure(cfg) != ErrInfrastructurePKI {
		t.Fatal("symlink recipient accepted")
	}
}
