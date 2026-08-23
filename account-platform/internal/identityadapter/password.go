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
	ErrPasswordInvalid   = errors.New("password does not satisfy the active policy")
	ErrPasswordEdgeSpace = errors.New("password must not start or end with a space")
	ErrReferenceInvalid  = errors.New("password reference is invalid or expired")
	ErrReferenceConflict = errors.New("password reference is bound to different registration data")
)

type UserDirectory interface {
	CreateMobileUser(context.Context, uuid.UUID, string, string, string) (string, error)
	FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error)
	DisableUserAndConfirm(context.Context, string) error
	DeleteUser(context.Context, string) error
	UpdateUsername(context.Context, string, uuid.UUID, string) error
	UpdateMobile(context.Context, string, uuid.UUID, string) error
	SetPassword(context.Context, string, string) error
	RevokeSessions(context.Context, string) error
}

func (v *PasswordVault) ResetPassword(ctx context.Context, operationID uuid.UUID, handle, policyVersion, subject, mobile string) error {
	if operationID == uuid.Nil || strings.TrimSpace(subject) == "" {
		return ErrReferenceInvalid
	}
	if err := v.ValidateReference(operationID, handle, policyVersion, subject, mobile); err != nil {
		return err
	}
	record, err := v.record(handle, policyVersion)
	if err != nil {
		return err
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if err := bindRecord(record, subject, mobile); err != nil || record.password == "" || v.now().After(record.expiresAt) {
		if err != nil {
			return err
		}
		return ErrReferenceInvalid
	}
	if err := ValidatePassword(record.password); err != nil {
		return err
	}
	if err := v.directory.SetPassword(ctx, subject, record.password); err != nil {
		return err
	}
	record.password = ""
	return v.directory.RevokeSessions(ctx, subject)
}

func (v *PasswordVault) UpdateUsername(ctx context.Context, subject string, operationID uuid.UUID, username string) error {
	return v.directory.UpdateUsername(ctx, subject, operationID, username)
}

func (v *PasswordVault) UpdateMobile(ctx context.Context, subject string, operationID uuid.UUID, mobile string) error {
	return v.directory.UpdateMobile(ctx, subject, operationID, mobile)
}

func (v *PasswordVault) RevokeSessions(ctx context.Context, subject string) error {
	return v.directory.RevokeSessions(ctx, subject)
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
}

type PasswordVault struct {
	mu         sync.Mutex
	records    map[string]*passwordRecord
	operations map[uuid.UUID]operationPasswordBinding
	hmacKey    []byte
	directory  UserDirectory
	now        func() time.Time
	ttl        time.Duration
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
	v.mu.Lock()
	defer v.mu.Unlock()
	v.removeExpiredLocked()
	if existing, ok := v.operations[operationID]; ok {
		if existing.username != username || existing.mobile != mobile || !hmac.Equal(existing.fingerprint[:], binding.fingerprint[:]) {
			return ErrReferenceConflict
		}
		return nil
	}
	v.operations[operationID] = binding
	return nil
}

func (v *PasswordVault) CreateMobileUser(ctx context.Context, operationID uuid.UUID, handle, policyVersion, username, mobile string) (string, error) {
	if operationID == uuid.Nil {
		return "", ErrReferenceInvalid
	}
	if subject, found, err := v.directory.FindMobileUserByRegistrationOperation(ctx, operationID); err != nil || found {
		return subject, err
	}
	if handle == "" {
		v.mu.Lock()
		v.removeExpiredLocked()
		handle = v.operations[operationID].handle
		v.mu.Unlock()
	}
	if err := v.ValidateReference(operationID, handle, policyVersion, username, mobile); err != nil {
		return "", err
	}
	record, err := v.record(handle, policyVersion)
	if err != nil {
		return "", err
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if err := bindRecord(record, username, mobile); err != nil {
		return "", err
	}
	if record.operationID != uuid.Nil && record.operationID != operationID {
		return "", ErrReferenceConflict
	}
	record.operationID = operationID
	if record.subject != "" {
		return record.subject, nil
	}
	if record.password == "" || v.now().After(record.expiresAt) {
		return "", ErrReferenceInvalid
	}
	currentFingerprint := v.fingerprint(record.password)
	if !hmac.Equal(record.fingerprint[:], currentFingerprint[:]) {
		return "", ErrReferenceConflict
	}
	if err := ValidatePassword(record.password); err != nil {
		return "", err
	}
	subject, err := v.directory.CreateMobileUser(ctx, operationID, username, mobile, record.password)
	if err != nil {
		return "", err
	}
	record.subject = subject
	record.password = ""
	return subject, nil
}

func (v *PasswordVault) FindMobileUserByRegistrationOperation(ctx context.Context, operationID uuid.UUID) (string, bool, error) {
	if operationID == uuid.Nil {
		return "", false, ErrReferenceInvalid
	}
	return v.directory.FindMobileUserByRegistrationOperation(ctx, operationID)
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
	for operationID, binding := range v.operations {
		if now.After(binding.expiresAt) {
			delete(v.operations, operationID)
		}
	}
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
