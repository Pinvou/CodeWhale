package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

var (
	ErrBootstrapPreflight = errors.New("BOOTSTRAP_PREFLIGHT_REJECTED_NO_CONFIGURATION_WRITTEN")
	ErrBootstrapPartial   = errors.New("BOOTSTRAP_PARTIAL_OR_UNCERTAIN_PRESERVE_OUTPUT_AND_ROOT_DO_NOT_REPEAT")
	ErrBootstrapVerify    = errors.New("BOOTSTRAP_IDENTITY_OR_POLICY_VERIFICATION_FAILED_ROOT_NOT_REVOKED")
	ErrBootstrapFinalize  = errors.New("BOOTSTRAP_FINALIZE_UNCERTAIN_RECHECK_SAME_PACKAGE_DO_NOT_BOOTSTRAP_AGAIN")
)

type roleCredential struct {
	RoleID   string `json:"role_id"`
	SecretID string `json:"secret_id"`
	Accessor string `json:"secret_id_accessor"`
}

type bootstrapEnvelope struct {
	Version         int                       `json:"version"`
	InstanceID      string                    `json:"instance_id"`
	Endpoint        string                    `json:"endpoint"`
	CASHA256        string                    `json:"ca_sha256"`
	RecipientSHA256 string                    `json:"recipient_sha256"`
	CreatedAt       string                    `json:"created_at"`
	Roles           map[string]roleCredential `json:"roles"`
}

type bootstrapSession struct {
	ctx    context.Context
	client *http.Client
	cfg    Config
	caHash string
	root   string
}

func newBootstrapSession(ctx context.Context, cfg Config, encrypted []byte, id *age.X25519Identity) (*bootstrapSession, error) {
	c, caHash, err := client(cfg)
	if err != nil {
		return nil, err
	}
	response, err := initResponse(cfg, caHash, encrypted, id)
	if err != nil {
		c.CloseIdleConnections()
		return nil, err
	}
	defer clear(response)
	var material struct {
		Root string `json:"root_token"`
	}
	if json.Unmarshal(response, &material) != nil || !regexp.MustCompile(`^[A-Za-z0-9._-]{20,256}$`).MatchString(material.Root) {
		c.CloseIdleConnections()
		return nil, ErrBinding
	}
	return &bootstrapSession{ctx: ctx, client: c, cfg: cfg, caHash: caHash, root: material.Root}, nil
}

func (s *bootstrapSession) call(path string, body any, token string, out any, want int) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return ErrBootstrapPartial
		}
		defer clear(b)
	}
	status, response, err := authenticatedRequest(s.ctx, s.client, s.cfg, path, b, token)
	defer clear(response)
	if err != nil || status != want {
		stage := strings.ToUpper(strings.NewReplacer("/", "_", "-", "_").Replace(path))
		return fmt.Errorf("%w: BOOTSTRAP_API_%s_HTTP_%d", ErrBootstrapPartial, stage, status)
	}
	if out != nil && json.Unmarshal(response, out) != nil {
		return ErrBootstrapPartial
	}
	return nil
}

func (s *bootstrapSession) fresh() error {
	sealed, err := state(s.ctx, s.client, s.cfg)
	if err != nil || sealed {
		return ErrBootstrapPreflight
	}
	var lookup struct {
		Data struct {
			Policies []string `json:"policies"`
		} `json:"data"`
	}
	if s.call("auth/token/lookup-self", nil, s.root, &lookup, 200) != nil || !slices.Equal(lookup.Data.Policies, []string{"root"}) {
		return ErrBootstrapPreflight
	}
	for _, mount := range []struct{ path, name string }{{"sys/mounts", "transit/"}, {"sys/auth", "approle/"}} {
		var result struct {
			Data map[string]any `json:"data"`
		}
		if s.call(mount.path, nil, s.root, &result, 200) != nil || result.Data == nil {
			return ErrBootstrapPreflight
		}
		if _, exists := result.Data[mount.name]; exists {
			return ErrBootstrapPreflight
		}
	}
	for _, role := range roleNames {
		if s.call("sys/policies/acl/"+role, nil, s.root, nil, 404) != nil {
			return ErrBootstrapPreflight
		}
	}
	return nil
}

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrConfig
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil || actual != path {
		return ErrConfig
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return ErrConfig
	}
	return nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	} // Windows ACL verified by deployment; file FlushFileBuffers is still required.
	f, err := os.Open(path)
	if err != nil {
		return ErrBootstrapPartial
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrBootstrapPartial
	}
	return nil
}

func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrBootstrapPartial
	}
	n, e := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if e != nil || n != len(b) || syncErr != nil || closeErr != nil || syncDirectory(filepath.Dir(path)) != nil {
		return ErrBootstrapPartial
	}
	stored, err := readLocal(path, true)
	defer clear(stored)
	if err != nil || !bytes.Equal(stored, b) {
		return ErrBootstrapPartial
	}
	return nil
}

func persistRoles(path string, e bootstrapEnvelope, id *age.X25519Identity) error {
	plain, err := json.Marshal(e)
	if err != nil {
		return ErrBootstrapPartial
	}
	defer clear(plain)
	encrypted, err := custody.Encrypt(plain, id.Recipient())
	if err != nil || writeNew(path, encrypted) != nil {
		return ErrBootstrapPartial
	}
	stored, err := readLocal(path, true)
	if err != nil {
		return ErrBootstrapPartial
	}
	recovered, err := custody.Decrypt(stored, id)
	defer clear(recovered)
	if err != nil || !bytes.Equal(recovered, plain) {
		return ErrBootstrapPartial
	}
	return nil
}

// PrepareBootstrap is deliberately one-shot. A partial attempt preserves every
// encrypted response and does not revoke root or retry any configuration write.
func PrepareBootstrap(ctx context.Context, cfg Config, encrypted []byte, id *age.X25519Identity, output string) error {
	s, err := newBootstrapSession(ctx, cfg, encrypted, id)
	if err != nil {
		return err
	}
	defer s.client.CloseIdleConnections()
	if !filepath.IsAbs(output) || filepath.Clean(output) != output || privateDirectory(filepath.Dir(output)) != nil {
		return ErrConfig
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return ErrBootstrapPreflight
	}
	if s.fresh() != nil {
		return ErrBootstrapPreflight
	}
	if os.Mkdir(output, 0700) != nil || syncDirectory(filepath.Dir(output)) != nil {
		return ErrBootstrapPartial
	}
	if writeNew(filepath.Join(output, "BOOTSTRAP_ATTEMPTED"), []byte("BOOTSTRAP_ATTEMPTED_V1\n")) != nil {
		return ErrBootstrapPartial
	}
	for _, step := range []struct {
		path   string
		body   map[string]any
		status int
	}{
		{"sys/mounts/transit", map[string]any{"type": "transit"}, 204},
		{"sys/auth/approle", map[string]any{"type": "approle"}, 204},
		{keyPath, map[string]any{"type": "aes256-gcm96", "exportable": false, "allow_plaintext_backup": false, "derived": false}, 200},
	} {
		if err := s.call(step.path, step.body, s.root, nil, step.status); err != nil {
			return err
		}
	}
	envelope := bootstrapEnvelope{Version: 1, InstanceID: cfg.InstanceID, Endpoint: cfg.Endpoint, CASHA256: s.caHash,
		RecipientSHA256: cfg.RecipientSHA256, CreatedAt: time.Now().UTC().Format(time.RFC3339), Roles: map[string]roleCredential{}}
	for _, role := range roleNames {
		if err := s.call("sys/policies/acl/"+role, map[string]any{"policy": rolePolicy(role), "cas": -1, "cas_required": true}, s.root, nil, 204); err != nil {
			return err
		}
		if err := s.call("auth/approle/role/"+role, roleSettings(role), s.root, nil, 204); err != nil {
			return err
		}
		var roleID struct {
			Data struct {
				ID string `json:"role_id"`
			} `json:"data"`
		}
		if s.call("auth/approle/role/"+role+"/role-id", nil, s.root, &roleID, 200) != nil {
			return ErrBootstrapPartial
		}
		// Save every nonempty one-time response even when the HTTP outcome or JSON
		// is uncertain; the aggregate success package is never written on failure.
		status, response, e := authenticatedRequest(ctx, s.client, cfg, "auth/approle/role/"+role+"/secret-id", []byte(`{}`), s.root)
		if len(response) > 0 {
			capture, encErr := custody.Encrypt(response, id.Recipient())
			if encErr != nil || writeNew(filepath.Join(output, role+"-response.age"), capture) != nil {
				clear(response)
				return ErrBootstrapPartial
			}
		}
		var secret struct {
			Data roleCredential `json:"data"`
		}
		parseErr := json.Unmarshal(response, &secret)
		clear(response)
		if e != nil || status != 200 || parseErr != nil {
			return ErrBootstrapPartial
		}
		secret.Data.RoleID = roleID.Data.ID
		if !validCredential(secret.Data) {
			return ErrBootstrapPartial
		}
		envelope.Roles[role] = secret.Data
	}
	if persistRoles(filepath.Join(output, "roles.age"), envelope, id) != nil {
		return ErrBootstrapPartial
	}
	if s.verifyConfiguration() != nil || s.verifyIdentities(envelope) != nil || s.checkAllCapabilities(envelope) != nil {
		return ErrBootstrapVerify
	}
	return writeNew(filepath.Join(output, "BOOTSTRAP_PREPARED"), []byte("BOOTSTRAP_PREPARED_V1\n"))
}

func validCredential(c roleCredential) bool {
	uuid := regexp.MustCompile(`^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
	return uuid.MatchString(c.RoleID) && uuid.MatchString(c.SecretID) && uuid.MatchString(c.Accessor)
}

// RunBootstrap only accepts a password from the separate real-terminal CLI.
// Neither the initial root nor role secrets are returned to its caller.
func RunBootstrap(ctx context.Context, cfg Config, dir, bundle string, pass []byte, output string, finalize bool) error {
	for _, name := range []string{"identity.age", "recipient.txt", "CUSTODY_READY"} {
		if _, err := readLocal(filepath.Join(dir, name), true); err != nil {
			return err
		}
	}
	b, err := readLocal(bundle, true)
	if err != nil {
		return err
	}
	id, err := custody.Unlock(dir, pass)
	if err != nil {
		return ErrBinding
	}
	if finalize {
		return FinalizeBootstrap(ctx, cfg, b, id, output)
	}
	return PrepareBootstrap(ctx, cfg, b, id, output)
}

func loadRoles(s *bootstrapSession, output string, id *age.X25519Identity) (bootstrapEnvelope, []byte, error) {
	var e bootstrapEnvelope
	if privateDirectory(output) != nil {
		return e, nil, ErrConfig
	}
	b, err := readLocal(filepath.Join(output, "roles.age"), true)
	if err != nil {
		return e, nil, err
	}
	plain, err := custody.Decrypt(b, id)
	if err != nil {
		return e, nil, ErrBinding
	}
	defer clear(plain)
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil || d.Decode(new(any)) != io.EOF {
		return e, nil, ErrBinding
	}
	at, err := time.Parse(time.RFC3339, e.CreatedAt)
	if err != nil || at.After(time.Now().Add(5*time.Minute)) || e.Version != 1 || e.InstanceID != s.cfg.InstanceID || e.Endpoint != s.cfg.Endpoint || e.CASHA256 != s.caHash || e.RecipientSHA256 != s.cfg.RecipientSHA256 || len(e.Roles) != len(roleNames) {
		return e, nil, ErrBinding
	}
	seen := map[string]bool{}
	for _, role := range roleNames {
		c := e.Roles[role]
		if !validCredential(c) || c.RoleID == c.SecretID || c.RoleID == c.Accessor || c.SecretID == c.Accessor || seen[c.RoleID] || seen[c.SecretID] || seen[c.Accessor] {
			return e, nil, ErrBinding
		}
		seen[c.RoleID], seen[c.SecretID], seen[c.Accessor] = true, true, true
	}
	return e, b, nil
}

// FinalizeBootstrap can be resumed with the SAME packages after an uncertain
// revoke response. It never reconfigures mounts, keys, policies or roles.
func FinalizeBootstrap(ctx context.Context, cfg Config, encrypted []byte, id *age.X25519Identity, output string) error {
	s, err := newBootstrapSession(ctx, cfg, encrypted, id)
	if err != nil {
		return err
	}
	defer s.client.CloseIdleConnections()
	envelope, b, err := loadRoles(s, output, id)
	if err != nil {
		return err
	}
	prepared, err := readLocal(filepath.Join(output, "BOOTSTRAP_PREPARED"), true)
	preparedExists := err == nil
	if preparedExists && string(prepared) != "BOOTSTRAP_PREPARED_V1\n" {
		return ErrBootstrapVerify
	}
	if !preparedExists {
		if _, statErr := os.Lstat(filepath.Join(output, "BOOTSTRAP_PREPARED")); !os.IsNotExist(statErr) {
			return ErrBootstrapVerify
		}
	}
	sealed, err := state(ctx, s.client, cfg)
	if err != nil || sealed {
		return ErrBootstrapVerify
	}
	status, response, err := authenticatedRequest(ctx, s.client, cfg, "auth/token/lookup-self", nil, s.root)
	clear(response)
	if err != nil || (status != 200 && status != 403) {
		return ErrBootstrapVerify
	}
	if status == 403 && !preparedExists {
		return ErrBootstrapVerify
	}
	if status == 200 && (s.verifyConfiguration() != nil || s.checkAllCapabilities(envelope) != nil) {
		return ErrBootstrapVerify
	}
	if s.verifyIdentities(envelope) != nil {
		return ErrBootstrapVerify
	}
	// A complete, authenticated roles package may survive a transient prepare
	// probe failure. Re-verify it without recreating any identity or mount.
	if !preparedExists && writeNew(filepath.Join(output, "BOOTSTRAP_PREPARED"), []byte("BOOTSTRAP_PREPARED_V1\n")) != nil {
		return ErrBootstrapVerify
	}
	if status == 200 {
		if s.call("auth/token/revoke-self", map[string]any{}, s.root, nil, 204) != nil {
			return ErrBootstrapFinalize
		}
	}
	if s.call("auth/token/lookup-self", nil, s.root, nil, 403) != nil || s.verifyIdentities(envelope) != nil {
		return ErrBootstrapFinalize
	}
	marker := []byte("BOOTSTRAP_FINALIZED_V1\n" + hash(b) + "\n")
	path := filepath.Join(output, "BOOTSTRAP_FINALIZED")
	if existing, err := readLocal(path, true); err == nil {
		if !bytes.Equal(existing, marker) {
			return ErrBootstrapFinalize
		}
		return nil
	}
	if writeNew(path, marker) != nil {
		return ErrBootstrapFinalize
	}
	return nil
}

func (s *bootstrapSession) verifyConfiguration() error {
	var key struct {
		Data struct {
			Type       string `json:"type"`
			Exportable *bool  `json:"exportable"`
			Backup     *bool  `json:"allow_plaintext_backup"`
			Delete     *bool  `json:"deletion_allowed"`
			Derived    *bool  `json:"derived"`
			Version    int    `json:"latest_version"`
		} `json:"data"`
	}
	if s.call(keyPath, nil, s.root, &key, 200) != nil || key.Data.Type != "aes256-gcm96" || key.Data.Exportable == nil || *key.Data.Exportable || key.Data.Backup == nil || *key.Data.Backup || key.Data.Delete == nil || *key.Data.Delete || key.Data.Derived == nil || *key.Data.Derived || key.Data.Version < 1 {
		return ErrBootstrapVerify
	}
	for _, role := range roleNames {
		var policy struct {
			Data struct {
				Policy string `json:"policy"`
			} `json:"data"`
		}
		if s.call("sys/policies/acl/"+role, nil, s.root, &policy, 200) != nil || strings.ReplaceAll(policy.Data.Policy, "\r\n", "\n") != strings.ReplaceAll(rolePolicy(role), "\r\n", "\n") {
			return ErrBootstrapVerify
		}
		var settings struct {
			Data map[string]any `json:"data"`
		}
		if s.call("auth/approle/role/"+role, nil, s.root, &settings, 200) != nil {
			return ErrBootstrapVerify
		}
		for field, expected := range roleSettings(role) {
			actualJSON, _ := json.Marshal(settings.Data[field])
			expectedJSON, _ := json.Marshal(expected)
			if !bytes.Equal(actualJSON, expectedJSON) {
				return ErrBootstrapVerify
			}
		}
	}
	return nil
}
