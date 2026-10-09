package approvalconsumer

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestSequenceFullUnsignedRange(t *testing.T) {
	for _, value := range []uint64{1, 9007199254740993, math.MaxInt64, uint64(math.MaxInt64) + 1, math.MaxUint64} {
		literal := strconv.FormatUint(value, 10)
		parsed, err := ParseSequence(literal)
		if err != nil || uint64(parsed) != value || parsed.String() != literal {
			t.Fatalf("parse %s lost precision: %v", literal, err)
		}
		encoded, err := json.Marshal(parsed)
		if err != nil || string(encoded) != `"`+literal+`"` {
			t.Fatalf("marshal %s: %s %v", literal, encoded, err)
		}
		var decoded Sequence
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != parsed {
			t.Fatalf("round trip %s: %v", literal, err)
		}
	}
}

func TestSequenceRejectsNoncanonicalAndInvalidValues(t *testing.T) {
	for _, value := range []string{"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1\n", "1.0", "1e1", "18446744073709551616", "١"} {
		if _, err := ParseSequence(value); !errors.Is(err, ErrInvalidSequence) {
			t.Errorf("accepted noncanonical sequence %q", value)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded := Sequence(7)
		if err := json.Unmarshal(encoded, &decoded); !errors.Is(err, ErrInvalidSequence) || decoded != 7 {
			t.Errorf("invalid sequence %q changed receiver or was accepted", value)
		}
	}
	for _, raw := range []string{`null`, `1`, `9007199254740993`, `true`, `{}`, `[]`, `"\u0031"`, `"1\u0030"`} {
		decoded := Sequence(7)
		if err := json.Unmarshal([]byte(raw), &decoded); !errors.Is(err, ErrInvalidSequence) || decoded != 7 {
			t.Errorf("invalid JSON %s changed receiver or was accepted", raw)
		}
	}
	if _, err := json.Marshal(Sequence(0)); !errors.Is(err, ErrInvalidSequence) {
		t.Fatal("zero sequence serialized")
	}
	var absent *Sequence
	if err := absent.UnmarshalJSON([]byte(`"1"`)); !errors.Is(err, ErrInvalidSequence) {
		t.Fatal("nil receiver accepted")
	}
}
