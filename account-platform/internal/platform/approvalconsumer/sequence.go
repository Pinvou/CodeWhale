package approvalconsumer

import (
	"errors"
	"strconv"
)

var ErrInvalidSequence = errors.New("invalid approval stream sequence")

// Sequence preserves the complete positive JetStream uint64 domain. Its JSON
// representation is a canonical decimal string, never a floating-point number.
// The zero value is invalid; callers must validate it before database dispatch.
type Sequence uint64

func ParseSequence(value string) (Sequence, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return 0, ErrInvalidSequence
	}
	return Sequence(parsed), nil
}

func (sequence Sequence) String() string {
	return strconv.FormatUint(uint64(sequence), 10)
}

func (sequence Sequence) MarshalJSON() ([]byte, error) {
	if sequence == 0 {
		return nil, ErrInvalidSequence
	}
	return strconv.AppendQuote(nil, sequence.String()), nil
}

func (sequence *Sequence) UnmarshalJSON(raw []byte) error {
	if sequence == nil || len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return ErrInvalidSequence
	}
	// Parse literal digits rather than unescaping aliases such as "\u0031".
	parsed, err := ParseSequence(string(raw[1 : len(raw)-1]))
	if err != nil {
		return err
	}
	*sequence = parsed
	return nil
}
