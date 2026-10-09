package operator

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"slices"
)

func (s *bootstrapSession) login(role string, credential roleCredential) (string, error) {
	var result struct {
		Auth struct {
			Token         string   `json:"client_token"`
			Policies      []string `json:"policies"`
			TokenPolicies []string `json:"token_policies"`
			TTL           int      `json:"lease_duration"`
			Renewable     bool     `json:"renewable"`
			Orphan        bool     `json:"orphan"`
			Type          string   `json:"token_type"`
		} `json:"auth"`
	}
	err := s.call("auth/approle/login", map[string]string{"role_id": credential.RoleID, "secret_id": credential.SecretID}, "", &result, 200)
	auth := result.Auth
	if err != nil || len(auth.Token) < 20 || !slices.Equal(auth.Policies, []string{role}) || !slices.Equal(auth.TokenPolicies, []string{role}) || auth.TTL <= 0 || auth.TTL > 300 || !auth.Renewable || !auth.Orphan || auth.Type != "service" {
		if auth.Token != "" {
			_ = s.call("auth/token/revoke-self", map[string]any{}, auth.Token, nil, 204)
		}
		return "", ErrBootstrapVerify
	}
	return auth.Token, nil
}

func (s *bootstrapSession) verifyRuntime(token string) error {
	plaintext := make([]byte, 32)
	if _, err := rand.Read(plaintext); err != nil {
		return ErrBootstrapVerify
	}
	defer clear(plaintext)
	aad := base64.StdEncoding.EncodeToString([]byte("pinvou-kms-bootstrap-identity-check-v1"))
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	var encrypted struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if s.call("transit/encrypt/pinvou-token-data", map[string]string{"plaintext": encoded, "associated_data": aad}, token, &encrypted, 200) != nil || len(encrypted.Data.Ciphertext) < 16 {
		return ErrBootstrapVerify
	}
	var decrypted struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if s.call("transit/decrypt/pinvou-token-data", map[string]string{"ciphertext": encrypted.Data.Ciphertext, "associated_data": aad}, token, &decrypted, 200) != nil || decrypted.Data.Plaintext != encoded {
		return ErrBootstrapVerify
	}
	if s.call("transit/encrypt/pinvou-token-data", map[string]string{"plaintext": encoded}, token, nil, 403) != nil ||
		s.call("transit/decrypt/pinvou-token-data", map[string]string{"ciphertext": encrypted.Data.Ciphertext, "associated_data": base64.StdEncoding.EncodeToString([]byte("wrong-aad"))}, token, nil, 400) != nil {
		return ErrBootstrapVerify
	}
	return nil
}

// This bounded read is a permission probe, NOT a retained recovery backup.
func (s *bootstrapSession) verifySnapshot(token string) error {
	req, err := http.NewRequestWithContext(s.ctx, "GET", s.cfg.Endpoint+"/v1/sys/storage/raft/snapshot", nil)
	if err != nil {
		return ErrBootstrapVerify
	}
	req.Header.Set("X-Vault-Token", token)
	r, err := s.client.Do(req)
	if err != nil {
		return ErrBootstrapVerify
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
	defer clear(b)
	if err != nil || r.StatusCode != 200 || len(b) < 64 || len(b) > 16<<20 || !bytes.Equal(b[:2], []byte{0x1f, 0x8b}) {
		return ErrBootstrapVerify
	}
	return nil
}

func (s *bootstrapSession) verifyOne(role, token string) error {
	if s.call("auth/token/lookup-self", nil, token, nil, 200) != nil {
		return ErrBootstrapVerify
	}
	for _, path := range []string{"sys/auth", "sys/policies/acl/" + role, "transit/export/encryption-key/pinvou-token-data"} {
		if s.call(path, nil, token, nil, 403) != nil {
			return ErrBootstrapVerify
		}
	}
	switch role {
	case roleNames[0], roleNames[1]:
		if s.verifyRuntime(token) != nil || s.call(keyPath, nil, token, nil, 403) != nil || s.call("sys/storage/raft/snapshot", nil, token, nil, 403) != nil {
			return ErrBootstrapVerify
		}
	case roleNames[2]:
		if s.verifySnapshot(token) != nil || s.call(keyPath, nil, token, nil, 403) != nil || s.call("transit/encrypt/pinvou-token-data", map[string]string{}, token, nil, 403) != nil {
			return ErrBootstrapVerify
		}
	case roleNames[3]:
		if s.call(keyPath, nil, token, nil, 200) != nil || s.call("sys/storage/raft/snapshot", nil, token, nil, 403) != nil || s.call("transit/encrypt/pinvou-token-data", map[string]string{}, token, nil, 403) != nil {
			return ErrBootstrapVerify
		}
	default:
		return ErrBootstrapVerify
	}
	return nil
}

func (s *bootstrapSession) verifyIdentities(e bootstrapEnvelope) error {
	for _, role := range roleNames {
		token, err := s.login(role, e.Roles[role])
		if err != nil {
			return ErrBootstrapVerify
		}
		verifyErr := s.verifyOne(role, token)
		revokeErr := s.call("auth/token/revoke-self", map[string]any{}, token, nil, 204)
		if verifyErr != nil || revokeErr != nil || s.call("auth/token/lookup-self", nil, token, nil, 403) != nil {
			return ErrBootstrapVerify
		}
	}
	return nil
}

// Check denied mutations by capability lookup, never by executing them.
func (s *bootstrapSession) verifyCapabilities(role, token string) error {
	paths := []string{"sys/mounts/transit", "sys/auth/approle", "sys/policies/acl/" + role,
		"auth/token/create", "auth/token/revoke-accessor", keyPath + "/config", "sys/storage/raft/snapshot-force"}
	if role != roleNames[3] {
		paths = append(paths, keyPath+"/rotate", "auth/approle/role/"+role+"/secret-id")
	}
	var result struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if s.call("sys/capabilities", map[string]any{"token": token, "paths": paths}, s.root, &result, 200) != nil {
		return ErrBootstrapVerify
	}
	for _, path := range paths {
		var caps []string
		if json.Unmarshal(result.Data[path], &caps) != nil || !slices.Equal(caps, []string{"deny"}) {
			return ErrBootstrapVerify
		}
	}
	return nil
}

func (s *bootstrapSession) checkAllCapabilities(e bootstrapEnvelope) error {
	for _, role := range roleNames {
		token, err := s.login(role, e.Roles[role])
		if err != nil {
			return ErrBootstrapVerify
		}
		checkErr := s.verifyCapabilities(role, token)
		revokeErr := s.call("auth/token/revoke-self", map[string]any{}, token, nil, 204)
		if checkErr != nil || revokeErr != nil {
			return ErrBootstrapVerify
		}
	}
	return nil
}
