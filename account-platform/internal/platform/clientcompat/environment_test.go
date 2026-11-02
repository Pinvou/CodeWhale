package clientcompat

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func clearCompatibilityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PINVOU_API_CONTRACT_VERSION", "PINVOU_CLIENT_UPGRADE_URL", "PINVOU_CLIENT_RELEASE_MANIFEST"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFromEnvUsesExistingDefaultsWhenUnconfigured(t *testing.T) {
	clearCompatibilityEnv(t)
	policy, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if policy.ContractVersion != "account-platform-v1" || policy.UpgradeURL != "https://github.com/Pinvou/pinvou3/releases/latest" {
		t.Fatalf("unexpected default contract or URL: %#v", policy)
	}
	var releases []Release
	if err := json.Unmarshal([]byte(defaultReleaseManifest), &releases); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Releases, releases) {
		t.Fatalf("unconfigured policy did not retain the built-in manifest: %#v", policy.Releases)
	}
}

func TestFromEnvConsumesExplicitValidManifest(t *testing.T) {
	clearCompatibilityEnv(t)
	releases := testPolicy(t).Releases
	raw, err := json.Marshal(releases)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PINVOU_API_CONTRACT_VERSION", " fixture-v2 ")
	t.Setenv("PINVOU_CLIENT_UPGRADE_URL", " https://updates.example.test/explicit ")
	t.Setenv("PINVOU_CLIENT_RELEASE_MANIFEST", " \n"+string(raw)+"\n ")
	policy, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if policy.ContractVersion != "fixture-v2" || policy.UpgradeURL != "https://updates.example.test/explicit" || !reflect.DeepEqual(policy.Releases, releases) {
		t.Fatalf("explicit configuration was not consumed: %#v", policy)
	}
}

func TestFromEnvRejectsMalformedOrInvalidExplicitManifest(t *testing.T) {
	for _, raw := range []string{"{", "{}", "null", "[]"} {
		t.Run(raw, func(t *testing.T) {
			clearCompatibilityEnv(t)
			t.Setenv("PINVOU_CLIENT_RELEASE_MANIFEST", raw)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("explicit invalid manifest %q was accepted", raw)
			}
		})
	}
}
