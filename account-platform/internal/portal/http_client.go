package portal

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

var errPortalPrivateCA = errors.New("portal private CA configuration is invalid")

// newPortalHTTPClient extends, rather than replaces, the system roots for this
// Portal client only. It lets server-side OIDC and health calls use private TLS
// without changing trust for browser-facing/public HTTPS integrations.
func newPortalHTTPClient(caFile string) (*http.Client, error) {
	caFile = strings.TrimSpace(caFile)
	if caFile == "" {
		return observability.NewHTTPClient(15 * time.Second), nil
	}
	body, err := readPortalCAFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	count := 0
	for len(bytes.TrimSpace(body)) > 0 {
		body = bytes.TrimSpace(body)
		if !bytes.HasPrefix(body, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errPortalPrivateCA
		}
		marker := []byte("-----END CERTIFICATE-----")
		end := bytes.Index(body, marker)
		if end < 0 {
			return nil, errPortalPrivateCA
		}
		end += len(marker)
		candidate := body[:end]
		block, rest := pem.Decode(candidate)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 ||
			len(bytes.TrimSpace(rest)) != 0 || bytes.Count(candidate, []byte("-----BEGIN ")) != 1 {
			return nil, errPortalPrivateCA
		}
		certificate, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 ||
			time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
			return nil, errPortalPrivateCA
		}
		pool.AddCert(certificate)
		count++
		body = body[end:]
	}
	if count == 0 {
		return nil, errPortalPrivateCA
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: observability.NewHTTPTransport(transport),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func readPortalCAFile(filename string) ([]byte, error) {
	const maximum = int64(1 << 20)
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	valid := func(candidate os.FileInfo) bool {
		if !candidate.Mode().IsRegular() || candidate.Size() <= 0 || candidate.Size() > maximum {
			return false
		}
		return runtime.GOOS == "windows" || candidate.Mode().Perm()&0022 == 0
	}
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename || statErr != nil || !valid(info) {
		return nil, errPortalPrivateCA
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errPortalPrivateCA
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !valid(opened) {
		return nil, errPortalPrivateCA
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum {
		return nil, errPortalPrivateCA
	}
	return body, nil
}
