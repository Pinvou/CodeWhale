package securityadmin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
)

type activationDirectory struct {
	enabled               bool
	activations, disables int
	partialFailure        error
}

func (*activationDirectory) EnsureBootstrapAdmin(context.Context, string) (string, error) {
	return "owned-bootstrap-subject", nil
}
func (d *activationDirectory) ActivateBootstrapAdmin(context.Context, string, string, string) error {
	d.activations++
	d.enabled = true
	return d.partialFailure
}
func (d *activationDirectory) DisableUserAndConfirm(context.Context, string) error {
	d.disables++
	d.enabled = false
	return nil
}

func TestBootstrapCompensatesPartialIdentityActivationAndRetainsRetry(t *testing.T) {
	pool := testdb.Open(t, "account")
	directory := &activationDirectory{partialFailure: errors.New("logout failed after enabling identity")}
	manager := &Manager{pool: pool, directory: directory, issuer: "test-issuer", now: func() time.Time { return time.Now().UTC() }}
	ctx := context.Background()
	issue, err := manager.IssueBootstrap(ctx, "test-owned-deployment", "INTERACTIVE_SET_PASSWORD", time.Minute)
	if err != nil || issue.Activated {
		t.Fatalf("issue=%+v err=%v", issue, err)
	}
	if err := manager.ActivateInteractive(ctx, "test-owned-deployment", "S3cure-Quartz-71"); err == nil {
		t.Fatal("partial activation cannot report success")
	}
	if directory.enabled || directory.disables != 2 {
		t.Fatalf("identity remained enabled: enabled=%v disables=%d", directory.enabled, directory.disables)
	}
	var status, credentialStatus string
	var roles int
	if err := pool.QueryRow(ctx, `SELECT status FROM users WHERE username=$1`, BootstrapUsername).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM super_admin_bootstrap_credentials WHERE id=$1`, issue.CredentialID).Scan(&credentialStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE role_code='PLATFORM_SUPER_ADMIN'`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if status != "BOOTSTRAP_PENDING" || credentialStatus != "PENDING" || roles != 0 {
		t.Fatalf("rolled-back facts: user=%s credential=%s roles=%d", status, credentialStatus, roles)
	}
	directory.partialFailure = nil
	if err := manager.ActivateInteractive(ctx, "test-owned-deployment", "S3cure-Quartz-71"); err != nil {
		t.Fatal(err)
	}
	if !directory.enabled {
		t.Fatal("successful retry must enable verified identity")
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM super_admin_bootstrap_credentials WHERE id=$1`, issue.CredentialID).Scan(&credentialStatus); err != nil || credentialStatus != "CONSUMED" {
		t.Fatalf("credential=%s err=%v", credentialStatus, err)
	}
	if err := manager.ActivateInteractive(ctx, "test-owned-deployment", "S3cure-Quartz-71"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("consumed credential replay=%v", err)
	}
}

func TestOneTimeBootstrapWrongCredentialLockAndImmutableEvidence(t *testing.T) {
	pool := testdb.Open(t, "account")
	directory := &activationDirectory{}
	manager := &Manager{pool: pool, directory: directory, issuer: "test-issuer", now: func() time.Time { return time.Now().UTC() }}
	ctx := context.Background()
	issue, err := manager.IssueBootstrap(ctx, "test-owned-deployment", "ONE_TIME_BOOTSTRAP", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkDelivered(ctx, issue.CredentialID); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		err := manager.Redeem(ctx, "test-owned-deployment", "wrong-credential", "S3cure-Quartz-71")
		want := ErrCredentialInvalid
		if attempt == 5 {
			want = ErrCredentialLocked
		}
		if !errors.Is(err, want) {
			t.Fatalf("attempt=%d err=%v", attempt, err)
		}
	}
	if directory.activations != 0 || directory.enabled {
		t.Fatal("invalid credential activated identity")
	}
	var status string
	var verifier []byte
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,verifier,failed_attempts FROM super_admin_bootstrap_credentials WHERE id=$1`, issue.CredentialID).Scan(&status, &verifier, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "LOCKED" || verifier != nil || attempts != 5 {
		t.Fatalf("status=%s verifier-length=%d attempts=%d", status, len(verifier), attempts)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM security_alerts`); err == nil {
		t.Fatal("security alerts must be immutable")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_logs WHERE action LIKE 'security.%'`); err == nil {
		t.Fatal("security audit must be immutable")
	}
}
