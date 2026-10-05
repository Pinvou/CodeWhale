package identityadapter

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const PasswordPolicyVersion = "user-v1"

var (
	ErrPasswordInvalid     = errors.New("password does not satisfy the active policy")
	ErrPasswordEdgeSpace   = errors.New("password must not start or end with a space")
	ErrReferenceInvalid    = errors.New("password reference is invalid or expired")
	ErrReferenceConflict   = errors.New("password reference is bound to different registration data")
	ErrCreateNotDispatched = errors.New("registration identity create was fenced before dispatch")
)

// ValidatePrivilegedPassword applies the stronger policy used by bootstrap and
// recovery paths. It intentionally lives inside the Identity boundary so a
// formal administrator password never has to cross Account Service.
func ValidatePrivilegedPassword(password, username string) error {
	if err := ValidatePassword(password); err != nil || len(password) < 14 {
		return ErrPasswordInvalid
	}
	normalizedPassword := strings.ToLower(password)
	normalizedUsername := strings.ToLower(strings.TrimSpace(username))
	if normalizedUsername != "" && strings.Contains(normalizedPassword, normalizedUsername) {
		return ErrPasswordInvalid
	}
	for _, weak := range []string{
		"password", "admin123", "qwerty", "letmein", "welcome", "pinvou",
		"123456", "abc123", "iloveyou", "monkey", "dragon", "sunshine",
		"princess", "football", "baseball", "trustno1", "passw0rd",
	} {
		if strings.Contains(normalizedPassword, weak) {
			return ErrPasswordInvalid
		}
	}
	return nil
}

type UserDirectory interface {
	CreateMobileUser(context.Context, uuid.UUID, string, string, string, bool) (RegistrationIdentity, error)
	FindMobileUserByRegistrationOperation(context.Context, uuid.UUID, bool) (RegistrationIdentity, bool, error)
	DisableUserAndConfirm(context.Context, string) error
	DeleteUser(context.Context, string) error
	UpdateUsername(context.Context, string, uuid.UUID, string) error
	UpdateMobile(context.Context, string, uuid.UUID, string) error
	SetPassword(context.Context, string, string) error
	RevokeSessions(context.Context, string) error
}

// RegistrationIdentity binds recovery to the exact server-owned intent and
// immutable registration facts. Recovery never changes an existing password.
type RegistrationIdentity struct {
	OperationID       uuid.UUID `json:"registration_operation_id"`
	Subject           string    `json:"subject"`
	Username          string    `json:"username"`
	Mobile            string    `json:"mobile"`
	PasswordPreserved bool      `json:"password_preserved"`
}

func (identity RegistrationIdentity) matches(operationID uuid.UUID, username, mobile string) bool {
	return operationID != uuid.Nil && identity.OperationID == operationID && strings.TrimSpace(identity.Subject) != "" && len(identity.Subject) <= 128 && identity.Username == username && identity.Mobile == mobile
}

func (v *PasswordVault) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, username string) error {
	return v.directory.UpdateUsername(ctx, subject, operationID, username)
}

func (v *PasswordVault) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, mobile string) error {
	return v.directory.UpdateMobile(ctx, subject, operationID, mobile)
}

type PasswordReference struct {
	Handle        string    `json:"password_handle"`
	PolicyVersion string    `json:"password_policy_version"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type passwordRecord struct {
	mu          sync.Mutex
	password    string
	fingerprint [32]byte
	expiresAt   time.Time
	username    string
	mobile      string
	subject     string
	operationID uuid.UUID
}

type operationPasswordBinding struct {
	fingerprint [32]byte
	expiresAt   time.Time
	username    string
	mobile      string
	handle      string
	dispatched  bool
	cancelled   bool
	reset       *passwordResetReceipt
}

type PasswordVault struct {
	mu           sync.Mutex
	operationsMu sync.Mutex
	records      map[string]*passwordRecord
	operations   map[uuid.UUID]operationPasswordBinding
	hmacKey      []byte
	directory    UserDirectory
	now          func() time.Time
	ttl          time.Duration
}

func NewPasswordVault(hmacKey []byte, directory UserDirectory) (*PasswordVault, error) {
	if len(hmacKey) < 32 {
		return nil, errors.New("password reference HMAC key must contain at least 32 bytes")
	}
	if directory == nil {
		return nil, errors.New("identity user directory is required")
	}
	return &PasswordVault{
		records: make(map[string]*passwordRecord), operations: make(map[uuid.UUID]operationPasswordBinding), hmacKey: append([]byte(nil), hmacKey...),
		directory: directory, now: func() time.Time { return time.Now().UTC() }, ttl: 15 * time.Minute,
	}, nil
}

func ValidatePassword(password string) error {
	if strings.HasPrefix(password, " ") || strings.HasSuffix(password, " ") {
		return ErrPasswordEdgeSpace
	}
	if len(password) < 8 || len(password) > 128 {
		return ErrPasswordInvalid
	}
	var letter, digit bool
	for i := 0; i < len(password); i++ {
		value := password[i]
		if value < 0x20 || value > 0x7e {
			return ErrPasswordInvalid
		}
		letter = letter || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
		digit = digit || value >= '0' && value <= '9'
	}
	if !letter || !digit {
		return ErrPasswordInvalid
	}
	return nil
}

func (v *PasswordVault) Precheck(password string) (PasswordReference, error) {
	if err := ValidatePassword(password); err != nil {
		return PasswordReference{}, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return PasswordReference{}, err
	}
	handle := base64.RawURLEncoding.EncodeToString(random)
	fingerprint := v.fingerprint(password)
	expiresAt := v.now().Add(v.ttl)

	v.mu.Lock()
	defer v.mu.Unlock()
	v.removeExpiredLocked()
	v.records[handle] = &passwordRecord{password: password, fingerprint: fingerprint, expiresAt: expiresAt}
	return PasswordReference{Handle: handle, PolicyVersion: PasswordPolicyVersion, ExpiresAt: expiresAt}, nil
}

func (v *PasswordVault) ValidateReference(operationID uuid.UUID, handle, policyVersion, username, mobile string) error {
	record, err := v.record(handle, policyVersion)
	if err != nil {
		return err
	}
	record.mu.Lock()
	if err := bindRecord(record, username, mobile); err != nil {
		record.mu.Unlock()
		return err
	}
	binding := operationPasswordBinding{fingerprint: record.fingerprint, expiresAt: v.now().Add(48 * time.Hour), username: username, mobile: mobile, handle: handle}
	record.mu.Unlock()
	if operationID == uuid.Nil {
		return nil
	}
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	if existing, ok := v.operations[operationID]; ok {
		if existing.username != username || existing.mobile != mobile || !hmac.Equal(existing.fingerprint[:], binding.fingerprint[:]) {
			return ErrReferenceConflict
		}
		if existing.cancelled {
			return ErrReferenceInvalid
		}
		return nil
	}
	v.operations[operationID] = binding
	return nil
}

func (v *PasswordVault) CreateMobileUser(ctx context.Context, operationID uuid.UUID, handle, policyVersion, username, mobile string, legacyIntent, firstCreateAttempt bool) (RegistrationIdentity, error) {
	if operationID == uuid.Nil {
		return RegistrationIdentity{}, ErrReferenceInvalid
	}
	if err := v.registrationFence(operationID, username, mobile, legacyIntent, firstCreateAttempt); err != nil {
		return RegistrationIdentity{}, err
	}
	if identity, found, err := v.directory.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent); err != nil || found {
		if err != nil {
			return RegistrationIdentity{}, err
		}
		if !identity.matches(operationID, username, mobile) {
			return RegistrationIdentity{}, ErrReferenceConflict
		}
		identity.PasswordPreserved = true
		return identity, nil
	}
	if handle == "" {
		v.operationsMu.Lock()
		v.removeExpiredOperationsLocked()
		handle = v.operations[operationID].handle
		v.operationsMu.Unlock()
	}
	if err := v.ValidateReference(operationID, handle, policyVersion, username, mobile); err != nil {
		return RegistrationIdentity{}, v.registrationReferenceError(operationID, handle, policyVersion, username, mobile, legacyIntent, firstCreateAttempt, err)
	}
	record, err := v.record(handle, policyVersion)
	if err != nil {
		return RegistrationIdentity{}, v.registrationReferenceError(operationID, handle, policyVersion, username, mobile, legacyIntent, firstCreateAttempt, err)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if err := bindRecord(record, username, mobile); err != nil {
		return RegistrationIdentity{}, err
	}
	if record.operationID != uuid.Nil && record.operationID != operationID {
		return RegistrationIdentity{}, ErrReferenceConflict
	}
	record.operationID = operationID
	if record.subject != "" {
		return RegistrationIdentity{OperationID: operationID, Subject: record.subject, Username: username, Mobile: mobile, PasswordPreserved: true}, nil
	}
	if record.password == "" || v.now().After(record.expiresAt) {
		return RegistrationIdentity{}, v.registrationReferenceError(operationID, handle, policyVersion, username, mobile, legacyIntent, firstCreateAttempt, ErrReferenceInvalid)
	}
	currentFingerprint := v.fingerprint(record.password)
	if !hmac.Equal(record.fingerprint[:], currentFingerprint[:]) {
		return RegistrationIdentity{}, ErrReferenceConflict
	}
	if err := ValidatePassword(record.password); err != nil {
		return RegistrationIdentity{}, err
	}
	if err := v.markRegistrationDispatch(operationID, username, mobile); err != nil {
		return RegistrationIdentity{}, err
	}
	identity, err := v.directory.CreateMobileUser(ctx, operationID, username, mobile, record.password, legacyIntent)
	if err != nil {
		return RegistrationIdentity{}, err
	}
	if !identity.matches(operationID, username, mobile) {
		return RegistrationIdentity{}, ErrReferenceConflict
	}
	record.subject = identity.Subject
	record.password = ""
	return identity, nil
}

func (v *PasswordVault) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID, legacyIntent bool) (RegistrationIdentity, bool, error) {
	if operationID == uuid.Nil {
		return RegistrationIdentity{}, false, ErrReferenceInvalid
	}
	return v.directory.FindMobileUserByRegistrationOperation(ctx, operationID, legacyIntent)
}

func (v *PasswordVault) DeleteUser(ctx context.Context, subject string) error {
	return v.directory.DeleteUser(ctx, subject)
}

func (v *PasswordVault) DisableUserAndConfirm(ctx context.Context, subject string) error {
	if strings.TrimSpace(subject) == "" {
		return ErrReferenceInvalid
	}
	return v.directory.DisableUserAndConfirm(ctx, subject)
}

func (v *PasswordVault) record(handle, policyVersion string) (*passwordRecord, error) {
	if policyVersion != PasswordPolicyVersion || len(handle) != 43 {
		return nil, ErrReferenceInvalid
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.removeExpiredLocked()
	record := v.records[handle]
	if record == nil {
		return nil, ErrReferenceInvalid
	}
	return record, nil
}

func (v *PasswordVault) removeExpiredLocked() {
	now := v.now()
	for handle, record := range v.records {
		if now.After(record.expiresAt) {
			record.mu.Lock()
			record.password = ""
			record.fingerprint = [32]byte{}
			record.mu.Unlock()
			delete(v.records, handle)
		}
	}
}

func (v *PasswordVault) removeExpiredOperationsLocked() {
	now := v.now()
	for operationID, binding := range v.operations {
		if now.After(binding.expiresAt) {
			delete(v.operations, operationID)
		}
	}
}

func (v *PasswordVault) registrationFence(operationID uuid.UUID, username, mobile string, legacyIntent, firstCreateAttempt bool) error {
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	if binding, ok := v.operations[operationID]; ok {
		if binding.username != username || binding.mobile != mobile {
			return ErrReferenceConflict
		}
		if binding.cancelled {
			if firstCreateAttempt && !legacyIntent {
				return ErrCreateNotDispatched
			}
			return ErrReferenceInvalid
		}
	}
	return nil
}

// A missing identity lookup is not proof of absence. The trusted Account caller
// must attest the durable first-create CAS winner. A retained local binding can
// then prove only that this first attempt was fenced before dispatch; Account
// must still compare the exact durable attempt version before terminating the
// intent, so a later attempt cannot be hidden by this result. Dispatched, legacy,
// retry or lost process state cannot provide this termination candidate.
func (v *PasswordVault) registrationReferenceError(operationID uuid.UUID, handle, policyVersion, username, mobile string, legacyIntent, firstCreateAttempt bool, cause error) error {
	// An A06 legacy intent may have dispatched through the old provisioner. Its
	// absence from this process cannot prove that no remote creation was issued.
	if legacyIntent || !firstCreateAttempt || !errors.Is(cause, ErrReferenceInvalid) || policyVersion != PasswordPolicyVersion {
		return cause
	}
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	binding, ok := v.operations[operationID]
	if !ok || binding.dispatched || binding.username != username || binding.mobile != mobile || binding.handle != handle {
		return cause
	}
	binding.cancelled = true
	v.operations[operationID] = binding
	return ErrCreateNotDispatched
}

func (v *PasswordVault) markRegistrationDispatch(operationID uuid.UUID, username, mobile string) error {
	v.operationsMu.Lock()
	defer v.operationsMu.Unlock()
	v.removeExpiredOperationsLocked()
	binding, ok := v.operations[operationID]
	if !ok {
		return ErrReferenceInvalid
	}
	if binding.username != username || binding.mobile != mobile {
		return ErrReferenceConflict
	}
	if binding.cancelled {
		return ErrReferenceInvalid
	}
	// Mark before entering the remote adapter, even if the call later times out.
	binding.dispatched = true
	v.operations[operationID] = binding
	return nil
}

func (v *PasswordVault) fingerprint(password string) [32]byte {
	mac := hmac.New(sha256.New, v.hmacKey)
	_, _ = mac.Write([]byte(password))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func bindRecord(record *passwordRecord, username, mobile string) error {
	if username == "" || mobile == "" {
		return ErrReferenceInvalid
	}
	if record.username == "" && record.mobile == "" {
		record.username, record.mobile = username, mobile
		return nil
	}
	if record.username != username || record.mobile != mobile {
		return ErrReferenceConflict
	}
	return nil
}
