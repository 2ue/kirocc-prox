// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Second-attempt fallback when the model keeps emitting invalid tool_use even
// after the retry round — writes a visible error response to the client
// instead of looping or hanging.

package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/respconv"
	"github.com/google/uuid"
)

func writeInvalidToolUseFallback(ctx context.Context, w http.ResponseWriter, model string, stream bool, calls []respconv.InvalidToolCall) {
	if len(calls) == 0 {
		return
	}
	slog.WarnContext(ctx, "forwarding invalid tool_use to client after retry failed",
		"invalid_tool_calls", len(calls),
	)
	if stream {
		writeInvalidToolUseFallbackSSE(w, model, calls)
		return
	}
	writeInvalidToolUseFallbackJSON(ctx, w, model, calls)
}

func invalidToolUseBlocks(calls []respconv.InvalidToolCall) []any {
	blocks := make([]any, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("toolu_invalid_%d", i+1)
		}
		name := call.Name
		if name == "" {
			name = "invalid_tool"
		}
		blocks = append(blocks, map[string]any{
			"type":  anthropic.BlockTypeToolUse,
			"id":    id,
			"name":  name,
			"input": parseInvalidToolInput(call.Input),
		})
	}
	return blocks
}

func writeInvalidToolUseFallbackJSON(ctx context.Context, w http.ResponseWriter, model string, calls []respconv.InvalidToolCall) {
	resp := map[string]any{
		"id":            "msg_" + uuid.New().String()[:24],
		"type":          "message",
		"role":          "assistant",
		"content":       invalidToolUseBlocks(calls),
		"model":         model,
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage":         zeroUsage(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, resp); err != nil {
		slog.ErrorContext(ctx, "write invalid tool fallback response failed", "err", err)
		return
	}
	_, _ = w.Write([]byte("\n"))
}

func writeInvalidToolUseFallbackSSE(w http.ResponseWriter, model string, calls []respconv.InvalidToolCall) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_" + uuid.New().String()[:24],
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         zeroUsage(),
		},
	})
	for i, block := range invalidToolUseBlocks(calls) {
		b := block.(map[string]any)
		inputBytes, _ := json.Marshal(b["input"])
		writeSSEEvent(w, flusher, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": i,
			"content_block": map[string]any{
				"type":  anthropic.BlockTypeToolUse,
				"id":    b["id"],
				"name":  b["name"],
				"input": map[string]any{},
			},
		})
		writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": i,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(inputBytes),
			},
		})
		writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": i,
		})
	}
	writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "tool_use",
			"stop_sequence": nil,
		},
		"usage": zeroUsage(),
	})
	writeSSEEvent(w, flusher, "message_stop", map[string]any{
		"type": "message_stop",
	})
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher != nil {
		flusher.Flush()
	}
}

func zeroUsage() map[string]any {
	return map[string]any{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_read_input_tokens":     0,
		"cache_creation_input_tokens": 0,
	}
}
