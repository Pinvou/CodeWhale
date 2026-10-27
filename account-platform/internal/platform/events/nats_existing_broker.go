package events

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ExistingBrokerResourceFiles adds an independently approved, immutable plan.
// The plan is captured under Account's empty-history fence before any broker
// mutation. Resume/check always use the original Q and original identities.
type ExistingBrokerResourceFiles struct {
	ApprovalConsumerResourceFiles
	PlanFile   string
	PlanSHA256 string
}

func (files ExistingBrokerResourceFiles) withAuthentication(cfg NATSConfig) ExistingBrokerResourceFiles {
	files.Identity.PrivateIdentityFiles = append(append([]string(nil), files.Identity.PrivateIdentityFiles...), cfg.SeedFile, cfg.CredentialsFile)
	return files
}

func openExistingBroker(ctx context.Context, rawURL string, cfg NATSConfig, files ApprovalConsumerResourceFiles) (*nats.Conn, jetstream.Stream, error) {
	files.Identity.PrivateIdentityFiles = append(append([]string(nil), files.Identity.PrivateIdentityFiles...), cfg.SeedFile, cfg.CredentialsFile)
	if files.Identity.Validate(time.Now().UTC()) != nil || !existingOperatorMatches(cfg, files.Identity) {
		return nil, nil, ErrNATSResources
	}
	manifest, _, err := readNATSPublicArtifact(files.ManifestFile, files.ManifestSHA256)
	expected, e := ApprovalConsumerManifest(files.Identity.CurrentSHA256)
	if err != nil || e != nil || string(manifest) != string(expected) || !isolatedNATSPublicArtifactPath(files.ManifestFile, files.Identity.PrivateIdentityFiles) {
		return nil, nil, ErrNATSResources
	}
	cfg.RequireExplicitAuthentication, cfg.ResourceMode = true, "provisioned"
	c, js, err := connectNATS(rawURL, "event-resource-operator", cfg)
	if err != nil {
		return nil, nil, ErrNATSResources
	}
	if checkedStream(ctx, js) != nil {
		c.Close()
		return nil, nil, ErrNATSResources
	}
	stream, err := js.Stream(ctx, platformStream)
	if err != nil || noAuthorizationMessages(ctx, stream) != nil {
		c.Close()
		return nil, nil, ErrNATSResources
	}
	return c, stream, nil
}

func noAuthorizationMessages(ctx context.Context, stream jetstream.Stream) error {
	for _, subject := range []string{approvalconsumer.AuthorizationSubject, "account-platform.dead-letter." + approvalconsumer.AuthorizationSubject} {
		info, err := stream.Info(ctx, jetstream.WithSubjectFilter(subject))
		if err != nil || info.State.Subjects[subject] != 0 {
			return ErrNATSResources
		}
	}
	return nil
}

func exactConsumerNames(ctx context.Context, stream jetstream.Stream, allowed map[string]bool, required map[string]bool) error {
	names := stream.ConsumerNames(ctx)
	seen := map[string]bool{}
	for name := range names.Name() {
		if !allowed[name] || seen[name] {
			return ErrNATSResources
		}
		seen[name] = true
	}
	if names.Err() != nil {
		return ErrNATSResources
	}
	for name := range required {
		if !seen[name] {
			return ErrNATSResources
		}
	}
	return nil
}

func existingNames() map[string]bool {
	result := map[string]bool{}
	for _, c := range existingBrokerConfigurations() {
		result[c.Name] = true
	}
	return result
}

func readExistingCaptures(ctx context.Context, stream jetstream.Stream, configurations []jetstream.ConsumerConfig) ([]existingConsumerCapture, error) {
	result := make([]existingConsumerCapture, 0, len(configurations))
	for _, expected := range configurations {
		c, err := stream.Consumer(ctx, expected.Name)
		if err != nil {
			return nil, ErrNATSResources
		}
		info, err := c.Info(ctx)
		if err != nil {
			return nil, ErrNATSResources
		}
		capture, err := captureExisting(info, expected)
		if err != nil {
			return nil, err
		}
		result = append(result, capture)
	}
	return result, nil
}

// PrepareExistingBrokerPlan performs only INFO calls and returns public bytes.
// tokenUpgrade selects the exact published retained-event legacy Token shape;
// it is not a caller-supplied configuration or a permissive drift allowance.
func PrepareExistingBrokerPlan(ctx context.Context, rawURL string, cfg NATSConfig, files ApprovalConsumerResourceFiles, facts FreshApprovalControl, operationID string, tokenUpgrade bool) ([]byte, error) {
	kind := "existing"
	if tokenUpgrade {
		kind = "token"
	}
	return prepareBrokerOperation(ctx, rawURL, cfg, files, facts, operationID, kind)
}

func PrepareRefundUpgradePlan(ctx context.Context, rawURL string, cfg NATSConfig, files ApprovalConsumerResourceFiles, facts FreshApprovalControl, operationID string) ([]byte, error) {
	return prepareBrokerOperation(ctx, rawURL, cfg, files, facts, operationID, "refund")
}

func prepareBrokerOperation(ctx context.Context, rawURL string, cfg NATSConfig, files ApprovalConsumerResourceFiles, facts FreshApprovalControl, operationID, kind string) ([]byte, error) {
	if facts.Version <= 0 || facts.ApprovalEpoch <= 0 || facts.RelayEpoch <= 0 {
		return nil, ErrNATSResources
	}
	c, stream, err := openExistingBroker(ctx, rawURL, cfg, files)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if exactConsumerNames(ctx, stream, existingNames(), existingNames()) != nil {
		return nil, ErrNATSResources
	}
	wants, schema := existingBrokerConfigurations(), existingBrokerPlanSchema
	switch kind {
	case "existing":
	case "token":
		wants, schema = tokenUpgradeConfigurations(), tokenUpgradePlanSchema
	case "refund":
		wants, schema = refundUpgradeConfigurations(), refundUpgradePlanSchema
	default:
		return nil, ErrNATSResources
	}
	captures, err := readExistingCaptures(ctx, stream, wants)
	if err != nil {
		return nil, err
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.LastSeq == math.MaxUint64 {
		return nil, ErrNATSResources
	}
	plan := existingBrokerPlan{Schema: schema, OperationID: operationID, IdentitySHA256: files.Identity.CurrentSHA256, ManifestSHA256: files.ManifestSHA256,
		StreamCreatedAt: info.Created.UTC().Format(time.RFC3339Nano), ControlVersion: strconv.FormatInt(facts.Version, 10), ApprovalEpoch: strconv.FormatInt(facts.ApprovalEpoch, 10), RelayEpoch: strconv.FormatInt(facts.RelayEpoch, 10),
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), HighWatermark: strconv.FormatUint(info.State.LastSeq, 10),
		StreamFirst: strconv.FormatUint(info.State.FirstSeq, 10), StreamMessages: strconv.FormatUint(info.State.Msgs, 10), StreamBytes: strconv.FormatUint(info.State.Bytes, 10), Existing: captures}
	if plan.validateOperation(time.Now().UTC(), kind) != nil || noAuthorizationMessages(ctx, stream) != nil {
		return nil, ErrNATSResources
	}
	return json.Marshal(plan)
}

func readBrokerPlan(files ExistingBrokerResourceFiles, facts FreshApprovalControl, tokenUpgrade bool) (existingBrokerPlan, error) {
	kind := "existing"
	if tokenUpgrade {
		kind = "token"
	}
	return readBrokerOperation(files, facts, kind)
}

func readBrokerOperation(files ExistingBrokerResourceFiles, facts FreshApprovalControl, kind string) (existingBrokerPlan, error) {
	var plan existingBrokerPlan
	if !isolatedNATSPublicArtifactPath(files.PlanFile, files.Identity.PrivateIdentityFiles) {
		return plan, ErrNATSResources
	}
	raw, _, err := readNATSPublicArtifact(files.PlanFile, files.PlanSHA256)
	if err != nil {
		return plan, ErrNATSResources
	}
	plan, err = decodeBrokerOperation(raw, files.PlanSHA256, time.Now().UTC(), kind)
	if err != nil || plan.IdentitySHA256 != files.Identity.CurrentSHA256 || plan.ManifestSHA256 != files.ManifestSHA256 ||
		plan.ControlVersion != strconv.FormatInt(facts.Version, 10) || plan.ApprovalEpoch != strconv.FormatInt(facts.ApprovalEpoch, 10) || plan.RelayEpoch != strconv.FormatInt(facts.RelayEpoch, 10) {
		return plan, ErrNATSResources
	}
	return plan, nil
}

func planStreamMatches(ctx context.Context, stream jetstream.Stream, plan existingBrokerPlan) bool {
	i, err := stream.Info(ctx)
	q, _ := exactUnsigned(plan.HighWatermark)
	first, _ := exactUnsigned(plan.StreamFirst)
	count, _ := exactUnsigned(plan.StreamMessages)
	byteCount, _ := exactUnsigned(plan.StreamBytes)
	return err == nil && validStreamConfig(i.Config) && i.Created.UTC().Format(time.RFC3339Nano) == plan.StreamCreatedAt &&
		i.State.LastSeq == q && i.State.FirstSeq == first && i.State.Msgs == count && i.State.Bytes == byteCount && noAuthorizationMessages(ctx, stream) == nil
}

func capturesMatch(ctx context.Context, stream jetstream.Stream, plan existingBrokerPlan) bool {
	captures, err := readExistingCaptures(ctx, stream, existingBrokerConfigurations())
	if err != nil || len(captures) != len(plan.Existing) {
		return false
	}
	for i := range captures {
		if !sameExistingCapture(captures[i], plan.Existing[i]) {
			return false
		}
	}
	return true
}

func existingNewConfigurations(plan existingBrokerPlan) []jetstream.ConsumerConfig {
	q, _ := exactUnsigned(plan.HighWatermark)
	result := []jetstream.ConsumerConfig{provisionedConsumers()[1]}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		config, _ := authorizationConsumerConfig(descriptor, q+1)
		result = append(result, config)
	}
	return result
}

func validNewExistingConsumer(info *jetstream.ConsumerInfo, expected jetstream.ConsumerConfig) bool {
	if info == nil {
		return false
	}
	if len(expected.BackOff) != 0 {
		return validConsumerConfig(info.Config, expected)
	}
	return validAuthorizationConsumerConfig(info.Config, expected) && info.NumPending == 0 && info.NumAckPending == 0
}

// InitializeExistingBrokerConsumers never updates/deletes a resource. An
// explicit resume uses the same reviewed plan after readback of partial work;
// no retry recaptures Q or silently adopts a new configuration.
func InitializeExistingBrokerConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles, facts FreshApprovalControl, createMissing, resume bool) ([]byte, error) {
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
	newConfigurations := existingNewConfigurations(plan)
	for _, config := range newConfigurations {
		allowed[config.Name] = true
	}
	if exactConsumerNames(ctx, stream, allowed, existingNames()) != nil || !planStreamMatches(ctx, stream, plan) || !capturesMatch(ctx, stream, plan) {
		return nil, ErrNATSResources
	}
	missing := []jetstream.ConsumerConfig{}
	existingCount := 0
	// Validate all siblings before the first CREATE, including ordinary member
	// resources and any effects of an explicitly resumed original operation.
	for _, expected := range newConfigurations {
		consumer, e := stream.Consumer(ctx, expected.Name)
		if errors.Is(e, jetstream.ErrConsumerNotFound) {
			missing = append(missing, expected)
			continue
		}
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil || !validNewExistingConsumer(info, expected) || info.Created.Before(mustPlanCaptureTime(plan)) {
			return nil, ErrNATSResources
		}
		existingCount++
	}
	if (len(missing) != 0 && !createMissing) || (createMissing && existingCount != 0 && !resume) {
		return nil, ErrNATSResources
	}
	for _, expected := range missing {
		// Unknown ACK is followed by INFO only, never a second CREATE.
		_, _ = stream.CreateConsumer(ctx, expected)
		consumer, e := stream.Consumer(ctx, expected.Name)
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil || !validNewExistingConsumer(info, expected) {
			return nil, ErrNATSResources
		}
	}
	if exactConsumerNames(ctx, stream, allowed, allowed) != nil || !planStreamMatches(ctx, stream, plan) || !capturesMatch(ctx, stream, plan) {
		return nil, ErrNATSResources
	}
	for _, expected := range newConfigurations {
		consumer, e := stream.Consumer(ctx, expected.Name)
		if e != nil {
			return nil, ErrNATSResources
		}
		info, e := consumer.Info(ctx)
		if e != nil || !validNewExistingConsumer(info, expected) {
			return nil, ErrNATSResources
		}
	}
	receipt := approvalConsumerCutoverReceipt{Schema: existingBrokerReceiptSchema, SchemaVersion: 1, IdentitySHA256: plan.IdentitySHA256, ManifestSHA256: plan.ManifestSHA256,
		Stream: platformStream, Control: "ADMIN_APPROVAL", CapturedMode: "QUIESCING", CapturedRelayState: "FENCED", ControlVersion: plan.ControlVersion,
		ApprovalEpoch: plan.ApprovalEpoch, AuthorizationRelayEpoch: plan.RelayEpoch, CapturedAt: plan.CapturedAt, HighWatermark: plan.HighWatermark,
		ExistingBroker: &existingBrokerReceiptFacts{PlanSHA256: files.PlanSHA256, Plan: plan}}
	q, _ := exactUnsigned(plan.HighWatermark)
	for _, d := range approvalconsumer.BillingCommands() {
		receipt.BillingStarts = append(receipt.BillingStarts, approvalConsumerReceiptStart{Durable: d.Durable, StartSequence: strconv.FormatUint(q+1, 10)})
	}
	return json.Marshal(receipt)
}

func mustPlanCaptureTime(plan existingBrokerPlan) time.Time {
	v, _ := time.Parse(time.RFC3339Nano, plan.CapturedAt)
	return v
}
