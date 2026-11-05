package tokenauthority

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const SchemaVersion = "1"

const (
	ModeOperatorAuthorize = "OPERATOR_AUTHORIZE"
	ModeT1Snapshot        = "T1_SNAPSHOT"
	ModeT3CreatorRecheck  = "T3_CREATOR_RECHECK"
	ModeT3OperatorRecheck = "T3_OPERATOR_RECHECK"
)

var RequiredPermissions = []string{"platform.tokens.manage", "platform.tokens.owner-candidates.read"}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Request deliberately uses nullable strings for every discriminated field.
// That keeps JSON null distinct from zero values and makes the canonical JCS
// contract identical in Go and browser clients.
type Request struct {
	AuthoritySchemaVersion               string   `json:"authority_schema_version"`
	Mode                                 string   `json:"mode"`
	IdentityIssuer                       *string  `json:"identity_issuer"`
	IdentitySubject                      *string  `json:"identity_subject"`
	IdentitySubjectSHA256                *string  `json:"identity_subject_sha256"`
	ActorUserID                          *string  `json:"actor_user_id"`
	OwnerType                            *string  `json:"owner_type"`
	OwnerID                              *string  `json:"owner_id"`
	RequiredPermissions                  []string `json:"required_permissions"`
	CommandReference                     string   `json:"command_reference"`
	CommandRequestDigest                 *string  `json:"command_request_digest"`
	ExpectedPlatformAuthorizationVersion *string  `json:"expected_platform_authorization_version"`
	ExpectedActorVersion                 *string  `json:"expected_actor_version"`
	ExpectedOwnerVersion                 *string  `json:"expected_owner_version"`
	ExpectedActorIdentityDigest          *string  `json:"expected_actor_identity_digest"`
	ExpectedOwnerIdentityDigest          *string  `json:"expected_owner_identity_digest"`
	AuthorityRequestSHA256               string   `json:"authority_request_sha256"`
}

type Response struct {
	AuthoritySchemaVersion               string    `json:"authority_schema_version"`
	Mode                                 string    `json:"mode"`
	Decision                             string    `json:"decision"`
	ReasonCode                           *string   `json:"reason_code"`
	ActorUserID                          string    `json:"actor_user_id"`
	ExpectedPlatformAuthorizationVersion *string   `json:"expected_platform_authorization_version"`
	ObservedPlatformAuthorizationVersion string    `json:"observed_platform_authorization_version"`
	ExpectedActorVersion                 *string   `json:"expected_actor_version"`
	ExpectedActorIdentityDigest          *string   `json:"expected_actor_identity_digest"`
	ObservedActorVersion                 string    `json:"observed_actor_version"`
	ActorStatus                          string    `json:"actor_status"`
	ActorIdentityDigest                  string    `json:"actor_identity_digest"`
	ExpectedOwnerVersion                 *string   `json:"expected_owner_version"`
	ExpectedOwnerIdentityDigest          *string   `json:"expected_owner_identity_digest"`
	ObservedOwnerVersion                 *string   `json:"observed_owner_version"`
	OwnerStatus                          *string   `json:"owner_status"`
	OwnerIdentityDigest                  *string   `json:"owner_identity_digest"`
	OwnerOrganizationID                  *string   `json:"owner_organization_id"`
	CommandReference                     string    `json:"command_reference"`
	CommandRequestDigest                 *string   `json:"command_request_digest"`
	AuthorityRequestSHA256               string    `json:"authority_request_sha256"`
	ObservedAt                           time.Time `json:"observed_at"`
}

func stringJSON(value *string) string {
	if value == nil {
		return "null"
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(*value)
	return strings.TrimSuffix(buffer.String(), "\n")
}

func (r Request) CanonicalJSON() []byte {
	permissions, _ := json.Marshal(r.RequiredPermissions)
	// Keys are emitted in Unicode code point order as required by RFC 8785.
	return []byte(`{"actor_user_id":` + stringJSON(r.ActorUserID) +
		`,"authority_schema_version":` + stringJSON(&r.AuthoritySchemaVersion) +
		`,"command_reference":` + stringJSON(&r.CommandReference) +
		`,"command_request_digest":` + stringJSON(r.CommandRequestDigest) +
		`,"expected_actor_identity_digest":` + stringJSON(r.ExpectedActorIdentityDigest) +
		`,"expected_actor_version":` + stringJSON(r.ExpectedActorVersion) +
		`,"expected_owner_identity_digest":` + stringJSON(r.ExpectedOwnerIdentityDigest) +
		`,"expected_owner_version":` + stringJSON(r.ExpectedOwnerVersion) +
		`,"expected_platform_authorization_version":` + stringJSON(r.ExpectedPlatformAuthorizationVersion) +
		`,"identity_issuer":` + stringJSON(r.IdentityIssuer) +
		`,"identity_subject_sha256":` + stringJSON(r.IdentitySubjectSHA256) +
		`,"mode":` + stringJSON(&r.Mode) +
		`,"owner_id":` + stringJSON(r.OwnerID) +
		`,"owner_type":` + stringJSON(r.OwnerType) +
		`,"required_permissions":` + string(permissions) + `}`)
}

func (r Request) Digest() string {
	digest := sha256.Sum256(r.CanonicalJSON())
	return hex.EncodeToString(digest[:])
}

func validCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func validVersion(value *string) bool {
	if value == nil || *value == "" || (*value)[0] == '0' {
		return false
	}
	for _, char := range *value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validDigest(value *string) bool { return value != nil && digestPattern.MatchString(*value) }

func (r Request) Validate() error {
	if r.AuthoritySchemaVersion != SchemaVersion || !validCanonicalUUID(r.CommandReference) ||
		len(r.RequiredPermissions) != 2 || r.RequiredPermissions[0] != RequiredPermissions[0] || r.RequiredPermissions[1] != RequiredPermissions[1] ||
		!digestPattern.MatchString(r.AuthorityRequestSHA256) {
		return errors.New("invalid authority request")
	}
	identityMode := r.Mode == ModeOperatorAuthorize || r.Mode == ModeT1Snapshot
	if identityMode {
		if r.IdentityIssuer == nil || strings.TrimSpace(*r.IdentityIssuer) == "" || *r.IdentityIssuer != strings.TrimSpace(*r.IdentityIssuer) ||
			r.IdentitySubject == nil || *r.IdentitySubject == "" || !validDigest(r.IdentitySubjectSHA256) || r.ActorUserID != nil {
			return errors.New("invalid identity authority request")
		}
		subjectDigest := sha256.Sum256([]byte(*r.IdentitySubject))
		if subtle.ConstantTimeCompare([]byte(*r.IdentitySubjectSHA256), []byte(hex.EncodeToString(subjectDigest[:]))) != 1 {
			return errors.New("identity subject digest mismatch")
		}
	} else if r.IdentityIssuer != nil || r.IdentitySubject != nil || r.IdentitySubjectSHA256 != nil || r.ActorUserID == nil || !validCanonicalUUID(*r.ActorUserID) {
		return errors.New("invalid actor authority request")
	}
	allExpectedNull := r.ExpectedPlatformAuthorizationVersion == nil && r.ExpectedActorVersion == nil && r.ExpectedOwnerVersion == nil && r.ExpectedActorIdentityDigest == nil && r.ExpectedOwnerIdentityDigest == nil
	switch r.Mode {
	case ModeOperatorAuthorize:
		if r.OwnerType != nil || r.OwnerID != nil || r.CommandRequestDigest != nil || !allExpectedNull {
			return errors.New("invalid operator authority request")
		}
	case ModeT1Snapshot:
		if !validOwner(r.OwnerType, r.OwnerID) || !validDigest(r.CommandRequestDigest) || !allExpectedNull {
			return errors.New("invalid T1 authority request")
		}
	case ModeT3CreatorRecheck, ModeT3OperatorRecheck:
		if !validOwner(r.OwnerType, r.OwnerID) || !validDigest(r.CommandRequestDigest) || !validVersion(r.ExpectedPlatformAuthorizationVersion) || !validVersion(r.ExpectedActorVersion) || !validVersion(r.ExpectedOwnerVersion) || !validDigest(r.ExpectedActorIdentityDigest) || !validDigest(r.ExpectedOwnerIdentityDigest) {
			return errors.New("invalid T3 authority request")
		}
	default:
		return errors.New("invalid authority mode")
	}
	if subtle.ConstantTimeCompare([]byte(r.AuthorityRequestSHA256), []byte(r.Digest())) != 1 {
		return errors.New("authority request digest mismatch")
	}
	return nil
}

func validOwner(ownerType, ownerID *string) bool {
	if ownerType == nil || ownerID == nil || !validCanonicalUUID(*ownerID) {
		return false
	}
	return *ownerType == "USER" || *ownerType == "ORGANIZATION" || *ownerType == "ORG_UNIT"
}

func ActorIdentityDigest(actor uuid.UUID, status string, version int64) string {
	digest := sha256.Sum256([]byte(actor.String() + "\n" + status + "\n" + strconv.FormatInt(version, 10) + "\n"))
	return hex.EncodeToString(digest[:])
}

func OwnerIdentityDigest(ownerType string, owner uuid.UUID, status string, version int64) string {
	digest := sha256.Sum256([]byte(ownerType + "\n" + owner.String() + "\n" + status + "\n" + strconv.FormatInt(version, 10) + "\n"))
	return hex.EncodeToString(digest[:])
}

func UnitOwnerIdentityDigest(owner, organization uuid.UUID, status string, version int64) string {
	digest := sha256.Sum256([]byte("ORG_UNIT\n" + owner.String() + "\n" + organization.String() + "\n" + status + "\n" + strconv.FormatInt(version, 10) + "\n"))
	return hex.EncodeToString(digest[:])
}
