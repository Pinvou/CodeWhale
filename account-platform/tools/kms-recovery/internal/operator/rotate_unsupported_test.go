//go:build !linux && !windows

package operator

import (
	"os"
	"path/filepath"
	"testing"
)

// Exercise the actual unsupported lock through the public operation. No
// provider rotation, login or success record may precede a supported lock.
func TestRotationUnsupportedPlatformFailsClosed(t *testing.T) {
	f := newRotationFixture(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "unsupported-attempt")
	if err := f.rotate(output, 1); err != ErrRotatePreflight {
		t.Fatalf("unsupported rotation must fail closed: %v", err)
	}
	if f.rotations != 0 || f.logins != 0 || f.rootRequests != 0 || len(f.tokens) != 0 {
		t.Fatal("unsupported operation obtained a token or called the provider")
	}
	for _, name := range []string{"ROTATION_ATTEMPT", "ROTATION_VERIFIED", "CURRENT_INVOCATION"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Fatalf("unsupported operation persisted %s: %v", name, err)
		}
	}
}
