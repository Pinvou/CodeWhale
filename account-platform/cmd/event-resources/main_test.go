package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/nats-io/nkeys"
)

func publicFixture(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	keys := map[string]string{}
	for _, role := range []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		public, err := pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal(err)
		}
		keys[role] = public
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "purpose": "pinvou-nats-service-identities", "public_keys": keys})
	path := filepath.Join(dir, name)
	if os.WriteFile(path, b, 0600) != nil {
		t.Fatal("fixture public artifact")
	}
	digest := sha256.Sum256(b)
	return path, hex.EncodeToString(digest[:])
}

func TestRenderModeNeverAccessesWorkloadIdentityOrNetwork(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	public, digest := publicFixture(t, dir, "current.json")
	previous, previousDigest := publicFixture(t, dir, "previous.json")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			c.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	t.Setenv("NATS_URL", "tls://"+listener.Addr().String())
	t.Setenv("NATS_NKEY_SEED_FILE", filepath.Join(dir, "unreadable.seed"))
	t.Setenv("NATS_CREDENTIALS_FILE", filepath.Join(dir, "unreadable.creds"))
	var envReads, managerCalls int
	environment := func(string) string { envReads++; t.Error("offline branch read workload environment"); return "" }
	manage := func(context.Context, string, events.NATSConfig, bool) error {
		managerCalls++
		t.Error("offline branch invoked network manager")
		return errArguments
	}
	output := filepath.Join(dir, "authorization.json")
	base := []string{"-render-authorization", "-public-keys", public, "-public-keys-sha256", digest, "-output", output}
	if result, err := run(base, environment, manage); err != nil || result != "NATS_PUBLIC_AUTHORIZATION_RENDERED" {
		t.Fatal("offline render failed")
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(base, environment, manage); err != errRender {
		t.Fatal("existing output overwritten")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing output changed")
	}
	for _, args := range [][]string{
		append(append([]string{}, base...), "-create-only"),
		append(append([]string{}, base...), "-previous-public-keys", previous, "-previous-public-keys-sha256", previousDigest),
		{"-render-authorization", "-public-keys", public, "-public-keys-sha256", strings.Repeat("0", 64), "-output", filepath.Join(dir, "bad.json")},
		{"-render-authorization", "-public-keys", public, "-public-keys-sha256", digest, "-output", filepath.Join(dir, "bad.json"), "-overlap"},
		{"-public-keys", public},
		{"-unknown"},
	} {
		if _, err := run(args, environment, manage); err == nil {
			t.Fatal("unsafe offline arguments accepted")
		}
	}
	overlap := []string{"-render-authorization", "-public-keys", public, "-public-keys-sha256", digest, "-output", filepath.Join(dir, "overlap.json"), "-overlap", "-previous-public-keys", previous, "-previous-public-keys-sha256", previousDigest}
	if _, err := run(overlap, environment, manage); err != nil {
		t.Fatal("explicit overlap failed")
	}
	if envReads != 0 || managerCalls != 0 || connections.Load() != 0 {
		t.Fatal("offline side effect")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Fatal("failed preflight created output")
	}
}

func TestResourceOnlineModeRemainsExplicit(t *testing.T) {
	for _, args := range [][]string{nil, {"-create-only"}} {
		called := 0
		result, err := run(args, func(name string) string { return name }, func(_ context.Context, url string, cfg events.NATSConfig, create bool) error {
			called++
			if url != "NATS_URL" || cfg.Environment != "production" || cfg.CAFile != "NATS_CA_FILE" || create != (len(args) > 0) {
				t.Fatal("online options changed")
			}
			return nil
		})
		if err != nil || called != 1 || result != "NATS_FIXED_RESOURCES_VERIFIED" {
			t.Fatal("online mode regressed")
		}
	}
}

func TestPublicArtifactAndOutputSafety(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	file, digest := publicFixture(t, dir, "public.json")
	for _, path := range []string{"relative", dir, filepath.Join(dir, "missing")} {
		if _, err := readPublicArtifact(path, digest); err != errRender {
			t.Fatal("unsafe public path")
		}
	}
	if renderAuthorization(file, digest, "", "", "relative") != errRender {
		t.Fatal("relative output")
	}
	if renderAuthorization(file, digest, "", "", filepath.Join(dir, "missing", "output")) != errRender {
		t.Fatal("missing parent")
	}
	large := filepath.Join(dir, "large.json")
	os.WriteFile(large, bytes.Repeat([]byte("x"), (32<<10)+1), 0600)
	if _, err := readPublicArtifact(large, digest); err != errRender {
		t.Fatal("unbounded public input")
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.Chmod(file, 0660)
	if _, err := readPublicArtifact(file, digest); err != errRender {
		t.Fatal("replaceable public artifact")
	}
	os.Chmod(file, 0600)
	link := filepath.Join(dir, "link.json")
	if os.Symlink(file, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := readPublicArtifact(link, digest); err != errRender {
		t.Fatal("symlink public artifact")
	}
	os.Chmod(dir, 0755)
	if renderAuthorization(file, digest, "", "", filepath.Join(dir, "unsafe-output.json")) != errRender {
		t.Fatal("unsafe output parent")
	}
}
