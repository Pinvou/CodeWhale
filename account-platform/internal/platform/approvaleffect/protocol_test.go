package approvaleffect

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestObservationClosedCommandAuthority(t *testing.T) {
	if authority, ok := AuthorityForCommand("WALLET_ADJUSTMENT"); !ok || authority != AuthorityBilling {
		t.Fatal("billing command authority missing")
	}
	if authority, ok := AuthorityForCommand("TOKEN_CREDENTIAL_REVOKE"); !ok || authority != AuthorityToken {
		t.Fatal("installed Token command authority missing")
	}
	if _, ok := AuthorityForCommand("UNKNOWN_FINANCIAL_REPAIR"); ok {
		t.Fatal("future financial repair acquired an authority")
	}

	if _, ok := AuthorityForCommand("UNKNOWN"); ok {
		t.Fatal("unknown command acquired an authority")
	}
	valid := Observation{SchemaVersion: 1, Authority: AuthorityBilling, ApprovalRequestID: uuid.Must(uuid.NewV7()),
		CommandType: "WALLET_ADJUSTMENT", CommandFingerprint: strings.Repeat("a", 64), State: StateAbsent, ObservedAt: time.Now().UTC()}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Observation){
		"schema":      func(o *Observation) { o.SchemaVersion++ },
		"authority":   func(o *Observation) { o.Authority = "TOKEN" },
		"request":     func(o *Observation) { o.ApprovalRequestID = uuid.Nil },
		"command":     func(o *Observation) { o.CommandType = "UNKNOWN" },
		"fingerprint": func(o *Observation) { o.CommandFingerprint = strings.Repeat("A", 64) },
		"state":       func(o *Observation) { o.State = "PRESENT" },
		"time":        func(o *Observation) { o.ObservedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if !errors.Is(candidate.Validate(), ErrInvalidObservation) {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}
