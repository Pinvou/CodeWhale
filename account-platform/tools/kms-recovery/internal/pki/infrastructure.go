package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

var ErrInfrastructurePKI = errors.New("INFRASTRUCTURE_PKI_FAILED_PRESERVE_PARTIAL_OUTPUT")

type InfrastructureConfig struct {
	RecipientFile   string `json:"recipient_file"`
	RecipientSHA256 string `json:"recipient_sha256"`
	OutputDirectory string `json:"output_directory"`
}

type InfrastructureManifest struct {
	Version         int               `json:"version"`
	Purpose         string            `json:"purpose"`
	RecipientSHA256 string            `json:"recipient_sha256"`
	Files           map[string]string `json:"files"`
}

func infrastructureServices() []string {
	return []string{"postgres", "redis", "nats", "keycloak", "newapi", "newapi-redis"}
}

func LoadInfrastructureConfig(filename string) (InfrastructureConfig, error) {
	var cfg InfrastructureConfig
	b, err := readPublic(filename)
	if err != nil {
		return cfg, ErrInfrastructurePKI
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return InfrastructureConfig{}, ErrInfrastructurePKI
	}
	return cfg, nil
}

// GenerateInfrastructure is offline and uses only a verified public recipient.
// It never opens KMS state, a custody identity, a passphrase or an existing CA.
func GenerateInfrastructure(cfg InfrastructureConfig) error {
	pub, err := readPublic(cfg.RecipientFile)
	if err != nil {
		return ErrInfrastructurePKI
	}
	canonical := strings.TrimSpace(string(pub))
	r, err := age.ParseX25519Recipient(canonical)
	if err != nil || infrastructureHash([]byte(canonical)) != cfg.RecipientSHA256 {
		return ErrInfrastructurePKI
	}
	parent := filepath.Dir(cfg.OutputDirectory)
	resolved, err := filepath.EvalSymlinks(parent)
	info, statErr := os.Stat(parent)
	if !filepath.IsAbs(cfg.OutputDirectory) || filepath.Clean(cfg.OutputDirectory) != cfg.OutputDirectory || err != nil || resolved != parent || statErr != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return ErrInfrastructurePKI
	}
	if os.Mkdir(cfg.OutputDirectory, 0700) != nil || syncDir(parent) != nil {
		return ErrInfrastructurePKI
	}
	manifest := InfrastructureManifest{Version: 2, Purpose: "pinvou-platform-internal-pki", RecipientSHA256: cfg.RecipientSHA256, Files: map[string]string{}}
	persist := func(name string, data []byte) error {
		if writeNew(cfg.OutputDirectory, filepath.FromSlash(name), data) != nil {
			return ErrInfrastructurePKI
		}
		if syncDir(filepath.Dir(filepath.Join(cfg.OutputDirectory, filepath.FromSlash(name)))) != nil {
			return ErrInfrastructurePKI
		}
		manifest.Files[name] = infrastructureHash(data)
		return nil
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ErrInfrastructurePKI
	}
	now := time.Now().UTC()
	caSerial, err := serial()
	if err != nil {
		return ErrInfrastructurePKI
	}
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Pinvou Private Infrastructure CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return ErrInfrastructurePKI
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		return ErrInfrastructurePKI
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return ErrInfrastructurePKI
	}
	defer clear(private)
	plain, err := json.Marshal(RootEnvelope{Version: 1, Purpose: "pinvou-platform-internal-ca", CAPEM: caPEM, PrivatePKCS8: private})
	if err != nil {
		return ErrInfrastructurePKI
	}
	defer clear(plain)
	encrypted, err := custody.Encrypt(plain, r)
	if err != nil || persist("ca-private.age", encrypted) != nil || persist("ca.crt", caPEM) != nil {
		return ErrInfrastructurePKI
	}
	for _, name := range infrastructureServices() {
		directory := filepath.Join(cfg.OutputDirectory, name)
		if os.Mkdir(directory, 0700) != nil || syncDir(cfg.OutputDirectory) != nil {
			return ErrInfrastructurePKI
		}
		certificate, key, err := infrastructureLeaf(name, now, ca, caKey)
		if err != nil {
			return err
		}
		err = persist(name+"/server.crt", certificate)
		if err == nil {
			err = persist(name+"/server.key", key)
		}
		clear(key)
		if err != nil {
			return err
		}
	}
	b, err := json.Marshal(manifest)
	if err != nil || len(manifest.Files) != 14 || writeNew(cfg.OutputDirectory, "manifest.json", b) != nil {
		return ErrInfrastructurePKI
	}
	// The readiness marker binds the exact manifest; deployment must verify all
	// hashes and the closed file set, not merely test whether this marker exists.
	if writeNew(cfg.OutputDirectory, "READY", []byte("INFRASTRUCTURE_PKI_READY_V2\n"+infrastructureHash(b)+"\n")) != nil {
		return ErrInfrastructurePKI
	}
	return nil
}

func infrastructureHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func infrastructureLeaf(name string, now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, ErrInfrastructurePKI
	}
	number, err := serial()
	if err != nil {
		return nil, nil, ErrInfrastructurePKI
	}
	leaf := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), BasicConstraintsValid: true, DNSNames: []string{name}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, ErrInfrastructurePKI
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, ErrInfrastructurePKI
	}
	defer clear(private)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		clear(keyPEM)
		return nil, nil, ErrInfrastructurePKI
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		clear(keyPEM)
		return nil, nil, ErrInfrastructurePKI
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: now}); err != nil {
		clear(keyPEM)
		return nil, nil, ErrInfrastructurePKI
	}
	return certPEM, keyPEM, nil
}
