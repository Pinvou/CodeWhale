package account

import (
	"bytes"
	"strings"
	"testing"
)

func TestUserCodeGenerationNormalizationAndFormatting(t *testing.T) {
	code, err := generateUserCode(bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}))
	if err != nil {
		t.Fatal(err)
	}
	if code != "USR0123456789ABCDEFMV" {
		t.Fatalf("code=%q", code)
	}
	formatted := FormatUserCode(code)
	if formatted != "USR-0123-4567-89AB-CDEF-MV" {
		t.Fatalf("formatted=%q", formatted)
	}
	if normalized, err := NormalizeUserCode(strings.ToLower(formatted)); err != nil || normalized != code {
		t.Fatalf("normalized=%q error=%v", normalized, err)
	}
}

func TestUserCodeRejectsInvalidAlphabetLengthPrefixAndChecksum(t *testing.T) {
	for _, value := range []string{
		"", "CUS-0123-4567-89AB-CDEFG-6T", "USR-0123-4567-89AB-CDEFI-6T",
		"USR-0123-4567-89AB-CDEFG-6V", "USR-0123-4567-89AB-CDEF-6T",
	} {
		if _, err := NormalizeUserCode(value); err == nil {
			t.Fatalf("invalid code accepted: %q", value)
		}
	}
}

func TestUserCodePayloadUsesEveryCrockfordSymbolWithoutBias(t *testing.T) {
	input := make([]byte, 256)
	for index := range input {
		input[index] = byte(index)
	}
	counts := map[byte]int{}
	for offset := 0; offset < len(input); offset += userCodePayloadLength {
		code, err := generateUserCode(bytes.NewReader(input[offset : offset+userCodePayloadLength]))
		if err != nil {
			t.Fatal(err)
		}
		for index := len(userCodePrefix); index < len(userCodePrefix)+userCodePayloadLength; index++ {
			counts[code[index]]++
		}
	}
	for _, symbol := range []byte(userCodeAlphabet) {
		if counts[symbol] != 8 {
			t.Fatalf("symbol %q count=%d", symbol, counts[symbol])
		}
	}
}

func TestOneMillionDeterministicUserCodeCandidatesAreUniqueAndValid(t *testing.T) {
	const total = 1_000_000
	seen := make(map[string]struct{}, total)
	var input [userCodePayloadLength]byte
	for value := 0; value < total; value++ {
		input[0] = byte(value & 31)
		input[1] = byte((value >> 5) & 31)
		input[2] = byte((value >> 10) & 31)
		input[3] = byte((value >> 15) & 31)
		code, err := generateUserCode(bytes.NewReader(input[:]))
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != userCodeStorageLength || !strings.HasPrefix(code, userCodePrefix) {
			t.Fatalf("candidate %d has invalid format: %s", value, code)
		}
		if value%1024 == 0 || value == total-1 {
			if _, err := NormalizeUserCode(code); err != nil {
				t.Fatalf("candidate %d has invalid checksum: %v", value, err)
			}
		}
		if _, exists := seen[code]; exists {
			t.Fatalf("candidate %d collided: %s", value, code)
		}
		seen[code] = struct{}{}
	}
}
