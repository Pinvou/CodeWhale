package events

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func natsFixtureSeed(t *testing.T, directory, role string) (string, string) {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal("fixture identity generation")
	}
	defer pair.Wipe()
	seed, err := pair.Seed()
	if err != nil {
		t.Fatal("fixture seed")
	}
	defer clear(seed)
	public, err := pair.PublicKey()
	if err != nil {
		t.Fatal("fixture public key")
	}
	file := filepath.Join(directory, role+".seed")
	if os.WriteFile(file, seed, 0600) != nil {
		t.Fatal("fixture seed write")
	}
	return file, public
}

func TestNATSUserSeedCallbacksAndBinding(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0700)
	file, public := natsFixtureSeed(t, directory, "first")
	options, err := natsAuthentication("tls://localhost:4222", file, "")
	if err != nil || len(options) != 1 {
		t.Fatal("valid identity rejected")
	}
	var cfg nats.Options
	if options[0](&cfg) != nil || cfg.Nkey != public || cfg.SignatureCB == nil {
		t.Fatal("NKey option binding")
	}
	verifier, err := nkeys.FromPublicKey(public)
	if err != nil {
		t.Fatal("fixture verifier")
	}
	defer verifier.Wipe()
	nonce := []byte("isolated-test-challenge")
	for range 2 {
		signature, err := cfg.SignatureCB(nonce)
		if err != nil || verifier.Verify(nonce, signature) != nil {
			t.Fatal("challenge signature invalid")
		}
	}
	original, _ := os.ReadFile(file)
	defer clear(original)
	other, _ := natsFixtureSeed(t, directory, "second")
	replacement, _ := os.ReadFile(other)
	defer clear(replacement)
	os.WriteFile(file, replacement, 0600)
	if signature, err := cfg.SignatureCB(nonce); err != ErrNATSResources || len(signature) != 0 {
		t.Fatal("replacement identity signed for stale public key")
	}
	os.WriteFile(file, original, 0600)
	if signature, err := cfg.SignatureCB(nonce); err != nil || verifier.Verify(nonce, signature) != nil {
		t.Fatal("same identity reload rejected")
	}
	os.WriteFile(file, []byte("untrusted-private-input"), 0600)
	if signature, err := cfg.SignatureCB(nonce); err != ErrNATSResources || len(signature) != 0 || strings.Contains(err.Error(), "untrusted-private-input") {
		t.Fatal("unsafe callback error")
	}
}

func TestNATSIdentityModesAndLegacyJWT(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0700)
	file, _ := natsFixtureSeed(t, directory, "user")
	for _, raw := range []string{"tls://user:password@localhost:4222", "tls://token@localhost:4222", "tls://@localhost:4222", "tls://localhost:4222,tls://token@localhost:4223", "tls://%zz@localhost:4222", "user:password@localhost:4222", "token@localhost:4222", "localhost:4222,token@localhost:4223", "us%65r:password@localhost:4222", "tls://tok%65n@localhost:4222"} {
		if _, err := natsAuthentication(raw, file, ""); err != ErrNATSResources {
			t.Fatal("mixed URL/NKey identity accepted")
		}
		if _, err := natsAuthentication(raw, "", file); err != ErrNATSResources {
			t.Fatal("mixed URL/JWT identity accepted")
		}
	}
	if _, err := natsAuthentication("tls://localhost:4222", file, file); err != ErrNATSResources {
		t.Fatal("mixed files accepted")
	}
	if options, err := natsAuthentication("nats://user:password@localhost:4222", "", ""); err != nil || len(options) != 0 {
		t.Fatal("legacy password path regressed")
	}
	// Exercise JWT and signature callbacks, without
	// claiming this unsigned fixture is a trusted server-issued JWT.
	seed, _ := os.ReadFile(file)
	defer clear(seed)
	credentials := filepath.Join(directory, "legacy.creds")
	content := append([]byte("-----BEGIN NATS USER JWT-----\neyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJmaXh0dXJlIn0.c2ln\n------END NATS USER JWT------\n-----BEGIN USER NKEY SEED-----\n"), seed...)
	content = append(content, []byte("\n------END USER NKEY SEED------\n")...)
	defer clear(content)
	os.WriteFile(credentials, content, 0600)
	options, err := natsAuthentication("tls://localhost:4222", "", credentials)
	if err != nil || len(options) != 1 {
		t.Fatal("legacy JWT selection regressed")
	}
	var cfg nats.Options
	if options[0](&cfg) != nil || cfg.UserJWT == nil || cfg.SignatureCB == nil || cfg.Nkey != "" {
		t.Fatal("legacy JWT options regressed")
	}
	if value, err := cfg.UserJWT(); err != nil || value != "eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJmaXh0dXJlIn0.c2ln" {
		t.Fatal("legacy JWT callback regressed")
	}
	if signature, err := cfg.SignatureCB([]byte("fixture")); err != nil || len(signature) == 0 {
		t.Fatal("legacy JWT signature callback regressed")
	}
	for _, raw := range []string{"localhost:4222", "localhost:4222,localhost:4223"} {
		options, err := natsAuthentication(raw, "", credentials)
		var legacy nats.Options
		if err != nil || len(options) != 1 || options[0](&legacy) != nil || legacy.UserJWT == nil {
			t.Fatal("identity-free bare server JWT compatibility regressed")
		}
	}
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	t.Setenv("NATS_NKEY_SEED_FILE", file)
	if conn, _, err := connectNATS("nats://localhost:4222", "fixture", NATSConfig{Environment: "development"}); err != ErrNATSResources {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("NKey plaintext accepted")
	}
}

func TestNATSCredentialsRecheckFilesAndIdentity(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("fixture permissions")
	}
	write := func(seedFile, destination string) []byte {
		t.Helper()
		seed, err := os.ReadFile(seedFile)
		if err != nil {
			t.Fatal("fixture seed read")
		}
		defer clear(seed)
		content := append([]byte("-----BEGIN NATS USER JWT-----\neyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJmaXh0dXJlIn0.c2ln\n------END NATS USER JWT------\n-----BEGIN USER NKEY SEED-----\n"), seed...)
		content = append(content, []byte("\n------END USER NKEY SEED------\n")...)
		if err := os.WriteFile(destination, content, 0600); err != nil {
			clear(content)
			t.Fatal("fixture credentials write")
		}
		return content
	}
	seed, public := natsFixtureSeed(t, directory, "first")
	file := filepath.Join(directory, "user.creds")
	original := write(seed, file)
	defer clear(original)
	t.Chdir(directory)
	for _, path := range []string{"user.creds", directory, filepath.Join(directory, "missing")} {
		if _, err := natsAuthentication("tls://localhost:4222", "", path); err != ErrNATSResources {
			t.Fatal("unsafe credentials path accepted")
		}
	}
	options, err := natsAuthentication("tls://localhost:4222", "", file)
	if err != nil || len(options) != 1 {
		t.Fatal("valid credentials rejected")
	}
	var cfg nats.Options
	if options[0](&cfg) != nil || cfg.UserJWT == nil || cfg.SignatureCB == nil {
		t.Fatal("credentials callback missing")
	}
	verifier, err := nkeys.FromPublicKey(public)
	if err != nil {
		t.Fatal("fixture verifier")
	}
	defer verifier.Wipe()
	nonce := []byte("controlled-credentials-challenge")
	checkValid := func() {
		t.Helper()
		if jwt, err := cfg.UserJWT(); err != nil || jwt == "" {
			t.Fatal("same identity JWT rejected")
		}
		if signature, err := cfg.SignatureCB(nonce); err != nil || verifier.Verify(nonce, signature) != nil {
			t.Fatal("same identity signature rejected")
		}
	}
	checkRejected := func() {
		t.Helper()
		if jwt, err := cfg.UserJWT(); err != ErrNATSResources || jwt != "" {
			t.Fatal("unsafe JWT callback accepted")
		}
		if signature, err := cfg.SignatureCB(nonce); err != ErrNATSResources || len(signature) != 0 {
			t.Fatal("unsafe signature callback accepted")
		}
	}
	checkValid()
	other, _ := natsFixtureSeed(t, directory, "second")
	replacement := write(other, file)
	clear(replacement)
	checkRejected()
	for _, bad := range [][]byte{nil, []byte("private-invalid-credentials"), bytes.Repeat([]byte("x"), 64*1024+1)} {
		if err := os.WriteFile(file, bad, 0600); err != nil {
			t.Fatal("fixture overwrite")
		}
		checkRejected()
	}
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal("fixture restore")
	}
	checkValid()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal("fixture file permissions")
		}
		checkRejected()
		if err := os.Chmod(file, 0600); err != nil {
			t.Fatal("fixture file permissions")
		}
		if err := os.Chmod(directory, 0750); err != nil {
			t.Fatal("fixture parent permissions")
		}
		checkRejected()
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal("fixture parent permissions")
		}
		link := filepath.Join(directory, "linked.creds")
		if err := os.Symlink(file, link); err != nil {
			t.Fatal("fixture symlink")
		}
		if _, err := natsAuthentication("tls://localhost:4222", "", link); err != ErrNATSResources {
			t.Fatal("credentials symlink accepted")
		}
	}
}

func TestNATSAsyncDiagnosticsDoNotExposeRawErrors(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer slog.SetDefault(previous)
	reportNATSAsyncError(nil, nil, errors.New("fixture-private-identity-or-subject"))
	if !strings.Contains(output.String(), "NATS_ASYNC_OPERATION_FAILED") || strings.Contains(output.String(), "fixture-private") {
		t.Fatal("raw NATS diagnostic exposed")
	}
}

func TestNATSUserSeedRejectsUnsafeFilesAndTypes(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0700)
	file, _ := natsFixtureSeed(t, directory, "user")
	for _, data := range [][]byte{nil, []byte("bad-seed"), bytes.Repeat([]byte("x"), 4097)} {
		os.WriteFile(file, data, 0600)
		if pair, err := readNATSUserSeed(file); err != ErrNATSResources {
			if pair != nil {
				pair.Wipe()
			}
			t.Fatal("invalid seed accepted")
		}
	}
	for _, create := range []func() (nkeys.KeyPair, error){nkeys.CreateAccount, nkeys.CreateOperator, nkeys.CreateServer} {
		pair, err := create()
		if err != nil {
			t.Fatal("fixture type")
		}
		seed, _ := pair.Seed()
		os.WriteFile(file, seed, 0600)
		clear(seed)
		pair.Wipe()
		if pair, err := readNATSUserSeed(file); err != ErrNATSResources {
			if pair != nil {
				pair.Wipe()
			}
			t.Fatal("non-user seed accepted")
		}
	}
	for _, path := range []string{"relative.seed", directory, filepath.Join(directory, "missing")} {
		if pair, err := readNATSUserSeed(path); err != ErrNATSResources {
			if pair != nil {
				pair.Wipe()
			}
			t.Fatal("unsafe seed path accepted")
		}
	}
	file, _ = natsFixtureSeed(t, directory, "user")
	if runtime.GOOS == "windows" {
		return
	}
	for _, mode := range []os.FileMode{0640, 0644, 0620, 0601} {
		os.Chmod(file, mode)
		if pair, err := readNATSUserSeed(file); err != ErrNATSResources {
			if pair != nil {
				pair.Wipe()
			}
			t.Fatal("unsafe seed permissions accepted")
		}
	}
	os.Chmod(file, 0600)
	os.Chmod(directory, 0750)
	if pair, err := readNATSUserSeed(file); err != ErrNATSResources {
		if pair != nil {
			pair.Wipe()
		}
		t.Fatal("unsafe seed parent accepted")
	}
	os.Chmod(directory, 0700)
	link := filepath.Join(directory, "linked.seed")
	if os.Symlink(file, link) != nil {
		t.Fatal("fixture symlink")
	}
	if pair, err := readNATSUserSeed(link); err != ErrNATSResources {
		if pair != nil {
			pair.Wipe()
		}
		t.Fatal("symlink seed accepted")
	}
}
