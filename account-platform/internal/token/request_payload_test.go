package token

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatCompletionPayloadPreservesNumericFacts(t *testing.T) {
	for _, number := range []string{"9007199254740993", "9223372036854775807", "-9223372036854775808", "0.1234567890123456789", "1.234567890123456789e+20"} {
		t.Run(number, func(t *testing.T) {
			body := []byte(`{"model":"client","seed":` + number + `,"tools":[{"function":{"parameters":{"const":` + number + `}}}]}`)
			payload, err := decodeChatCompletionPayload(body)
			if err != nil {
				t.Fatal(err)
			}
			payload["model"] = "upstream"
			forwarded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(forwarded), `"seed":`+number) || !strings.Contains(string(forwarded), `"const":`+number) || !strings.Contains(string(forwarded), `"model":"upstream"`) {
				t.Fatal("changing the upstream model altered unrelated numeric facts")
			}
		})
	}
}

func TestChatCompletionPayloadRejectsIncompleteJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "null": "null", "array": "[]", "second-object": `{} {}`,
		"second-scalar": `{} true`, "garbage": `{} invalid`, "truncated": `{"seed":`,
	} {
		t.Run(name, func(t *testing.T) {
			if payload, err := decodeChatCompletionPayload([]byte(body)); err == nil || payload != nil {
				t.Fatal("incomplete or non-object JSON accepted as a complete request")
			}
		})
	}
	if _, err := decodeChatCompletionPayload([]byte("{} \n\t")); err != nil {
		t.Fatal("complete JSON with trailing whitespace was rejected")
	}
}

func TestDefaultChatMaximumOutputPreservesNumericFacts(t *testing.T) {
	body := []byte(`{"model":"client","seed":9007199254740993,"tools":[{"function":{"parameters":{"const":9007199254740993}}}]}`)
	request := &chatCompletionRequest{}
	normalized, err := applyDefaultChatMaximumOutput(body, request, 128)
	if err != nil {
		t.Fatal(err)
	}
	if request.MaxCompletionTokens != 128 || !strings.Contains(string(normalized), `"max_completion_tokens":128`) || !strings.Contains(string(normalized), `"seed":9007199254740993`) || !strings.Contains(string(normalized), `"const":9007199254740993`) {
		t.Fatal("default maximum changed unrelated request facts or omitted the maximum")
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(key, func(t *testing.T) {
			explicit := []byte(`{"model":"client","` + key + `":0,"seed":9007199254740993}`)
			kept, err := applyDefaultChatMaximumOutput(explicit, &chatCompletionRequest{}, 128)
			if err != nil || string(kept) != string(explicit) {
				t.Fatal("an explicitly supplied maximum was replaced")
			}
		})
	}
}
