package operator

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

var ErrAgentStage = errors.New("AGENT_FILE_STAGING_INCOMPLETE_PRESERVE_OUTPUT_DO_NOT_DEPLOY")

type agentDeliveryManifest struct {
	Version            int               `json:"version"`
	InstanceID         string            `json:"instance_id"`
	RolesPackageSHA256 string            `json:"roles_package_sha256"`
	Files              map[string]string `json:"files_sha256"`
}

func nestedPath(path, directory string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// StageAgentFiles is OFFLINE. It never reads the initialization package, sends
// a token, or exports the fourth (operations) role. Only the local custodian
// invokes it; runtime business containers must never mount these output files.
func StageAgentFiles(cfg Config, rolesDirectory string, id *age.X25519Identity, output string) error {
	if id == nil || hash([]byte(id.Recipient().String())) != cfg.RecipientSHA256 {
		return ErrBinding
	}
	c, caHash, err := client(cfg) // Validate public configuration and dedicated CA, without dialing.
	if err != nil {
		return err
	}
	c.CloseIdleConnections()
	session := &bootstrapSession{cfg: cfg, caHash: caHash}
	roles, encrypted, err := loadRoles(session, rolesDirectory, id)
	if err != nil {
		return err
	}
	marker, err := readLocal(filepath.Join(rolesDirectory, "BOOTSTRAP_FINALIZED"), true)
	if err != nil || string(marker) != "BOOTSTRAP_FINALIZED_V1\n"+hash(encrypted)+"\n" {
		return ErrBinding
	}
	recoveryRoot := filepath.Dir(rolesDirectory)
	if !filepath.IsAbs(output) || filepath.Clean(output) != output || privateDirectory(filepath.Dir(output)) != nil || nestedPath(output, recoveryRoot) || nestedPath(recoveryRoot, output) {
		return ErrConfig
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return ErrConfig
	}
	if os.Mkdir(output, 0700) != nil || syncDirectory(filepath.Dir(output)) != nil {
		return ErrAgentStage
	}
	manifest := agentDeliveryManifest{Version: 1, InstanceID: cfg.InstanceID, RolesPackageSHA256: hash(encrypted), Files: map[string]string{}}
	for _, role := range roleNames[:3] {
		directory := filepath.Join(output, role)
		if os.Mkdir(directory, 0700) != nil || syncDirectory(output) != nil {
			return ErrAgentStage
		}
		for _, file := range []struct{ name, value string }{{"role-id", roles.Roles[role].RoleID}, {"secret-id", roles.Roles[role].SecretID}} {
			data := []byte(file.value)
			digest := hash(data)
			err := writeNew(filepath.Join(directory, file.name), data)
			clear(data)
			if err != nil {
				return ErrAgentStage
			}
			manifest.Files[role+"/"+file.name] = digest
		}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || writeNew(filepath.Join(output, "AGENT_FILES_STAGED"), encoded) != nil {
		return ErrAgentStage
	}
	return nil
}

func RunStageAgentFiles(cfg Config, custodyDirectory, bundle string, pass []byte, output string) error {
	for _, name := range []string{"identity.age", "recipient.txt", "CUSTODY_READY"} {
		if _, err := readLocal(filepath.Join(custodyDirectory, name), true); err != nil {
			return err
		}
	}
	if filepath.Base(bundle) != "roles.age" || !filepath.IsAbs(bundle) || filepath.Clean(bundle) != bundle {
		return ErrConfig
	}
	if nestedPath(output, filepath.Dir(custodyDirectory)) {
		return ErrConfig
	}
	id, err := custody.Unlock(custodyDirectory, pass)
	if err != nil {
		return ErrBinding
	}
	return StageAgentFiles(cfg, filepath.Dir(bundle), id, output)
}
