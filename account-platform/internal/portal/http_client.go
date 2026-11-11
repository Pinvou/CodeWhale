package portal

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"golang.org/x/net/idna"
)

var errPortalPrivateCA = errors.New("portal private CA configuration is invalid")

// newPortalHTTPClient extends, rather than replaces, the system roots for this
// Portal client only. It lets server-side OIDC and health calls use private TLS
// without changing trust for browser-facing/public HTTPS integrations.
func newPortalHTTPClient(caFile string) (*http.Client, error) {
	return newPortalHTTPClientPolicy(caFile, portalOrdinaryHTTP)
}

func newPortalAuditHTTPClient(caFile string) (*http.Client, error) {
	return newPortalHTTPClientPolicy(caFile, portalAuditHTTP)
}

func newPortalEmptyCommandHTTPClient(caFile string) (*http.Client, error) {
	return newPortalHTTPClientPolicy(caFile, portalEmptyCommandHTTP)
}

type portalHTTPPolicy int

const (
	portalOrdinaryHTTP portalHTTPPolicy = iota
	portalAuditHTTP
	portalEmptyCommandHTTP
)

func newPortalHTTPClientPolicy(caFile string, policy portalHTTPPolicy) (*http.Client, error) {
	caFile = strings.TrimSpace(caFile)
	if caFile == "" {
		if policy == portalOrdinaryHTTP {
			return observability.NewHTTPClient(15 * time.Second), nil
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		return portalClientForPolicy(transport, policy), nil
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
	return portalClientForPolicy(transport, policy), nil
}

func portalClientForPolicy(transport *http.Transport, policy portalHTTPPolicy) *http.Client {
	if policy == portalEmptyCommandHTTP {
		return portalClientForTransport(portalSingleRequestTransport{transport}, false)
	}
	return portalClientForTransport(transport, policy == portalAuditHTTP)
}

func portalClientForTransport(transport http.RoundTripper, auditDownload bool) *http.Client {
	timeout := 15 * time.Second
	if auditDownload {
		// The caller retains the original finite setup budget through headers.
		// Body progress and stalls are governed by AuditDownloadStream instead.
		timeout = 0
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: observability.NewHTTPTransport(transport),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type portalSingleRequestTransport struct{ transport *http.Transport }

func (t portalSingleRequestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport := t.transport
	if transport.Proxy != nil {
		// NewClientConn's proxy probe omits the port. Use the real request so
		// port-specific proxy exclusions are evaluated as in the ordinary client.
		proxyURL, err := transport.Proxy(request)
		if err != nil {
			return nil, err
		}
		transport = transport.Clone()
		transport.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	}
	host := request.URL.Hostname()
	if strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		if asciiHost, err := idna.Lookup.ToASCII(host); err == nil {
			host = asciiHost
		}
	}
	address := net.JoinHostPort(host, request.URL.Port())
	conn, err := transport.NewClientConn(request.Context(), request.URL.Scheme, address)
	if err != nil {
		return nil, err
	}
	// Use the negotiated connection once, bypassing Transport's retry loop.
	response, err := conn.RoundTrip(request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	response.Body = &portalSingleRequestBody{ReadCloser: response.Body, conn: conn}
	return response, nil
}

type portalSingleRequestBody struct {
	io.ReadCloser
	conn *http.ClientConn
}

func (b *portalSingleRequestBody) Close() error {
	defer b.conn.Close()
	return b.ReadCloser.Close()
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
