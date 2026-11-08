package operator

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
)

type rotationInvocation struct {
	Schema  int    `json:"schema"`
	ID      string `json:"id"`
	Context string `json:"context_sha256"`
	Started int64  `json:"server_started_at"`
}

type rotationLease struct {
	Schema     int    `json:"schema"`
	Invocation string `json:"invocation_sha256"`
	Created    int64  `json:"server_created_at"`
	Expires    int64  `json:"server_hard_expiry"`
}

func readRotationJSON(file string, out any) ([]byte, error) {
	b, err := readLocal(file, true)
	if err != nil || len(b) > 4096 {
		return nil, ErrRotateUncertain
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrRotateUncertain
	}
	return b, nil
}

func rotationLock(output string) (*os.File, error) {
	filename := filepath.Join(output, "LOCK")
	f, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, ErrRotateUncertain
	}
	valid := false
	defer func() {
		if !valid {
			f.Close()
		}
	}()
	actual, err := filepath.EvalSymlinks(filename)
	before, statErr := os.Lstat(filename)
	opened, openErr := f.Stat()
	if err != nil || actual != filename || statErr != nil || openErr != nil || !before.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != 0 || (runtime.GOOS != "windows" && opened.Mode().Perm()&0077 != 0) {
		return nil, ErrRotateUncertain
	}
	if lockRotationFile(f) != nil {
		return nil, ErrRotateUncertain
	}
	if f.Sync() != nil || syncDirectory(output) != nil {
		unlockRotationFile(f)
		return nil, ErrRotateUncertain
	}
	valid = true
	return f, nil
}

func (s *bootstrapSession) rotationServerTime() (int64, error) {
	var response struct {
		Initialized bool   `json:"initialized"`
		Sealed      *bool  `json:"sealed"`
		Time        *int64 `json:"server_time_utc"`
	}
	// sys/health is a top-level object, unlike authenticated data envelopes.
	if s.call("sys/health", nil, "", &response, 200) != nil || !response.Initialized || response.Sealed == nil || *response.Sealed || response.Time == nil || *response.Time <= 0 || *response.Time > 253402299899 {
		return 0, ErrRotatePreflight
	}
	return *response.Time, nil
}

func archiveRotationInvocation(output string, invocation rotationInvocation, raw []byte, state string) error {
	current := filepath.Join(output, "CURRENT_INVOCATION")
	var live rotationInvocation
	b, err := readRotationJSON(filepath.Join(current, "STARTED"), &live)
	if err != nil || live != invocation || !bytes.Equal(b, raw) {
		return ErrRotateUncertain
	}
	marker := []byte("KMS_INVOCATION_" + state + "_V1\n" + hash(raw) + "\n")
	markerPath := filepath.Join(current, "CLOSED")
	if existing, err := readLocal(markerPath, true); err == nil {
		if !bytes.Equal(existing, marker) {
			return ErrRotateUncertain
		}
	} else if writeNew(markerPath, marker) != nil {
		return ErrRotateUncertain
	}
	destination := filepath.Join(output, "invocation-"+invocation.ID)
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return ErrRotateUncertain
	}
	// Every caller holds the same OS process lock; a crash releases that lock.
	// The invocation ID and closed marker are checked before the atomic rename.
	if os.Rename(current, destination) != nil || syncDirectory(output) != nil {
		return ErrRotateUncertain
	}
	return nil
}

func recoverRotationInvocation(output string, contextHash string, serverNow int64) error {
	current := filepath.Join(output, "CURRENT_INVOCATION")
	if _, err := os.Lstat(current); os.IsNotExist(err) {
		return nil
	}
	if privateDirectory(current) != nil {
		return ErrRotateUncertain
	}
	var invocation rotationInvocation
	raw, err := readRotationJSON(filepath.Join(current, "STARTED"), &invocation)
	if err != nil || invocation.Schema != 1 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(invocation.ID) || invocation.Context != contextHash || invocation.Started <= 0 || invocation.Started > 253402299899 || serverNow < invocation.Started {
		return ErrRotateUncertain
	}
	closedPath := filepath.Join(current, "CLOSED")
	if closed, err := readLocal(closedPath, true); err == nil {
		for _, state := range []string{"RETIRED", "EXPIRED"} {
			if string(closed) == "KMS_INVOCATION_"+state+"_V1\n"+hash(raw)+"\n" {
				return archiveRotationInvocation(output, invocation, raw, state)
			}
		}
		return ErrRotateUncertain
	} else if _, err := os.Lstat(closedPath); !os.IsNotExist(err) {
		return ErrRotateUncertain
	}
	var lease rotationLease
	if _, err := readRotationJSON(filepath.Join(current, "LEASE"), &lease); err != nil {
		return ErrRotateCleanup
	}
	if lease.Schema != 1 || lease.Invocation != hash(raw) || lease.Created < invocation.Started || lease.Created > invocation.Started+60 || lease.Expires != lease.Created+900 || serverNow <= lease.Expires+5 {
		return ErrRotateCleanup
	}
	return archiveRotationInvocation(output, invocation, raw, "EXPIRED")
}

func startRotationInvocation(output, contextHash string, serverNow int64) (rotationInvocation, []byte, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return rotationInvocation{}, nil, ErrRotateUncertain
	}
	invocation := rotationInvocation{Schema: 1, ID: hex.EncodeToString(nonce[:]), Context: contextHash, Started: serverNow}
	raw, err := json.Marshal(invocation)
	current := filepath.Join(output, "CURRENT_INVOCATION")
	if err != nil || os.Mkdir(current, 0700) != nil || syncDirectory(output) != nil || writeNew(filepath.Join(current, "STARTED"), raw) != nil {
		return rotationInvocation{}, nil, ErrRotateUncertain
	}
	return invocation, raw, nil
}

// Return any parseable issued token even when its attributes are invalid, so
// the caller owns background retirement. Lost/unknown issuance remains pending.
func (s *bootstrapSession) rotationLogin(credential roleCredential) (string, bool) {
	body, _ := json.Marshal(map[string]string{"role_id": credential.RoleID, "secret_id": credential.SecretID})
	defer clear(body)
	status, response, err := authenticatedRequest(s.ctx, s.client, s.cfg, "auth/approle/login", body, "")
	defer clear(response)
	// Extract the identity independently: malformed policy/lease attributes
	// must not discard an otherwise known token that still needs retirement.
	var received struct {
		Auth struct {
			Token string `json:"client_token"`
		} `json:"auth"`
	}
	if json.Unmarshal(response, &received) != nil || len(received.Auth.Token) < 20 || len(received.Auth.Token) > 4096 || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(received.Auth.Token) {
		return "", false
	}
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
	if json.Unmarshal(response, &result) != nil {
		return received.Auth.Token, false
	}
	a := result.Auth
	valid := err == nil && status == 200 && slices.Equal(a.Policies, []string{roleNames[3]}) && slices.Equal(a.TokenPolicies, []string{roleNames[3]}) && a.TTL > 0 && a.TTL <= 300 && a.Renewable && a.Orphan && a.Type == "service"
	return a.Token, valid
}
