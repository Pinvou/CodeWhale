package approvalencoding

import (
	"bytes"
	"testing"
)

func TestIndependentCanonicalUnicode(t *testing.T) {
	cases := []struct{ input, want string }{
		{`{"text":"<>&/\u2028\u2029😀e\u0301"}`, "{\"text\":\"<>&/\u2028\u2029😀e\u0301\"}"},
		{`{"\ue000":"later","😀":"first"}`, "{\"😀\":\"first\",\"\ue000\":\"later\"}"},
		{`{"text":"\b\t\n\f\r\u0000\"\\"}`, `{"text":"\b\t\n\f\r\u0000\"\\"}`},
	}
	for _, test := range cases {
		value, err := parseDocument([]byte(test.input))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := canonicalJSON(value)
		if err != nil || string(actual) != test.want {
			t.Fatalf("canonical Unicode mismatch: got %q, want %q, err %v", actual, test.want, err)
		}
	}
	decomposed, _ := parseDocument([]byte(`{"text":"e\u0301"}`))
	composed, _ := parseDocument([]byte(`{"text":"é"}`))
	first, _ := canonicalJSON(decomposed)
	second, _ := canonicalJSON(composed)
	if bytes.Equal(first, second) {
		t.Fatal("canonical encoding must not normalize Unicode")
	}
	for _, raw := range []string{`{"text":"\ud800"}`, `{"text":"\udfff"}`, "{\"text\":\"\xff\"}"} {
		if _, err := parseDocument([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid Unicode %q", raw)
		}
	}
}
