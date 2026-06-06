package respconv

// Usage is the final Anthropic-compatible token accounting emitted to the
// downstream client and copied into local metrics.
type Usage struct {
	InputTokens                int
	OutputTokens               int
	CacheReadInputTokens       int
	CacheWriteInputTokens      int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

// UsageAdjuster can rewrite final usage just before a response is emitted.
// It is intentionally generic so optional modules such as local prompt-cache
// simulation can plug in without being coupled to response conversion.
type UsageAdjuster func(Usage) Usage

func (u Usage) mapValue() map[string]any {
	m := map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"cache_creation_input_tokens": u.CacheWriteInputTokens,
	}
	if u.CacheCreation5mInputTokens > 0 || u.CacheCreation1hInputTokens > 0 {
		m["cache_creation_5m_input_tokens"] = u.CacheCreation5mInputTokens
		m["cache_creation_1h_input_tokens"] = u.CacheCreation1hInputTokens
	}
	return m
}
