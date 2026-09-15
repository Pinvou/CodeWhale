// Package natsidentity creates an offline, fixed-role NKey custody bundle.
// It knows identity names, never event subjects or broker permissions.
package natsidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/nats-io/nkeys"
)

var ErrIdentity = errors.New("NATS_IDENTITY_GENERATION_FAILED_PRESERVE_PARTIAL_OUTPUT")

const purpose = "pinvou-nats-service-identities"
const identityOverlapMinimum = 5 * time.Minute
const identityOverlapMaximum = 7 * 24 * time.Hour

type Config struct {
	RecipientFile     string `json:"recipient_file"`
	RecipientSHA256   string `json:"recipient_sha256"`
	OutputDirectory   string `json:"output_directory"`
	ArtifactVersion   int    `json:"artifact_version,omitempty"`
	PreviousDirectory string `json:"previous_directory,omitempty"`
	OverlapExpiresAt  string `json:"overlap_expires_at,omitempty"`
}
type PublicKeys struct {
	Version          int               `json:"version"`
	Purpose          string            `json:"purpose"`
	Keys             map[string]string `json:"public_keys"`
	PreviousV1SHA256 string            `json:"previous_v1_sha256,omitempty"`
	OverlapExpiresAt string            `json:"overlap_expires_at,omitempty"`
}
type publicKeysV1 struct {
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
func rolesV2() []string {
	return []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
}
func hash(b []byte) string { digest := sha256.Sum256(b); return hex.EncodeToString(digest[:]) }

func canonicalDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func decodeClosedJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if validateUniqueJSONValue(decoder) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrIdentity
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrIdentity
	}
	return nil
}

func validateUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return ErrIdentity
			}
			seen[key] = true
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrIdentity
		}
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

func loadVerifiedV1Bundle(directory string) (PublicKeys, []byte, map[string][]byte, error) {
	var public PublicKeys
	var v1 publicKeysV1
	seeds := map[string][]byte{}
	fail := func() (PublicKeys, []byte, map[string][]byte, error) {
		for _, seed := range seeds {
			clear(seed)
		}
		return PublicKeys{}, nil, nil, ErrIdentity
	}
	if privateDirectory(directory) != nil {
		return fail()
	}
	publicRaw, err := readPublic(filepath.Join(directory, "public-keys.json"))
	if err != nil || decodeClosedJSON(publicRaw, &v1) != nil || v1.Version != 1 || v1.Purpose != purpose || len(v1.Keys) != len(roles()) {
		return fail()
	}
	public = PublicKeys{Version: v1.Version, Purpose: v1.Purpose, Keys: v1.Keys}
	unique := map[string]bool{}
	for _, role := range roles() {
		value := public.Keys[role]
		if !nkeys.IsValidPublicUserKey(value) || unique[value] {
			return fail()
		}
		unique[value] = true
	}
	var metadata manifest
	manifestRaw, err := readPublic(filepath.Join(directory, "manifest.json"))
	if err != nil || decodeClosedJSON(manifestRaw, &metadata) != nil || metadata.Version != 1 || metadata.Purpose != purpose || !canonicalDigest(metadata.RecipientSHA256) || len(metadata.Files) != len(roles())+2 {
		return fail()
	}
	ready, err := readPublic(filepath.Join(directory, "READY"))
	if err != nil || string(ready) != "NATS_IDENTITIES_READY_V1\n"+hash(manifestRaw)+"\n" {
		return fail()
	}
	expected := map[string]bool{"identities.age": true, "public-keys.json": true}
	for _, role := range roles() {
		expected[role+"/nkey.seed"] = true
	}
	for name, digest := range metadata.Files {
		if !expected[name] || !canonicalDigest(digest) {
			return fail()
		}
		path := filepath.Join(directory, filepath.FromSlash(name))
		var contents []byte
		var readErr error
		if strings.HasSuffix(name, "/nkey.seed") {
			contents, readErr = readSeed(path)
		} else {
			contents, readErr = readPublic(path)
		}
		if readErr != nil || hash(contents) != digest {
			clear(contents)
			return fail()
		}
		if strings.HasSuffix(name, "/nkey.seed") {
			role := strings.TrimSuffix(name, "/nkey.seed")
			pair, pairErr := nkeys.FromSeed(contents)
			if pairErr != nil {
				clear(contents)
				return fail()
			}
			actual, pubErr := pair.PublicKey()
			pair.Wipe()
			if pubErr != nil || actual != public.Keys[role] {
				clear(contents)
				return fail()
			}
			seeds[role] = contents
		}
	}
	if len(seeds) != len(roles()) {
		return fail()
	}
	allowedFiles := map[string]bool{"identities.age": true, "public-keys.json": true, "manifest.json": true, "READY": true}
	allowedDirectories := map[string]bool{".": true}
	for _, role := range roles() {
		allowedFiles[role+"/nkey.seed"] = true
	}
	for _, role := range roles() {
		allowedDirectories[role] = true
	}
	count := 0
	if filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return ErrIdentity
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if !allowedDirectories[relative] {
				return ErrIdentity
			}
			return nil
		}
		if !allowedFiles[relative] {
			return ErrIdentity
		}
		count++
		return nil
	}) != nil || count != len(allowedFiles) {
		return fail()
	}
	return public, publicRaw, seeds, nil
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := readPublic(path)
	if err != nil {
		return cfg, ErrIdentity
	}
	if decodeClosedJSON(b, &cfg) != nil {
		return Config{}, ErrIdentity
	}
	return cfg, nil
}

func Generate(cfg Config) error {
	return generateAt(cfg, time.Now().UTC().Truncate(time.Second))
}

func generateAt(cfg Config, now time.Time) error {
	if now.Location() != time.UTC {
		return ErrIdentity
	}
	b, err := readPublic(cfg.RecipientFile)
	if err != nil {
		return ErrIdentity
	}
	canonical := strings.TrimSpace(string(b))
	recipient, err := age.ParseX25519Recipient(canonical)
	if err != nil || hash([]byte(canonical)) != cfg.RecipientSHA256 {
		return ErrIdentity
	}
	version := cfg.ArtifactVersion
	if version == 0 {
		version = 1
	}
	selectedRoles := roles()
	var previous PublicKeys
	var previousRaw []byte
	var previousSeeds map[string][]byte
	if version == 2 {
		selectedRoles = rolesV2()
		expiresAt, parseErr := time.Parse(time.RFC3339, cfg.OverlapExpiresAt)
		remaining := expiresAt.Sub(now)
		if parseErr != nil || expiresAt.Location() != time.UTC || expiresAt.Format(time.RFC3339) != cfg.OverlapExpiresAt || remaining < identityOverlapMinimum || remaining > identityOverlapMaximum || privateDirectory(cfg.PreviousDirectory) != nil {
			return ErrIdentity
		}
		previous, previousRaw, previousSeeds, err = loadVerifiedV1Bundle(cfg.PreviousDirectory)
		if err != nil {
			return ErrIdentity
		}
		defer func() {
			for _, seed := range previousSeeds {
				clear(seed)
			}
		}()
		relative, relativeErr := filepath.Rel(cfg.PreviousDirectory, cfg.OutputDirectory)
		if relativeErr != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return ErrIdentity
		}
	} else if version != 1 || cfg.PreviousDirectory != "" || cfg.OverlapExpiresAt != "" {
		return ErrIdentity
	}
	if privateDirectory(filepath.Dir(cfg.OutputDirectory)) != nil || !filepath.IsAbs(cfg.OutputDirectory) || filepath.Clean(cfg.OutputDirectory) != cfg.OutputDirectory || os.Mkdir(cfg.OutputDirectory, 0700) != nil || syncDirectory(filepath.Dir(cfg.OutputDirectory)) != nil {
		return ErrIdentity
	}
	data := envelope{PublicKeys: PublicKeys{Version: version, Purpose: purpose, Keys: map[string]string{}}, Seeds: map[string][]byte{}}
	if version == 2 {
		data.PreviousV1SHA256 = hash(previousRaw)
		data.OverlapExpiresAt = cfg.OverlapExpiresAt
	}
	defer func() {
		for _, seed := range data.Seeds {
			clear(seed)
		}
	}()
	seen := map[string]bool{}
	shared := map[string]bool{"billing-outbox-relay": true, "token-outbox-relay": true, "billing-command-executor": true, "token-command-executor": true, "event-resource-operator": true}
	for _, role := range selectedRoles {
		if version == 2 && shared[role] {
			seed := bytes.Clone(previousSeeds[role])
			pair, pairErr := nkeys.FromSeed(seed)
			if pairErr != nil {
				clear(seed)
				return ErrIdentity
			}
			public, publicErr := pair.PublicKey()
			pair.Wipe()
			if publicErr != nil || public != previous.Keys[role] || seen[public] {
				clear(seed)
				return ErrIdentity
			}
			data.Seeds[role] = seed
			data.Keys[role] = public
			seen[public] = true
			continue
		}
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
		if seedErr != nil || pubErr != nil || !nkeys.IsValidPublicUserKey(public) || seen[public] || (version == 2 && public == previous.Keys["account-outbox-relay"]) {
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
	m := manifest{Version: version, Purpose: purpose, RecipientSHA256: cfg.RecipientSHA256, Files: map[string]string{}}
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
	for _, role := range selectedRoles {
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
	if err != nil || len(m.Files) != len(selectedRoles)+2 || writeNew(filepath.Join(cfg.OutputDirectory, "manifest.json"), metadata) != nil {
		return ErrIdentity
	}
	if writeNew(filepath.Join(cfg.OutputDirectory, "READY"), []byte("NATS_IDENTITIES_READY_V"+fmt.Sprint(version)+"\n"+hash(metadata)+"\n")) != nil {
		return ErrIdentity
	}
	return nil
}
