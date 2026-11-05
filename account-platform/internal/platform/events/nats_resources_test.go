package events

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNATSPrivateCARootsRejectUnsafeInputs(t *testing.T) {
	ca, leaf, key := natsFixtureTLS(t)
	root := t.TempDir()
	for _, item := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"valid", ca, true}, {"empty", nil, false}, {"garbage", []byte("not a certificate"), false},
		{"leaf", leaf, false}, {"key", key, false},
		{"prefix", append([]byte("garbage\n"), ca...), false},
		{"suffix", append(append([]byte{}, ca...), key...), false},
		{"oversized", make([]byte, (1<<20)+1), false},
	} {
		file := filepath.Join(root, item.name)
		if os.WriteFile(file, item.body, 0600) != nil {
			t.Fatal("CA fixture write failed")
		}
		_, err := natsRoots(file)
		if item.valid != (err == nil) {
			t.Fatalf("CA reader wrong result: %s", item.name)
		}
	}
	if _, err := natsRoots("relative.crt"); err == nil {
		t.Fatal("relative CA path accepted")
	}
	if runtime.GOOS != "windows" {
		file := filepath.Join(root, "valid")
		if os.Chmod(file, 0666) != nil {
			t.Fatal("CA mode setup failed")
		}
		if _, err := natsRoots(file); err == nil {
			t.Fatal("writable CA accepted")
		}
		os.Chmod(file, 0600)
		link := filepath.Join(root, "linked")
		if os.Symlink(file, link) != nil {
			t.Fatal("CA symlink setup failed")
		}
		if _, err := natsRoots(link); err == nil {
			t.Fatal("symlink CA accepted")
		}
	}
}

func TestNATSResourceModeCannotDowngradeProduction(t *testing.T) {
	for _, environment := range []string{"production", "production-new", " PRODUCTION-uat "} {
		for _, mode := range []string{"", "provisioned", "auto", "unknown"} {
			provisioned, err := (NATSConfig{Environment: environment, ResourceMode: mode}).provisioned()
			allowed := mode == "" || mode == "provisioned"
			if allowed != (err == nil) || (allowed && !provisioned) {
				t.Fatal("production provisioning mode bypass")
			}
		}
	}
	if provisioned, err := (NATSConfig{Environment: "development"}).provisioned(); err != nil || provisioned {
		t.Fatal("development behavior changed")
	}
}
