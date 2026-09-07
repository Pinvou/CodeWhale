package operator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func bootstrapProof(t *testing.T, cfg Config, id *age.X25519Identity, encrypted []byte, parent, custodyDir string, pass []byte) {
	t.Helper()
	ctx := context.Background()
	output := filepath.Join(parent, "bootstrap")
	if err := PrepareBootstrap(ctx, cfg, encrypted, id, output); err != nil {
		s, e := newBootstrapSession(ctx, cfg, encrypted, id)
		if e == nil {
			defer s.client.CloseIdleConnections()
			for _, path := range []string{"sys/mounts", "sys/auth"} {
				var data struct {
					Data map[string]any `json:"data"`
				}
				if s.call(path, nil, s.root, &data, 200) == nil {
					if _, ok := data.Data["transit/"]; ok {
						t.Log("BOOTSTRAP_FIXTURE_TRANSIT_EXISTS")
					}
					if _, ok := data.Data["approle/"]; ok {
						t.Log("BOOTSTRAP_FIXTURE_APPROLE_EXISTS")
					}
				}
			}
			if s.call(keyPath, nil, s.root, nil, 200) == nil {
				t.Log("BOOTSTRAP_FIXTURE_KEY_EXISTS")
			}
			if s.verifyConfiguration() != nil {
				t.Log("BOOTSTRAP_FIXTURE_PREPARE_CONFIG_FAILED")
			}
			roles, _, e := loadRoles(s, output, id)
			if e == nil {
				if s.verifyIdentities(roles) != nil {
					t.Log("BOOTSTRAP_FIXTURE_PREPARE_IDENTITIES_FAILED")
				}
				if s.checkAllCapabilities(roles) != nil {
					t.Log("BOOTSTRAP_FIXTURE_PREPARE_CAPABILITIES_FAILED")
				}
			}
		}
		t.Fatal("BOOTSTRAP_FIXTURE_PREPARE_FAILED", err)
	}
	t.Log("BOOTSTRAP_FIXTURE_PREPARED")
	if err := PrepareBootstrap(ctx, cfg, encrypted, id, output); err != ErrBootstrapPreflight {
		t.Fatal("bootstrap output was overwritten")
	}
	if err := PrepareBootstrap(ctx, cfg, encrypted, id, filepath.Join(parent, "duplicate")); err != ErrBootstrapPreflight {
		t.Fatal("existing mounts accepted")
	}
	s, err := newBootstrapSession(ctx, cfg, encrypted, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()
	e, _, err := loadRoles(s, output, id)
	if err != nil {
		t.Fatal("encrypted roles failed readback")
	}
	wrong, _ := age.GenerateX25519Identity()
	if _, _, err := loadRoles(s, output, wrong); err != ErrBinding {
		t.Fatal("wrong recovery identity accepted")
	}
	bad := e.Roles[roleNames[0]]
	bad.SecretID = e.Roles[roleNames[1]].SecretID
	if _, err := s.login(roleNames[0], bad); err == nil {
		t.Fatal("cross-role bootstrap accepted")
	}
	if err := FinalizeBootstrap(ctx, cfg, encrypted, id, output); err != nil {
		t.Fatal("BOOTSTRAP_FIXTURE_FINALIZE_FAILED", err)
	}
	t.Log("BOOTSTRAP_FIXTURE_FINALIZED")
	if err := FinalizeBootstrap(ctx, cfg, encrypted, id, output); err != nil {
		t.Fatal("uncertain finalization not resumable", err)
	}
	if s.call("auth/token/lookup-self", nil, s.root, nil, 403) != nil {
		t.Fatal("initial root survived")
	}
	ops, err := s.login(roleNames[3], e.Roles[roleNames[3]])
	if err != nil {
		t.Fatal("operations identity did not survive root")
	}
	defer s.call("auth/token/revoke-self", map[string]any{}, ops, nil, 204)
	if s.call(keyPath+"/rotate", map[string]any{}, ops, nil, 200) != nil {
		t.Fatal("bounded key rotation unavailable")
	}
	var newSecret struct {
		Data roleCredential `json:"data"`
	}
	if s.call("auth/approle/role/"+roleNames[0]+"/secret-id", map[string]any{}, ops, &newSecret, 200) != nil {
		t.Fatal("bounded SecretID rotation unavailable")
	}
	newSecret.Data.RoleID = e.Roles[roleNames[0]].RoleID
	token, err := s.login(roleNames[0], newSecret.Data)
	if err != nil || s.verifyRuntime(token) != nil {
		t.Fatal("rotated bootstrap cannot authenticate")
	}
	if s.call("auth/token/revoke-self", map[string]any{}, token, nil, 204) != nil {
		t.Fatal("probe revoke failed")
	}
	if s.call("auth/approle/role/"+roleNames[0]+"/secret-id-accessor/destroy", map[string]string{"secret_id_accessor": newSecret.Data.Accessor}, ops, nil, 204) != nil {
		t.Fatal("SecretID revocation unavailable")
	}
	if _, err := s.login(roleNames[0], newSecret.Data); err == nil {
		t.Fatal("revoked bootstrap accepted")
	}
	if s.call("auth/approle/role/outside-fixture/secret-id", map[string]any{}, ops, nil, 403) != nil {
		t.Fatal("operations identity escaped fixed role set")
	}
	if _, err := os.Stat(filepath.Join(output, "BOOTSTRAP_FINALIZED")); err != nil {
		t.Fatal("final state marker absent")
	}
	stageParent := t.TempDir()
	if err := os.Chmod(stageParent, 0700); err != nil {
		t.Fatal("fixture staging parent permission failed")
	}
	stage := filepath.Join(stageParent, "delivery")
	if err := RunStageAgentFiles(cfg, custodyDir, filepath.Join(output, "roles.age"), pass, stage); err != nil {
		t.Fatal("BOOTSTRAP_AGENT_STAGING_FAILED", err)
	}
	for _, role := range roleNames[:3] {
		roleID, err := readLocal(filepath.Join(stage, role, "role-id"), true)
		if err != nil {
			t.Fatal("staged RoleID absent")
		}
		secretID, err := readLocal(filepath.Join(stage, role, "secret-id"), true)
		if err != nil {
			t.Fatal("staged SecretID absent")
		}
		token, err := s.login(role, roleCredential{RoleID: string(roleID), SecretID: string(secretID)})
		clear(roleID)
		clear(secretID)
		if err != nil || s.verifyOne(role, token) != nil {
			t.Fatal("staged identity rejected by actual server")
		}
		if s.call("auth/token/revoke-self", map[string]any{}, token, nil, 204) != nil {
			t.Fatal("staged probe token cleanup failed")
		}
	}
	if _, err := os.Stat(filepath.Join(stage, roleNames[3])); !os.IsNotExist(err) {
		t.Fatal("offline operations role was staged")
	}
	t.Log("ACTUAL_OPENBAO_AGENT_STAGING_PASS")
	t.Log("ACTUAL_OPENBAO_BOOTSTRAP_PASS")
}
