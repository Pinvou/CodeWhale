package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestStageAgentFilesIsOfflineAndExcludesOperations(t *testing.T) {
	f := newBootstrapFixture(t)
	ctx := context.Background()
	roles := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
	b := f.bundle(t)
	if err := PrepareBootstrap(ctx, f.cfg, b, f.id, roles); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeBootstrap(ctx, f.cfg, b, f.id, roles); err != nil {
		t.Fatal(err)
	}
	before := f.requests.Load()
	f.server.Close() // The KMS is now unreachable: staging must remain offline.
	stageParent := t.TempDir()
	if err := os.Chmod(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(stageParent, "delivery")
	if err := StageAgentFiles(f.cfg, roles, f.id, output); err != nil {
		t.Fatal(err)
	}
	if f.requests.Load() != before {
		t.Fatal("offline staging made an API call")
	}
	raw, err := os.ReadFile(filepath.Join(output, "AGENT_FILES_STAGED"))
	if err != nil {
		t.Fatal("staging marker missing")
	}
	var manifest agentDeliveryManifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.Version != 1 || manifest.InstanceID != f.cfg.InstanceID || len(manifest.Files) != 6 {
		t.Fatal("manifest invalid")
	}
	files := 0
	err = filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		defer clear(data)
		if bytes.Contains(data, []byte(f.credentials[roleNames[3]].SecretID)) || bytes.Contains(data, []byte(f.credentials[roleNames[3]].RoleID)) || bytes.Contains(data, []byte("root-canary")) {
			t.Fatal("offline operator or root material exported")
		}
		return nil
	})
	if err != nil || files != 7 {
		t.Fatal("unexpected staged output")
	}
	for _, role := range roleNames[:3] {
		for _, file := range []struct{ name, value string }{{"role-id", f.credentials[role].RoleID}, {"secret-id", f.credentials[role].SecretID}} {
			raw, err := readLocal(filepath.Join(output, role, file.name), true)
			if err != nil || string(raw) != file.value || hash(raw) != manifest.Files[role+"/"+file.name] {
				t.Fatal("staged identity or digest mismatch")
			}
			clear(raw)
		}
	}
	if err := StageAgentFiles(f.cfg, roles, f.id, output); err != ErrConfig {
		t.Fatal("existing plaintext output overwritten")
	}
}

func TestStageAgentFilesRejectsUnsafeSourcesAndLocations(t *testing.T) {
	f := newBootstrapFixture(t)
	ctx := context.Background()
	roles := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
	b := f.bundle(t)
	if err := PrepareBootstrap(ctx, f.cfg, b, f.id, roles); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "delivery")
	if err := StageAgentFiles(f.cfg, roles, f.id, output); err != ErrBinding {
		t.Fatal("stage accepted before retirement marker")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("premature output created")
	}
	if err := FinalizeBootstrap(ctx, f.cfg, b, f.id, roles); err != nil {
		t.Fatal(err)
	}
	wrong, _ := age.GenerateX25519Identity()
	if err := StageAgentFiles(f.cfg, roles, wrong, output); err != ErrBinding {
		t.Fatal("wrong custodian accepted")
	}
	if err := StageAgentFiles(f.cfg, roles, nil, output); err != ErrBinding {
		t.Fatal("nil custodian accepted")
	}
	for _, out := range []string{"relative", filepath.Join(roles, "nested"), filepath.Join(filepath.Dir(roles), "delivery")} {
		if err := StageAgentFiles(f.cfg, roles, f.id, out); err != ErrConfig {
			t.Fatal("overlapping or relative output accepted")
		}
	}
	marker := filepath.Join(roles, "BOOTSTRAP_FINALIZED")
	if os.WriteFile(marker, []byte("BOOTSTRAP_FINALIZED_V1\nwrong-digest\n"), 0600) != nil {
		t.Fatal("fixture mutation failed")
	}
	before := f.requests.Load()
	if err := StageAgentFiles(f.cfg, roles, f.id, output); err != ErrBinding {
		t.Fatal("wrong finalized package accepted")
	}
	if f.requests.Load() != before {
		t.Fatal("invalid staging made network request")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid output created")
	}
}
