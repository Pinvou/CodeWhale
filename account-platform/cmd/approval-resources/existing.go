package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

// This separate command never changes the original fresh-only CLI contract.
// Prepare writes a plan, not a receipt. Its reviewed digest and committed
// Account audit are required before a later, explicitly invoked operation.
func runExistingBroker(args []string, environment func(string) string) ([]byte, error) {
	blocked := errors.New("APPROVAL_EXISTING_BROKER_OPERATION_BLOCKED")
	flags := flag.NewFlagSet("existing-broker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	action := flags.String("action", "status", "explicit bounded operation")
	version := flags.Int64("expected-version", 0, "current authoritative Account version")
	operationID := flags.String("operation-id", "", "new plan identity, prepare only")
	resume := flags.Bool("resume", false, "continue only known missing/legacy effects of the original reviewed plan after readback")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *version < 0 || (*version == 0 && *action != "status") {
		return nil, blocked
	}
	profile, err := config.ParseAccountPlatformProfile(environment("ACCOUNT_PLATFORM_PROFILE"))
	if err != nil || profile != config.AccountPlatformProfileModels {
		return nil, blocked
	}
	step := account.FreshApprovalStep(*action)
	prepareRefund := step == account.ExistingBrokerPrepareRefundUpgrade
	prepareRecovery := step == account.ExistingBrokerPrepareRefundRecovery
	recovery := prepareRecovery || step == account.ExistingBrokerUpgradeRefundRecovery || step == account.ExistingBrokerCheckRefundRecovery
	prepareToken := step == account.ExistingBrokerPrepareTokenUpgrade
	prepare := prepareRefund || prepareRecovery || prepareToken || step == account.ExistingBrokerPrepare
	mutatingResources := step == account.ExistingBrokerUpgradeRefund || step == account.ExistingBrokerUpgradeRefundRecovery || step == account.ExistingBrokerUpgradeToken || step == account.FreshApprovalResources
	if (*resume && !mutatingResources && !prepare) || (!prepare && *operationID != "") || (prepare && *resume && *operationID != "") || (prepare && !*resume && *operationID == "") {
		return nil, blocked
	}
	var originalPlanFile, originalPlanSHA string
	if recovery {
		if *resume {
			return nil, blocked
		}
		originalPlanFile, originalPlanSHA = environment("APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_FILE"), environment("APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_SHA256")
		if originalPlanFile == "" || originalPlanSHA == "" {
			return nil, blocked
		}
	}
	switch step {
	case account.FreshApprovalStatus, account.FreshApprovalQuiesce, account.FreshApprovalFence,
		account.ExistingBrokerPrepareRefundUpgrade, account.ExistingBrokerUpgradeRefund, account.ExistingBrokerCheckRefundUpgrade, account.ExistingBrokerInspectRefundUpgrade,
		account.ExistingBrokerPrepareRefundRecovery, account.ExistingBrokerUpgradeRefundRecovery, account.ExistingBrokerCheckRefundRecovery,
		account.ExistingBrokerPrepareTokenUpgrade, account.ExistingBrokerUpgradeToken, account.ExistingBrokerCheckTokenUpgrade,
		account.ExistingBrokerPrepare, account.FreshApprovalResources, account.ExistingBrokerVerify,
		account.FreshApprovalActivate, account.FreshApprovalResume, account.ExistingBrokerInspectTokenUpgrade, account.ExistingBrokerInspect:
	default:
		return nil, blocked
	}
	cfg := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", RequireExplicitAuthentication: true, CAFile: environment("NATS_CA_FILE"), SeedFile: environment("NATS_NKEY_SEED_FILE"), CredentialsFile: environment("NATS_CREDENTIALS_FILE")}
	files := events.ExistingBrokerResourceFiles{ApprovalConsumerResourceFiles: events.ApprovalConsumerResourceFiles{
		Identity: events.NATSAuthorizationArtifactFiles{CurrentFile: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE"), CurrentSHA256: environment("APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256"),
			PreviousFile: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_FILE"), PreviousSHA256: environment("APPROVAL_RESOURCE_PREVIOUS_PUBLIC_KEYS_SHA256"), PrivateIdentityFiles: []string{cfg.SeedFile, cfg.CredentialsFile}},
		ManifestFile: environment("APPROVAL_RESOURCE_MANIFEST_FILE"), ManifestSHA256: environment("APPROVAL_RESOURCE_MANIFEST_SHA256"), ReceiptFile: environment("APPROVAL_RESOURCE_RECEIPT_FILE"), ReceiptSHA256: environment("APPROVAL_RESOURCE_RECEIPT_SHA256")},
		PlanFile: environment("APPROVAL_EXISTING_PLAN_FILE"), PlanSHA256: environment("APPROVAL_EXISTING_PLAN_SHA256")}
	planSHA := files.PlanSHA256
	if step == account.FreshApprovalStatus || step == account.FreshApprovalQuiesce || step == account.FreshApprovalFence {
		planSHA = ""
	}
	var callback func(context.Context, account.AdminApprovalControl) (json.RawMessage, error)
	if prepare || mutatingResources || step == account.ExistingBrokerCheckRefundRecovery || step == account.ExistingBrokerCheckRefundUpgrade || step == account.ExistingBrokerInspectRefundUpgrade || step == account.ExistingBrokerCheckTokenUpgrade || step == account.ExistingBrokerVerify || step == account.FreshApprovalActivate || step == account.FreshApprovalResume || step == account.ExistingBrokerInspectTokenUpgrade || step == account.ExistingBrokerInspect {
		if files.PlanFile == files.ManifestFile || files.PlanFile == files.ReceiptFile {
			return nil, blocked
		}
		if recovery && (originalPlanFile == files.PlanFile || originalPlanFile == files.ManifestFile || originalPlanFile == files.ReceiptFile) {
			return nil, blocked
		}
		if prepare {
			if !*resume && (planSHA != "" || validateNewResourcePath(files.PlanFile, files.Identity) != nil) {
				return nil, blocked
			}
			if *resume && planSHA == "" {
				return nil, blocked
			}
			// Prepare's SQL audit is the trust anchor being recovered. The
			// callback still verifies the original hash and unchanged resources.
			planSHA = ""
		} else if planSHA == "" {
			return nil, blocked
		}
		reuseReceipt := step == account.FreshApprovalResources && *resume && files.ReceiptSHA256 != ""
		if step == account.FreshApprovalResources {
			if reuseReceipt {
				if _, e := files.VerifiedReceipt(time.Now().UTC()); e != nil {
					return nil, blocked
				}
			} else if validateNewResourcePath(files.ReceiptFile, files.Identity) != nil {
				return nil, blocked
			}
		}
		callback = func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			facts := events.FreshApprovalControl{Version: control.Version, ApprovalEpoch: control.ApprovalEpoch, RelayEpoch: control.AuthorizationRelayEpoch}
			url := environment("NATS_URL")
			if prepare {
				if *resume {
					if prepareRefund {
						return events.VerifyPreparedRefundUpgradePlan(ctx, url, cfg, files, facts)
					}
					return events.VerifyPreparedBrokerPlan(ctx, url, cfg, files, facts, prepareToken)
				}
				var raw []byte
				var e error
				if prepareRecovery {
					original := files
					original.PlanFile, original.PlanSHA256 = originalPlanFile, originalPlanSHA
					raw, e = events.PrepareRefundRecoveryPlan(ctx, url, cfg, original, facts, *operationID)
				} else if prepareRefund {
					raw, e = events.PrepareRefundUpgradePlan(ctx, url, cfg, files.ApprovalConsumerResourceFiles, facts, *operationID)
				} else {
					raw, e = events.PrepareExistingBrokerPlan(ctx, url, cfg, files.ApprovalConsumerResourceFiles, facts, *operationID, prepareToken)
				}
				if e != nil {
					return nil, e
				}
				if writePublicReceipt(files.PlanFile, raw) != nil {
					return nil, blocked
				}
				return raw, nil
			}
			if step == account.ExistingBrokerUpgradeRefund || step == account.ExistingBrokerCheckRefundUpgrade || step == account.ExistingBrokerUpgradeRefundRecovery || step == account.ExistingBrokerCheckRefundRecovery {
				return events.UpgradeLegacyRefundConsumer(ctx, url, cfg, files, facts, step == account.ExistingBrokerUpgradeRefund || step == account.ExistingBrokerUpgradeRefundRecovery, *resume)
			}
			if step == account.ExistingBrokerInspectRefundUpgrade {
				return events.InspectLegacyRefundConsumer(ctx, url, cfg, files, facts)
			}
			if step == account.ExistingBrokerUpgradeToken || step == account.ExistingBrokerCheckTokenUpgrade {
				return events.UpgradeLegacyTokenConsumers(ctx, url, cfg, files, facts, step == account.ExistingBrokerUpgradeToken, *resume)
			}
			if step == account.ExistingBrokerInspectTokenUpgrade {
				return events.InspectLegacyTokenConsumers(ctx, url, cfg, files, facts)
			}
			if step == account.ExistingBrokerInspect {
				return events.InspectExistingBrokerConsumers(ctx, url, cfg, files, facts)
			}
			if step == account.FreshApprovalResources {
				raw, e := events.InitializeExistingBrokerConsumers(ctx, url, cfg, files, facts, !reuseReceipt, *resume)
				if e != nil {
					return nil, e
				}
				if reuseReceipt {
					existing, e := files.VerifiedReceipt(time.Now().UTC())
					if e != nil || !bytes.Equal(existing, raw) {
						return nil, blocked
					}
				} else if writePublicReceipt(files.ReceiptFile, raw) != nil {
					return nil, blocked
				}
				return raw, nil
			}
			captured, e := events.CheckExistingBrokerConsumers(ctx, url, cfg, files)
			if e != nil {
				return nil, e
			}
			if step == account.FreshApprovalResume {
				captured.Version++
				captured.ApprovalEpoch++
			}
			if facts != captured {
				return nil, blocked
			}
			return files.VerifiedReceipt(time.Now().UTC())
		}
	}
	var resourceResult json.RawMessage
	if callback != nil {
		operation := callback
		callback = func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			raw, err := operation(ctx, control)
			if err == nil {
				resourceResult = raw
			}
			return raw, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var control account.AdminApprovalControl
	if recovery {
		control, err = account.RunRefundRecoveryApprovalStep(ctx, environment("APPROVAL_INITIALIZER_DATABASE_URL"), *version, step, originalPlanSHA, planSHA, callback)
	} else {
		control, err = account.RunExistingBrokerApprovalStep(ctx, environment("APPROVAL_INITIALIZER_DATABASE_URL"), *version, step, planSHA, callback)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Control        account.AdminApprovalControl `json:"control"`
		ResourceResult json.RawMessage              `json:"resource_result,omitempty"`
	}{control, resourceResult})
}
