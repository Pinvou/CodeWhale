package natsidentity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
)

func v2Config(t *testing.T, now time.Time) (Config, Config) {
	t.Helper()
	v1, _ := fixture(t)
	if err := generateAt(v1, now); err != nil {
		t.Fatal(err)
	}
	v2, _ := fixture(t)
	v2.ArtifactVersion = 2
	v2.PreviousDirectory = v1.OutputDirectory
	v2.OverlapExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
	return v1, v2
}

func rewriteV1Manifest(t *testing.T, directory string) {
	t.Helper()
	var metadata manifest
	if json.Unmarshal(read(t, directory, "manifest.json"), &metadata) != nil {
		t.Fatal("manifest fixture")
	}
	for name := range metadata.Files {
		metadata.Files[name] = hash(read(t, directory, name))
	}
	raw, err := json.Marshal(metadata)
	if err != nil || os.WriteFile(filepath.Join(directory, "manifest.json"), raw, 0600) != nil ||
		os.WriteFile(filepath.Join(directory, "READY"), []byte("NATS_IDENTITIES_READY_V1\n"+hash(raw)+"\n"), 0600) != nil {
		t.Fatal("rewrite manifest fixture")
	}
}

func TestIdentityBundleV2ReusesOnlyUnrelatedRoleKeys(t *testing.T) {
	v1, _ := fixture(t)
	if err := Generate(v1); err != nil {
		t.Fatal(err)
	}
	v2, _ := fixture(t)
	v2.ArtifactVersion = 2
	v2.PreviousDirectory = v1.OutputDirectory
	v2.OverlapExpiresAt = time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if err := Generate(v2); err != nil {
		t.Fatal(err)
	}
	var oldKeys, newKeys PublicKeys
	oldRaw := read(t, v1.OutputDirectory, "public-keys.json")
	if json.Unmarshal(oldRaw, &oldKeys) != nil || json.Unmarshal(read(t, v2.OutputDirectory, "public-keys.json"), &newKeys) != nil {
		t.Fatal("public artifacts")
	}
	if newKeys.Version != 2 || len(newKeys.Keys) != 7 || newKeys.PreviousV1SHA256 != hash(oldRaw) || newKeys.OverlapExpiresAt != v2.OverlapExpiresAt {
		t.Fatal("v2 binding")
	}
	for _, role := range []string{"billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		if newKeys.Keys[role] != oldKeys.Keys[role] || string(read(t, v2.OutputDirectory, filepath.ToSlash(role+"/nkey.seed"))) != string(read(t, v1.OutputDirectory, filepath.ToSlash(role+"/nkey.seed"))) {
			t.Fatalf("shared role changed: %s", role)
		}
	}
	if newKeys.Keys["account-outbox-relay"] == oldKeys.Keys["account-outbox-relay"] || newKeys.Keys["account-approval-relay"] == oldKeys.Keys["account-outbox-relay"] || newKeys.Keys["account-approval-relay"] == newKeys.Keys["account-outbox-relay"] {
		t.Fatal("new relay identities reused")
	}
	if string(read(t, v2.OutputDirectory, "READY"))[:25] != "NATS_IDENTITIES_READY_V2\n" {
		t.Fatal("v2 ready marker")
	}
	var metadata manifest
	manifestRaw := read(t, v2.OutputDirectory, "manifest.json")
	if json.Unmarshal(manifestRaw, &metadata) != nil || metadata.Version != 2 || len(metadata.Files) != 9 ||
		string(read(t, v2.OutputDirectory, "READY")) != "NATS_IDENTITIES_READY_V2\n"+hash(manifestRaw)+"\n" {
		t.Fatal("v2 manifest or READY binding")
	}
	for _, role := range rolesV2() {
		seed := read(t, v2.OutputDirectory, role+"/nkey.seed")
		pair, err := nkeys.FromSeed(seed)
		clear(seed)
		if err != nil {
			t.Fatal("v2 seed")
		}
		public, err := pair.PublicKey()
		pair.Wipe()
		if err != nil || public != newKeys.Keys[role] {
			t.Fatalf("v2 seed binding: %s", role)
		}
	}
}

func TestIdentityBundleV2RejectsMissingOrExpiredPrevious(t *testing.T) {
	for _, kind := range []string{"missing", "expired"} {
		cfg, _ := fixture(t)
		cfg.ArtifactVersion = 2
		cfg.PreviousDirectory = filepath.Join(filepath.Dir(cfg.OutputDirectory), "missing")
		cfg.OverlapExpiresAt = time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
		if kind == "expired" {
			cfg.OverlapExpiresAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
		}
		if Generate(cfg) != ErrIdentity {
			t.Fatalf("accepted %s previous", kind)
		}
	}
}

func TestIdentityBundleV2RejectsUnverifiedPreviousBundle(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"bad-ready", func(t *testing.T, dir string) { os.WriteFile(filepath.Join(dir, "READY"), []byte("not-ready"), 0600) }},
		{"bad-hash", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "public-keys.json"), []byte(`{}`), 0600)
		}},
		{"unknown-public-field", func(t *testing.T, dir string) {
			var value map[string]any
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			value["unexpected"] = true
			raw, _ := json.Marshal(value)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"v2-digest-empty", func(t *testing.T, dir string) {
			var value map[string]any
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			value["previous_v1_sha256"] = ""
			raw, _ := json.Marshal(value)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"v2-digest-null", func(t *testing.T, dir string) {
			var value map[string]any
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			value["previous_v1_sha256"] = nil
			raw, _ := json.Marshal(value)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"v2-expiry-empty", func(t *testing.T, dir string) {
			var value map[string]any
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			value["overlap_expires_at"] = ""
			raw, _ := json.Marshal(value)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"v2-expiry-null", func(t *testing.T, dir string) {
			var value map[string]any
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			value["overlap_expires_at"] = nil
			raw, _ := json.Marshal(value)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"duplicate-public-field", func(t *testing.T, dir string) {
			var value PublicKeys
			json.Unmarshal(read(t, dir, "public-keys.json"), &value)
			keys, _ := json.Marshal(value.Keys)
			raw := []byte(`{"version":1,"version":1,"purpose":"` + purpose + `","public_keys":` + string(keys) + `}`)
			os.WriteFile(filepath.Join(dir, "public-keys.json"), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"swapped-seeds", func(t *testing.T, dir string) {
			first := filepath.Join(dir, roles()[0], "nkey.seed")
			second := filepath.Join(dir, roles()[1], "nkey.seed")
			left, right := read(t, dir, roles()[0]+"/nkey.seed"), read(t, dir, roles()[1]+"/nkey.seed")
			os.WriteFile(first, right, 0600)
			os.WriteFile(second, left, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"whitespace-seed", func(t *testing.T, dir string) {
			name := roles()[0] + "/nkey.seed"
			raw := append(read(t, dir, name), '\n')
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), raw, 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"invalid-seed", func(t *testing.T, dir string) {
			name := roles()[0] + "/nkey.seed"
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte("invalid-seed"), 0600)
			rewriteV1Manifest(t, dir)
		}},
		{"unexpected-file", func(t *testing.T, dir string) { os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0600) }},
		{"unexpected-directory", func(t *testing.T, dir string) { os.Mkdir(filepath.Join(dir, "extra"), 0700) }},
	}
	for _, role := range roles() {
		role := role
		cases = append(cases, struct {
			name   string
			mutate func(*testing.T, string)
		}{"missing-" + role, func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, role, "nkey.seed"))
		}})
	}
	if runtime.GOOS != "windows" {
		cases = append(cases,
			struct {
				name   string
				mutate func(*testing.T, string)
			}{"unsafe-seed-permission", func(t *testing.T, dir string) { os.Chmod(filepath.Join(dir, roles()[0], "nkey.seed"), 0644) }},
			struct {
				name   string
				mutate func(*testing.T, string)
			}{"symlink-seed", func(t *testing.T, dir string) {
				name := filepath.Join(dir, roles()[0], "nkey.seed")
				copyName := name + ".copy"
				os.Rename(name, copyName)
				os.Symlink(copyName, name)
			}},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v1, v2 := v2Config(t, now)
			tc.mutate(t, v1.OutputDirectory)
			if err := generateAt(v2, now); err != ErrIdentity {
				t.Fatalf("accepted unverified previous bundle: %v", err)
			}
			if _, err := os.Stat(v2.OutputDirectory); !os.IsNotExist(err) {
				t.Fatal("created output before verifying previous bundle")
			}
		})
	}
}

func TestIdentityBundleV2RejectsOutputInsidePreviousBundle(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same", false: "descendant"}[same], func(t *testing.T) {
			v1, v2 := v2Config(t, now)
			before := map[string][]byte{}
			if err := filepath.WalkDir(v1.OutputDirectory, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				relative, _ := filepath.Rel(v1.OutputDirectory, path)
				before[filepath.ToSlash(relative)] = read(t, v1.OutputDirectory, relative)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			v2.OutputDirectory = v1.OutputDirectory
			if !same {
				v2.OutputDirectory = filepath.Join(v1.OutputDirectory, "v2")
			}
			if err := generateAt(v2, now); err != ErrIdentity {
				t.Fatal("accepted output inside previous bundle")
			}
			for name, expected := range before {
				if !bytes.Equal(expected, read(t, v1.OutputDirectory, name)) {
					t.Fatalf("previous bundle changed: %s", name)
				}
			}
			if !same {
				if _, err := os.Stat(v2.OutputDirectory); !os.IsNotExist(err) {
					t.Fatal("descendant output was created")
				}
			}
		})
	}
}

func TestIdentityBundleV2OverlapBounds(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		window  time.Duration
		allowed bool
	}{
		{"below-minimum", identityOverlapMinimum - time.Second, false},
		{"minimum", identityOverlapMinimum, true},
		{"maximum", identityOverlapMaximum, true},
		{"above-maximum", identityOverlapMaximum + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, v2 := v2Config(t, now)
			v2.OverlapExpiresAt = now.Add(tc.window).Format(time.RFC3339)
			err := generateAt(v2, now)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && err != ErrIdentity {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
