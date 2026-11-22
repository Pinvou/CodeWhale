//go:build linux

package billing

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEvidenceImageValidatorMissingAndStaleSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "piv-dial-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(directory, "validator.sock")
	if len([]byte(path)) >= 108 {
		t.Fatal("Unix socket fixture exceeds the Linux path limit")
	}
	workerUID := uint32(1)
	if uint32(os.Geteuid()) == workerUID {
		workerUID = 2
	}
	v, err := newEvidenceImageValidator(path, workerUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(context.Background(), "image/png", []byte("original")); err != errImageValidatorUnavailable {
		t.Fatalf("missing socket did not fail closed: %v", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("fixture must retain a stale socket after closing its listener")
	}
	if err := v.Validate(context.Background(), "image/png", []byte("original")); err != errImageValidatorUnavailable {
		t.Fatalf("stale socket did not fail closed: %v", err)
	}
}

func TestEvidenceImageValidatorRejectsUntrustedLinuxPeer(t *testing.T) {
	if validImageValidatorUID(0) || validImageValidatorUID(uint32(os.Geteuid())) {
		t.Fatal("root or own UID accepted")
	}
	// Linux Unix socket names are limited to 108 bytes. RUNNER_TEMP and the
	// test name can already exceed that budget before the socket is appended.
	directory, err := os.MkdirTemp("/tmp", "piv-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(directory, "peer.sock")
	if len([]byte(path)) >= 108 {
		t.Fatal("Unix socket fixture exceeds the Linux path limit")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if verifyImageValidatorPeer(client, 0) || verifyImageValidatorPeer(client, uint32(os.Geteuid())) || verifyImageValidatorPeer(client, uint32(os.Geteuid())+1) {
		t.Fatal("wrong worker peer accepted")
	}
}
