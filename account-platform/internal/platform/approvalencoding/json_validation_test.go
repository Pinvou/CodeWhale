package approvalencoding

import (
	"strings"
	"testing"
)

func TestJSONParserClosedBoundaries(t *testing.T) {
	for name, raw := range map[string]string{
		"decoded duplicate key": `{"a":1,"\u0061":2}`,
		"raw control":           "{\"a\":\"\x01\"}",
		"trailing value":        `{"a":1}{"b":2}`,
		"lone high surrogate":   `{"a":"\ud800"}`,
		"lone low surrogate":    `{"a":"\udfff"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDocument([]byte(raw)); err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"escaped quote":   `{"a":"\""}`,
		"surrogate pair":  `{"a":"\ud83d\ude00"}`,
		"maximum depth":   strings.Repeat("[", maximumJSONDepth) + "0" + strings.Repeat("]", maximumJSONDepth),
		"ordinary object": `{"a":"ok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDocument([]byte(raw)); err != nil {
				t.Fatalf("valid JSON rejected: %v", err)
			}
		})
	}
	tooDeep := strings.Repeat("[", maximumJSONDepth+1) + "0" + strings.Repeat("]", maximumJSONDepth+1)
	if _, err := parseDocument([]byte(tooDeep)); err == nil {
		t.Fatal("over-depth JSON accepted")
	}
}

func TestProtocolNumbersRejectNonIntegerForms(t *testing.T) {
	for _, raw := range []string{"1.0", "1e0", "-0", "9223372036854775808", "-9223372036854775809"} {
		value, err := parseDocument([]byte(raw))
		if err != nil {
			continue
		}
		if _, err := canonicalJSON(value); err == nil {
			t.Fatalf("non-protocol number %q canonicalized", raw)
		}
	}
}
