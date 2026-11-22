package events

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func stagedCredentialsFixture(t *testing.T, directory string) ([]byte, string) {
	t.Helper()
	seed, public := natsFixtureSeed(t, directory, "init-user")
	body, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal("read controlled seed")
	}
	defer clear(body)
	// This document exercises encoding and the challenge-response callback, not
	// trust in a server-issued account JWT or a real broker connection.
	return []byte("-----BEGIN NATS USER JWT-----\neyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJmaXh0dXJlIn0.c2ln\n------END NATS USER JWT------\n-----BEGIN USER NKEY SEED-----\n" + string(body) + "\n------END USER NKEY SEED------\n"), public
}

// This is a controlled loopback NATS handshake peer, not a real JetStream server
// or account/JWT trust acceptance. It proves that connectNATS actually sends the
// staged credential and signs the peer nonce over a verified TLS connection.
func TestStageNATSCredentialsConnectsThroughStrictTLSReader(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, public := stagedCredentialsFixture(t, directory)
	defer clear(body)
	source, destination := filepath.Join(directory, "projection"), filepath.Join(directory, "private", "runtime.creds")
	if err := os.WriteFile(source, body, 0440); err != nil {
		t.Fatal(err)
	}
	if err := StageNATSCredentials(source, destination); err != nil {
		t.Fatal("prepare controlled credential")
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(directory, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan error, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		observed <- func() error {
			raw, err := listener.Accept()
			if err != nil {
				return errors.New("controlled accept failed")
			}
			defer raw.Close()
			raw.SetDeadline(time.Now().Add(5 * time.Second))
			const nonce = "controlled-init-reader-nonce"
			if _, err := fmt.Fprintf(raw, "INFO {\"server_id\":\"fixture\",\"version\":\"2.11.0\",\"proto\":1,\"tls_required\":true,\"auth_required\":true,\"nonce\":\"%s\",\"max_payload\":1048576}\r\n", nonce); err != nil {
				return errors.New("controlled INFO failed")
			}
			secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS12,
				Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
			if err := secure.Handshake(); err != nil {
				return errors.New("controlled TLS failed")
			}
			reader := bufio.NewReader(secure)
			connected := false
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return errors.New("controlled protocol read failed")
				}
				if strings.HasPrefix(line, "CONNECT ") {
					var identity struct{ JWT, Sig string }
					if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))), &identity) != nil || identity.JWT != "eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJmaXh0dXJlIn0.c2ln" {
						return errors.New("controlled JWT mismatch")
					}
					signature, err := base64.RawURLEncoding.DecodeString(identity.Sig)
					verifier, verifyErr := nkeys.FromPublicKey(public)
					if verifyErr != nil {
						return errors.New("controlled public key failed")
					}
					defer verifier.Wipe()
					if err != nil || verifier.Verify([]byte(nonce), signature) != nil {
						return errors.New("controlled nonce signature mismatch")
					}
					connected = true
				}
				if strings.TrimSpace(line) == "PING" {
					if !connected {
						return errors.New("controlled CONNECT missing")
					}
					_, err := secure.Write([]byte("PONG\r\n"))
					if err == nil {
						<-release
					}
					return err
				}
			}
		}()
	}()
	connection, _, err := connectNATS("tls://"+listener.Addr().String(), "prepared-credential-test", NATSConfig{
		ResourceMode: "provisioned", RequireExplicitAuthentication: true, CredentialsFile: destination, CAFile: ca})
	if err != nil {
		t.Fatal("strict connection could not use prepared credential")
	}
	connection.Close()
	close(release)
	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("controlled handshake did not complete")
	}
}

func TestStageNATSCredentialsPreservesDocumentAndStrictReader(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, public := stagedCredentialsFixture(t, directory)
	defer clear(body)
	source, destination := filepath.Join(directory, "projected.creds"), filepath.Join(directory, "private", "runtime.creds")
	if err := os.WriteFile(source, body, 0440); err != nil {
		t.Fatal(err)
	}
	if err := StageNATSCredentials(source, destination); err != nil {
		t.Fatal("prepare controlled credential", err)
	}
	info, err := os.Lstat(destination)
	parent, parentErr := os.Lstat(filepath.Dir(destination))
	if err != nil || parentErr != nil || !info.Mode().IsRegular() || !parent.IsDir() ||
		(runtime.GOOS != "windows" && (info.Mode().Perm() != 0400 || parent.Mode().Perm() != 0700)) {
		t.Fatal("runtime file/directory must be real and private")
	}
	copy, err := os.ReadFile(destination)
	defer clear(copy)
	if err != nil || !bytes.Equal(copy, body) {
		t.Fatal("multi-line credential changed")
	}
	options, err := natsAuthentication("tls://localhost:4222", "", destination)
	var cfg nats.Options
	if err != nil || len(options) != 1 || options[0](&cfg) != nil {
		t.Fatal("strict runtime reader rejected prepared file")
	}
	if jwt, err := cfg.UserJWT(); err != nil || jwt == "" {
		t.Fatal("JWT callback rejected prepared file")
	}
	verifier, err := nkeys.FromPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Wipe()
	nonce := []byte("controlled-init-reader-connection-challenge")
	signature, err := cfg.SignatureCB(nonce)
	if err != nil || verifier.Verify(nonce, signature) != nil {
		t.Fatal("prepared file could not sign the connection challenge")
	}
	if err := StageNATSCredentials(source, destination); err != nil {
		t.Fatal("same complete file must survive init retry")
	}
	after, _ := os.Lstat(destination)
	if !os.SameFile(info, after) {
		t.Fatal("retry replaced credential")
	}
	if err := os.Chmod(source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if StageNATSCredentials(source, destination) == nil {
		t.Fatal("changed projection replaced an existing snapshot")
	}
	entries, _ := filepath.Glob(filepath.Join(directory, "private", ".nats-credentials-*"))
	if len(entries) != 0 {
		t.Fatal("temporary credential remained")
	}
}

func TestStageNATSCredentialsRejectsInvalidDocument(t *testing.T) {
	for _, body := range []string{"", " \n", "not a credential", strings.Repeat("x", (64<<10)+1),
		"-----BEGIN NATS USER JWT-----\nfixture\n------END NATS USER JWT------\n"} {
		t.Run("invalid", func(t *testing.T) {
			directory, _ := filepath.EvalSymlinks(t.TempDir())
			source, destination := filepath.Join(directory, "source"), filepath.Join(directory, "private", "runtime.creds")
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if StageNATSCredentials(source, destination) != ErrNATSResources {
				t.Fatal("invalid projected credential accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid document published")
			}
		})
	}
}

func TestStageNATSCredentialsPathBoundaries(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	body, _ := stagedCredentialsFixture(t, directory)
	defer clear(body)
	source, destination := filepath.Join(directory, "source"), filepath.Join(directory, "private", "runtime.creds")
	if err := os.WriteFile(source, body, 0600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{{"relative", destination}, {source, "relative"}, {directory, destination},
		{source, filepath.Join(directory, "absent", "private", "runtime.creds")}} {
		if StageNATSCredentials(paths[0], paths[1]) != ErrNATSResources {
			t.Fatal("unsafe source or output path accepted")
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Mkdir(filepath.Dir(destination), 0755); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(destination), 0755); err != nil {
			t.Fatal(err)
		}
		if StageNATSCredentials(source, destination) != ErrNATSResources {
			t.Fatal("shared output directory accepted")
		}
	}
	for _, args := range [][]string{{"prepare-nats-credentials"}, {"prepare-nats-credentials", "other"},
		{"prepare-nats-credentials", "allowed", "extra"}, {"prepare-nats-credentials", "../../other"}} {
		if handled, err := PrepareNATSCredentials(args, "allowed"); !handled || err != ErrNATSResources {
			t.Fatal("init command escaped its fixed identity/path boundary")
		}
	}
	if handled, err := PrepareNATSCredentials(nil, "allowed"); handled || err != nil {
		t.Fatal("ordinary startup intercepted")
	}
}

func TestStageNATSCredentialsAcceptsOnlySourceProjection(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	body, _ := stagedCredentialsFixture(t, directory)
	defer clear(body)
	source, projection := filepath.Join(directory, "source"), filepath.Join(directory, "projection")
	if err := os.WriteFile(source, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, projection); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("Windows file symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "private", "runtime.creds")
	if err := StageNATSCredentials(projection, destination); err != nil {
		t.Fatal("projected source rejected")
	}
	link := filepath.Join(directory, "private", "linked.creds")
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}
	if StageNATSCredentials(source, link) != ErrNATSResources {
		t.Fatal("destination symlink accepted")
	}
	if _, _, err := readNATSUserCredentials(link); err != ErrNATSResources {
		t.Fatal("strict runtime symlink rejection weakened")
	}
}
