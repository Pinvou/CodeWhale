package account

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

type SMSProviderProfile struct {
	ID            string                 `json:"profile_id"`
	ProviderID    string                 `json:"provider_id"`
	Configured    bool                   `json:"configured"`
	Aliyun        AliyunSMSConfig        `json:"-"`
	HTTPGenerated HTTPGeneratedSMSConfig `json:"-"`
}

type SMSConfigurationProjection struct {
	SMSConfiguration
	DeploymentCapability bool                 `json:"deployment_capability"`
	Readiness            string               `json:"readiness"`
	AvailableProfiles    []SMSProviderProfile `json:"available_profiles"`
}

type routedSMSProvider interface {
	SMSProvider
	ActiveProvider(context.Context) (SMSProvider, error)
	ProviderForSend(context.Context, SMSProviderDescriptor) (SMSProvider, error)
	ProviderForVerification(context.Context, SMSProviderDescriptor) (SMSProvider, error)
}

type VersionedSMSProviderResolver struct {
	store         smsConfigurationStore
	legacy        SMSProvider
	defaultConfig SMSConfiguration
	profiles      map[string]SMSProviderProfile
	cache         sync.Map
}

func NewVersionedSMSProviderResolver(store smsConfigurationStore, legacy SMSProvider, defaultConfiguration SMSConfiguration, profiles []SMSProviderProfile) (*VersionedSMSProviderResolver, error) {
	if store == nil || legacy == nil {
		return nil, errors.New("SMS provider resolver is not fully configured")
	}
	defaultConfiguration.Source, defaultConfiguration.Revision = "DEPLOYMENT_DEFAULT", 0
	var err error
	defaultConfiguration, err = normalizeSMSConfiguration(defaultConfiguration)
	if err != nil || legacy.Descriptor() != defaultConfiguration.descriptor() {
		return nil, errors.New("SMS deployment default does not match the legacy provider")
	}
	indexed := make(map[string]SMSProviderProfile, len(profiles))
	for _, profile := range profiles {
		if profile.ID == "" || profile.ProviderID == "" || indexed[profile.ID].ID != "" {
			return nil, errors.New("SMS provider profile registry is invalid")
		}
		indexed[profile.ID] = profile
	}
	return &VersionedSMSProviderResolver{store: store, legacy: legacy, defaultConfig: defaultConfiguration, profiles: indexed}, nil
}

func (r *VersionedSMSProviderResolver) Descriptor() SMSProviderDescriptor {
	return r.legacy.Descriptor()
}
func (r *VersionedSMSProviderResolver) DeliveryRetryPolicy() SMSDeliveryRetryPolicy {
	return r.legacy.DeliveryRetryPolicy()
}
func (r *VersionedSMSProviderResolver) PrepareVerification(id uuid.UUID) ([]byte, error) {
	return r.legacy.PrepareVerification(id)
}
func (r *VersionedSMSProviderResolver) SendCode(ctx context.Context, mobile string, purpose SMSPurpose, id uuid.UUID) (SMSDelivery, error) {
	provider, err := r.ActiveProvider(ctx)
	if err != nil {
		return SMSDelivery{}, err
	}
	return provider.SendCode(ctx, mobile, purpose, id)
}
func (r *VersionedSMSProviderResolver) VerifyCode(ctx context.Context, mobile string, id uuid.UUID, verification SMSVerificationData, code string) (bool, error) {
	provider, err := r.ProviderForVerification(ctx, verification.Provider)
	if err != nil {
		return false, err
	}
	return provider.VerifyCode(ctx, mobile, id, verification, code)
}

func (r *VersionedSMSProviderResolver) current(ctx context.Context) (SMSConfiguration, error) {
	configuration, found, err := r.store.ActiveSMSConfiguration(ctx)
	if err != nil {
		return SMSConfiguration{}, err
	}
	if !found {
		return r.defaultConfig, nil
	}
	return configuration, nil
}

func (r *VersionedSMSProviderResolver) ActiveProvider(ctx context.Context) (SMSProvider, error) {
	configuration, err := r.current(ctx)
	if err != nil || !configuration.Enabled {
		return nil, ErrSMSUnavailable
	}
	return r.build(configuration)
}

func (r *VersionedSMSProviderResolver) ProviderForSend(ctx context.Context, descriptor SMSProviderDescriptor) (SMSProvider, error) {
	active, err := r.current(ctx)
	if err != nil || !active.Enabled {
		return nil, ErrSMSUnavailable
	}
	return r.resolve(ctx, descriptor)
}

func (r *VersionedSMSProviderResolver) ProviderForVerification(ctx context.Context, descriptor SMSProviderDescriptor) (SMSProvider, error) {
	return r.resolve(ctx, descriptor)
}

func (r *VersionedSMSProviderResolver) resolve(ctx context.Context, descriptor SMSProviderDescriptor) (SMSProvider, error) {
	if descriptor == r.legacy.Descriptor() {
		return r.legacy, nil
	}
	revision, ok := parseSMSConfigurationRevision(descriptor)
	if !ok {
		return nil, ErrSMSUnavailable
	}
	configuration, err := r.store.SMSConfigurationVersion(ctx, revision)
	if err != nil || configuration.descriptor() != descriptor {
		return nil, ErrSMSUnavailable
	}
	return r.build(configuration)
}

func (r *VersionedSMSProviderResolver) build(configuration SMSConfiguration) (SMSProvider, error) {
	descriptor := configuration.descriptor()
	if cached, ok := r.cache.Load(descriptor); ok {
		return cached.(SMSProvider), nil
	}
	provider, err := r.provider(configuration)
	if err != nil {
		return nil, err
	}
	actual, _ := r.cache.LoadOrStore(descriptor, provider)
	return actual.(SMSProvider), nil
}

func (r *VersionedSMSProviderResolver) provider(configuration SMSConfiguration) (SMSProvider, error) {
	descriptor := configuration.descriptor()
	profile, ok := r.profiles[configuration.CredentialProfile]
	if !ok || !profile.Configured || profile.ProviderID != configuration.ProviderID {
		return nil, ErrSMSProviderProfileUnavailable
	}
	var provider SMSProvider
	var err error
	switch configuration.ProviderID {
	case "aliyun-managed":
		candidate := profile.Aliyun
		candidate.SignName = configuration.SignName
		candidate.RegisterTemplate = configuration.Templates.Register
		candidate.LoginTemplate = configuration.Templates.Login
		candidate.ProfileMobileTemplate = configuration.Templates.ProfileMobile
		candidate.PasswordResetTemplate = configuration.Templates.PasswordReset
		provider, err = newAliyunSMSProvider(candidate, descriptor)
	case "http-generated":
		provider, err = newHTTPGeneratedSMSProvider(profile.HTTPGenerated, descriptor)
	default:
		err = ErrSMSConfigurationInvalid
	}
	if err != nil {
		return nil, ErrSMSProviderProfileUnavailable
	}
	return provider, nil
}

func (r *VersionedSMSProviderResolver) Projection(ctx context.Context) (SMSConfigurationProjection, error) {
	configuration, err := r.current(ctx)
	if err != nil {
		return SMSConfigurationProjection{}, err
	}
	return r.projectionFor(configuration), nil
}

func (r *VersionedSMSProviderResolver) projectionFor(configuration SMSConfiguration) SMSConfigurationProjection {
	profiles := make([]SMSProviderProfile, 0, len(r.profiles))
	for _, id := range []string{"aliyun-default", "http-default"} {
		if profile, ok := r.profiles[id]; ok {
			profile.Aliyun, profile.HTTPGenerated = AliyunSMSConfig{}, HTTPGeneratedSMSConfig{}
			profiles = append(profiles, profile)
		}
	}
	readiness := "DISABLED"
	if configuration.Enabled {
		if _, buildErr := r.build(configuration); buildErr == nil {
			readiness = "READY"
		} else {
			readiness = "PROFILE_UNAVAILABLE"
		}
	}
	return SMSConfigurationProjection{SMSConfiguration: configuration, DeploymentCapability: true, Readiness: readiness, AvailableProfiles: profiles}
}

func (r *VersionedSMSProviderResolver) ValidateCandidate(configuration SMSConfiguration) (SMSConfiguration, error) {
	normalized, err := normalizeSMSConfiguration(configuration)
	if err != nil {
		return SMSConfiguration{}, err
	}
	if normalized.Enabled {
		// A synthetic positive revision gives the provider its final descriptor
		// shape without persisting or contacting the remote provider.
		normalized.Revision = 1
		if _, err = r.provider(normalized); err != nil {
			return SMSConfiguration{}, err
		}
		normalized.Revision = 0
	}
	return normalized, nil
}
