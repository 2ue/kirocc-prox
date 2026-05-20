// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Helpers for sanitizing/normalizing InvalidToolCall records before they are
// reflowed to the model as tool_result(is_error=true) or surfaced as fallback.

package respconv

import (
	"unicode/utf8"
)

func truncateLogValue(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	for i := range s {
		if maxRunes == 0 {
			return s[:i] + "…"
		}
		maxRunes--
	}
	return s
}

// discardInvalidToolCallWarning clears the recorded invalid tool calls without
// emitting any text. The caller (streaming Finish / non-streaming
// BuildResponse) is responsible for ensuring the proxy never synthesizes
// assistant-visible text describing dropped tool_use; if such text reached the
// client it would be treated as the model's own output and pollute history.
// Invalid call details remain available for audit via slog (logged at the
// drop site in event_processor.go) and for retry orchestration via the
// InvalidToolCalls() accessor before this function runs.
func (a *responseAccumulator) discardInvalidToolCallWarning() {
	a.InvalidToolCalls = nil
}
