package identityadapter

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestStageKeycloakClientSecret(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "projected")
	if err := os.WriteFile(source, []byte(fixtureProvisionerSecret+"\n"), 0440); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "client-secret")
	if err := StageKeycloakClientSecret(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0400) {
		t.Fatal("staged file must be real and owner-only", err)
	}
	value, err := readKeycloakFile(destination, 16<<10, true)
	if err != nil || string(value) != fixtureProvisionerSecret {
		t.Fatal("strict runtime reader rejected prepared file", err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil || os.SameFile(sourceInfo, info) {
		t.Fatal("projected source must not become the runtime file", err)
	}
	original, err := os.ReadFile(source)
	if err != nil || string(original) != fixtureProvisionerSecret+"\n" {
		t.Fatal("projected source was modified", err)
	}
	server, ca := keycloakTestTLS(t, false, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	ca, err = filepath.EvalSymlinks(ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewKeycloakDirectoryWithConfig(KeycloakConnectionConfig{
		AdminURL: server.URL, TokenURL: server.URL + "/realms/pinvou/protocol/openid-connect/token",
		Realm: "pinvou", ClientID: "provisioner", SecretFile: destination, CAFile: ca, Production: true,
	}); err != nil {
		t.Fatal("prepared input did not satisfy production startup", err)
	}
	if err := StageKeycloakClientSecret(source, destination); err == nil {
		t.Fatal("existing destination overwritten")
	}
	after, err := os.Lstat(destination)
	if err != nil || !os.SameFile(info, after) {
		t.Fatal("existing destination changed", err)
	}
	entries, err := filepath.Glob(filepath.Join(directory, ".keycloak-secret-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary secrets left behind", err)
	}
}

func TestStageKeycloakClientSecretRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""}, {"whitespace", " \n"}, {"newline", "first\nsecond"},
		{"nul", "first\x00second"}, {"oversized", strings.Repeat("s", (16<<10)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source, destination := filepath.Join(directory, "source"), filepath.Join(directory, "destination")
			if err := os.WriteFile(source, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := StageKeycloakClientSecret(source, destination); err == nil {
				t.Fatal("invalid secret accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid input published a destination")
			}
		})
	}
}

func TestStageKeycloakClientSecretPathBoundaries(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "source")
	if err := os.WriteFile(source, []byte(fixtureProvisionerSecret), 0600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{
		{"relative", filepath.Join(directory, "target")}, {source, "relative"},
		{directory, filepath.Join(directory, "target")}, {source, filepath.Join(directory, "absent", "target")},
	} {
		if err := StageKeycloakClientSecret(paths[0], paths[1]); err == nil {
			t.Fatal("invalid path accepted")
		}
	}
	if err := StageKeycloakClientSecret(source, directory); err == nil {
		t.Fatal("destination directory replaced")
	}
}

func TestStageKeycloakClientSecretProjectedSymlink(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, projected := filepath.Join(directory, "source"), filepath.Join(directory, "projected")
	if err := os.WriteFile(source, []byte(fixtureProvisionerSecret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, projected); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("file symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	if err := StageKeycloakClientSecret(projected, filepath.Join(directory, "real-output")); err != nil {
		t.Fatal("projected source rejected", err)
	}
	if err := StageKeycloakClientSecret(source, projected); err == nil {
		t.Fatal("destination symlink replaced")
	}
	if _, err := readKeycloakFile(projected, 16<<10, true); err == nil {
		t.Fatal("runtime symlink rejection relaxed")
	}
}
