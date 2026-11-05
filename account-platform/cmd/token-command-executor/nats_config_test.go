package main

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenExecutorExplicitNATSConfiguration(t *testing.T) {
	for _, environment := range []string{"development", "test", "production", " PRODUCTION-eu "} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("APP_ENV", environment)
			t.Setenv("NATS_CA_FILE", "/controlled/ca.pem")
			t.Setenv("NATS_NKEY_SEED_FILE", "/controlled/executor.seed")
			t.Setenv("NATS_CREDENTIALS_FILE", "")
			cfg := tokenExecutorNATSConfig()
			if cfg.Environment != environment || cfg.ResourceMode != "provisioned" || !cfg.RequireExplicitAuthentication || cfg.CAFile != "/controlled/ca.pem" || cfg.SeedFile != "/controlled/executor.seed" || cfg.CredentialsFile != "" {
				t.Fatalf("dedicated executor assembly lost explicit boundary: %+v", cfg)
			}
			t.Setenv("NATS_NKEY_SEED_FILE", "")
			t.Setenv("NATS_CREDENTIALS_FILE", "/controlled/executor.creds")
			cfg = tokenExecutorNATSConfig()
			if cfg.SeedFile != "" || cfg.CredentialsFile != "/controlled/executor.creds" {
				t.Fatal("explicit credentials projection lost")
			}
		})
	}
}

// Actual broker configuration rejects before a network connection. No NATS
// server/broker, TLS handshake, ACL or successful consumer test is claimed.
func TestTokenExecutorRejectsMissingOrConflictingIdentityBeforeConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("APP_ENV", "test")
	t.Setenv("NATS_CA_FILE", "")
	for _, pair := range [][2]string{{"", ""}, {"/controlled/executor.seed", "/controlled/executor.creds"}} {
		t.Setenv("NATS_NKEY_SEED_FILE", pair[0])
		t.Setenv("NATS_CREDENTIALS_FILE", pair[1])
		cfg := tokenExecutorNATSConfig()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		broker, err := events.NewNATSBroker(ctx, "tls://"+listener.Addr().String(), "token-command-executor", cfg)
		cancel()
		if broker != nil || !errors.Is(err, events.ErrNATSResources) {
			t.Fatalf("invalid explicitidentity accepted: broker=%v err=%v", broker, err)
		}
	}
	// An ambient process identity cannot fill omitted explicit config fields.
	path := filepath.Join(t.TempDir(), "unrelated-process.seed")
	if err := os.WriteFile(path, []byte("controlled invalid private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATS_NKEY_SEED_FILE", path)
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	cfg := tokenExecutorNATSConfig()
	cfg.SeedFile = ""
	if broker, err := events.NewNATSBroker(context.Background(), "tls://"+listener.Addr().String(), "token-command-executor", cfg); broker != nil || !errors.Is(err, events.ErrNATSResources) {
		t.Fatal("ambient identity fallback accepted")
	}
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if conn != nil {
		conn.Close()
		t.Fatal("invalid identity reached network")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("network assertion failed: %v", err)
	}
}
