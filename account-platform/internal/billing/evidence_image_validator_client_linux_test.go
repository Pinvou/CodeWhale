//go:build linux

package billing

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEvidenceImageValidatorRejectsUntrustedLinuxPeer(t *testing.T) {
	if validImageValidatorUID(0) || validImageValidatorUID(uint32(os.Geteuid())) {
		t.Fatal("root or own UID accepted")
	}
	path := filepath.Join(t.TempDir(), "peer.sock")
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
