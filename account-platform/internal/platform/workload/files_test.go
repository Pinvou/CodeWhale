package workload

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectionAtomicReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows open-file sharing denies concurrent rename; Linux production atomicity is verified separately")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "token")
	first, second := strings.Repeat("a", maximumToken), strings.Repeat("b", maximumToken)
	if err := publish(path, []byte(first)); err != nil {
		t.Fatal(err)
	}
	done, result := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				result <- nil
				return
			default:
			}
			body, err := os.ReadFile(path)
			if err != nil {
				result <- err
				return
			}
			if string(body) != first && string(body) != second {
				result <- os.ErrInvalid
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		value := first
		if i%2 == 0 {
			value = second
		}
		if err := publish(path, []byte(value)); err != nil {
			close(done)
			<-result
			t.Fatal(err)
		}
	}
	close(done)
	if err := <-result; err != nil {
		t.Fatal("reader observed incomplete projection", err)
	}
}

func TestPrivateFilesAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission and symlink assertions; production runs Linux")
	}
	f := newFixture(t)
	if err := os.Chmod(f.agent.config.ClientSecretFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agent.Refresh(context.Background()); err == nil {
		t.Fatal("public bootstrap file accepted")
	}
	if err := os.Chmod(f.agent.config.ClientSecretFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.agent.config.ClientSecretFile, f.agent.config.TokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agent.Refresh(context.Background()); err == nil {
		t.Fatal("symlink destination accepted")
	}
	if err := os.Remove(f.agent.config.TokenFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(f.agent.config.TokenFile), 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agent.Refresh(context.Background()); err == nil {
		t.Fatal("group-writable projection directory accepted")
	}
}
