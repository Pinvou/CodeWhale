package token

import (
	"bytes"
	"encoding/json"

	"math"
)

// EstimateChatUsage applies the published CHAT v1 estimator. It intentionally
// uses a versioned, deterministic byte rule instead of a provider-dependent
// tokenizer so the same request always receives the same preauthorization.
// It counts only content strings and string-valued text fields in content
// parts. Image, audio and other non-text fields contribute zero estimated
// input tokens; trusted provider usage still determines actual settlement.
func EstimateChatUsage(payload []byte, maxOutputTokens int64) (Usage, error) {
	if maxOutputTokens < 0 {
		return Usage{}, ErrMeteringConfiguration
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var request struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := decoder.Decode(&request); err != nil {
		return Usage{}, ErrMeteringConfiguration
	}
	var bytesTotal int64
	for _, message := range request.Messages {
		count, err := meteredContentBytes(message.Content)
		if err != nil || bytesTotal > math.MaxInt64-count {
			return Usage{}, ErrMeteringConfiguration
		}
		bytesTotal += count
	}
	inputTokens := bytesTotal / 4
	if bytesTotal%4 != 0 {
		inputTokens++
	}
	return Usage{InputTokens: inputTokens, OutputTokens: maxOutputTokens}, nil
}

func meteredContentBytes(content any) (int64, error) {
	switch value := content.(type) {
	case nil:
		return 0, nil
	case string:
		return int64(len([]byte(value))), nil
	case []any:
		var total int64
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return 0, ErrMeteringConfiguration
			}
			text, _ := part["text"].(string)
			if total > math.MaxInt64-int64(len([]byte(text))) {
				return 0, ErrMeteringConfiguration
			}
			total += int64(len([]byte(text)))
		}
		return total, nil
	default:
		return 0, ErrMeteringConfiguration
	}
}
