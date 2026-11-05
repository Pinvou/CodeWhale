package workload

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParentAliasPreservesProjectionIsolation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(root, "real")
	for _, directory := range []string{realRoot, filepath.Join(realRoot, "bootstrap"), filepath.Join(realRoot, "projected"), filepath.Join(realRoot, "projected", "nested")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink unavailable on this host: %v", err)
		}
		t.Fatal(err)
	}
	config := Config{
		Issuer: "https://fixture.example/realms/workload", Audience: "fixture-api",
		ClientID: "fixture-client", Subject: "fixture-service",
		ClientSecretFile: filepath.Join(realRoot, "bootstrap", "secret"),
		TokenFile:        filepath.Join(alias, "projected", "token"),
	}
	if err := config.validate(); err != nil {
		t.Fatalf("safe parent alias rejected: %v", err)
	}
	if err := publish(config.TokenFile, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := publish(config.TokenFile, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(realRoot, "projected", "token")); err != nil || string(body) != "second" {
		t.Fatalf("alias projection did not replace the real file: %q, %v", body, err)
	}
	for _, nested := range []string{"", "nested"} {
		for _, reverse := range []bool{false, true} {
			unsafe := config
			secretRoot, tokenRoot := realRoot, alias
			if reverse {
				secretRoot, tokenRoot = alias, realRoot
			}
			unsafe.ClientSecretFile = filepath.Join(secretRoot, "projected", nested, "secret")
			unsafe.TokenFile = filepath.Join(tokenRoot, "projected", "token")
			if err := unsafe.validate(); err == nil {
				t.Fatal("parent alias exposed a bootstrap secret through the projected directory")
			}
		}
	}
	leafAlias := filepath.Join(root, "leaf-alias")
	if err := os.Symlink(filepath.Join(realRoot, "projected"), leafAlias); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(leafAlias); err == nil {
		t.Fatal("symlink at the private directory itself was accepted")
	}
}
