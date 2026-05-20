package messages

import (
	"net/http"
)

// metricsResponseWriter wraps http.ResponseWriter to capture token usage
// reported by logResponseStats for dashboard metrics.
type metricsResponseWriter struct {
	http.ResponseWriter
	inputTokens  int
	outputTokens int
	contextPct   float64
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	return &metricsResponseWriter{ResponseWriter: w}
}

// setUsage is called by response handlers to record token counts.
func (m *metricsResponseWriter) setUsage(inputTokens, outputTokens int, contextPct float64) {
	m.inputTokens = inputTokens
	m.outputTokens = outputTokens
	m.contextPct = contextPct
}

// Flush implements http.Flusher by delegating to the underlying writer.
func (m *metricsResponseWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap allows http.ResponseController and similar to access the underlying writer.
func (m *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return m.ResponseWriter
}
