// Package natsidentity creates an offline, fixed-role NKey custody bundle.
// It knows identity names, never event subjects or broker permissions.
package natsidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/nats-io/nkeys"
)

var ErrIdentity = errors.New("NATS_IDENTITY_GENERATION_FAILED_PRESERVE_PARTIAL_OUTPUT")

const purpose = "pinvou-nats-service-identities"

type Config struct {
	RecipientFile   string `json:"recipient_file"`
	RecipientSHA256 string `json:"recipient_sha256"`
	OutputDirectory string `json:"output_directory"`
}
type PublicKeys struct {
	Version int               `json:"version"`
	Purpose string            `json:"purpose"`
	Keys    map[string]string `json:"public_keys"`
}
type envelope struct {
	PublicKeys
	Seeds map[string][]byte `json:"seeds"`
}
type manifest struct {
	Version         int               `json:"version"`
	Purpose         string            `json:"purpose"`
	RecipientSHA256 string            `json:"recipient_sha256"`
	Files           map[string]string `json:"files"`
}

func roles() []string {
	return []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
}
func hash(b []byte) string { digest := sha256.Sum256(b); return hex.EncodeToString(digest[:]) }

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := readPublic(path)
	if err != nil {
		return cfg, ErrIdentity
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return Config{}, ErrIdentity
	}
	return cfg, nil
}

func Generate(cfg Config) error {
	b, err := readPublic(cfg.RecipientFile)
	if err != nil {
		return ErrIdentity
	}
	canonical := strings.TrimSpace(string(b))
	recipient, err := age.ParseX25519Recipient(canonical)
	if err != nil || hash([]byte(canonical)) != cfg.RecipientSHA256 {
		return ErrIdentity
	}
	if privateDirectory(filepath.Dir(cfg.OutputDirectory)) != nil || !filepath.IsAbs(cfg.OutputDirectory) || filepath.Clean(cfg.OutputDirectory) != cfg.OutputDirectory || os.Mkdir(cfg.OutputDirectory, 0700) != nil || syncDirectory(filepath.Dir(cfg.OutputDirectory)) != nil {
		return ErrIdentity
	}
	data := envelope{PublicKeys: PublicKeys{Version: 1, Purpose: purpose, Keys: map[string]string{}}, Seeds: map[string][]byte{}}
	defer func() {
		for _, seed := range data.Seeds {
			clear(seed)
		}
	}()
	seen := map[string]bool{}
	for _, role := range roles() {
		pair, err := nkeys.CreateUser()
		if err != nil {
			return ErrIdentity
		}
		borrowedSeed, seedErr := pair.Seed()
		// Seed returns the keypair's own buffer. Take ownership before Wipe,
		// otherwise wiping the SDK object also destroys the projected seed.
		seed := bytes.Clone(borrowedSeed)
		public, pubErr := pair.PublicKey()
		pair.Wipe()
		if seedErr != nil || pubErr != nil || !nkeys.IsValidPublicUserKey(public) || seen[public] {
			clear(seed)
			return ErrIdentity
		}
		check, err := nkeys.FromSeed(seed)
		if err != nil {
			clear(seed)
			return ErrIdentity
		}
		actual, err := check.PublicKey()
		check.Wipe()
		if err != nil || actual != public {
			clear(seed)
			return ErrIdentity
		}
		data.Seeds[role] = seed
		data.Keys[role] = public
		seen[public] = true
	}
	plain, err := json.Marshal(data)
	if err != nil {
		return ErrIdentity
	}
	defer clear(plain)
	encrypted, err := custody.Encrypt(plain, recipient)
	if err != nil {
		return ErrIdentity
	}
	m := manifest{Version: 1, Purpose: purpose, RecipientSHA256: cfg.RecipientSHA256, Files: map[string]string{}}
	persist := func(name string, contents []byte) error {
		if writeNew(filepath.Join(cfg.OutputDirectory, filepath.FromSlash(name)), contents) != nil {
			return ErrIdentity
		}
		m.Files[name] = hash(contents)
		return nil
	}
	// Durable encrypted custody precedes every unencrypted seed projection.
	if persist("identities.age", encrypted) != nil {
		return ErrIdentity
	}
	for _, role := range roles() {
		dir := filepath.Join(cfg.OutputDirectory, role)
		if os.Mkdir(dir, 0700) != nil || syncDirectory(cfg.OutputDirectory) != nil || persist(role+"/nkey.seed", data.Seeds[role]) != nil {
			return ErrIdentity
		}
	}
	public, err := json.Marshal(data.PublicKeys)
	if err != nil || persist("public-keys.json", public) != nil {
		return ErrIdentity
	}
	metadata, err := json.Marshal(m)
	if err != nil || len(m.Files) != 8 || writeNew(filepath.Join(cfg.OutputDirectory, "manifest.json"), metadata) != nil {
		return ErrIdentity
	}
	if writeNew(filepath.Join(cfg.OutputDirectory, "READY"), []byte("NATS_IDENTITIES_READY_V1\n"+hash(metadata)+"\n")) != nil {
		return ErrIdentity
	}
	return nil
}
