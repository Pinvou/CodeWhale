package usagewaiver

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func contractCommand() Command {
	c := Command{Version: Version, Reference: uuid.New(), HoldID: uuid.New(), UsageID: uuid.New(), WalletID: uuid.New(), Maximum: 9007199254740993, AllocationDigest: strings.Repeat("a", 64), ActorID: uuid.New(), Issuer: "https://issuer.invalid", Subject: "operator", AuthorizationVersion: 19, Reason: "explicit release-only decision", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	c.Digest = c.Fingerprint()
	return c
}
func TestUsageWaiverWireAuthority(t *testing.T) {
	t.Run("amounts-remain-exact-beyond-javascript-safe-integer", func(t *testing.T) {
		c := contractCommand()
		raw, err := json.Marshal(c)
		if err != nil || !strings.Contains(string(raw), `"maximum_microcredits":"9007199254740993"`) {
			t.Fatal("money not exact decimal string", string(raw), err)
		}
		var got Command
		if json.Unmarshal(raw, &got) != nil || got != c || !got.Valid() {
			t.Fatal("command roundtrip changed identity")
		}
		for _, numeric := range []string{`"maximum_microcredits":9007199254740993`, `"maximum_microcredits":null`} {
			bad := strings.Replace(string(raw), `"maximum_microcredits":"9007199254740993"`, numeric, 1)
			var rejected Command
			if json.Unmarshal([]byte(bad), &rejected) == nil && rejected.Valid() {
				t.Fatal("missing or numeric amount borrowed a valid monetary identity", numeric)
			}
		}
	})
	t.Run("every-frozen-identity-and-actor-change-invalidates-command", func(t *testing.T) {
		c := contractCommand()
		for _, change := range []func(*Command){func(v *Command) { v.Maximum++ }, func(v *Command) { v.HoldID = uuid.New() }, func(v *Command) { v.WalletID = uuid.New() }, func(v *Command) { v.ActorID = uuid.New() }, func(v *Command) { v.AuthorizationVersion++ }, func(v *Command) { v.Subject = "another" }, func(v *Command) { v.Reason = "changed" }, func(v *Command) { v.AllocationDigest = strings.Repeat("b", 64) }} {
			changed := c
			change(&changed)
			if changed.Valid() {
				t.Fatal("changed frozen facts accepted")
			}
		}
	})
	t.Run("rejection-is-explicit-original-authority-not-status-or-empty-json", func(t *testing.T) {
		c := contractCommand()
		d := Decision{CommandReference: c.Reference, RequestDigest: c.Digest, Admission: Rejected, ReasonCode: "EXECUTION_ACTIVE"}
		if !d.ValidFor(c) {
			t.Fatal("valid original rejection refused")
		}
		for _, change := range []func(*Decision){func(v *Decision) { v.CommandReference = uuid.New() }, func(v *Decision) { v.RequestDigest = strings.Repeat("a", 64) }, func(v *Decision) { v.ReasonCode = "HTTP_403" }, func(v *Decision) { v.Admission = "UNKNOWN" }, func(v *Decision) { v.Result = &Result{} }} {
			bad := d
			change(&bad)
			if bad.ValidFor(c) {
				t.Fatal("non-authoritative rejection accepted", bad)
			}
		}
	})
	t.Run("zero-actual-alone-is-not-successful-waiver-proof", func(t *testing.T) {
		c := contractCommand()
		zero := int64(0)
		r := Result{CommandReference: c.Reference, RequestDigest: c.Digest, HoldID: c.HoldID, UsageID: c.UsageID, WalletID: c.WalletID, Maximum: c.Maximum, Outcome: Waived, HoldStatus: "RELEASED", Actual: &zero, UsageSnapshot: json.RawMessage(`{}`), CompletedAt: time.Now()}
		if r.Matches(c) {
			t.Fatal("zero actual without admin proof accepted")
		}
		r.UsageSnapshot, _ = json.Marshal(map[string]any{"admin_waiver": true, "waiver_command_reference": c.Reference, "waiver_request_digest": c.Digest, "actor_user_id": c.ActorID, "actor_issuer": c.Issuer, "actor_subject": c.Subject, "reason": c.Reason, "release_reason": "admin_waiver"})
		if !r.Matches(c) {
			t.Fatal("exact genuine proof refused")
		}
		wrong := c
		wrong.Reason = "different operator decision"
		wrong.Digest = wrong.Fingerprint()
		if r.Matches(wrong) {
			t.Fatal("proof attached to another frozen decision")
		}
	})
}
