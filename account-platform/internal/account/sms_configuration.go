package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/jackc/pgx/v5"
)

var (
	ErrSMSConfigurationInvalid       = errors.New("SMS configuration is invalid")
	ErrSMSConfigurationConflict      = errors.New("SMS configuration version conflict")
	ErrSMSConfigurationKeyReused     = errors.New("SMS configuration idempotency key was reused")
	ErrSMSProviderProfileUnavailable = errors.New("SMS provider profile is unavailable")
)

type SMSPurposeTemplates struct {
	Register      string `json:"REGISTER"`
	Login         string `json:"LOGIN"`
	ProfileMobile string `json:"PROFILE_MOBILE"`
	PasswordReset string `json:"PASSWORD_RESET"`
}

type SMSConfiguration struct {
	Revision                 int64               `json:"revision"`
	Source                   string              `json:"source"`
	Enabled                  bool                `json:"enabled"`
	ProviderID               string              `json:"provider_id"`
	AdapterVersion           string              `json:"adapter_version"`
	CredentialProfile        string              `json:"credential_profile"`
	SignName                 string              `json:"sign_name,omitempty"`
	Templates                SMSPurposeTemplates `json:"templates"`
	ConfigurationFingerprint string              `json:"configuration_fingerprint"`
	UpdatedAt                time.Time           `json:"updated_at,omitempty"`
}

func (configuration SMSConfiguration) descriptor() SMSProviderDescriptor {
	version := configuration.AdapterVersion
	if configuration.Revision > 0 {
		version += "-c" + strconv.FormatInt(configuration.Revision, 10)
	}
	return SMSProviderDescriptor{ID: configuration.ProviderID, AdapterVersion: version}
}

func normalizeSMSConfiguration(configuration SMSConfiguration) (SMSConfiguration, error) {
	configuration.ProviderID = strings.ToLower(strings.TrimSpace(configuration.ProviderID))
	configuration.AdapterVersion = strings.TrimSpace(configuration.AdapterVersion)
	configuration.CredentialProfile = strings.TrimSpace(configuration.CredentialProfile)
	configuration.SignName = strings.TrimSpace(configuration.SignName)
	configuration.Templates = SMSPurposeTemplates{
		Register: strings.TrimSpace(configuration.Templates.Register), Login: strings.TrimSpace(configuration.Templates.Login),
		ProfileMobile: strings.TrimSpace(configuration.Templates.ProfileMobile), PasswordReset: strings.TrimSpace(configuration.Templates.PasswordReset),
	}
	if configuration.AdapterVersion == "" {
		configuration.AdapterVersion = "v1"
	}
	if configuration.AdapterVersion != "v1" {
		return SMSConfiguration{}, ErrSMSConfigurationInvalid
	}
	switch configuration.ProviderID {
	case "aliyun-managed":
		if configuration.CredentialProfile != "aliyun-default" || configuration.SignName == "" || len(configuration.SignName) > 100 ||
			!validSMSTemplate(configuration.Templates.Register) || !validSMSTemplate(configuration.Templates.Login) ||
			!validSMSTemplate(configuration.Templates.ProfileMobile) || !validSMSTemplate(configuration.Templates.PasswordReset) {
			return SMSConfiguration{}, ErrSMSConfigurationInvalid
		}
	case "http-generated":
		if configuration.CredentialProfile != "http-default" || configuration.SignName != "" || configuration.Templates != (SMSPurposeTemplates{}) {
			return SMSConfiguration{}, ErrSMSConfigurationInvalid
		}
	default:
		return SMSConfiguration{}, ErrSMSConfigurationInvalid
	}
	configuration.ConfigurationFingerprint = smsConfigurationFingerprint(configuration)
	return configuration, nil
}

func validSMSTemplate(value string) bool { return value != "" && len(value) <= 120 }

func smsConfigurationFingerprint(configuration SMSConfiguration) string {
	canonical := strings.Join([]string{
		strconv.FormatBool(configuration.Enabled), configuration.ProviderID, configuration.AdapterVersion, configuration.CredentialProfile,
		configuration.SignName, configuration.Templates.Register, configuration.Templates.Login,
		configuration.Templates.ProfileMobile, configuration.Templates.PasswordReset,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func smsConfigurationRequestHash(configuration SMSConfiguration, expectedRevision int64, reason string) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{
		configuration.ConfigurationFingerprint, strconv.FormatInt(expectedRevision, 10), strings.TrimSpace(reason),
	}, "\x00")))
}

type smsConfigurationStore interface {
	ActiveSMSConfiguration(context.Context) (SMSConfiguration, bool, error)
	SMSConfigurationVersion(context.Context, int64) (SMSConfiguration, error)
	UpdateSMSConfiguration(context.Context, identity.Principal, SMSConfiguration, int64, string, string) (SMSConfiguration, bool, error)
}

func (r *PostgresRepository) ActiveSMSConfiguration(ctx context.Context) (SMSConfiguration, bool, error) {
	var configuration SMSConfiguration
	var signName, register, login, profile, reset *string
	err := r.pool.QueryRow(ctx, `SELECT v.revision,v.enabled,v.provider_id,v.adapter_version,v.credential_profile,
v.sign_name,v.register_template,v.login_template,v.profile_mobile_template,v.password_reset_template,v.configuration_fingerprint,v.created_at
FROM sms_configuration_state s JOIN sms_configuration_versions v ON v.revision=s.active_revision WHERE s.singleton=TRUE`).Scan(
		&configuration.Revision, &configuration.Enabled, &configuration.ProviderID, &configuration.AdapterVersion, &configuration.CredentialProfile,
		&signName, &register, &login, &profile, &reset, &configuration.ConfigurationFingerprint, &configuration.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SMSConfiguration{}, false, nil
	}
	if err != nil {
		return SMSConfiguration{}, false, err
	}
	configuration.Source = "ADMIN"
	configuration.SignName, configuration.Templates = stringValue(signName), SMSPurposeTemplates{
		Register: stringValue(register), Login: stringValue(login), ProfileMobile: stringValue(profile), PasswordReset: stringValue(reset),
	}
	return configuration, true, nil
}

func (r *PostgresRepository) SMSConfigurationVersion(ctx context.Context, revision int64) (SMSConfiguration, error) {
	var configuration SMSConfiguration
	var signName, register, login, profile, reset *string
	err := r.pool.QueryRow(ctx, `SELECT revision,enabled,provider_id,adapter_version,credential_profile,sign_name,register_template,
login_template,profile_mobile_template,password_reset_template,configuration_fingerprint,created_at
FROM sms_configuration_versions WHERE revision=$1`, revision).Scan(
		&configuration.Revision, &configuration.Enabled, &configuration.ProviderID, &configuration.AdapterVersion, &configuration.CredentialProfile,
		&signName, &register, &login, &profile, &reset, &configuration.ConfigurationFingerprint, &configuration.UpdatedAt,
	)
	if err != nil {
		return SMSConfiguration{}, err
	}
	configuration.Source = "ADMIN"
	configuration.SignName, configuration.Templates = stringValue(signName), SMSPurposeTemplates{
		Register: stringValue(register), Login: stringValue(login), ProfileMobile: stringValue(profile), PasswordReset: stringValue(reset),
	}
	return configuration, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func recentlyAuthenticated(authenticatedAt, now time.Time) bool {
	return !authenticatedAt.IsZero() && !authenticatedAt.After(now.Add(time.Minute)) && now.Sub(authenticatedAt) <= 15*time.Minute
}

func (r *PostgresRepository) UpdateSMSConfiguration(ctx context.Context, principal identity.Principal, configuration SMSConfiguration, expectedRevision int64, reason, idempotencyKey string) (SMSConfiguration, bool, error) {
	var err error
	configuration, err = normalizeSMSConfiguration(configuration)
	reason, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(idempotencyKey)
	if err != nil || expectedRevision < 0 || len(reason) < 10 || len(reason) > 500 || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || !recentlyAuthenticated(principal.AuthenticatedAt, r.now()) {
		if !recentlyAuthenticated(principal.AuthenticatedAt, r.now()) {
			return SMSConfiguration{}, false, ErrRecentAuthenticationRequired
		}
		return SMSConfiguration{}, false, ErrSMSConfigurationInvalid
	}
	actorID, err := r.userIDByPrincipal(ctx, principal)
	if err != nil {
		return SMSConfiguration{}, false, err
	}
	requestHash := smsConfigurationRequestHash(configuration, expectedRevision, reason)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SMSConfiguration{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize configuration commands before checking idempotency. A concurrent
	// replay can otherwise miss the first insert, wait for the state lock, and
	// incorrectly return a revision conflict instead of the original result.
	var currentRevision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM sms_configuration_state WHERE singleton=TRUE FOR UPDATE`).Scan(&currentRevision); err != nil {
		return SMSConfiguration{}, false, err
	}
	var existingRevision int64
	var existingHash []byte
	err = tx.QueryRow(ctx, `SELECT revision,request_hash FROM sms_configuration_versions WHERE actor_user_id=$1 AND idempotency_key=$2`, actorID, idempotencyKey).Scan(&existingRevision, &existingHash)
	if err == nil {
		if !equalBytes(existingHash, requestHash[:]) {
			return SMSConfiguration{}, false, ErrSMSConfigurationKeyReused
		}
		existing, loadErr := scanSMSConfigurationRow(tx.QueryRow(ctx, `SELECT revision,enabled,provider_id,adapter_version,credential_profile,sign_name,register_template,login_template,profile_mobile_template,password_reset_template,configuration_fingerprint,created_at FROM sms_configuration_versions WHERE revision=$1`, existingRevision))
		if loadErr != nil {
			return SMSConfiguration{}, false, loadErr
		}
		return existing, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SMSConfiguration{}, false, err
	}
	if currentRevision != expectedRevision {
		return SMSConfiguration{}, false, ErrSMSConfigurationConflict
	}
	configuration.Revision, configuration.Source, configuration.UpdatedAt = currentRevision+1, "ADMIN", r.now()
	if _, err = tx.Exec(ctx, `INSERT INTO sms_configuration_versions(revision,enabled,provider_id,adapter_version,credential_profile,sign_name,register_template,login_template,profile_mobile_template,password_reset_template,configuration_fingerprint,actor_user_id,idempotency_key,request_hash,change_reason,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, configuration.Revision, configuration.Enabled, configuration.ProviderID, configuration.AdapterVersion, configuration.CredentialProfile,
		nullableString(configuration.SignName), nullableString(configuration.Templates.Register), nullableString(configuration.Templates.Login), nullableString(configuration.Templates.ProfileMobile), nullableString(configuration.Templates.PasswordReset),
		configuration.ConfigurationFingerprint, actorID, idempotencyKey, requestHash[:], reason, configuration.UpdatedAt); err != nil {
		return SMSConfiguration{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sms_configuration_state SET revision=$1,active_revision=$1,updated_at=$2 WHERE singleton=TRUE AND revision=$3`, configuration.Revision, configuration.UpdatedAt, expectedRevision); err != nil {
		return SMSConfiguration{}, false, err
	}
	if err = appendAudit(ctx, tx, actorID, principal.SessionID, nil, "account.sms_configuration.update", "sms_configuration", "active", "SUCCEEDED", map[string]any{
		"revision": configuration.Revision, "provider_id": configuration.ProviderID, "enabled": configuration.Enabled, "configuration_fingerprint": configuration.ConfigurationFingerprint,
	}); err != nil {
		return SMSConfiguration{}, false, err
	}
	return configuration, false, tx.Commit(ctx)
}

func scanSMSConfigurationRow(row pgx.Row) (SMSConfiguration, error) {
	var configuration SMSConfiguration
	var signName, register, login, profile, reset *string
	err := row.Scan(&configuration.Revision, &configuration.Enabled, &configuration.ProviderID, &configuration.AdapterVersion, &configuration.CredentialProfile,
		&signName, &register, &login, &profile, &reset, &configuration.ConfigurationFingerprint, &configuration.UpdatedAt)
	if err != nil {
		return SMSConfiguration{}, err
	}
	configuration.Source, configuration.SignName = "ADMIN", stringValue(signName)
	configuration.Templates = SMSPurposeTemplates{Register: stringValue(register), Login: stringValue(login), ProfileMobile: stringValue(profile), PasswordReset: stringValue(reset)}
	return configuration, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

func parseSMSConfigurationRevision(descriptor SMSProviderDescriptor) (int64, bool) {
	prefix := "v1-c"
	if !strings.HasPrefix(descriptor.AdapterVersion, prefix) {
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimPrefix(descriptor.AdapterVersion, prefix), 10, 64)
	return revision, err == nil && revision > 0
}
