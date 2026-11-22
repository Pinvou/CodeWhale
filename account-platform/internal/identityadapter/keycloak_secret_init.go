package identityadapter

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// StageKeycloakClientSecret is an init-container operation, not a relaxation of
// the runtime reader. Kubernetes projected inputs may use symlinks; the output
// must be a new, real, owner-only file in the private memory volume.
func StageKeycloakClientSecret(source, destination string) error {
	invalid := errors.New("Keycloak secret preparation failed")
	if !filepath.IsAbs(source) || filepath.Clean(source) != source ||
		!filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return invalid
	}
	parent := filepath.Dir(destination)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil || canonical != parent {
		return invalid
	}
	before, err := os.Stat(source)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 16<<10 {
		return invalid
	}
	input, err := os.Open(source)
	if err != nil {
		return invalid
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<10 {
		return invalid
	}
	body, err := io.ReadAll(io.LimitReader(input, (16<<10)+1))
	defer clear(body)
	if err != nil || len(body) > 16<<10 {
		return invalid
	}
	value := strings.TrimSpace(string(body))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return invalid
	}
	output, err := os.CreateTemp(parent, ".keycloak-secret-*")
	if err != nil {
		return invalid
	}
	defer func() {
		output.Close()
		os.Remove(output.Name())
	}()
	if _, err := io.WriteString(output, value); err != nil {
		return invalid
	}
	if err := output.Chmod(0400); err != nil {
		return invalid
	}
	if err := output.Close(); err != nil {
		return invalid
	}
	// Link publishes the complete file atomically and never replaces an existing
	// destination (including a symlink). Both names are in the same memory volume.
	if err := os.Link(output.Name(), destination); err != nil {
		return invalid
	}
	return nil
}
