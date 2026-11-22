package transit

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ErrCA indicates invalid or insecure dedicated Transit trust.
var ErrCA = errors.New("transit CA file is unavailable, insecure or invalid")

// This transport does not inherit proxies or mutate process-wide trust. A
// configured bundle replaces trust for this KMS client only; omission preserves
// Go's system roots. CA changes require a controlled service restart.
// NewTransport creates an isolated Transit-only HTTP transport.
func NewTransport(caFile string) (*http.Transport, error) {
	var roots *x509.CertPool
	if caFile != "" {
		var err error
		roots, err = ReadCA(caFile)
		if err != nil {
			return nil, err
		}
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 32 << 10, DisableCompression: true,
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
	}, nil
}

// ReadCA reads and validates a dedicated Transit CA bundle.
func ReadCA(path string) (*x509.CertPool, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrCA
	}
	// Public Kubernetes projected CA files can be symlinks. Resolve once, then
	// compare the opened file identity; the deployment must mount trust read-only.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, ErrCA
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrCA
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, ErrCA
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0022 != 0) {
		return nil, ErrCA
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, ErrCA
	}
	roots := x509.NewCertPool()
	count := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, ErrCA
		}
		// Bound the first candidate so pem.Decode cannot skip a damaged block.
		marker := []byte("-----END CERTIFICATE-----")
		end := bytes.Index(rest, marker)
		if end < 0 {
			return nil, ErrCA
		}
		end += len(marker)
		candidate := rest[:end]
		block, next := pem.Decode(candidate)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 ||
			len(bytes.TrimSpace(next)) != 0 || bytes.Count(candidate, []byte("-----BEGIN ")) != 1 {
			return nil, ErrCA
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, ErrCA
		}
		roots.AddCert(certificate)
		count++
		rest = bytes.TrimSpace(rest[end:])
	}
	if count == 0 {
		return nil, ErrCA
	}
	return roots, nil
}
