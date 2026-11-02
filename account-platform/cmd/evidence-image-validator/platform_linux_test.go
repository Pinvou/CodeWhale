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
