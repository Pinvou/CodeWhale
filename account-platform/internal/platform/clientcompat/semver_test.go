package clientcompat

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSemVerSharedStandardVectors(t *testing.T) {
	var vectors struct {
		Spec        string   `json:"spec"`
		Valid       []string `json:"valid"`
		Invalid     []string `json:"invalid"`
		Comparisons []struct {
			Left, Right string
			Expected    int
		} `json:"comparisons"`
	}
	bytes, err := os.ReadFile("testdata/semver-vectors.json")
	if err != nil || json.Unmarshal(bytes, &vectors) != nil || vectors.Spec != "https://semver.org/spec/v2.0.0.html" || len(vectors.Valid) == 0 || len(vectors.Invalid) == 0 || len(vectors.Comparisons) == 0 {
		t.Fatal("shared SemVer standard vectors unavailable or empty", err)
	}
	for _, value := range vectors.Valid {
		t.Run("valid_"+value, func(t *testing.T) {
			if !ValidVersion(value) {
				t.Fatal("valid standard vector rejected")
			}
		})
	}
	for _, value := range vectors.Invalid {
		t.Run("invalid_"+value, func(t *testing.T) {
			if ValidVersion(value) {
				t.Fatal("invalid standard vector accepted")
			}
			if _, err := CompareVersions(value, "1.2.3"); err == nil {
				t.Fatal("invalid left operand did not return an error")
			}
			if _, err := CompareVersions("1.2.3", value); err == nil {
				t.Fatal("invalid right operand did not return an error")
			}
		})
	}
	for _, sample := range vectors.Comparisons {
		t.Run("compare_"+sample.Left+"_"+sample.Right, func(t *testing.T) {
			comparison, err := CompareVersions(sample.Left, sample.Right)
			if err != nil || comparison != sample.Expected {
				t.Fatalf("precedence=%d want=%d err=%v", comparison, sample.Expected, err)
			}
			reverse, err := CompareVersions(sample.Right, sample.Left)
			if err != nil || reverse != -sample.Expected {
				t.Fatal("reverse comparison violated precedence")
			}
		})
	}
}

func TestSemVerSortValidatedAtomicAndStable(t *testing.T) {
	values := []string{"1.0.0-rc.1", "1.0.0+second", "0.9.0", "1.0.0+first", "1.0.0-beta.11", "1.0.0-beta.2"}
	if err := SortVersions(values); err != nil {
		t.Fatal(err)
	}
	want := []string{"1.0.0+second", "1.0.0+first", "1.0.0-rc.1", "1.0.0-beta.11", "1.0.0-beta.2", "0.9.0"}
	if !reflect.DeepEqual(values, want) {
		t.Fatal("descending precedence or stable original build identity lost", values)
	}
	for _, invalidIndex := range []int{0, 1, 2} {
		t.Run(string(rune('A'+invalidIndex)), func(t *testing.T) {
			values := []string{"1.0.0", "3.0.0", "2.0.0"}
			values[invalidIndex] = "9.0.0-01"
			before := append([]string(nil), values...)
			if err := SortVersions(values); err == nil || !reflect.DeepEqual(values, before) {
				t.Fatal("invalid array did not fail before mutating any input")
			}
		})
	}
	if err := SortVersions(nil); err != nil {
		t.Fatal("empty input rejected", err)
	}
}

func TestSemVerHasNoMachineIntegerOrNewLengthLimit(t *testing.T) {
	number := strings.Repeat("9", 4096)
	left, right := number+".0.0-"+number, "1"+strings.Repeat("0", 4096)+".0.0-0"
	if !ValidVersion(left) || !ValidVersion(right) {
		t.Fatal("arbitrary precision numeric identifier rejected")
	}
	if comparison, err := CompareVersions(left, right); err != nil || comparison != -1 {
		t.Fatal("large core numeric precedence lost", comparison, err)
	}
	if comparison, err := CompareVersions("1.0.0-"+number, "1.0.0-1"+strings.Repeat("0", 4096)); err != nil || comparison != -1 {
		t.Fatal("large prerelease numeric precedence lost", comparison, err)
	}
}

func TestSemVerFromEnvPublishesOriginalReleaseIdentities(t *testing.T) {
	versions := [3]string{"1.2.3+stable.001", "1.2.3-rc.1+ci.009", "1.2.3-beta.11"}
	manifest, err := json.Marshal(semverPolicyReleases(versions))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PINVOU_CLIENT_RELEASE_MANIFEST", string(manifest))
	t.Setenv("PINVOU_API_CONTRACT_VERSION", "account-platform-v1")
	t.Setenv("PINVOU_CLIENT_UPGRADE_URL", "https://updates.example.invalid/latest")
	policy, err := FromEnv()
	if err != nil {
		t.Fatal("actual environment policy rejected full SemVer", err)
	}
	metadata := policy.Manifest()
	if metadata.CurrentClientVersion != versions[0] || !reflect.DeepEqual(metadata.Releases, semverPolicyReleases(versions)) {
		t.Fatal("actual compatibility metadata rewrote full release strings")
	}
}
