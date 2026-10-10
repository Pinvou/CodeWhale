package billing

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistrationBonusRevisionPreservesExactWire(t *testing.T) {
	for _, raw := range []string{`9007199254740993`, `"9007199254740993"`} {
		var revision registrationBonusRevision
		if err := json.Unmarshal([]byte(raw), &revision); err != nil || int64(revision) != 9007199254740993 {
			t.Fatalf("exact revision decode: %v %d", err, revision)
		}
	}
	for _, raw := range []string{`null`, `0`, `-1`, `1.5`, `1e3`, `"01"`, `"+1"`, `"9223372036854775808"`} {
		var revision registrationBonusRevision
		if json.Unmarshal([]byte(raw), &revision) == nil {
			t.Fatalf("invalid revision accepted: %s", raw)
		}
	}
	w := httptest.NewRecorder()
	writeRegistrationBonusPolicy(w, RegistrationBonusPolicy{Revision: 9007199254740993, AmountMicrocredits: 9007199254740000})
	if !strings.Contains(w.Body.String(), `"revision":9007199254740993`) || !strings.Contains(w.Body.String(), `"amount_microcredits":9007199254740000`) {
		t.Fatalf("lossy policy wire: %s", w.Body.String())
	}
}
