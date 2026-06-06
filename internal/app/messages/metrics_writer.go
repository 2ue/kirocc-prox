package messages

import (
	"net/http"
	"time"

	"github.com/niuma/kirocc-pro/internal/respconv"
)

// metricsResponseWriter wraps http.ResponseWriter to capture token usage
// reported by logResponseStats for dashboard metrics.
type metricsResponseWriter struct {
	http.ResponseWriter
	startedAt           time.Time
	inputTokens         int
	outputTokens        int
	cacheReadTokens     int
	cacheWriteTokens    int
	rawInputTokens      int
	rawOutputTokens     int
	rawCacheReadTokens  int
	rawCacheWriteTokens int
	firstTokenMs        int
	contextPct          float64
}

func newMetricsResponseWriter(w http.ResponseWriter, startedAt ...time.Time) *metricsResponseWriter {
	start := time.Now()
	if len(startedAt) > 0 && !startedAt[0].IsZero() {
		start = startedAt[0]
	}
	return &metricsResponseWriter{ResponseWriter: w, startedAt: start}
}

// setUsage is called by response handlers to record token counts.
func (m *metricsResponseWriter) setUsage(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int, contextPct float64) {
	m.setUsageDetailed(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, contextPct)
}

func (m *metricsResponseWriter) setUsageDetailed(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, rawInputTokens, rawOutputTokens, rawCacheReadTokens, rawCacheWriteTokens int, contextPct float64) {
	m.inputTokens = inputTokens
	m.outputTokens = outputTokens
	m.cacheReadTokens = cacheReadTokens
	m.cacheWriteTokens = cacheWriteTokens
	m.rawInputTokens = rawInputTokens
	m.rawOutputTokens = rawOutputTokens
	m.rawCacheReadTokens = rawCacheReadTokens
	m.rawCacheWriteTokens = rawCacheWriteTokens
	m.contextPct = contextPct
}

func (m *metricsResponseWriter) markFirstToken() {
	if m == nil || m.firstTokenMs > 0 {
		return
	}
	ms := int(time.Since(m.startedAt).Milliseconds())
	if ms <= 0 {
		ms = 1
	}
	m.firstTokenMs = ms
}

func markMetricsFirstToken(w http.ResponseWriter) {
	if mw, ok := w.(*metricsResponseWriter); ok {
		mw.markFirstToken()
	}
}

func markMetricsFirstTokenForDelta(w http.ResponseWriter, d respconv.EventDelta) {
	if d.TextDelta != "" || d.ToolStop {
		markMetricsFirstToken(w)
	}
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
