package token

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This child runs while the Account parent fixture keeps its actual signed
// workload receiver alive. Only the unrelated wallet supplier is controlled.
type ownerCombinedWallet struct {
	calls    *atomic.Int64
	wallets  map[string]uuid.UUID
	receipts map[uuid.UUID]WalletProvisioningReceipt
}

func (supplier ownerCombinedWallet) ProvisionTokenAccountWallet(_ context.Context, ref uuid.UUID, kind string, id uuid.UUID, digest string) (WalletProvisioningReceipt, error) {
	if supplier.calls != nil {
		supplier.calls.Add(1)
	}
	if receipt, exists := supplier.receipts[ref]; exists {
		return receipt, nil
	}
	now := time.Now()
	ownerKey := kind + ":" + id.String()
	wallet, exists := supplier.wallets[ownerKey]
	if !exists {
		wallet = uuid.New()
		supplier.wallets[ownerKey] = wallet
	}
	receipt := WalletProvisioningReceipt{CommandReference: ref, OwnerType: kind, OwnerID: id, RequestDigest: digest, Status: "SUCCEEDED", WalletID: &wallet, AttemptCount: 1, CreatedAt: now, CompletedAt: &now}
	supplier.receipts[ref] = receipt
	return receipt, nil
}
func (supplier ownerCombinedWallet) TokenAccountWalletProvisioningReceipt(_ context.Context, reference uuid.UUID) (WalletProvisioningReceipt, error) {
	if supplier.calls != nil {
		supplier.calls.Add(1)
	}
	if receipt, exists := supplier.receipts[reference]; exists {
		return receipt, nil
	}
	return WalletProvisioningReceipt{}, ErrNotFound
}

func TestOwnerCombinedSameVersionPersistentWorker(t *testing.T) {
	if os.Getenv("OWNER_COMBINED_ACCOUNT_URL") == "" {
		t.Skip("requires the companion owned Account HTTP fixture")
	}
	r, ctx := retainedManagementTokenRepository(t)
	actor := uuid.MustParse(os.Getenv("OWNER_COMBINED_ACTOR"))
	org := uuid.MustParse(os.Getenv("OWNER_COMBINED_ORGANIZATION"))
	unit := uuid.MustParse(os.Getenv("OWNER_COMBINED_UNIT"))
	authority, err := NewHTTPOwnerAuthorityClient(os.Getenv("OWNER_COMBINED_ACCOUNT_URL"), os.Getenv("OWNER_COMBINED_WORKLOAD_JWT"))
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := authorization.NewClient(os.Getenv("OWNER_COMBINED_ACCOUNT_URL"), os.Getenv("OWNER_COMBINED_WORKLOAD_JWT"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{repository: r, authorizer: authorizer}
	var walletCalls atomic.Int64
	wallet := ownerCombinedWallet{calls: &walletCalls, wallets: map[string]uuid.UUID{}, receipts: map[uuid.UUID]WalletProvisioningReceipt{}}
	s.WithTokenAccountProvisioning(wallet, authority).WithTokenAccountCreateCapability(true)
	digest := strings.Repeat("a", 64)
	store, err := r.GovernanceStore(digest, digest)
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_migrator"
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='ACTIVE',epoch=1,handler_set_digest=$1,policy_set_digest=$1,approved_runtime_roles=ARRAY['token_gateway'],operator_id='owned-fixture',reason='combined owner authority negative control',request_id=$2 WHERE singleton`, digest, uuid.New()); err != nil {
		t.Fatal(err)
	}
	cfg.User = "token_policy_operator"
	op, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = op.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'combined owner consumer',$2)`, uuid.NewString(), uuid.New())
	op.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.routeActivationGuard = store
	s.WithTokenCommandGovernanceActivation(store)
	worker, err := NewTokenAccountProvisioningWorker(r, wallet, authority, "owner-combined-real-consumer", TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, positive := range []governedAccountCreateCommand{
		{OwnerType: "USER", OwnerID: actor, Name: "combined-personal", ShareScope: "OWNER_UNIT_ONLY"},
		{OwnerType: "ORG", OwnerID: org, OwnerOrganization: &org, Name: "combined-organization", ShareScope: "ORG_ALL"},
		{OwnerType: "ORG_UNIT", OwnerID: unit, OwnerOrganization: &org, OwnerUnit: &unit, Name: "combined-unit", ShareScope: "OWNER_SUBTREE"},
	} {
		in := TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: positive.OwnerID.String(), Command: mustMarshalJSON(t, positive)}
		pre := f04Request(f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck)), "POST", "/precheck", mustMarshalJSON(t, in), "")
		var projection TokenCommandPrecheckResult
		if pre.Code != 200 || json.Unmarshal(pre.Body.Bytes(), &projection) != nil || len(projection.Blockers) != 0 {
			t.Fatalf("positive %s precheck: %d %s", positive.OwnerType, pre.Code, pre.Body.String())
		}
		envelope := modelAccountEnvelope(t, in.CommandType, in.TargetResourceType, in.TargetResourceID, in.Command, "Combined actual owner positive", projection)
		calls := walletCalls.Load()
		response := f04Request(f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand)), "POST", "/admin/v1/commands", mustMarshalJSON(t, envelope), uuid.NewString())
		var receipt TokenCommandResult
		if (response.Code != 201 && response.Code != 202) || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.Status != "PENDING" || walletCalls.Load() != calls {
			t.Fatalf("positive T1 %s: %d %s", positive.OwnerType, response.Code, response.Body.String())
		}
		if did, err := worker.ProcessOne(ctx); !did || err != nil {
			t.Fatal("positive actual worker", did, err)
		}
		var state, reservation string
		var accountCount int
		if err := r.pool.QueryRow(ctx, `SELECT c.status,p.status,(SELECT count(*) FROM token_accounts WHERE id=p.token_account_id AND owner_type=$2 AND owner_id=$3) FROM token_command_results c JOIN token_account_provisioning_reservations p USING(command_reference) WHERE c.command_reference=$1`, receipt.CommandReference, positive.OwnerType, positive.OwnerID).Scan(&state, &reservation, &accountCount); err != nil || state != "SUCCEEDED" || reservation != "COMPLETED" || accountCount != 1 || walletCalls.Load() != calls+1 {
			t.Fatalf("positive actual owner %s state=%s/%s accounts=%d err=%v", positive.OwnerType, state, reservation, accountCount, err)
		}
		t.Logf("actual Token %s create: SUCCEEDED/COMPLETED, one account", positive.OwnerType)
	}
	submitUnit := func(name string) TokenCommandResult {
		command := governedAccountCreateCommand{OwnerType: "ORG_UNIT", OwnerID: unit, OwnerOrganization: &org, OwnerUnit: &unit, Name: name, ShareScope: "OWNER_SUBTREE", DefaultPriority: 100}
		in := TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: unit.String(), Command: mustMarshalJSON(t, command)}
		pre := f04Request(f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck)), "POST", "/precheck", mustMarshalJSON(t, in), "")
		var projection TokenCommandPrecheckResult
		if pre.Code != 200 || json.Unmarshal(pre.Body.Bytes(), &projection) != nil || len(projection.Blockers) != 0 {
			t.Fatalf("actual T1 precheck %d %s", pre.Code, pre.Body.String())
		}
		envelope := modelAccountEnvelope(t, in.CommandType, in.TargetResourceType, in.TargetResourceID, in.Command, "Combined actual owner negative", projection)
		response := f04Request(f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand)), "POST", "/admin/v1/commands", mustMarshalJSON(t, envelope), uuid.NewString())
		var receipt TokenCommandResult
		if response.Code != 202 && response.Code != 201 && response.Code != 200 {
			t.Fatalf("actual producer %d %s", response.Code, response.Body.String())
		}
		if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.Status != "PENDING" {
			t.Fatal("pending receipt")
		}
		return receipt
	}
	setParent := func(status string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, os.Getenv("OWNER_COMBINED_ACCOUNT_URL")+"/admin/v1/organizations/"+org.String()+"/status", strings.NewReader(`{"status":"`+status+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer owned-parent-status-administrator")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("real Account parent status %s returned %d", status, response.StatusCode)
		}
	}
	// A normal parent transition must give the real worker newer persisted
	// owner evidence. Billing's existing shared owner wallet is retained.
	normal := submitUnit("combined-domain-parent-version")
	normalCapacity, err := loadTokenAccountOwnerCapacity(ctx, r.pool, "ORG_UNIT", unit)
	if err != nil {
		t.Fatal(err)
	}
	if walletCalls.Load() != 3 {
		t.Fatal("normal T1 called wallet supplier")
	}
	setParent("DISABLED")
	defer setParent("ACTIVE")
	if did, err := worker.ProcessOne(ctx); !did || err != nil {
		t.Fatal("normal parent lifecycle worker", did, err)
	}
	var normalResult, normalReservation, normalCode string
	var normalAccounts int
	if err := r.pool.QueryRow(ctx, `SELECT c.status,p.status,COALESCE(c.stable_error_code,''),(SELECT count(*) FROM token_accounts WHERE name='combined-domain-parent-version') FROM token_command_results c JOIN token_account_provisioning_reservations p USING(command_reference) WHERE c.command_reference=$1`, normal.CommandReference).Scan(&normalResult, &normalReservation, &normalCode, &normalAccounts); err != nil {
		t.Fatal(err)
	}
	if normalResult != "FAILED" || normalReservation != "FAILED" || normalCode != "ACCOUNT_OWNER_UNAVAILABLE" || normalAccounts != 0 {
		t.Fatalf("normal domain parent disable must finish FAILED at newer owner version: %s/%s %s accounts=%d", normalResult, normalReservation, normalCode, normalAccounts)
	}
	released, err := loadTokenAccountOwnerCapacity(ctx, r.pool, "ORG_UNIT", unit)
	if err != nil || released.Count != normalCapacity.Count-1 {
		t.Fatal("terminal parent loss retained owner capacity", normalCapacity.Count, released.Count, err)
	}
	shared := wallet.wallets["ORG_UNIT:"+unit.String()]
	var retained uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT wallet_id FROM token_accounts WHERE name='combined-unit'`).Scan(&retained); err != nil || retained != shared || shared == uuid.Nil || wallet.receipts[normal.CommandReference].WalletID == nil || *wallet.receipts[normal.CommandReference].WalletID != shared {
		t.Fatal("shared owner wallet was changed on Token terminal failure", err)
	}
	setParent("ACTIVE")
	t.Log("actual AdminSetOrganizationStatus -> higher-version owner DENY -> FAILED/FAILED; reservation capacity released, zero new account, controlled shared wallet retained")
	// Bypass the domain writer only to simulate legacy/abnormal same-version
	// parent facts. Such evidence must still remain PENDING rather than weaken DENY.
	legacyCalls := walletCalls.Load()
	receipt := submitUnit("combined-parent-version")
	if walletCalls.Load() != legacyCalls {
		t.Fatal("T1 command submission called wallet supplier")
	}
	accountConfig, err := pgx.ParseConfig(os.Getenv("OWNER_COMBINED_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	accountConfig.User = "account_service"
	accountConfig.Database = os.Getenv("OWNER_COMBINED_ACCOUNT_DATABASE_NAME")
	account, err := pgx.ConnectConfig(ctx, accountConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer account.Close(context.Background())
	var actorName string
	if err = account.QueryRow(ctx, `SELECT current_user`).Scan(&actorName); err != nil || actorName != "account_service" {
		t.Fatal("owned Account runtime", err)
	}
	var beforeUnitVersion int64
	if err = account.QueryRow(ctx, `SELECT version FROM organization_units WHERE id=$1 AND organization_id=$2`, unit, org).Scan(&beforeUnitVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = account.Exec(ctx, `UPDATE organizations SET status='DISABLED',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := account.Exec(context.Background(), `UPDATE organizations SET status='ACTIVE',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, org); e != nil {
			t.Error(e)
		}
	}()
	did, err := worker.ProcessOne(ctx)
	if !did || err != nil {
		t.Fatal("real worker", did, err)
	}
	var result, reservation string
	var count int
	var expectedOwner int64
	if err = r.pool.QueryRow(ctx, `SELECT c.status,p.status,p.active_owner_version,(SELECT count(*) FROM token_accounts WHERE owner_type='ORG_UNIT' AND owner_id=$2 AND name='combined-parent-version') FROM token_command_results c JOIN token_account_provisioning_reservations p USING(command_reference) WHERE c.command_reference=$1`, receipt.CommandReference, unit).Scan(&result, &reservation, &expectedOwner, &count); err != nil {
		t.Fatal(err)
	}
	if result != "PENDING" || reservation != "PENDING" || count != 0 || expectedOwner != beforeUnitVersion {
		t.Fatalf("same-version parent decline changed effect %s/%s ownerVersion=%d accounts=%d", result, reservation, expectedOwner, count)
	}
	// ADM-61/APT-123-R2 explicitly obtains Billing receipt before T3. That
	// owner infrastructure can exist even though T3 cannot create a Token account.
	if walletCalls.Load() != legacyCalls+1 {
		t.Fatal("worker did not follow receipt-then-T3 contract")
	}
	check := ownerCombinedWorkerRequest(actor, unit, org, receipt.CommandReference, expectedOwner)
	decision, err := authority.CheckTokenOwnerAuthority(ctx, check)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Decision != "DENY" || decision.ReasonCode == nil || *decision.ReasonCode != "OWNER_INACTIVE" || *decision.ObservedOwnerVersion != *check.ExpectedOwnerVersion {
		t.Fatal("actual Account did not produce same-version negative")
	}
	// Prepare the already defined manual state in this owned fixture. Human
	// verification is controlled; both authority modes use the actual JWT receiver.
	if _, err := r.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='MANUAL_REVIEW',lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL,reservation_version=reservation_version+1 WHERE command_reference=$1`, receipt.CommandReference); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/commands/:command_reference/recovery-actions", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminTokenAccountRecoveryAction))
	recovery := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://controlled.invalid", Subject: "recovery-operator", AuthenticatedAt: time.Now()}, nil
	}, router)
	path := "/commands/" + receipt.CommandReference.String() + "/recovery-actions"
	body := []byte(`{"action":"REQUEUE","expected_recovery_epoch":1,"reason":"Controlled complete owner recovery"}`)
	key := uuid.NewString()
	facts := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT count(*) FROM token_account_provisioning_recovery_actions),(SELECT count(*) FROM token_audit_logs),(SELECT count(*) FROM outbox_events))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := facts()
	denied := f04Request(recovery, "POST", path, body, key)
	if denied.Code != 404 || facts() != before || walletCalls.Load() != legacyCalls+1 {
		t.Fatalf("inactive parent recovery effect: %d %s", denied.Code, denied.Body.String())
	}
	if _, err := account.Exec(ctx, `UPDATE organizations SET status='ACTIVE',version=version+1 WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := account.Exec(context.Background(), `UPDATE users SET status='ACTIVE',version=version+1 WHERE id=$1`, actor); err != nil {
			t.Error(err)
		}
	}()
	accepted := f04Request(recovery, "POST", path, body, key)
	var action TokenAccountRecoveryActionResult
	if accepted.Code != 202 || json.Unmarshal(accepted.Body.Bytes(), &action) != nil || action.Status != "SUCCEEDED" {
		t.Fatalf("actual operator recovery %d %s", accepted.Code, accepted.Body.String())
	}
	var creator, operator uuid.UUID
	var creatorVersion int64
	if err := r.pool.QueryRow(ctx, `SELECT p.actor_user_id,a.operator_actor_id,p.active_actor_version FROM token_account_provisioning_reservations p JOIN token_account_provisioning_recovery_actions a USING(command_reference) WHERE command_reference=$1`, receipt.CommandReference).Scan(&creator, &operator, &creatorVersion); err != nil || creator != actor || operator == actor || creatorVersion != 1 {
		t.Fatalf("operator replaced creator: %s %s version%d err%v", creator, operator, creatorVersion, err)
	}
	before = facts()
	replayed := f04Request(recovery, "POST", path, body, key)
	var replay TokenAccountRecoveryActionResult
	if replayed.Code != 200 || json.Unmarshal(replayed.Body.Bytes(), &replay) != nil || replay.ActionID != action.ActionID || facts() != before || walletCalls.Load() != legacyCalls+1 {
		t.Fatalf("recovery replay: %d %s", replayed.Code, replayed.Body.String())
	}
	t.Log("actual Token OPERATOR_AUTHORIZE/T3_OPERATOR REQUEUE succeeds with disabled creator; replay adds zero actions/audit/outbox and keeps creator checkpoint")
	t.Logf("legacy raw parent corruption only -> actual Account T3 -> ProcessOne: %s/%s, same unit.version=%d, zero new Token accounts", result, reservation, expectedOwner)
}

func ownerCombinedWorkerRequest(actor, unit, org, reference uuid.UUID, version int64) OwnerAuthorityCheck {
	actorID, unitID, kind := actor.String(), unit.String(), "ORG_UNIT"
	v := "1"
	ownerVersion := strconv.FormatInt(version, 10)
	digest := strings.Repeat("a", 64)
	actorDigest := tokenauthority.ActorIdentityDigest(actor, "ACTIVE", 1)
	ownerDigest := tokenauthority.UnitOwnerIdentityDigest(unit, org, "ACTIVE", version)
	q := OwnerAuthorityCheck{AuthoritySchemaVersion: "1", Mode: tokenauthority.ModeT3CreatorRecheck, ActorUserID: &actorID, OwnerType: &kind, OwnerID: &unitID, RequiredPermissions: tokenauthority.RequiredPermissions, CommandReference: reference.String(), CommandRequestDigest: &digest, ExpectedPlatformAuthorizationVersion: &v, ExpectedActorVersion: &v, ExpectedOwnerVersion: &ownerVersion, ExpectedActorIdentityDigest: &actorDigest, ExpectedOwnerIdentityDigest: &ownerDigest}
	q.AuthorityRequestSHA256 = q.Digest()
	return q
}

func TestOwnerCombinedGovernedOwnerCompatibility(t *testing.T) {
	organization, unit, user := uuid.New(), uuid.New(), uuid.New()
	cases := []governedAccountCreateCommand{
		{OwnerType: "USER", OwnerID: user},
		{OwnerType: "ORG", OwnerID: organization, OwnerOrganization: &organization},
		{OwnerType: "ORG_UNIT", OwnerID: unit, OwnerOrganization: &organization, OwnerUnit: &unit},
	}
	for _, command := range cases {
		for _, scope := range []string{"OWNER_UNIT_ONLY", "OWNER_SUBTREE", "ORG_ALL", "EXPLICIT_MEMBERS"} {
			command.ShareScope, command.Name, command.DefaultPriority = scope, "existing-validation", 100
			if _, err := parseGovernedAccountCreate(command.OwnerID.String(), mustMarshalJSON(t, command)); err != nil {
				t.Fatalf("existing validOwner/validShareScope %s/%s rejected: %v", command.OwnerType, scope, err)
			}
		}
		command.ShareScope = "UNKNOWN"
		if _, err := parseGovernedAccountCreate(command.OwnerID.String(), mustMarshalJSON(t, command)); err == nil {
			t.Fatal("unknown share scope accepted")
		}
	}
	invalid := cases[2]
	invalid.Name, invalid.ShareScope = "invalid-parent", "OWNER_SUBTREE"
	invalid.OwnerUnit = &user
	if _, err := parseGovernedAccountCreate(invalid.OwnerID.String(), mustMarshalJSON(t, invalid)); err == nil {
		t.Fatal("mismatched unit accepted")
	}
	invalid = cases[0]
	invalid.Name, invalid.ShareScope, invalid.OwnerOrganization = "membership-rejection", "OWNER_UNIT_ONLY", &organization
	if _, err := parseGovernedAccountCreate(invalid.OwnerID.String(), mustMarshalJSON(t, invalid)); err == nil {
		t.Fatal("USER parent accepted")
	}
}
