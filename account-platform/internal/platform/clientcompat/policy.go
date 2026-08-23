package clientcompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const (
	ClientVersionHeader    = "X-Pinvou-Client-Version"
	ContractVersionHeader  = "X-API-Contract-Version"
	MinimumSupportedHeader = "X-Minimum-Supported-Client-Version"
	DefaultContractVersion = "account-platform-v1"
	defaultUpgradeURL      = "https://github.com/Pinvou/pinvou3/releases/latest"
	defaultReleaseManifest = `[{"version":"0.8.0","released_at":"2026-08-01T00:00:00Z"},{"version":"0.7.0","released_at":"2026-06-01T00:00:00Z","deprecation_notice_at":"2026-08-31T00:00:00Z","sunset_at":"2026-10-30T00:00:00Z"},{"version":"0.6.0","released_at":"2026-03-01T00:00:00Z","deprecation_notice_at":"2026-07-03T00:00:00Z","sunset_at":"2026-09-01T00:00:00Z"}]`
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:[-+][0-9A-Za-z.-]+)?$`)

type Release struct {
	Version             string `json:"version"`
	ReleasedAt          string `json:"released_at"`
	DeprecationNoticeAt string `json:"deprecation_notice_at,omitempty"`
	SunsetAt            string `json:"sunset_at,omitempty"`
	SecurityException   bool   `json:"security_exception,omitempty"`
	ExceptionApprovalID string `json:"exception_approval_id,omitempty"`
}

type Policy struct {
	ContractVersion string    `json:"api_contract_version"`
	UpgradeURL      string    `json:"upgrade_url"`
	Releases        []Release `json:"releases"`
	now             func() time.Time
}

type Manifest struct {
	APIContractVersion            string    `json:"api_contract_version"`
	CurrentClientVersion          string    `json:"current_client_version"`
	MinimumSupportedClientVersion string    `json:"minimum_supported_client_version"`
	SunsetAt                      string    `json:"sunset_at,omitempty"`
	UpgradeURL                    string    `json:"upgrade_url"`
	Releases                      []Release `json:"releases"`
}

func FromEnv() (Policy, error) {
	contractVersion := strings.TrimSpace(os.Getenv("PINVOU_API_CONTRACT_VERSION"))
	if contractVersion == "" {
		contractVersion = DefaultContractVersion
	}
	upgradeURL := strings.TrimSpace(os.Getenv("PINVOU_CLIENT_UPGRADE_URL"))
	if upgradeURL == "" {
		upgradeURL = defaultUpgradeURL
	}
	raw := strings.TrimSpace(os.Getenv("PINVOU_CLIENT_RELEASE_MANIFEST"))
	if raw == "" {
		raw = defaultReleaseManifest
	}
	var releases []Release
	if err := json.Unmarshal([]byte(raw), &releases); err != nil {
		return Policy{}, fmt.Errorf("PINVOU_CLIENT_RELEASE_MANIFEST must be valid JSON: %w", err)
	}
	return NewPolicy(contractVersion, upgradeURL, releases)
}

func NewPolicy(contractVersion, upgradeURL string, releases []Release) (Policy, error) {
	policy := Policy{ContractVersion: strings.TrimSpace(contractVersion), UpgradeURL: strings.TrimSpace(upgradeURL), Releases: append([]Release(nil), releases...), now: func() time.Time { return time.Now().UTC() }}
	if policy.ContractVersion == "" || !strings.HasPrefix(policy.UpgradeURL, "https://") || len(policy.Releases) < 3 {
		return Policy{}, errors.New("client compatibility requires a contract, HTTPS upgrade URL, and at least N/N-1/N-2 releases")
	}
	type parsedRelease struct{ released, notice, sunset time.Time }
	parsed := make([]parsedRelease, len(policy.Releases))
	seen := map[string]struct{}{}
	for index, release := range policy.Releases {
		if !versionPattern.MatchString(release.Version) {
			return Policy{}, fmt.Errorf("invalid client release version %q", release.Version)
		}
		if _, duplicate := seen[release.Version]; duplicate {
			return Policy{}, fmt.Errorf("duplicate client release version %q", release.Version)
		}
		seen[release.Version] = struct{}{}
		var err error
		if parsed[index].released, err = time.Parse(time.RFC3339, release.ReleasedAt); err != nil {
			return Policy{}, fmt.Errorf("release %s has invalid released_at", release.Version)
		}
		if release.SunsetAt != "" {
			if parsed[index].sunset, err = time.Parse(time.RFC3339, release.SunsetAt); err != nil {
				return Policy{}, fmt.Errorf("release %s has invalid sunset_at", release.Version)
			}
			if parsed[index].notice, err = time.Parse(time.RFC3339, release.DeprecationNoticeAt); err != nil {
				return Policy{}, fmt.Errorf("release %s has invalid deprecation_notice_at", release.Version)
			}
			minimumNotice := 60 * 24 * time.Hour
			if release.SecurityException {
				minimumNotice = 7 * 24 * time.Hour
				if strings.TrimSpace(release.ExceptionApprovalID) == "" {
					return Policy{}, fmt.Errorf("release %s security exception requires approval ID", release.Version)
				}
			}
			if parsed[index].sunset.Sub(parsed[index].notice) < minimumNotice {
				return Policy{}, fmt.Errorf("release %s deprecation notice is shorter than %s", release.Version, minimumNotice)
			}
		}
	}
	for index := 1; index < len(policy.Releases); index++ {
		if compareVersions(policy.Releases[index-1].Version, policy.Releases[index].Version) <= 0 {
			return Policy{}, errors.New("client release versions must be ordered current to oldest")
		}
		if !parsed[index-1].released.After(parsed[index].released) {
			return Policy{}, errors.New("client releases must be ordered current to oldest")
		}
		if parsed[index].sunset.IsZero() || parsed[index].sunset.Sub(parsed[index-1].released) < 90*24*time.Hour {
			return Policy{}, fmt.Errorf("release %s is not supported for 90 days after its successor", policy.Releases[index].Version)
		}
	}
	return policy, nil
}

func (p Policy) Manifest() Manifest {
	now := p.now()
	minimum, sunset := p.Releases[0].Version, ""
	for _, release := range p.Releases {
		if release.SunsetAt == "" {
			minimum = release.Version
			continue
		}
		deadline, _ := time.Parse(time.RFC3339, release.SunsetAt)
		if now.Before(deadline) {
			minimum, sunset = release.Version, release.SunsetAt
		}
	}
	return Manifest{APIContractVersion: p.ContractVersion, CurrentClientVersion: p.Releases[0].Version, MinimumSupportedClientVersion: minimum, SunsetAt: sunset, UpgradeURL: p.UpgradeURL, Releases: append([]Release(nil), p.Releases...)}
}

func (p Policy) WithNow(now func() time.Time) Policy {
	p.now = now
	return p
}

func (p Policy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifest := p.Manifest()
		w.Header().Set(ContractVersionHeader, manifest.APIContractVersion)
		w.Header().Set(MinimumSupportedHeader, manifest.MinimumSupportedClientVersion)
		if manifest.SunsetAt != "" {
			w.Header().Set("Sunset", manifest.SunsetAt)
		}
		version := strings.TrimSpace(r.Header.Get(ClientVersionHeader))
		if release, ok := p.release(version); ok && release.SunsetAt != "" {
			w.Header().Set("X-Client-Sunset-At", release.SunsetAt)
			if notice, err := time.Parse(time.RFC3339, release.DeprecationNoticeAt); err == nil && !p.now().Before(notice) {
				w.Header().Set("Deprecation", "true")
				w.Header().Set("Link", "<"+manifest.UpgradeURL+">; rel=\"latest-version\"")
			}
		}
		requiresVersion := version == "" && strings.TrimSpace(r.Header.Get("Authorization")) != ""
		if isUnsafe(r.Method) && isExternalPath(r.URL.Path) && (requiresVersion || (version != "" && compareVersions(version, manifest.MinimumSupportedClientVersion) < 0)) {
			w.Header().Set("Link", "<"+manifest.UpgradeURL+">; rel=\"latest-version\"")
			api.WriteJSON(w, http.StatusUpgradeRequired, api.Error{Code: "CLIENT_UPGRADE_REQUIRED", Message: "this client version is no longer allowed to perform remote writes", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"api_contract_version": manifest.APIContractVersion, "minimum_supported_client_version": manifest.MinimumSupportedClientVersion, "sunset_at": manifest.SunsetAt, "upgrade_url": manifest.UpgradeURL}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p Policy) release(version string) (Release, bool) {
	for _, release := range p.Releases {
		if release.Version == version {
			return release, true
		}
	}
	return Release{}, false
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func isExternalPath(path string) bool {
	return !strings.HasPrefix(path, "/internal/") && !strings.HasPrefix(path, "/health") && path != "/metrics"
}

func compareVersions(left, right string) int {
	parse := func(value string) []int {
		value = strings.SplitN(strings.SplitN(value, "+", 2)[0], "-", 2)[0]
		parts := strings.Split(value, ".")
		result := make([]int, 3)
		for index := range result {
			if index < len(parts) {
				_, _ = fmt.Sscanf(parts[index], "%d", &result[index])
			}
		}
		return result
	}
	a, b := parse(left), parse(right)
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	return 0
}

func SortVersions(versions []string) {
	sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
}
