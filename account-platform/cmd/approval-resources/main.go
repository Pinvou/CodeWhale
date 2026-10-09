// approval-resources is a bounded operator, never a service startup fallback.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token-consumers" {
		raw, err := runTokenResources(os.Args[1:], os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "TOKEN_APPROVAL_RESOURCES_BLOCKED")
			os.Exit(1)
		}
		fmt.Println(string(raw))
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "render" {
		result, err := renderPublicResources(os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "PUBLIC_RESOURCE_RENDER_FAILED")
			os.Exit(1)
		}
		raw, _ := json.Marshal(result)
		fmt.Println(string(raw))
		return
	}
	result, err := run(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "APPROVAL_FRESH_INITIALIZATION_BLOCKED")
		os.Exit(1)
	}
	raw, _ := json.Marshal(result)
	fmt.Println(string(raw))
}

func run(args []string, environment func(string) string) (account.AdminApprovalControl, error) {
	flags := flag.NewFlagSet("approval-resources", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	action := flags.String("action", "status", "status, quiesce, fence, initialize-consumers, activate or resume")
	version := flags.Int64("expected-version", 0, "current authoritative control version")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *version <= 0 {
		return account.AdminApprovalControl{}, account.ErrApprovalFreshInitialization
	}
	step := account.FreshApprovalStep(*action)
	switch step {
	case account.FreshApprovalStatus, account.FreshApprovalQuiesce, account.FreshApprovalFence, account.FreshApprovalResources, account.FreshApprovalActivate, account.FreshApprovalResume:
	default:
		return account.AdminApprovalControl{}, account.ErrApprovalFreshInitialization
	}
	cfg := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", RequireExplicitAuthentication: true, CAFile: environment("NATS_CA_FILE"), SeedFile: environment("NATS_NKEY_SEED_FILE"), CredentialsFile: environment("NATS_CREDENTIALS_FILE")}
	files := events.ApprovalConsumerResourceFiles{
		Identity:     events.NATSAuthorizationArtifactFiles{CurrentFile: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"), PreviousFile: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_SHA256"), PrivateIdentityFiles: []string{cfg.SeedFile, cfg.CredentialsFile}},
		ManifestFile: environment("APPROVAL_RESOURCE_MANIFEST_FILE"), ManifestSHA256: environment("APPROVAL_RESOURCE_MANIFEST_SHA256"), ReceiptFile: environment("APPROVAL_RESOURCE_RECEIPT_FILE"), ReceiptSHA256: environment("APPROVAL_RESOURCE_RECEIPT_SHA256"),
	}
	var callback func(context.Context, account.AdminApprovalControl) (json.RawMessage, error)
	if step == account.FreshApprovalResources {
		if err := validateNewResourcePath(files.ReceiptFile, files.Identity); err != nil {
			return account.AdminApprovalControl{}, account.ErrApprovalFreshInitialization
		}
		callback = func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			raw, err := events.InitializeFreshApprovalConsumers(ctx, environment("NATS_URL"), cfg, files.Identity, files.ManifestFile, files.ManifestSHA256, events.FreshApprovalControl{Version: control.Version, ApprovalEpoch: control.ApprovalEpoch, RelayEpoch: control.AuthorizationRelayEpoch})
			if err != nil {
				return nil, err
			}
			// Immutable receipt is written before the non-mutating DB audit
			// commits. A failure does not activate the control or erase evidence.
			if err = writePublicReceipt(files.ReceiptFile, raw); err != nil {
				return nil, err
			}
			return raw, nil
		}
	} else if step == account.FreshApprovalActivate || step == account.FreshApprovalResume {
		callback = func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			facts, err := events.CheckCurrentApprovalConsumers(ctx, environment("NATS_URL"), cfg, files)
			if err != nil {
				return nil, err
			}
			version, epoch := facts.Version, facts.ApprovalEpoch
			if step == account.FreshApprovalResume {
				version++
				epoch++
			}
			if control.Version != version || control.ApprovalEpoch != epoch || control.AuthorizationRelayEpoch != facts.RelayEpoch {
				return nil, account.ErrApprovalFreshInitialization
			}
			raw, err := files.VerifiedReceipt(time.Now().UTC())
			if err != nil {
				return nil, account.ErrApprovalFreshInitialization
			}
			return raw, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return account.RunFreshApprovalStep(ctx, environment("APPROVAL_INITIALIZER_DATABASE_URL"), *version, step, callback)
}

func validateNewPublicPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("PUBLIC_RECEIPT_PATH_INVALID")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("PUBLIC_RECEIPT_EXISTS_OR_UNAVAILABLE")
	}
	parent := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	info, e := os.Lstat(parent)
	if err != nil || e != nil || resolved != parent || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return errors.New("PUBLIC_RECEIPT_PARENT_INVALID")
	}
	return nil
}

// Identity directories contain only identity artifacts. Public resources
// must also remain outside every configured private credential directory.
func validateNewResourcePath(path string, identity events.NATSAuthorizationArtifactFiles) error {
	if validateNewPublicPath(path) != nil || filepath.Dir(path) == filepath.Dir(identity.CurrentFile) {
		return errors.New("PUBLIC_RESOURCE_PATH_INVALID")
	}
	for _, private := range identity.PrivateIdentityFiles {
		if private == "" {
			continue
		}
		relative, err := filepath.Rel(filepath.Dir(private), path)
		if err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return errors.New("PUBLIC_RESOURCE_PATH_INVALID")
		}
	}
	return nil
}

func writePublicReceipt(path string, raw []byte) error {
	if len(raw) == 0 || len(raw) > 256<<10 || !json.Valid(raw) || validateNewPublicPath(path) != nil {
		return errors.New("PUBLIC_RECEIPT_WRITE_REJECTED")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("PUBLIC_RECEIPT_WRITE_FAILED")
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("PUBLIC_RECEIPT_WRITE_FAILED")
	}
	return nil
}
