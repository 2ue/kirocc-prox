package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/niuma/kirocc-pro/internal/anthropic"
)

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"-"`
}

func hasWebSearchServerTool(req *anthropic.Request) bool {
	for _, tool := range req.Tools {
		if tool.IsWebSearchTool() {
			return true
		}
	}
	return false
}

func (s *Service) handleLocalWebSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, model string, contextWindowSize int, ccSessionID, short string) {
	query := extractWebSearchQuery(req)
	if query == "" {
		writeLocalWebSearchError(ctx, w, req.Stream, model, "invalid_input", "empty web search query")
		markMetricsFirstToken(w)
		return
	}
	results, err := duckDuckGoHTMLSearch(ctx, query, 5)
	if err != nil {
		slog.WarnContext(ctx, "local web search failed", "trace_id", short, "err", err)
		writeLocalWebSearchError(ctx, w, req.Stream, model, "unavailable", err.Error())
		markMetricsFirstToken(w)
		return
	}
	writeLocalWebSearchResponse(ctx, w, req.Stream, model, query, results)
	markMetricsFirstToken(w)
	logResponseStats(ctx, short, 0, 0, false, 0, contextWindowSize)
	_ = ccSessionID
}

func extractWebSearchQuery(req *anthropic.Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	text := req.Messages[len(req.Messages)-1].Content.String()
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?is)perform a web search for (?:the )?query:\s*(.+?)\s*$`),
		regexp.MustCompile(`(?is)web search (?:for|query):\s*(.+?)\s*$`),
		regexp.MustCompile(`(?is)搜索\s+(.+?)\s*$`),
	}
	for _, pat := range patterns {
		if m := pat.FindStringSubmatch(text); len(m) > 1 {
			return cleanWebSearchQuery(m[1])
		}
	}
	return cleanWebSearchQuery(text)
}

func cleanWebSearchQuery(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'“”‘’`)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len([]rune(s)) > 400 {
		r := []rune(s)
		s = string(r[:400])
	}
	return s
}

func duckDuckGoHTMLSearch(ctx context.Context, query string, limit int) ([]webSearchResult, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return parseDuckDuckGoResults(string(body), limit), nil
}

var ddgResultRE = regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)

func parseDuckDuckGoResults(body string, limit int) []webSearchResult {
	var out []webSearchResult
	seen := map[string]struct{}{}
	for _, m := range ddgResultRE.FindAllStringSubmatch(body, -1) {
		title := strings.TrimSpace(stripHTML(html.UnescapeString(m[2])))
		link := unwrapDuckDuckGoURL(html.UnescapeString(m[1]))
		if title == "" || link == "" {
			continue
		}
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		out = append(out, webSearchResult{Title: title, URL: link})
		if len(out) >= limit {
			break
		}
	}
	return out
}

var htmlTagRE = regexp.MustCompile(`(?is)<[^>]+>`)

func stripHTML(s string) string {
	return strings.Join(strings.Fields(htmlTagRE.ReplaceAllString(s, " ")), " ")
}

func unwrapDuckDuckGoURL(raw string) string {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(parsed.Host, "duckduckgo.com") {
		if uddg := parsed.Query().Get("uddg"); uddg != "" {
			return uddg
		}
	}
	return raw
}

func writeLocalWebSearchResponse(ctx context.Context, w http.ResponseWriter, stream bool, model, query string, results []webSearchResult) {
	id := "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	content := webSearchContent(results)
	if stream {
		writeLocalWebSearchStream(ctx, w, model, id, query, content, false)
		return
	}
	writeLocalWebSearchJSON(w, model, id, query, content, false)
}

func writeLocalWebSearchError(ctx context.Context, w http.ResponseWriter, stream bool, model, code, message string) {
	id := "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	content := map[string]any{"type": "web_search_tool_result_error", "error_code": code}
	if message != "" {
		content["message"] = message
	}
	if stream {
		writeLocalWebSearchStream(ctx, w, model, id, "", content, true)
		return
	}
	writeLocalWebSearchJSON(w, model, id, "", content, true)
}

func webSearchContent(results []webSearchResult) []any {
	content := make([]any, 0, len(results))
	for _, r := range results {
		content = append(content, map[string]any{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": "local",
		})
	}
	return content
}

func localWebSearchUsage(requests int) map[string]any {
	return map[string]any{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_read_input_tokens":     0,
		"cache_creation_input_tokens": 0,
		"server_tool_use": map[string]any{
			"web_search_requests": requests,
		},
	}
}

func writeLocalWebSearchJSON(w http.ResponseWriter, model, id, query string, content any, isError bool) {
	w.Header().Set("Content-Type", "application/json")
	text := "Search completed."
	if isError {
		text = "Search failed."
	}
	resp := map[string]any{
		"id":            "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24],
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         localWebSearchUsage(1),
		"content": []any{
			map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content},
			map[string]any{"type": "text", "text": text},
		},
	}
	_ = json.MarshalWrite(w, resp)
	_, _ = w.Write([]byte("\n"))
}

func writeLocalWebSearchStream(ctx context.Context, w http.ResponseWriter, model, id, query string, content any, isError bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	write := func(event string, payload map[string]any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24], "type": "message", "role": "assistant", "content": []any{}, "model": model,
			"usage": localWebSearchUsage(0), "stop_reason": nil, "stop_sequence": nil,
		},
	})
	write("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{}}})
	if query != "" {
		input, _ := json.Marshal(map[string]any{"query": query})
		write("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	}
	write("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	write("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content}})
	write("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	text := "Search completed."
	if isError {
		text = "Search failed."
	}
	write("content_block_start", map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "text", "text": ""}})
	write("content_block_delta", map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "text_delta", "text": text}})
	write("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2})
	write("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": localWebSearchUsage(1)})
	write("message_stop", map[string]any{"type": "message_stop"})
	_ = ctx
}
