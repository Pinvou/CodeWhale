// Package pki creates an offline, dedicated KMS CA and first server certificate.
// CA private material is never written unencrypted. It does not contact servers.
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
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

var ErrPKI = errors.New("KMS_PKI_OPERATION_FAILED_OUTPUT_MUST_NOT_BE_OVERWRITTEN")

type Config struct {
	RecipientFile   string   `json:"recipient_file"`
	RecipientSHA256 string   `json:"recipient_sha256"`
	OutputDirectory string   `json:"output_directory"`
	ServerIPs       []string `json:"server_ips"`
}
type RootEnvelope struct {
	Version      int    `json:"version"`
	Purpose      string `json:"purpose"`
	CAPEM        []byte `json:"ca_pem"`
	PrivatePKCS8 []byte `json:"private_pkcs8"`
}

func readPublic(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrPKI
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, ErrPKI
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrPKI
	}
	b, err := custody.ReadBounded(path)
	if err != nil || len(b) > 16<<10 {
		return nil, ErrPKI
	}
	return b, nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := readPublic(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return Config{}, ErrPKI
	}
	return c, nil
}
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrPKI
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrPKI
	}
	return nil
}
func writeNew(dir, name string, b []byte) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrPKI
	}
	n, e := f.Write(b)
	s := f.Sync()
	c := f.Close()
	if e != nil || n != len(b) || s != nil || c != nil || syncDir(dir) != nil {
		return ErrPKI
	}
	read, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(b, read) {
		clear(read)
		return ErrPKI
	}
	clear(read)
	return nil
}
func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
	if err != nil {
		return nil, ErrPKI
	}
	return n.Add(n, big.NewInt(1)), nil
}

func Generate(cfg Config) error {
	pub, err := readPublic(cfg.RecipientFile)
	if err != nil {
		return err
	}
	canonical := strings.TrimSpace(string(pub))
	sum := sha256.Sum256([]byte(canonical))
	r, err := age.ParseX25519Recipient(canonical)
	if err != nil || hex.EncodeToString(sum[:]) != cfg.RecipientSHA256 {
		return ErrPKI
	}
	if len(cfg.ServerIPs) < 1 || len(cfg.ServerIPs) > 4 {
		return ErrPKI
	}
	ips := make([]net.IP, 0, len(cfg.ServerIPs))
	seen := map[string]bool{}
	for _, text := range cfg.ServerIPs {
		ip, err := netip.ParseAddr(text)
		if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != text || seen[text] {
			return ErrPKI
		}
		seen[text] = true
		ips = append(ips, net.ParseIP(text))
	}
	parent := filepath.Dir(cfg.OutputDirectory)
	resolved, err := filepath.EvalSymlinks(parent)
	if !filepath.IsAbs(cfg.OutputDirectory) || filepath.Clean(cfg.OutputDirectory) != cfg.OutputDirectory || err != nil || resolved != parent {
		return ErrPKI
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return ErrPKI
	}
	if os.Mkdir(cfg.OutputDirectory, 0700) != nil || syncDir(parent) != nil {
		return ErrPKI
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ErrPKI
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ErrPKI
	}
	now := time.Now().UTC()
	caSerial, err := serial()
	if err != nil {
		return ErrPKI
	}
	leafSerial, err := serial()
	if err != nil {
		return ErrPKI
	}
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Pinvou Private KMS CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return ErrPKI
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		return ErrPKI
	}
	leaf := &x509.Certificate{SerialNumber: leafSerial, Subject: pkix.Name{CommonName: "openbao"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), BasicConstraintsValid: true, DNSNames: []string{"openbao"}, IPAddresses: ips, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return ErrPKI
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	rootPrivate, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return ErrPKI
	}
	defer clear(rootPrivate)
	rootPlain, err := json.Marshal(RootEnvelope{Version: 1, Purpose: "pinvou-kms-ca", CAPEM: caPEM, PrivatePKCS8: rootPrivate})
	if err != nil {
		return ErrPKI
	}
	defer clear(rootPlain)
	rootEncrypted, err := custody.Encrypt(rootPlain, r)
	if err != nil {
		return ErrPKI
	}
	leafPrivate, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return ErrPKI
	}
	defer clear(leafPrivate)
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafPrivate})
	defer clear(leafKeyPEM)
	if _, err := tls.X509KeyPair(leafPEM, leafKeyPEM); err != nil {
		return ErrPKI
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return ErrPKI
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "openbao", CurrentTime: now}); err != nil {
		return ErrPKI
	}
	// The encrypted CA is persisted before the only cleartext private output,
	// which is the separate server leaf key. Partial outputs remain for review.
	for _, file := range []struct {
		name string
		data []byte
	}{{"ca-private.age", rootEncrypted}, {"ca.crt", caPEM}, {"server.crt", leafPEM}, {"server.key", leafKeyPEM}} {
		if writeNew(cfg.OutputDirectory, file.name, file.data) != nil {
			return ErrPKI
		}
	}
	return writeNew(cfg.OutputDirectory, "PKI_READY", []byte("Encrypted CA and server certificate created. Offline custody and certificate rotation remain required.\n"))
}
