package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
)

var (
	ErrRotatePreflight = errors.New("KMS_ROTATION_PREFLIGHT_REJECTED_NO_ROTATE_SENT")
	ErrRotateUncertain = errors.New("KMS_ROTATION_UNCERTAIN_PRESERVE_SAME_ATTEMPT_DO_NOT_ROTATE_AGAIN")
	ErrRotateCleanup   = errors.New("KMS_ROTATION_TOKEN_RETIREMENT_UNCONFIRMED_PRESERVE_ATTEMPT")
)

type rotationAttempt struct {
	Schema    int    `json:"schema"`
	CreatedAt string `json:"created_at"`
	Instance  string `json:"instance_id"`
	Endpoint  string `json:"endpoint"`
	CA        string `json:"ca_sha256"`
	Recipient string `json:"recipient_sha256"`
	Roles     string `json:"roles_sha256"`
	Key       string `json:"key"`
	Previous  int    `json:"previous_version"`
}

func (s *bootstrapSession) rotationVersion(token string) (int, error) {
	var response struct {
		Data struct {
			Type       string `json:"type"`
			Version    int    `json:"latest_version"`
			Exportable *bool  `json:"exportable"`
			Backup     *bool  `json:"allow_plaintext_backup"`
			Deletion   *bool  `json:"deletion_allowed"`
			Derived    *bool  `json:"derived"`
			MinDecrypt *int   `json:"min_decryption_version"`
		} `json:"data"`
	}
	if s.call(keyPath, nil, token, &response, 200) != nil {
		return 0, ErrRotatePreflight
	}
	d := response.Data
	if d.Type != "aes256-gcm96" || d.Version < 1 || d.Version > 2147483647 || d.MinDecrypt == nil || *d.MinDecrypt != 1 {
		return 0, ErrRotatePreflight
	}
	for _, flag := range []*bool{d.Exportable, d.Backup, d.Deletion, d.Derived} {
		if flag == nil || *flag {
			return 0, ErrRotatePreflight
		}
	}
	return d.Version, nil
}

func (s *bootstrapSession) rotationIdentity(token string) (int64, error) {
	var response struct {
		Data struct {
			Policies         []string        `json:"policies"`
			IdentityPolicies json.RawMessage `json:"identity_policies"`
			TTL              int             `json:"ttl"`
			MaxTTL           int             `json:"explicit_max_ttl"`
			Created          int64           `json:"creation_time"`
			Type             string          `json:"type"`
			Path             string          `json:"path"`
			Renewable        bool            `json:"renewable"`
			Orphan           bool            `json:"orphan"`
			Meta             struct {
				Role string `json:"role_name"`
			} `json:"meta"`
		} `json:"data"`
	}
	if s.call("auth/token/lookup-self", nil, token, &response, 200) != nil {
		return 0, ErrRotatePreflight
	}
	d := response.Data
	if !slices.Equal(d.Policies, []string{roleNames[3]}) || !emptySnapshotIdentityPolicies(d.IdentityPolicies) || d.TTL <= 0 || d.TTL > 300 || d.MaxTTL != 900 || d.Created <= 0 || d.Created > 253402299899 || d.Type != "service" || d.Path != "auth/approle/login" || !d.Renewable || !d.Orphan || d.Meta.Role != roleNames[3] {
		return 0, ErrRotatePreflight
	}
	return d.Created + 900, nil
}

// Cleanup does not inherit a cancelled operation context. It only retires this
// invocation's short-lived token, never another token, SecretID or root.
func (s *bootstrapSession) retireRotationToken(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, body, err := authenticatedRequest(ctx, s.client, s.cfg, "auth/token/revoke-self", []byte("{}"), token)
	clear(body)
	if err != nil || status != 204 {
		return ErrRotateCleanup
	}
	status, body, err = authenticatedRequest(ctx, s.client, s.cfg, "auth/token/lookup-self", nil, token)
	clear(body)
	if err != nil || status != 403 {
		return ErrRotateCleanup
	}
	return nil
}

// RotateTransit never reads an initialization package or constructs a root
// session. An existing attempt is query-only even if no change is observed.
func RotateTransit(ctx context.Context, cfg Config, rolesDirectory string, id *age.X25519Identity, output string, expected int) (resultErr error) {
	if id == nil || hash([]byte(id.Recipient().String())) != cfg.RecipientSHA256 || expected < 1 || expected >= 2147483647 || !filepath.IsAbs(output) || filepath.Clean(output) != output || privateDirectory(filepath.Dir(output)) != nil || nestedPath(output, rolesDirectory) || nestedPath(rolesDirectory, output) {
		return ErrRotatePreflight
	}
	c, caHash, err := client(cfg)
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	s := &bootstrapSession{ctx: ctx, client: c, cfg: cfg, caHash: caHash}
	roles, encrypted, err := loadRoles(s, rolesDirectory, id)
	if err != nil {
		return err
	}
	marker, err := readLocal(filepath.Join(rolesDirectory, "BOOTSTRAP_FINALIZED"), true)
	if err != nil || string(marker) != "BOOTSTRAP_FINALIZED_V1\n"+hash(encrypted)+"\n" {
		return ErrBinding
	}

	created := false
	if err := os.Mkdir(output, 0700); err == nil {
		created = true
		if syncDirectory(filepath.Dir(output)) != nil {
			return ErrRotateUncertain
		}
	} else if !os.IsExist(err) {
		return ErrRotatePreflight
	}
	if privateDirectory(output) != nil {
		return ErrRotateUncertain
	}
	lock, err := rotationLock(output)
	if err != nil {
		return err
	}
	defer func() { unlockRotationFile(lock); lock.Close() }()
	serverNow, err := s.rotationServerTime()
	if err != nil {
		return err
	}
	attempt := rotationAttempt{Schema: 1, Instance: cfg.InstanceID, Endpoint: cfg.Endpoint, CA: caHash, Recipient: cfg.RecipientSHA256, Roles: hash(encrypted), Key: keyPath, Previous: expected}
	contextPath := filepath.Join(output, "ROTATION_CONTEXT")
	var record []byte
	if created {
		attempt.CreatedAt = time.Unix(serverNow, 0).UTC().Format(time.RFC3339Nano)
		record, err = json.Marshal(attempt)
		if err != nil || writeNew(contextPath, record) != nil {
			return ErrRotateUncertain
		}
	} else {
		var stored rotationAttempt
		record, err = readRotationJSON(contextPath, &stored)
		attempt.CreatedAt = stored.CreatedAt
		at, parseErr := time.Parse(time.RFC3339Nano, stored.CreatedAt)
		if err != nil || parseErr != nil || stored != attempt || serverNow < at.Unix() || serverNow-at.Unix() >= 86400 {
			return ErrRotateUncertain
		}
	}
	if err := recoverRotationInvocation(output, hash(record), serverNow); err != nil {
		return err
	}
	attemptPath := filepath.Join(output, "ROTATION_ATTEMPT")
	if !created {
		old, err := readLocal(attemptPath, true)
		if err != nil || !bytes.Equal(old, record) {
			return ErrRotateUncertain
		}
	}
	invocation, invocationBytes, err := startRotationInvocation(output, hash(record), serverNow)
	if err != nil {
		return err
	}
	var token string
	retired := false
	retire := func() error {
		if retired {
			return nil
		}
		if token == "" {
			return ErrRotateCleanup
		} // Unknown issuance cannot be assumed harmless.
		if err := s.retireRotationToken(token); err != nil {
			return err
		}
		retired = true
		return archiveRotationInvocation(output, invocation, invocationBytes, "RETIRED")
	}
	defer func() {
		if !retired {
			if err := retire(); err != nil {
				resultErr = err
			}
		}
	}()
	var loginValid bool
	token, loginValid = s.rotationLogin(roles.Roles[roleNames[3]])
	if !loginValid {
		return ErrRotatePreflight
	}
	expiry, err := s.rotationIdentity(token)
	if err != nil {
		return err
	}
	observed, err := s.rotationServerTime()
	issued := expiry - 900
	if err != nil || observed < serverNow || issued < invocation.Started || issued > invocation.Started+60 || issued > observed || observed >= expiry {
		return ErrRotatePreflight
	}
	lease, err := json.Marshal(rotationLease{Schema: 1, Invocation: hash(invocationBytes), Created: issued, Expires: expiry})
	if err != nil || writeNew(filepath.Join(output, "CURRENT_INVOCATION", "LEASE"), lease) != nil {
		return ErrRotateUncertain
	}

	current, err := s.rotationVersion(token)
	if err != nil {
		return err
	}
	if created {
		if current != expected {
			return ErrRotatePreflight
		}
		// No API CAS exists: the custodian must exclude other rotation operators.
		if writeNew(attemptPath, record) != nil {
			return ErrRotateUncertain
		}
		if s.call(keyPath+"/rotate", map[string]any{}, token, nil, 200) != nil {
			return ErrRotateUncertain
		}
		current, err = s.rotationVersion(token)
		if err != nil || current != expected+1 {
			return ErrRotateUncertain
		}
	} else if current != expected+1 {
		return ErrRotateUncertain
	}
	if err := retire(); err != nil {
		return err
	}
	complete := []byte("KMS_ROTATION_VERIFIED_V1\n" + hash(record) + "\n" + strconv.Itoa(current) + "\n")
	completePath := filepath.Join(output, "ROTATION_VERIFIED")
	if old, err := readLocal(completePath, true); err == nil {
		if !bytes.Equal(old, complete) {
			return ErrRotateUncertain
		}
		return nil
	}
	if writeNew(completePath, complete) != nil {
		return ErrRotateUncertain
	}
	return nil
}

func RunRotateTransit(ctx context.Context, cfg Config, directory, bundle string, pass []byte, output string, expected int) error {
	if filepath.Base(bundle) != "roles.age" || !filepath.IsAbs(bundle) || filepath.Clean(bundle) != bundle || nestedPath(output, directory) || nestedPath(directory, output) {
		return ErrConfig
	}
	for _, name := range []string{"identity.age", "recipient.txt", "CUSTODY_READY"} {
		if _, err := readLocal(filepath.Join(directory, name), true); err != nil {
			return err
		}
	}
	id, err := custody.Unlock(directory, pass)
	if err != nil {
		return ErrBinding
	}
	return RotateTransit(ctx, cfg, filepath.Dir(bundle), id, output, expected)
}
