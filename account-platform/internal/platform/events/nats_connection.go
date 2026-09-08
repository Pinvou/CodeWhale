package events

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func connectNATS(rawURL, name string, cfg NATSConfig) (*nats.Conn, jetstream.JetStream, error) {
	provisioned, err := cfg.provisioned()
	if err != nil {
		return nil, nil, err
	}
	options := []nats.Option{nats.Name(name), nats.Timeout(5 * time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second)}
	parsed, parseErr := url.Parse(rawURL)
	if provisioned || cfg.CAFile != "" {
		if parseErr != nil || parsed.Scheme != "tls" || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(rawURL, ",") {
			return nil, nil, ErrNATSResources
		}
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pool, err := natsRoots(cfg.CAFile)
			if err != nil {
				return nil, nil, err
			}
			tlsConfig.RootCAs = pool
		}
		options = append(options, nats.Secure(tlsConfig))
	}
	if provisioned {
		if name == "" || strings.ContainsAny(name, ". *>\t\r\n/\\") {
			return nil, nil, ErrNATSResources
		}
		options = append(options, nats.CustomInboxPrefix("_INBOX."+name))
	}
	if file := strings.TrimSpace(os.Getenv("NATS_CREDENTIALS_FILE")); file != "" {
		options = append(options, nats.UserCredentials(file))
	}
	connection, err := nats.Connect(rawURL, options...)
	if err != nil {
		return nil, nil, ErrNATSResources
	}
	js, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		return nil, nil, ErrNATSResources
	}
	return connection, js, nil
}

func natsRoots(filename string) (*x509.CertPool, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename || statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrNATSResources
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, ErrNATSResources
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrNATSResources
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, ErrNATSResources
	}
	pool := x509.NewCertPool()
	count := 0
	for len(bytes.TrimSpace(b)) > 0 {
		b = bytes.TrimSpace(b)
		if !bytes.HasPrefix(b, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, ErrNATSResources
		}
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, ErrNATSResources
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
			return nil, ErrNATSResources
		}
		pool.AddCert(cert)
		count++
		b = rest
	}
	if count == 0 {
		return nil, ErrNATSResources
	}
	return pool, nil
}
