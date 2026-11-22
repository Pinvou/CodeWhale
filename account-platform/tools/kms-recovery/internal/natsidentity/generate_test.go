package natsidentity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/nats-io/nkeys"
)

func fixture(t *testing.T) (Config, *age.X25519Identity) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	public := id.Recipient().String()
	cfg := Config{RecipientFile: filepath.Join(dir, "recipient.txt"), RecipientSHA256: hash([]byte(public)), OutputDirectory: filepath.Join(dir, "identities")}
	if os.WriteFile(cfg.RecipientFile, []byte(public), 0600) != nil {
		t.Fatal("fixture recipient")
	}
	return cfg, id
}
func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal("fixture file:", name)
	}
	return b
}

func TestIdentityBundleCustodyAndSeparation(t *testing.T) {
	unique := map[string]bool{}
	for range 2 {
		cfg, id := fixture(t)
		if Generate(cfg) != nil {
			t.Fatal("generate")
		}
		public := read(t, cfg.OutputDirectory, "public-keys.json")
		var keys PublicKeys
		if json.Unmarshal(public, &keys) != nil || keys.Version != 1 || keys.Purpose != purpose || len(keys.Keys) != 6 {
			t.Fatal("public metadata")
		}
		ciphertext := read(t, cfg.OutputDirectory, "identities.age")
		plain, err := custody.Decrypt(ciphertext, id)
		if err != nil {
			t.Fatal("test custody recovery")
		}
		defer clear(plain)
		var recovered envelope
		if json.Unmarshal(plain, &recovered) != nil || recovered.Version != 1 || recovered.Purpose != purpose || len(recovered.Seeds) != 6 || len(recovered.Keys) != 6 {
			t.Fatal("envelope")
		}
		defer func() {
			for _, seed := range recovered.Seeds {
				clear(seed)
			}
		}()
		var m manifest
		metadata := read(t, cfg.OutputDirectory, "manifest.json")
		if json.Unmarshal(metadata, &m) != nil || m.Version != 1 || m.Purpose != purpose || m.RecipientSHA256 != cfg.RecipientSHA256 || len(m.Files) != 8 {
			t.Fatal("manifest")
		}
		if string(read(t, cfg.OutputDirectory, "READY")) != "NATS_IDENTITIES_READY_V1\n"+hash(metadata)+"\n" {
			t.Fatal("READY binding")
		}
		expected := map[string]bool{"identities.age": true, "public-keys.json": true, "manifest.json": true, "READY": true}
		for _, role := range roles() {
			expected[role+"/nkey.seed"] = true
		}
		for name, digest := range m.Files {
			if !expected[name] || name == "manifest.json" || name == "READY" || hash(read(t, cfg.OutputDirectory, name)) != digest {
				t.Fatal("manifest digest")
			}
		}
		count := 0
		if err := filepath.WalkDir(cfg.OutputDirectory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0600)
			if entry.IsDir() {
				mode = 0700
			} else {
				rel, _ := filepath.Rel(cfg.OutputDirectory, path)
				if !info.Mode().IsRegular() || !expected[filepath.ToSlash(rel)] {
					t.Fatal("unexpected artifact")
				}
				count++
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
				t.Fatal("unsafe artifact mode")
			}
			return nil
		}); err != nil || count != 10 {
			t.Fatal("artifact count")
		}
		for _, role := range roles() {
			seed := read(t, cfg.OutputDirectory, role+"/nkey.seed")
			if !bytes.Equal(seed, recovered.Seeds[role]) || bytes.Contains(public, seed) || bytes.Contains(ciphertext, seed) {
				clear(seed)
				t.Fatal("seed separation")
			}
			pair, err := nkeys.FromSeed(seed)
			clear(seed)
			if err != nil {
				t.Fatal("seed format")
			}
			pub, err := pair.PublicKey()
			pair.Wipe()
			if err != nil || keys.Keys[role] != pub || recovered.Keys[role] != pub || unique[pub] {
				t.Fatal("identity binding or uniqueness")
			}
			unique[pub] = true
		}
		wrong, _ := age.GenerateX25519Identity()
		if _, err := custody.Decrypt(ciphertext, wrong); err == nil {
			t.Fatal("wrong custody identity accepted")
		}
		ciphertext[len(ciphertext)-1] ^= 1
		if _, err := custody.Decrypt(ciphertext, id); err == nil {
			t.Fatal("corrupt custody accepted")
		}
		before := read(t, cfg.OutputDirectory, "identities.age")
		if Generate(cfg) != ErrIdentity || !bytes.Equal(before, read(t, cfg.OutputDirectory, "identities.age")) {
			t.Fatal("existing bundle overwritten")
		}
	}
	t.Log("NATS_IDENTITY_BUNDLE_CUSTODY_PASS")
}

func TestIdentityBundleInputsAndPartialPreservation(t *testing.T) {
	for _, kind := range []string{"recipient", "hash", "relative", "missing-parent", "partial", "empty"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := fixture(t)
			switch kind {
			case "recipient":
				os.WriteFile(cfg.RecipientFile, []byte("invalid"), 0600)
			case "hash":
				cfg.RecipientSHA256 = strings.Repeat("0", 64)
			case "relative":
				cfg.OutputDirectory = "relative"
			case "missing-parent":
				cfg.OutputDirectory = filepath.Join(cfg.OutputDirectory, "child")
			case "partial", "empty":
				os.Mkdir(cfg.OutputDirectory, 0700)
				if kind == "partial" {
					os.WriteFile(filepath.Join(cfg.OutputDirectory, "identities.age"), []byte("preserve"), 0600)
				}
			}
			if Generate(cfg) != ErrIdentity {
				t.Fatal("unsafe generation accepted")
			}
			if _, err := os.Stat(filepath.Join(cfg.OutputDirectory, "READY")); !os.IsNotExist(err) {
				t.Fatal("failure marked ready")
			}
			if kind == "partial" && string(read(t, cfg.OutputDirectory, "identities.age")) != "preserve" {
				t.Fatal("partial material changed")
			}
		})
	}
	cfg, _ := fixture(t)
	path := filepath.Join(filepath.Dir(cfg.OutputDirectory), "config.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(path, b, 0600)
	if actual, err := LoadConfig(path); err != nil || actual != cfg {
		t.Fatal("config roundtrip")
	}
	for _, bad := range []string{`{"roles":["admin"]}`, string(b) + `{}`, `{`} {
		os.WriteFile(path, []byte(bad), 0600)
		if _, err := LoadConfig(path); err != ErrIdentity {
			t.Fatal("unsafe config")
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.Chmod(filepath.Dir(cfg.OutputDirectory), 0755)
	if Generate(cfg) != ErrIdentity {
		t.Fatal("public parent")
	}
	os.Chmod(filepath.Dir(cfg.OutputDirectory), 0700)
	os.Chmod(cfg.RecipientFile, 0660)
	if Generate(cfg) != ErrIdentity {
		t.Fatal("replaceable recipient")
	}
	os.Chmod(cfg.RecipientFile, 0600)
	link := cfg.RecipientFile + ".link"
	if os.Symlink(cfg.RecipientFile, link) != nil {
		t.Fatal("symlink fixture")
	}
	cfg.RecipientFile = link
	if Generate(cfg) != ErrIdentity {
		t.Fatal("symlink recipient")
	}
}
