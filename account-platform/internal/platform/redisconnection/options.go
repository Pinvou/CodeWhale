// Package redisconnection configures per-client Redis trust without changing
// process-wide public roots used by OIDC and external business services.
package redisconnection

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrConfiguration = errors.New("Redis connection configuration is invalid")

func Options(rawURL, caFile string) (*redis.Options, error) {
	rawURL = strings.TrimSpace(rawURL)
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, ErrConfiguration // Parse errors can contain the password-bearing URL.
	}
	if caFile == "" {
		return options, nil // Preserve existing development callers.
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "rediss" || parsed.Hostname() == "" || parsed.Fragment != "" || options.TLSConfig == nil || options.TLSConfig.InsecureSkipVerify {
		return nil, ErrConfiguration
	}
	pool, err := roots(caFile)
	if err != nil {
		return nil, err
	}
	options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsed.Hostname(), RootCAs: pool}
	return options, nil
}

func roots(filename string) (*x509.CertPool, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename || statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrConfiguration
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, ErrConfiguration
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > 1<<20 || (runtime.GOOS != "windows" && opened.Mode().Perm()&0022 != 0) {
		return nil, ErrConfiguration
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, ErrConfiguration
	}
	pool, count := x509.NewCertPool(), 0
	for len(bytes.TrimSpace(b)) > 0 {
		b = bytes.TrimSpace(b)
		if !bytes.HasPrefix(b, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, ErrConfiguration
		}
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, ErrConfiguration
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
			return nil, ErrConfiguration
		}
		pool.AddCert(cert)
		count++
		b = rest
	}
	if count == 0 {
		return nil, ErrConfiguration
	}
	return pool, nil
}
