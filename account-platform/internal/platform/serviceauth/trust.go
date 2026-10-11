package serviceauth

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var errWorkloadTrust = errors.New("workload identity trust configuration is invalid")

// Only this receiver's JWKS client uses these roots. Public user OIDC, Mall,
// and other HTTP clients retain their own trust and transport configuration.
func workloadHTTPClient(caFile string) (*http.Client, error) {
	var roots *x509.CertPool
	if caFile != "" {
		var err error
		roots, err = workloadRoots(caFile)
		if err != nil {
			return nil, err
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func canonicalWorkloadHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && u.Opaque == ""
}

func workloadRoots(filename string) (*x509.CertPool, error) {
	const maximum = 1 << 20
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	valid := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= maximum &&
			(runtime.GOOS == "windows" || info.Mode().Perm()&0022 == 0)
	}
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename ||
		statErr != nil || !valid(info) {
		return nil, errWorkloadTrust
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, errWorkloadTrust
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !valid(opened) {
		return nil, errWorkloadTrust
	}
	body, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || len(body) == 0 || len(body) > maximum {
		return nil, errWorkloadTrust
	}
	roots, count := x509.NewCertPool(), 0
	now := time.Now()
	for len(bytes.TrimSpace(body)) > 0 {
		body = bytes.TrimSpace(body)
		if !bytes.HasPrefix(body, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errWorkloadTrust
		}
		// Decode only the first bounded candidate: pem.Decode on the whole
		// bundle can silently skip a malformed block before a valid one.
		endMarker := []byte("-----END CERTIFICATE-----")
		end := bytes.Index(body, endMarker)
		if end < 0 {
			return nil, errWorkloadTrust
		}
		end += len(endMarker)
		candidate := body[:end]
		block, rest := pem.Decode(candidate)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 ||
			len(bytes.TrimSpace(rest)) != 0 || bytes.Count(candidate, []byte("-----BEGIN ")) != 1 {
			return nil, errWorkloadTrust
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 ||
			now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return nil, errWorkloadTrust
		}
		roots.AddCert(cert)
		count++
		body = body[end:]
	}
	if count == 0 {
		return nil, errWorkloadTrust
	}
	return roots, nil
}
