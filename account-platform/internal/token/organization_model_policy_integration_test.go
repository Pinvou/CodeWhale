package token

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Public Token migrations and the non-owner runtime actor are used. Policy
// callers are explicit controlled identities; this does not attest Account
// authorization, a deployed issuer or supplier discovery.
func TestOrganizationModelPolicyPublicPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor := uuid.New()
	readCount := func(org uuid.UUID, want int) {
		t.Helper()
		var commands, audits int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM organization_model_policy_commands WHERE organization_id=$1),(SELECT count(*) FROM organization_model_policy_audit WHERE organization_id=$1)`, org).Scan(&commands, &audits); err != nil || commands != want || audits != want {
			t.Fatalf("immutable receipts/audits count: commands=%d audits=%d want=%d err=%v", commands, audits, want, err)
		}
	}
	t.Run("initial-default-is-not-a-stored-policy", func(t *testing.T) {
		org := uuid.New()
		value, err := r.GetOrganizationModelPolicy(ctx, org)
		if err != nil || value.Version != 0 || value.Mode != "ALLOW_ALL" || value.OrganizationID != org || len(value.ModelIDs) != 0 {
			t.Fatalf("initial policy facts: %+v %v", value, err)
		}
		readCount(org, 0)
	})
	t.Run("invalid-input-preserves-original-policy-and-next-write", func(t *testing.T) {
		org := uuid.New()
		first, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, "policy-input-original-key", "controlled original")
		if err != nil {
			t.Fatal(err)
		}
		facts := func() string {
			t.Helper()
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'policy',(SELECT to_jsonb(x) FROM organization_model_policies x WHERE organization_id=$1),
 'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY idempotency_key) FROM organization_model_policy_commands x WHERE organization_id=$1),
 'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM organization_model_policy_audit x WHERE organization_id=$1))::text`, org).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := facts()
		for _, invalid := range []struct {
			mode, key, reason string
			models            []uuid.UUID
			version           int64
		}{
			{"INVALID", "controlled-invalid-key", "controlled", nil, 1},
			{"ALLOW_ALL", "controlled-invalid-key", "", nil, 1},
			{"ALLOW_ALL", "short", "controlled", nil, 1},
			{"ALLOW_LIST", "controlled-invalid-key", "controlled", []uuid.UUID{uuid.Nil}, 1},
			{"ALLOW_ALL", "controlled-invalid-key", "controlled", []uuid.UUID{uuid.New()}, 1},
			{"ALLOW_ALL", "controlled-invalid-key", "controlled", nil, -1},
		} {
			_, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, invalid.version, invalid.mode, invalid.models, invalid.key, invalid.reason)
			if !errors.Is(err, errOrganizationPolicyInvalidInput) || errors.Is(err, ErrForbidden) || facts() != before {
				t.Fatal("invalid input changed immutable policy facts", err)
			}
		}
		next, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, first.Version, "ALLOW_LIST", nil, "policy-input-next-valid-key", "controlled next")
		if err != nil || next.Version != first.Version+1 {
			t.Fatal("valid write after input rejection failed", err)
		}
		replay, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, "policy-input-original-key", "controlled original")
		if err != nil || replay.Version != first.Version || replay.Mode != first.Mode {
			t.Fatal("original receipt changed after rejected input", err)
		}
		readCount(org, 2)
	})
	t.Run("nil-and-explicit-empty-model-sets-have-identical-meaning", func(t *testing.T) {
		for _, mode := range []string{"ALLOW_ALL", "ALLOW_LIST", "DENY_LIST"} {
			org, key := uuid.New(), "organization-policy-empty-"+mode
			first, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, mode, nil, key, "controlled empty selection")
			if err != nil || first.Version != 1 || first.Mode != mode || len(first.ModelIDs) != 0 {
				t.Fatalf("valid empty mode=%s: %+v %v", mode, first, err)
			}
			replay, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, mode, []uuid.UUID{}, key, "controlled empty selection")
			if err != nil || replay.Version != first.Version || replay.Mode != mode || len(replay.ModelIDs) != 0 {
				t.Fatalf("nil/empty replay mode=%s: %+v %v", mode, replay, err)
			}
			allowed, err := r.OrganizationModelAllowed(ctx, org, uuid.New())
			if err != nil || allowed != (mode != "ALLOW_LIST") {
				t.Fatalf("empty selection changed mode=%s semantics: allowed=%v err=%v", mode, allowed, err)
			}
			readCount(org, 1)
		}
	})
	t.Run("immutable-replay-after-later-revision-and-actor-or-facts-conflict", func(t *testing.T) {
		org := uuid.New()
		key := "organization-policy-first-key"
		first, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, key, "controlled first")
		if err != nil || first.Version != 1 {
			t.Fatalf("first policy: %+v %v", first, err)
		}
		if _, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 1, "DENY_LIST", nil, "organization-policy-second-key", "controlled second"); err != nil {
			t.Fatal(err)
		}
		replay, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, key, "controlled first")
		if err != nil || replay.Version != first.Version || replay.Mode != first.Mode || replay.UpdatedBy == nil || *replay.UpdatedBy != actor {
			t.Fatalf("replay did not return original owned version: %+v %v", replay, err)
		}
		for _, mutation := range []struct {
			actor  uuid.UUID
			reason string
		}{{uuid.New(), "controlled first"}, {actor, "different facts"}} {
			if _, err := r.SaveOrganizationModelPolicy(ctx, org, mutation.actor, 3, 0, "ALLOW_ALL", nil, key, mutation.reason); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("foreign actor/facts reused receipt: %v", err)
			}
		}
		readCount(org, 2)
	})
	t.Run("same-key-first-writers-replay-one-version", func(t *testing.T) {
		org := uuid.New()
		start := make(chan struct{})
		values := make([]OrganizationModelPolicy, 2)
		errorsByWorker := make([]error, 2)
		var group sync.WaitGroup
		for index := range values {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				<-start
				values[index], errorsByWorker[index] = r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, "organization-policy-concurrent-key", "controlled concurrent")
			}(index)
		}
		close(start)
		group.Wait()
		for index := range values {
			if errorsByWorker[index] != nil || values[index].Version != 1 {
				t.Fatalf("first same-key worker=%d value=%+v err=%v", index, values[index], errorsByWorker[index])
			}
		}
		readCount(org, 1)
	})
	t.Run("different-key-initial-CAS-has-one-winner", func(t *testing.T) {
		org := uuid.New()
		start := make(chan struct{})
		result := make(chan error, 2)
		for index := 0; index < 2; index++ {
			go func(index int) {
				<-start
				_, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_ALL", nil, "organization-policy-race-"+strings.Repeat("k", index+1), "controlled race")
				result <- err
			}(index)
		}
		close(start)
		var wins, conflicts int
		for index := 0; index < 2; index++ {
			err := <-result
			if err == nil {
				wins++
			} else if errors.Is(err, ErrModelPolicyVersionConflict) {
				conflicts++
			} else {
				t.Fatalf("unexpected first-policy CAS error: %v", err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("CAS outcomes wins=%d conflicts=%d", wins, conflicts)
		}
		readCount(org, 1)
	})
	t.Run("stale-CAS-and-unavailable-model-do-not-write", func(t *testing.T) {
		org := uuid.New()
		if _, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 1, "ALLOW_ALL", nil, "organization-policy-stale-key", "controlled stale"); !errors.Is(err, ErrModelPolicyVersionConflict) {
			t.Fatalf("stale initial version accepted: %v", err)
		}
		_, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 3, 0, "ALLOW_LIST", []uuid.UUID{uuid.New()}, "organization-policy-model-key", "controlled model")
		if !errors.Is(err, ErrModelUnavailable) || !errors.Is(err, errOrganizationPolicyModelUnavailable) {
			t.Fatalf("unknown model accepted: %v", err)
		}
		out := httptest.NewRecorder()
		(&Service{}).writeError(out, httptest.NewRequest(http.MethodPut, "/v1/organizations/"+org.String()+"/model-policy", nil), err)
		if out.Code != http.StatusUnprocessableEntity || !strings.Contains(out.Body.String(), `"code":"MODEL_UNAVAILABLE"`) || out.Header().Get("X-Pinvou-Model-Rejection-v1") != "" {
			t.Fatal("policy selection incorrectly rejected a conversation model", out.Code, out.Body.String(), out.Header())
		}
		readCount(org, 0)
	})
}
