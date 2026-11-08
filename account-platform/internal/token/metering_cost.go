package token

func MaximumUsage(maxInputTokens, maxOutputTokens int64) Usage {
	return Usage{InputTokens: maxInputTokens, OutputTokens: maxOutputTokens}
}
