package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestNATSUserSeedActualReconnectAndRevocation(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture binary path")
	}
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	t.Setenv("NATS_NKEY_SEED_FILE", "")
	root := t.TempDir()
	os.Chmod(root, 0700)
	ca, cert, key := natsFixtureTLS(t)
	for name, data := range map[string][]byte{"ca.crt": ca, "server.crt": cert, "server.key": key} {
		if os.WriteFile(filepath.Join(root, name), data, 0600) != nil {
			t.Fatal("fixture TLS setup")
		}
	}
	oldFile, oldPublic := natsFixtureSeed(t, root, "old")
	newFile, newPublic := natsFixtureSeed(t, root, "new")
	oldSeed, _ := os.ReadFile(oldFile)
	defer clear(oldSeed)
	newSeed, _ := os.ReadFile(newFile)
	defer clear(newSeed)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture port")
	}
	address := listener.Addr().String()
	listener.Close()
	configPath := filepath.Join(root, "server.json")
	var process *exec.Cmd
	stop := func() {
		if process != nil {
			process.Process.Kill()
			process.Wait()
			process = nil
		}
	}
	defer stop()
	start := func(publicKeys ...string) {
		t.Helper()
		users := []map[string]any{}
		for _, public := range publicKeys {
			users = append(users, map[string]any{"nkey": public})
		}
		config := map[string]any{"listen": address, "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")}, "tls": map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"}, "authorization": map[string]any{"users": users}}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(config) != nil || os.WriteFile(configPath, encoded.Bytes(), 0600) != nil {
			t.Fatal("fixture server config")
		}
		if exec.Command(binary, "-t", "-c", configPath).Run() != nil {
			t.Fatal("fixture NKey config rejected")
		}
		process = exec.Command(binary, "-c", configPath)
		process.Stdout = io.Discard
		process.Stderr = io.Discard
		if process.Start() != nil {
			process = nil
			t.Fatal("fixture start")
		}
	}
	cfg := NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt")}
	connect := func(file string) (*nats.Conn, error) {
		t.Setenv("NATS_NKEY_SEED_FILE", file)
		connection, _, err := connectNATS("tls://"+address, "fixture-rotation", cfg)
		return connection, err
	}
	wait := func(condition func() bool, label string, observed ...*nats.Conn) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		if len(observed) == 1 {
			failure := observed[0].LastError()
			t.Logf("fixture connection state=%s identity_file_error=%t authorization_error=%t timeout=%t", observed[0].Status().String(), errors.Is(failure, ErrNATSResources), errors.Is(failure, nats.ErrAuthorization), errors.Is(failure, nats.ErrTimeout))
		}
		t.Fatal("fixture condition failed:", label)
	}
	start(oldPublic, newPublic)
	var original *nats.Conn
	wait(func() bool { original, err = connect(oldFile); return err == nil }, "initial NKey connection")
	defer original.Close()
	reconnected := make(chan bool, 2)
	original.SetReconnectHandler(func(*nats.Conn) {
		select {
		case reconnected <- true:
		default:
		}
	})
	// Replacing the file with the same identity remains valid on reconnect.
	if os.WriteFile(oldFile, append(append([]byte{}, oldSeed...), '\n'), 0600) != nil || original.ForceReconnect() != nil {
		t.Fatal("fixture same identity reconnect")
	}
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("same identity reconnect failed")
	}
	if original.FlushTimeout(time.Second) != nil {
		t.Fatal("reconnected identity unusable")
	}
	// A different, otherwise server-authorized key cannot change this client's
	// identity. Observe the actual signing error, not only disconnection timing.
	if os.WriteFile(oldFile, newSeed, 0600) != nil || original.ForceReconnect() != nil {
		t.Fatal("fixture replaced identity reconnect")
	}
	wait(func() bool { return errors.Is(original.LastError(), ErrNATSResources) }, "replacement callback rejected")
	if original.IsConnected() {
		t.Fatal("reconnect switched NKey identity")
	}
	original.Close()
	if os.WriteFile(oldFile, oldSeed, 0600) != nil {
		t.Fatal("fixture old identity restore")
	}
	oldConnection, err := connect(oldFile)
	if err != nil {
		t.Fatal("fixture old connection")
	}
	defer oldConnection.Close()
	newConnection, err := connect(newFile)
	if err != nil {
		t.Fatal("new identity denied during overlap")
	}
	defer newConnection.Close()
	// Controlled broker restart applies the new public allowlist and terminates
	// existing connections. No live or production broker is addressed here.
	stop()
	wait(func() bool { return !oldConnection.IsConnected() }, "old connection terminated")
	start(newPublic)
	// Distinguish child-process startup from client reconnect. Both remain
	// bounded and the readiness probe must authenticate with the new identity.
	wait(func() bool {
		probe, err := connect(newFile)
		if err != nil {
			return false
		}
		defer probe.Close()
		return probe.FlushTimeout(time.Second) == nil
	}, "new broker verified ready")
	wait(func() bool { return newConnection.IsConnected() && newConnection.FlushTimeout(time.Second) == nil }, "new identity after revocation", newConnection)
	if oldConnection.IsConnected() || oldConnection.FlushTimeout(time.Second) == nil {
		t.Fatal("revoked existing identity still usable")
	}
	if connection, err := connect(oldFile); err == nil {
		connection.Close()
		t.Fatal("revoked old seed reauthenticated")
	}
	final, err := connect(newFile)
	if err != nil {
		t.Fatal("new seed fresh connection denied")
	}
	defer final.Close()
	if final.FlushTimeout(time.Second) != nil {
		t.Fatal("new seed unusable")
	}
	t.Log("NATS_NKEY_RECONNECT_REVOCATION_ACTUAL_PASS")
}
