package respconv

// finalResult bundles the Anthropic-compatible stop reason and usage payload
// derived from an accumulator. Both streaming and non-streaming paths emit
// identical values here; callers just pack them into different wire formats.
type finalResult struct {
	StopReason          string
	StopSequence        any
	InputTokens         int
	OutputTokens        int
	RawInputTokens      int
	RawOutputTokens     int
	RawCacheReadTokens  int
	RawCacheWriteTokens int
	Usage               map[string]any
}

// finalizeResult consolidates the FinalizeStream → resolveStopReason →
// resolvedUsage → UsageMap pipeline shared by streaming.Finish and
// nonstreaming.buildResponseFromAcc. FinalizeStream mutates the accumulator,
// so this must be called exactly once per stream.
func finalizeResult(a *responseAccumulator) (textDelta, thinkingDelta string, r finalResult) {
	textDelta, thinkingDelta = a.FinalizeStream()
	r.StopReason, r.StopSequence = a.resolveStopReason()
	r.InputTokens, r.OutputTokens = a.rawResolvedUsage()
	r.RawInputTokens = r.InputTokens
	r.RawOutputTokens = r.OutputTokens
	r.RawCacheReadTokens = a.CacheReadInputTokens
	r.RawCacheWriteTokens = a.CacheWriteInputTokens
	a.RawInputTokens = r.RawInputTokens
	a.RawOutputTokens = r.RawOutputTokens
	a.RawCacheReadTokens = r.RawCacheReadTokens
	a.RawCacheWriteTokens = r.RawCacheWriteTokens
	usage := Usage{
		InputTokens:           r.InputTokens,
		OutputTokens:          r.OutputTokens,
		CacheReadInputTokens:  a.CacheReadInputTokens,
		CacheWriteInputTokens: a.CacheWriteInputTokens,
	}
	if a.UsageAdjuster != nil {
		usage = a.UsageAdjuster(usage)
		if usage.InputTokens < 0 {
			usage.InputTokens = 0
		}
		if usage.OutputTokens < 0 {
			usage.OutputTokens = 0
		}
		if usage.CacheReadInputTokens < 0 {
			usage.CacheReadInputTokens = 0
		}
		if usage.CacheWriteInputTokens < 0 {
			usage.CacheWriteInputTokens = 0
		}
		if usage.CacheCreation5mInputTokens < 0 {
			usage.CacheCreation5mInputTokens = 0
		}
		if usage.CacheCreation1hInputTokens < 0 {
			usage.CacheCreation1hInputTokens = 0
		}
	}
	a.FinalUsage = &usage
	r.InputTokens = usage.InputTokens
	r.OutputTokens = usage.OutputTokens
	r.Usage = a.UsageMap(r.InputTokens, r.OutputTokens)
	return
}
