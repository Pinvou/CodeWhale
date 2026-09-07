// Package operator implements bounded custodian operations, not an API proxy.
package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/initialize"
)

var (
	ErrConfig    = errors.New("OPERATOR_CONFIGURATION_REJECTED")
	ErrBinding   = errors.New("OPERATOR_RECOVERY_PACKAGE_OR_BINDING_REJECTED")
	ErrPreflight = errors.New("OPERATOR_PREFLIGHT_FAILED_NO_KEY_SENT")
	ErrUncertain = errors.New("OPERATOR_UNSEAL_RESULT_UNCERTAIN_CHECK_STATE_BEFORE_RETRY")
)

type Config struct {
	Endpoint        string `json:"endpoint"`
	CAFile          string `json:"ca_file"`
	RecipientSHA256 string `json:"recipient_sha256"`
	InstanceID      string `json:"instance_id"`
	ConnectAddress  string `json:"connect_address,omitempty"`
}

func readLocal(path string, secret bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfig
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil || p != path {
		return nil, ErrConfig
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrConfig
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm()&0022 != 0 || (secret && info.Mode().Perm()&0077 != 0)) {
		return nil, ErrConfig
	}
	b, err := custody.ReadBounded(path)
	if err != nil {
		return nil, ErrConfig
	}
	return b, nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := readLocal(path, false)
	if err != nil || len(b) > 16<<10 {
		return c, ErrConfig
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return Config{}, ErrConfig
	}
	return c, nil
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func client(cfg Config) (*http.Client, string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != cfg.Endpoint {
		return nil, "", ErrConfig
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{15,63}$`).MatchString(cfg.InstanceID) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(cfg.RecipientSHA256) {
		return nil, "", ErrConfig
	}
	ca, err := readLocal(cfg.CAFile, false)
	if err != nil || len(ca) > 64<<10 {
		return nil, "", ErrConfig
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, "", ErrConfig
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}, DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableKeepAlives: true, DisableCompression: true}
	if cfg.ConnectAddress != "" {
		host, port, err := net.SplitHostPort(cfg.ConnectAddress)
		p, e := strconv.Atoi(port)
		if err != nil || e != nil || p < 1 || p > 65535 || !net.ParseIP(host).IsLoopback() {
			return nil, "", ErrConfig
		}
		// SSH owns the tunnel. This changes only the TCP destination, never the
		// URL Host or TLS identity. No proxy or external command is spawned here.
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", cfg.ConnectAddress)
		}
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, hash(ca), nil
}

func material(cfg Config, caHash string, encrypted []byte, identity *age.X25519Identity) (string, error) {
	if hash([]byte(identity.Recipient().String())) != cfg.RecipientSHA256 {
		return "", ErrBinding
	}
	plain, err := custody.Decrypt(encrypted, identity)
	if err != nil {
		return "", ErrBinding
	}
	defer clear(plain)
	var envelope initialize.Envelope
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if d.Decode(&envelope) != nil || d.Decode(new(any)) != io.EOF {
		return "", ErrBinding
	}
	defer clear(envelope.Response)
	at, err := time.Parse(time.RFC3339, envelope.CapturedAt)
	if err != nil || at.After(time.Now().Add(5*time.Minute)) || envelope.Version != 1 || !envelope.Complete || envelope.HTTPStatus != 200 || envelope.InstanceID != cfg.InstanceID || envelope.Endpoint != cfg.Endpoint || envelope.CASHA256 != caHash || envelope.RecipientSHA256 != cfg.RecipientSHA256 {
		return "", ErrBinding
	}
	var m struct {
		Keys []string `json:"keys_base64"`
	}
	if json.Unmarshal(envelope.Response, &m) != nil || len(m.Keys) != 1 {
		return "", ErrBinding
	}
	key, err := base64.StdEncoding.Strict().DecodeString(m.Keys[0])
	defer clear(key)
	if err != nil || len(key) < 32 || len(key) > 128 {
		return "", ErrBinding
	}
	// Never parse or send the root token: ordinary unseal does not need it.
	return m.Keys[0], nil
}

func request(ctx context.Context, c *http.Client, cfg Config, path string, body []byte) (int, []byte, error) {
	method := "GET"
	var r io.Reader
	if body != nil {
		method = "POST"
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Endpoint+"/v1/"+path, r)
	if err != nil {
		return 0, nil, ErrPreflight
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	response, err := c.Do(req)
	if err != nil {
		return 0, nil, ErrPreflight
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, initialize.MaxResponse+1))
	if err != nil || len(b) > initialize.MaxResponse {
		clear(b)
		return 0, nil, ErrPreflight
	}
	return response.StatusCode, b, nil
}

type sealState struct {
	Initialized *bool  `json:"initialized"`
	Sealed      *bool  `json:"sealed"`
	Type        string `json:"type"`
	N           int    `json:"n"`
	T           int    `json:"t"`
}

func state(ctx context.Context, c *http.Client, cfg Config) (bool, error) {
	status, b, err := request(ctx, c, cfg, "sys/seal-status", nil)
	defer clear(b)
	var s sealState
	if err != nil || status != 200 || json.Unmarshal(b, &s) != nil || s.Initialized == nil || !*s.Initialized || s.Sealed == nil || s.Type != "shamir" || s.N != 1 || s.T != 1 {
		return false, ErrPreflight
	}
	return *s.Sealed, nil
}

// Unseal verifies the entire authenticated envelope before any network request.
// A false result means the instance was already unsealed; no key was sent.
func Unseal(ctx context.Context, cfg Config, encrypted []byte, identity *age.X25519Identity) (bool, error) {
	if identity == nil {
		return false, ErrBinding
	}
	c, caHash, err := client(cfg)
	if err != nil {
		return false, err
	}
	defer c.CloseIdleConnections()
	key, err := material(cfg, caHash, encrypted, identity)
	if err != nil {
		return false, err
	}
	sealed, err := state(ctx, c, cfg)
	if err != nil {
		return false, ErrPreflight
	}
	if !sealed {
		return false, nil
	}
	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return false, ErrBinding
	}
	defer clear(body)
	status, response, err := request(ctx, c, cfg, "sys/unseal", body)
	clear(response)
	if err != nil || status != 200 {
		return false, ErrUncertain
	}
	sealed, err = state(ctx, c, cfg)
	if err != nil || sealed {
		return false, ErrUncertain
	}
	return true, nil
}

// Run reads only encrypted/private local artifacts. The CLI supplies a password
// from a local no-echo terminal; neither passwords nor key material are logged.
func Run(ctx context.Context, cfg Config, dir, bundle string, passphrase []byte) (bool, error) {
	for _, name := range []string{"identity.age", "recipient.txt", "CUSTODY_READY"} {
		if _, err := readLocal(filepath.Join(dir, name), true); err != nil {
			return false, err
		}
	}
	b, err := readLocal(bundle, true)
	if err != nil {
		return false, err
	}
	id, err := custody.Unlock(dir, passphrase)
	if err != nil {
		return false, ErrBinding
	}
	return Unseal(ctx, cfg, b, id)
}
