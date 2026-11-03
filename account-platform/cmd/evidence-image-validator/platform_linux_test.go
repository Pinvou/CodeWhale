//go:build linux

package main

import (
	"fmt"
	"image"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSecureSocketPath(t *testing.T) {
	// /tmp is deliberately rejected by the production ancestor policy. Use a
	// private fixture under the physical home, whose ancestry must also pass.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(home, ".piv-path-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	dir := filepath.Join(base, "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "validator.sock")
	if os.Geteuid() == 0 {
		if secureSocketPath(path) {
			t.Fatal("root-owned worker directory accepted")
		}
		t.Log("root refusal verified; owner and unsafe-path cases require a non-root Linux run")
		return
	}
	assertSafe := func(t *testing.T) {
		t.Helper()
		if !secureSocketPath(path) {
			t.Fatal("positive control rejected: fixture needs worker-owned, protected physical home ancestry")
		}
	}
	assertSafe(t)
	for _, candidate := range []string{"validator.sock", dir + "/./validator.sock", filepath.Join(base, "missing", "validator.sock")} {
		if secureSocketPath(candidate) {
			t.Fatalf("invalid or absent parent path accepted: %s", candidate)
		}
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if secureSocketPath(filepath.Join(link, "validator.sock")) {
		t.Fatal("symlinked socket parent accepted")
	}
	for _, tc := range []struct {
		name string
		dir  string
		mode os.FileMode
	}{
		{"writable leaf group", dir, 0720},
		{"readable leaf other", dir, 0704},
		{"writable ancestor group", base, 0720},
		{"writable ancestor other", base, 0702},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSafe(t)
			if err := os.Chmod(tc.dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Chmod(tc.dir, 0700); err != nil {
					t.Error(err)
				}
			}()
			if secureSocketPath(path) {
				t.Fatal("unsafe permissions accepted")
			}
		})
	}
	assertSafe(t)
	if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if secureSocketPath(path) {
		t.Fatal("existing endpoint accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertSafe(t)
}

func TestLinuxPeerUIDAndHalfClose(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "piv-peer-")
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
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if allowedPeer(server, uid+1) || allowedPeer(server, 0) {
		t.Fatal("wrong or root UID accepted")
	}
	if uid != 0 && !allowedPeer(server, uid) {
		t.Fatal("exact peer UID rejected")
	}
	marker := filepath.Join(t.TempDir(), "child-started")
	sleeper := buildBinary(t, fmt.Sprintf(`package main
import ("os";"time")
func main(){_ = os.WriteFile(%q,[]byte("ready"),0600);time.Sleep(60*time.Second)}`, marker))
	done := make(chan struct{})
	go func() { handle(server, sleeper); close(done) }()
	data := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	if _, err = client.Write(frame(1, data)); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("half-close failed to cancel child")
	}
}
