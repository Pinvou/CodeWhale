package token

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/google/uuid"
)

func TestClientReadinessSemVerOriginalSignedIdentity(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer.URL, "jwks_uri": issuer.URL + "/jwks"})
	}))
	defer issuer.Close()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controlled-readiness-semver" {
			t.Error("existing readiness service authorization missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"service": "account", "service_version": "0.8.0", "contract_version": "account-client-v1"})
	}))
	defer account.Close()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	billing, recorder := &readinessBillingFake{}, &readinessRecorderFake{}
	readiness, err := NewClientReadinessService(ClientReadinessConfig{Environment: "test", ServiceVersion: "0.8.0", MinimumClient: "0.6.0", MaximumClient: "0.8.0", AccountURL: account.URL, OIDCIssuer: issuer.URL, ServiceToken: "controlled-readiness-semver", SigningKeyID: "controlled-semver-key", SigningKey: key, AttestationTTL: 5 * time.Minute}, readinessCatalogFake{model: CatalogModel{ModelID: uuid.New(), CatalogVersion: 7, LifecycleStatus: "PUBLISHED", SourceAvailable: true, ProtocolCode: "CHAT"}}, readinessRoutesFake{}, billing, recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.8.0-rc.1+build.001", "0.8.0+build.001", "999999999999999999999999999999999999.2.3-999999999999999999999999+Build.0001"} {
		t.Run("valid_"+version, func(t *testing.T) {
			attestation, err := readiness.Issue(context.Background(), strings.Repeat("a", 64), version, "test", "CHAT")
			if err != nil || attestation.Claims.ClientVersion != version {
				t.Fatal("valid SemVer original client identity not issued", err)
			}
			payload, _ := json.Marshal(attestation.Claims)
			signature, err := base64.RawURLEncoding.DecodeString(attestation.Signature)
			if err != nil || !ed25519.Verify(public, payload, signature) {
				t.Fatal("actual Ed25519 claim signature invalid")
			}
			altered := attestation.Claims
			altered.ClientVersion += "x"
			changedPayload, _ := json.Marshal(altered)
			if ed25519.Verify(public, changedPayload, signature) {
				t.Fatal("original signed client string was replaced with a precedence/canonical identity")
			}
		})
	}
	for _, invalid := range []string{"01.2.3", "1.2.3-01", "1.2.3-alpha..1", "1.2.3+build..1", " 1.2.3", "1.2.3 ", "1.2.3-α", "1.2.3+"} {
		t.Run("invalid_"+invalid, func(t *testing.T) {
			calls, records := billing.calls, recorder.recorded
			if _, err := readiness.Issue(context.Background(), strings.Repeat("a", 64), invalid, "test", "CHAT"); !errors.Is(err, ErrForbidden) {
				t.Error("invalid SemVer client string did not fail existing forbidden boundary")
			}
			if billing.calls != calls || recorder.recorded != records {
				t.Error("invalid client version reached readiness dependency or durable attestation recorder")
			}
		})
	}
	t.Run("readiness-metadata-keeps-original-release-strings", func(t *testing.T) {
		releases := []clientcompat.Release{
			{Version: "1.2.3+stable.001", ReleasedAt: "2026-08-01T00:00:00Z"},
			{Version: "1.2.3-rc.1+ci.009", ReleasedAt: "2026-07-01T00:00:00Z", DeprecationNoticeAt: "2026-08-31T00:00:00Z", SunsetAt: "2026-11-30T00:00:00Z"},
			{Version: "1.2.3-beta.11", ReleasedAt: "2026-06-01T00:00:00Z", DeprecationNoticeAt: "2026-08-31T00:00:00Z", SunsetAt: "2026-11-30T00:00:00Z"},
		}
		raw, err := json.Marshal(releases)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("PINVOU_CLIENT_RELEASE_MANIFEST", string(raw))
		t.Setenv("PINVOU_API_CONTRACT_VERSION", "account-platform-v1")
		t.Setenv("PINVOU_CLIENT_UPGRADE_URL", "https://updates.example.invalid/latest")
		policy, err := clientcompat.FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		manifest := policy.WithNow(func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }).Manifest()
		readiness.config.Compatibility = manifest
		metadata := readiness.Compatibility()
		if metadata.CurrentClient != releases[0].Version || metadata.MaximumClient != releases[0].Version || metadata.MinimumClient != releases[2].Version || !reflect.DeepEqual(metadata.Releases, releases) {
			t.Fatal("actual environment policy to readiness metadata lost exact original versions")
		}
	})
}
