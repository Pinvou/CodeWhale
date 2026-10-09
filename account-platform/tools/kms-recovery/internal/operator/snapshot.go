package operator

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

const MaxSnapshot = 64 << 20
const snapshotMagic = "PINVOU_KMS_SNAPSHOT_V1\n"

var ErrSnapshot = errors.New("KMS_SNAPSHOT_FAILED_PRESERVE_INCOMPLETE_OUTPUT")

func emptySnapshotIdentityPolicies(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true // Omitted means no identity policies; JSON null is not omitted.
	}
	var policies []json.RawMessage
	return json.Unmarshal(raw, &policies) == nil && policies != nil && len(policies) == 0
}

type SnapshotConfig struct {
	Config
	RecipientFile   string `json:"recipient_file"`
	TokenFile       string `json:"token_file"`
	OutputDirectory string `json:"output_directory"`
}

type snapshotMetadata struct {
	Version         int    `json:"version"`
	InstanceID      string `json:"instance_id"`
	Endpoint        string `json:"endpoint"`
	CASHA256        string `json:"ca_sha256"`
	RecipientSHA256 string `json:"recipient_sha256"`
	CapturedAt      string `json:"captured_at"`
	SnapshotSHA256  string `json:"snapshot_sha256"`
}

func LoadSnapshotConfig(path string) (SnapshotConfig, error) {
	var cfg SnapshotConfig
	b, err := readLocal(path, false)
	if err != nil || len(b) > 16<<10 {
		return cfg, ErrConfig
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return SnapshotConfig{}, ErrConfig
	}
	return cfg, nil
}

func validSnapshot(raw []byte) bool {
	if len(raw) < 64 || len(raw) > MaxSnapshot {
		return false
	}
	r, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	defer r.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(r, (512<<20)+1))
	return err == nil && n >= 64 && n <= 512<<20
}

func snapshotRead(ctx context.Context, c *http.Client, cfg SnapshotConfig, token, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.Endpoint+"/v1/"+path, nil)
	if err != nil {
		return nil, ErrSnapshot
	}
	req.Header.Set("X-Vault-Token", token)
	response, err := c.Do(req)
	if err != nil {
		return nil, ErrSnapshot
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrSnapshot
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, ErrSnapshot
	}
	return raw, nil
}

// CaptureSnapshot never reads AppRole/bootstrap/custody secrets or creates a
// token. It uses only the existing backup Agent sink and a public age recipient.
func CaptureSnapshot(ctx context.Context, cfg SnapshotConfig) (string, error) {
	c, caHash, err := client(cfg.Config)
	if err != nil || privateDirectory(cfg.OutputDirectory) != nil {
		return "", ErrConfig
	}
	defer c.CloseIdleConnections()
	c.Timeout = 45 * time.Second
	for _, path := range []string{cfg.TokenFile, cfg.RecipientFile, cfg.CAFile} {
		if nestedPath(path, cfg.OutputDirectory) || nestedPath(cfg.OutputDirectory, filepath.Dir(path)) {
			return "", ErrConfig
		}
	}
	pub, err := readLocal(cfg.RecipientFile, false)
	if err != nil || len(pub) > 1024 {
		return "", ErrConfig
	}
	r, err := age.ParseX25519Recipient(strings.TrimSpace(string(pub)))
	if err != nil || hash([]byte(r.String())) != cfg.RecipientSHA256 {
		return "", ErrConfig
	}
	token, err := readLocal(cfg.TokenFile, true)
	defer clear(token)
	if err != nil || len(token) < 20 || len(token) > 4096 || bytes.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return "", ErrConfig
	}
	lease, err := snapshotRead(ctx, c, cfg, string(token), "auth/token/lookup-self", 64<<10)
	defer clear(lease)
	var identity struct {
		Data struct {
			IdentityPolicies json.RawMessage `json:"identity_policies"`
			Policies         []string        `json:"policies"`
			TTL              int             `json:"ttl"`
			MaxTTL           int             `json:"explicit_max_ttl"`
			Type             string          `json:"type"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal(lease, &identity) != nil || !emptySnapshotIdentityPolicies(identity.Data.IdentityPolicies) || len(identity.Data.Policies) != 1 || identity.Data.Policies[0] != "pinvou-kms-backup" || identity.Data.Type != "service" || identity.Data.TTL < 60 || identity.Data.TTL > 300 || identity.Data.MaxTTL < identity.Data.TTL || identity.Data.MaxTTL > 900 {
		return "", ErrSnapshot
	}
	// Reserve an exclusive attempt before requesting snapshot. Failed attempts
	// remain distinguishable by their absent completion manifest.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", ErrSnapshot
	}
	at := time.Now().UTC()
	out := filepath.Join(cfg.OutputDirectory, "snapshot-"+at.Format("20060102T150405Z")+"-"+hex.EncodeToString(nonce[:]))
	if os.Mkdir(out, 0700) != nil || syncDirectory(cfg.OutputDirectory) != nil {
		return "", ErrSnapshot
	}
	raw, err := snapshotRead(ctx, c, cfg, string(token), "sys/storage/raft/snapshot", MaxSnapshot)
	defer clear(raw)
	if err != nil || !validSnapshot(raw) {
		return "", ErrSnapshot
	}
	meta := snapshotMetadata{1, cfg.InstanceID, cfg.Endpoint, caHash, cfg.RecipientSHA256, at.Format(time.RFC3339), hash(raw)}
	header, err := json.Marshal(meta)
	if err != nil {
		return "", ErrSnapshot
	}
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, r)
	if err != nil {
		return "", ErrSnapshot
	}
	for _, part := range [][]byte{[]byte(snapshotMagic), header, []byte("\n"), raw} {
		if _, err := w.Write(part); err != nil {
			return "", ErrSnapshot
		}
	}
	if w.Close() != nil || writeSnapshotFile(filepath.Join(out, "snapshot.age"), encrypted.Bytes()) != nil {
		return "", ErrSnapshot
	}
	manifest, _ := json.Marshal(map[string]any{"version": 1, "captured_at": meta.CapturedAt, "ciphertext_sha256": hash(encrypted.Bytes()), "ciphertext_bytes": encrypted.Len()})
	if writeNew(filepath.Join(out, "SNAPSHOT_READY"), manifest) != nil {
		return "", ErrSnapshot
	}
	return out, nil
}

// VerifySnapshot is entirely offline. Complete age authentication, metadata
// binding and gzip CRC must pass before any snapshot bytes can be used.
func VerifySnapshot(cfg Config, encrypted []byte, id *age.X25519Identity) ([]byte, error) {
	if id == nil || hash([]byte(id.Recipient().String())) != cfg.RecipientSHA256 || len(encrypted) > MaxSnapshot+(1<<20) {
		return nil, ErrBinding
	}
	c, caHash, err := client(cfg) // Configuration validation only; never Do.
	if err != nil {
		return nil, ErrConfig
	}
	c.CloseIdleConnections()
	r, err := age.Decrypt(bytes.NewReader(encrypted), id)
	if err != nil {
		return nil, ErrBinding
	}
	plain, err := io.ReadAll(io.LimitReader(r, MaxSnapshot+(16<<10)+1))
	defer clear(plain)
	if err != nil || len(plain) > MaxSnapshot+(16<<10) || !bytes.HasPrefix(plain, []byte(snapshotMagic)) {
		return nil, ErrBinding
	}
	parts := bytes.SplitN(plain[len(snapshotMagic):], []byte("\n"), 2)
	if len(parts) != 2 || len(parts[0]) > 16<<10 {
		return nil, ErrBinding
	}
	var meta snapshotMetadata
	d := json.NewDecoder(bytes.NewReader(parts[0]))
	d.DisallowUnknownFields()
	if d.Decode(&meta) != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrBinding
	}
	at, err := time.Parse(time.RFC3339, meta.CapturedAt)
	if err != nil || at.IsZero() || at.After(time.Now().Add(5*time.Minute)) || meta.Version != 1 || meta.InstanceID != cfg.InstanceID || meta.Endpoint != cfg.Endpoint || meta.CASHA256 != caHash || meta.RecipientSHA256 != cfg.RecipientSHA256 || meta.SnapshotSHA256 != hash(parts[1]) || !validSnapshot(parts[1]) {
		return nil, ErrBinding
	}
	return append([]byte(nil), parts[1]...), nil
}

func RunVerifySnapshot(cfg Config, custodyDirectory, bundle string, pass []byte) error {
	for _, name := range []string{"identity.age", "recipient.txt", "CUSTODY_READY"} {
		if _, err := readLocal(filepath.Join(custodyDirectory, name), true); err != nil {
			return err
		}
	}
	id, err := custody.Unlock(custodyDirectory, pass)
	if err != nil {
		return ErrBinding
	}
	encrypted, err := readSnapshotFile(bundle)
	if err != nil {
		return ErrBinding
	}
	verified, err := VerifySnapshot(cfg, encrypted, id)
	clear(verified)
	return err
}
