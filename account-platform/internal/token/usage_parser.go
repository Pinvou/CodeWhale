package token

import (
	"encoding/json"
)

func parseUsageJSON(payload []byte) (Usage, bool) {
	var body struct {
		Usage *struct {
			PromptTokens       *int64 `json:"prompt_tokens"`
			CompletionTokens   *int64 `json:"completion_tokens"`
			PromptTokenDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Usage == nil || body.Usage.PromptTokens == nil || body.Usage.CompletionTokens == nil {
		return Usage{}, false
	}
	usage := Usage{InputTokens: *body.Usage.PromptTokens, OutputTokens: *body.Usage.CompletionTokens}
	if body.Usage.PromptTokenDetails != nil {
		usage.CachedInputTokens = body.Usage.PromptTokenDetails.CachedTokens
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return Usage{}, false
	}
	usage.InputTokens -= usage.CachedInputTokens
	return usage, true
}
