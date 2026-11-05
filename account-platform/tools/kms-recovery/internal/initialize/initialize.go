// Package initialize captures a single OpenBao initialization response directly
// into an age envelope. It never unseals, resets, or retries initialization.
package initialize

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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
)

const MaxResponse = 256 << 10

var (
	ErrConfig    = errors.New("INITIALIZER_CONFIGURATION_REJECTED")
	ErrOutput    = errors.New("INITIALIZER_OUTPUT_UNAVAILABLE_OR_EXISTS")
	ErrPreflight = errors.New("INITIALIZER_PREFLIGHT_REJECTED_NO_INIT_SENT")
	ErrUncertain = errors.New("INITIALIZER_RESULT_UNCERTAIN_DO_NOT_RETRY_OR_DELETE_OUTPUT")
)

type Config struct {
	Endpoint        string `json:"endpoint"`
	CAFile          string `json:"ca_file"`
	RecipientFile   string `json:"recipient_file"`
	RecipientSHA256 string `json:"recipient_sha256"`
	OutputDirectory string `json:"output_directory"`
	InstanceID      string `json:"instance_id"`
}

// Envelope metadata is encrypted along with the exact response (base64 in JSON).
// No ready marker substitutes for the custodian's actual decrypt/unseal check.
type Envelope struct {
	Version         int    `json:"version"`
	InstanceID      string `json:"instance_id"`
	Endpoint        string `json:"endpoint"`
	CASHA256        string `json:"ca_sha256"`
	RecipientSHA256 string `json:"recipient_sha256"`
	CapturedAt      string `json:"captured_at"`
	HTTPStatus      int    `json:"http_status"`
	Complete        bool   `json:"complete"`
	Response        []byte `json:"response"`
}

func readFile(path string, bound int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfig
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, ErrConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrConfig
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > bound || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrConfig
	}
	b, err := io.ReadAll(io.LimitReader(f, bound+1))
	if err != nil || len(b) == 0 || int64(len(b)) > bound {
		return nil, ErrConfig
	}
	return b, nil
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := readFile(path, 16<<10)
	if err != nil {
		return cfg, ErrConfig
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil {
		return Config{}, ErrConfig
	}
	if d.Decode(new(any)) != io.EOF {
		return Config{}, ErrConfig
	}
	return cfg, nil
}

func sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func clientFor(cfg Config) (*http.Client, *age.X25519Recipient, string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != cfg.Endpoint {
		return nil, nil, "", ErrConfig
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{15,63}$`).MatchString(cfg.InstanceID) {
		return nil, nil, "", ErrConfig
	}
	pub, err := readFile(cfg.RecipientFile, 1024)
	if err != nil {
		return nil, nil, "", ErrConfig
	}
	canonical := strings.TrimSpace(string(pub))
	r, err := age.ParseX25519Recipient(canonical)
	if err != nil || sum([]byte(canonical)) != cfg.RecipientSHA256 {
		return nil, nil, "", ErrConfig
	}
	ca, err := readFile(cfg.CAFile, 64<<10)
	if err != nil {
		return nil, nil, "", ErrConfig
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, nil, "", ErrConfig
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 32 << 10,
		// No ProxyFromEnvironment, custom root fallback, or insecure TLS mode.
	}
	c := &http.Client{Transport: transport, Timeout: 45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, r, sum(ca), nil
}

func syncDirectory(path string) error {
	// The shipped online command is Linux-only. Windows unit tests do not prove
	// POSIX directory durability; the real Linux integration covers this path.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrOutput
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrOutput
	}
	return nil
}

func reserve(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrOutput
	}
	parent := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return nil, ErrOutput
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, ErrOutput
	}
	if os.Mkdir(path, 0700) != nil {
		return nil, ErrOutput
	}
	// Keep every artifact even on failure. Never silently erase an attempt.
	if syncDirectory(parent) != nil {
		return nil, ErrOutput
	}
	f, err := os.OpenFile(filepath.Join(path, "initialization.age"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrOutput
	}
	if f.Sync() != nil || syncDirectory(path) != nil {
		f.Close()
		return nil, ErrOutput
	}
	return f, nil
}

func marker(dir, name, message string) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrOutput
	}
	_, writeErr := io.WriteString(f, message+"\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || syncDirectory(dir) != nil {
		return ErrOutput
	}
	return nil
}

func request(ctx context.Context, c *http.Client, cfg Config, path string, body []byte) (int, []byte, bool) {
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method = http.MethodPost
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Endpoint+"/v1/"+path, reader)
	if err != nil {
		return 0, nil, false
	}
	// Explicitly not replayable, including after transport-level connection loss.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	return resp.StatusCode, raw, err == nil && len(raw) <= MaxResponse
}

func preflight(ctx context.Context, c *http.Client, cfg Config) bool {
	status, raw, complete := request(ctx, c, cfg, "sys/init", nil)
	var state struct {
		Initialized *bool `json:"initialized"`
	}
	if status != 200 || !complete || json.Unmarshal(raw, &state) != nil || state.Initialized == nil || *state.Initialized {
		return false
	}
	status, raw, complete = request(ctx, c, cfg, "sys/seal-status", nil)
	var seal struct {
		Initialized *bool  `json:"initialized"`
		Sealed      *bool  `json:"sealed"`
		Type        string `json:"type"`
	}
	return status == 200 && complete && json.Unmarshal(raw, &seal) == nil && seal.Initialized != nil && !*seal.Initialized && seal.Sealed != nil && *seal.Sealed && seal.Type == "shamir"
}

func validMaterial(raw []byte) bool {
	var material struct {
		Keys []string `json:"keys_base64"`
		Root string   `json:"root_token"`
	}
	if json.Unmarshal(raw, &material) != nil || len(material.Keys) != 1 || len(material.Root) < 16 || len(material.Root) > 4096 || strings.ContainsAny(material.Root, " \t\r\n") {
		return false
	}
	key, err := base64.StdEncoding.Strict().DecodeString(material.Keys[0])
	defer clear(key)
	return err == nil && len(key) >= 32 && len(key) <= 128
}

func capture(f *os.File, recipient age.Recipient, envelope Envelope) error {
	plain, err := json.Marshal(envelope)
	if err != nil {
		return ErrUncertain
	}
	defer clear(plain)
	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, recipient)
	if err != nil {
		return ErrUncertain
	}
	if _, err = w.Write(plain); err != nil {
		return ErrUncertain
	}
	if w.Close() != nil {
		return ErrUncertain
	}
	b := ciphertext.Bytes()
	if n, err := f.Write(b); err != nil || n != len(b) {
		return ErrUncertain
	}
	if f.Sync() != nil {
		return ErrUncertain
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return ErrUncertain
	}
	readback, err := io.ReadAll(io.LimitReader(f, int64(len(b)+1)))
	if err != nil || !bytes.Equal(b, readback) {
		return ErrUncertain
	}
	return nil
}

// Run requires explicit custody and fresh-instance acknowledgement. Once the
// POST can have been sent, all failures become uncertain: no retry or cleanup.
func Run(ctx context.Context, cfg Config, custodyConfirmed, freshConfirmed bool) error {
	if !custodyConfirmed || !freshConfirmed {
		return ErrConfig
	}
	c, recipient, caHash, err := clientFor(cfg)
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	f, err := reserve(cfg.OutputDirectory)
	if err != nil {
		return err
	}
	defer f.Close()
	if !preflight(ctx, c, cfg) {
		return ErrPreflight
	}
	if marker(cfg.OutputDirectory, "INIT_ATTEMPTED", "Initialization may have been sent. Never retry or delete without manual review.") != nil {
		return ErrOutput
	}
	status, raw, complete := request(ctx, c, cfg, "sys/init", []byte(`{"secret_shares":1,"secret_threshold":1}`))
	defer clear(raw)
	if len(raw) == 0 {
		return ErrUncertain
	}
	e := Envelope{Version: 1, InstanceID: cfg.InstanceID, Endpoint: cfg.Endpoint, CASHA256: caHash, RecipientSHA256: cfg.RecipientSHA256, CapturedAt: time.Now().UTC().Format(time.RFC3339), HTTPStatus: status, Complete: complete, Response: raw}
	if capture(f, recipient, e) != nil || f.Close() != nil || syncDirectory(cfg.OutputDirectory) != nil {
		return ErrUncertain
	}
	if status != 200 || !complete || !validMaterial(raw) {
		return ErrUncertain
	}
	if marker(cfg.OutputDirectory, "INIT_CAPTURED", "Encrypted response captured and read back. Custodian decrypt/unseal and offline backup still required.") != nil {
		return ErrUncertain
	}
	return nil
}
