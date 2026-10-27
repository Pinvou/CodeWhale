package events

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// VerifyPreparedBrokerPlan recovers only the SQL audit of an already written
// plan. All original resource facts must still match; it never recaptures Q,
// writes the plan file, or performs a broker mutation.
func VerifyPreparedBrokerPlan(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl, tokenUpgrade bool) ([]byte, error) {
	kind := "existing"
	if tokenUpgrade {
		kind = "token"
	}
	return verifyPreparedBrokerOperation(ctx, rawURL, cfg, files, facts, kind)
}

func VerifyPreparedRefundUpgradePlan(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl) ([]byte, error) {
	return verifyPreparedBrokerOperation(ctx, rawURL, cfg, files, facts, "refund")
}

func verifyPreparedBrokerOperation(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl, kind string) ([]byte, error) {
	files = files.withAuthentication(cfg)
	plan, err := readBrokerOperation(files, facts, kind)
	if err != nil {
		return nil, err
	}
	c, stream, err := openExistingBroker(ctx, rawURL, cfg, files.ApprovalConsumerResourceFiles)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if exactConsumerNames(ctx, stream, existingNames(), existingNames()) != nil || !planStreamMatches(ctx, stream, plan) {
		return nil, ErrNATSResources
	}
	wants := existingBrokerConfigurations()
	switch kind {
	case "existing":
	case "token":
		wants = tokenUpgradeConfigurations()
	case "refund":
		wants = refundUpgradeConfigurations()
	default:
		return nil, ErrNATSResources
	}
	actual, err := readExistingCaptures(ctx, stream, wants)
	if err != nil {
		return nil, err
	}
	for i := range actual {
		if !sameExistingCapture(actual[i], plan.Existing[i]) {
			return nil, ErrNATSResources
		}
	}
	raw, _, err := readNATSPublicArtifact(files.PlanFile, files.PlanSHA256)
	return raw, err
}

// InspectExistingBrokerConsumers provides bounded readback for partial or
// unknown initialization. It classifies the original plan's exact resources;
// replacement instances, configuration drift and unknown siblings still fail.
func InspectExistingBrokerConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl) ([]byte, error) {
	files = files.withAuthentication(cfg)
	plan, err := readBrokerPlan(files, facts, false)
	if err != nil {
		return nil, err
	}
	c, stream, err := openExistingBroker(ctx, rawURL, cfg, files.ApprovalConsumerResourceFiles)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	allowed := existingNames()
	for _, want := range existingNewConfigurations(plan) {
		allowed[want.Name] = true
	}
	if exactConsumerNames(ctx, stream, allowed, existingNames()) != nil || !planStreamMatches(ctx, stream, plan) || !capturesMatch(ctx, stream, plan) {
		return nil, ErrNATSResources
	}
	type resource struct {
		Durable             string `json:"durable"`
		Present             bool   `json:"present"`
		CreatedAt           string `json:"created_at,omitempty"`
		ConfigurationSHA256 string `json:"configuration_sha256,omitempty"`
	}
	resources := []resource{}
	for _, want := range existingNewConfigurations(plan) {
		consumer, e := stream.Consumer(ctx, want.Name)
		if errors.Is(e, jetstream.ErrConsumerNotFound) {
			resources = append(resources, resource{Durable: want.Name})
			continue
		}
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil || !validNewExistingConsumer(info, want) || info.Created.Before(mustPlanCaptureTime(plan)) {
			return nil, ErrNATSResources
		}
		resources = append(resources, resource{want.Name, true, info.Created.UTC().Format(time.RFC3339Nano), configurationDigest(info.Config)})
	}
	return json.Marshal(struct {
		Schema      string     `json:"schema"`
		OperationID string     `json:"operation_id"`
		PlanSHA256  string     `json:"plan_sha256"`
		Resources   []resource `json:"resources"`
	}{"approval-existing-broker-inspection-v1", plan.OperationID, files.PlanSHA256, resources})
}
