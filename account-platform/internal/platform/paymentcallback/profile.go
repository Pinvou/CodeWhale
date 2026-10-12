package paymentcallback

import (
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

var ErrProfileUnavailable = errors.New("payment callback provider profile is unavailable")

type Notification struct {
	ID       uuid.UUID
	SentAt   time.Time
	Provider payment.ProviderOrder
}

type Profile interface {
	ProviderID() string
	ProviderVersion() string
	ParseNotification(body []byte, now time.Time) (Notification, error)
}

type Profiles struct {
	profiles map[string]Profile
}

func NewProfiles(profiles ...Profile) (*Profiles, error) {
	result := &Profiles{profiles: make(map[string]Profile, len(profiles))}
	for _, profile := range profiles {
		if profile == nil {
			return nil, ErrProfileUnavailable
		}
		providerID, version := strings.TrimSpace(profile.ProviderID()), strings.TrimSpace(profile.ProviderVersion())
		key := profileKey(providerID, version)
		if providerID == "" || version == "" {
			return nil, ErrProfileUnavailable
		}
		if _, exists := result.profiles[key]; exists {
			return nil, errors.New("payment callback provider profiles must be unique")
		}
		result.profiles[key] = profile
	}
	if len(result.profiles) == 0 {
		return nil, ErrProfileUnavailable
	}
	return result, nil
}

func (p *Profiles) Parse(principal Principal, body []byte, now time.Time) (Notification, error) {
	profile, ok := p.profiles[profileKey(principal.ProviderID, principal.ProviderVersion)]
	if !ok {
		return Notification{}, ErrProfileUnavailable
	}
	return profile.ParseNotification(body, now)
}

func profileKey(providerID, version string) string {
	return strings.TrimSpace(providerID) + "\x00" + strings.TrimSpace(version)
}
