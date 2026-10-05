package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCredentialDeliverySupportsControlledPipeAndRejectsStandardStreams(t *testing.T) {
	for _, file := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if err := writeSecret(int(file.Fd()), "synthetic-one-time-credential"); err == nil {
			t.Fatal("standard streams must not receive bootstrap credentials")
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := writeSecret(int(w.Fd()), "synthetic-one-time-credential"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("synthetic-one-time-credential\n"))
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "synthetic-one-time-credential\n" {
		t.Fatalf("pipe delivery length=%d err=%v", len(buf), err)
	}
	runtime.KeepAlive(w)
}

func TestControlledPasswordDescriptorRequiresExactlyTwoUnchangedLines(t *testing.T) {
	for _, body := range []string{"Exact Middle 123\nExact Middle 123\n", "Exact Middle 123\nDifferent 456\n", "Exact Middle 123\nExact Middle 123\nextra\n"} {
		file, err := os.Create(filepath.Join(t.TempDir(), "owned-password-input"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := io.WriteString(file, body); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		password, err := readConfirmedPassword(int(file.Fd()))
		valid := body == "Exact Middle 123\nExact Middle 123\n"
		if valid && (err != nil || password != "Exact Middle 123") {
			t.Fatalf("exact password changed: err=%v", err)
		}
		if !valid && err == nil {
			t.Fatal("malformed confirmation accepted")
		}
		runtime.KeepAlive(file)
	}
}
