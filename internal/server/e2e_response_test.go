package server

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// errorClient always returns an error from GenerateAssistantResponse.
type errorClient struct {
	err error
}

func (c *errorClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	return nil, c.err
}

type headerMultiResponseClient struct {
	responses    [][]any
	headers      []http.Header
	promptTokens int
	callCount    int
}

func TestE2E_UsageRecordIncludesPathProfileRawAndFirstToken(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 10_000,
	}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{"custom-a": "custom-a"},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {
				Enabled:                true,
				SimulateCache:          true,
				SynthesizeStablePrefix: true,
				TargetReadRatio:        0.90,
				Input:                  promptcache.FieldPolicy{Mode: promptcache.FieldModeSampleMax, MaxTokens: 256, MoveDeltaToCacheRead: true},
				Output:                 promptcache.FieldPolicy{Mode: promptcache.FieldModeRaw},
				CacheRead:              promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
				CacheCreation:          promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
			},
		},
	}
	agg := usage.NewAggregator(usage.NewMemoryStore(10), nil)
	defer func() { _ = agg.Close() }()
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, agg, "", client,
		WithCapture(true),
		WithPromptCacheReports(cfg),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{map[string]any{
			"type": "text",
			"text": strings.Repeat("stable system prompt ", 700),
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	resp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", string(bodyBytes))
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, http.StatusOK)
	_, _ = io.ReadAll(resp.Body)

	records, err := agg.Recent(context.Background(), usage.Filter{}, 10)
	if err != nil {
		t.Fatalf("recent usage: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.RequestPath != "/api/custom-a/v1/messages" {
		t.Fatalf("request_path = %q, want profile path", rec.RequestPath)
	}
	if rec.PromptCacheProfile != "custom-a" || rec.PromptCachePrefix != "/api/custom-a" {
		t.Fatalf("profile/prefix = %q/%q, want custom-a//api/custom-a", rec.PromptCacheProfile, rec.PromptCachePrefix)
	}
	if rec.Status != usage.StatusSuccess {
		t.Fatalf("status = %q, want success", rec.Status)
	}
	if rec.FirstTokenMs <= 0 {
		t.Fatalf("first_token_ms = %d, want > 0", rec.FirstTokenMs)
	}
	if rec.RawInputTokens <= rec.InputTokens {
		t.Fatalf("input/raw_input = %d/%d, want raw greater than sampled input", rec.InputTokens, rec.RawInputTokens)
	}
	if rec.RawOutputTokens != rec.OutputTokens {
		t.Fatalf("output/raw_output = %d/%d, output raw policy must not change output", rec.OutputTokens, rec.RawOutputTokens)
	}
}

func TestE2E_InvalidRequestIsRecordedInUsage(t *testing.T) {
	client := &capturingClient{}
	agg := usage.NewAggregator(usage.NewMemoryStore(10), nil)
	defer func() { _ = agg.Close() }()
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, agg, "", client)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":`)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, http.StatusBadRequest)
	_, _ = io.ReadAll(resp.Body)

	records, err := agg.Recent(context.Background(), usage.Filter{}, 10)
	if err != nil {
		t.Fatalf("recent usage: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.Status != usage.StatusInvalidRequest {
		t.Fatalf("status = %q, want invalid_request", rec.Status)
	}
	if rec.RequestPath != "/v1/messages" {
		t.Fatalf("request_path = %q, want /v1/messages", rec.RequestPath)
	}
	if rec.ErrorMessage == "" {
		t.Fatal("error_message is empty")
	}
	if client.captured != nil {
		t.Fatal("invalid request should not call upstream")
	}
}

func (c *headerMultiResponseClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	idx := c.callCount
	if idx >= len(c.responses) {
		idx = len(c.responses) - 1
	}
	c.callCount++
	body := buildEventStream(c.responses[idx]...)
	header := http.Header{}
	if idx < len(c.headers) {
		header = c.headers[idx].Clone()
	}
	return &kiroclient.Response{StatusCode: 200, Body: body, Header: header, PromptTokens: c.promptTokens}, nil
}

func TestE2E_InvalidStateEvent_PreStream(t *testing.T) {
	p1 := mustJSON(map[string]string{
		"reason":  "CONTENT_LENGTH_EXCEEDS_THRESHOLD",
		"message": "Too long",
	})
	client := &capturingClient{events: []any{"invalidStateEvent", p1}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestE2E_InvalidStateEvent_MidStream(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "partial"})
	p2 := mustJSON(map[string]string{"message": "limit exceeded"})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1, "invalidStateEvent", p2}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"invalid_state"`) {
		t.Fatalf("missing error event in SSE: %s", body)
	}
}

func TestE2E_InvalidStateEvent_MidStream_ErrorEvent(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "partial"})
	p2 := mustJSON(map[string]string{"message": "throttled"})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1, "invalidStateEvent", p2}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	sseBody := string(body)
	if !strings.Contains(sseBody, "event: error") {
		t.Fatalf("missing error event: %s", sseBody)
	}
}

func TestE2E_EmptyMessages(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	reqBody := `{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":""},
			{"role":"assistant","content":""},
			{"role":"user","content":"hi"}
		],
		"stream":false
	}`
	resp := postMessages(t, srv.URL, reqBody)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	if client.captured == nil {
		t.Fatal("payload not captured")
	}
	// Verify history was built (empty content is allowed — v2 captures show
	// tool-result continuations use content="" in real kiro-cli).
	if len(client.captured.ConversationState.History) == 0 {
		t.Fatal("expected non-empty history")
	}
}

func TestE2E_TokenUsage_CacheFields(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	meta := mustJSON(map[string]any{
		"tokenUsage": map[string]any{
			"uncachedInputTokens":   50,
			"outputTokens":          20,
			"totalTokens":           120,
			"cacheReadInputTokens":  40,
			"cacheWriteInputTokens": 10,
		},
	})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1, "metadataEvent", meta}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if usage["cache_read_input_tokens"] == nil {
		t.Fatal("missing cache_read_input_tokens")
	}
	if usage["cache_creation_input_tokens"] == nil {
		t.Fatal("missing cache_creation_input_tokens")
	}
	if int(usage["cache_read_input_tokens"].(float64)) != 40 {
		t.Fatalf("cache_read = %v", usage["cache_read_input_tokens"])
	}
}

func TestE2E_PromptCacheSimulation_CreationThenRead(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 10_000,
	}

	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", client,
		WithCapture(true),
		WithPromptCacheOptions(promptcache.Options{Enabled: true, TargetReadRatio: 0.90}),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{map[string]any{
			"type":          "text",
			"text":          strings.Repeat("cacheable prompt ", 700),
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	body := string(bodyBytes)

	resp := postMessages(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	requireCaptured(t, client)
	assertNoUpstreamCachePoint(t, client.captured)
	var first map[string]any
	_ = json.UnmarshalRead(resp.Body, &first)
	firstUsage := first["usage"].(map[string]any)
	if got := int(firstUsage["cache_creation_input_tokens"].(float64)); got <= 0 {
		t.Fatalf("first cache_creation_input_tokens = %d, want > 0", got)
	}
	if got := int(firstUsage["cache_read_input_tokens"].(float64)); got != 0 {
		t.Fatalf("first cache_read_input_tokens = %d, want 0", got)
	}

	resp2 := postMessages(t, srv.URL, body)
	defer func() { _ = resp2.Body.Close() }()
	requireStatus(t, resp2, 200)
	var second map[string]any
	_ = json.UnmarshalRead(resp2.Body, &second)
	secondUsage := second["usage"].(map[string]any)
	if got := int(secondUsage["cache_read_input_tokens"].(float64)); got <= 0 {
		t.Fatalf("second cache_read_input_tokens = %d, want > 0", got)
	}
	if got := int(secondUsage["cache_creation_input_tokens"].(float64)); got != 0 {
		t.Fatalf("second cache_creation_input_tokens = %d, want 0", got)
	}
	assertNoUpstreamCachePoint(t, client.captured)
}

func TestE2E_PromptCacheSimulation_DisabledByDefault(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 10_000,
	}

	srv := newE2EServer(t, client)
	defer srv.Close()

	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{map[string]any{
			"type":          "text",
			"text":          strings.Repeat("cacheable prompt ", 700),
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	resp := postMessages(t, srv.URL, string(bodyBytes))
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	requireCaptured(t, client)
	assertNoUpstreamCachePoint(t, client.captured)

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 0 {
		t.Fatalf("cache_creation_input_tokens = %d, want 0 when disabled", got)
	}
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 0 {
		t.Fatalf("cache_read_input_tokens = %d, want 0 when disabled", got)
	}
}

func TestE2E_PromptCacheReports_PathProfileStablePrefix(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 10_000,
	}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{
			"custom-a": "custom-a",
		},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {
				Enabled:                true,
				SimulateCache:          true,
				SynthesizeStablePrefix: true,
				TargetReadRatio:        0.90,
				Input:                  promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
				Output:                 promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
				CacheRead:              promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
				CacheCreation:          promptcache.FieldPolicy{Mode: promptcache.FieldModePreserve},
			},
		},
	}
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", client,
		WithCapture(true),
		WithPromptCacheReports(cfg),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{map[string]any{
			"type": "text",
			"text": strings.Repeat("stable system prompt ", 700),
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	body := string(bodyBytes)

	resp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", body)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	requireCaptured(t, client)
	assertNoUpstreamCachePoint(t, client.captured)
	var first map[string]any
	_ = json.UnmarshalRead(resp.Body, &first)
	firstUsage := first["usage"].(map[string]any)
	if got := int(firstUsage["cache_creation_input_tokens"].(float64)); got <= 0 {
		t.Fatalf("first cache_creation_input_tokens = %d, want > 0", got)
	}
	if got := int(firstUsage["cache_read_input_tokens"].(float64)); got != 0 {
		t.Fatalf("first cache_read_input_tokens = %d, want 0", got)
	}

	resp2 := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", body)
	defer func() { _ = resp2.Body.Close() }()
	requireStatus(t, resp2, 200)
	var second map[string]any
	_ = json.UnmarshalRead(resp2.Body, &second)
	secondUsage := second["usage"].(map[string]any)
	if got := int(secondUsage["cache_read_input_tokens"].(float64)); got <= 0 {
		t.Fatalf("second cache_read_input_tokens = %d, want > 0", got)
	}
	if got := int(secondUsage["cache_creation_input_tokens"].(float64)); got != 0 {
		t.Fatalf("second cache_creation_input_tokens = %d, want 0", got)
	}
	assertNoUpstreamCachePoint(t, client.captured)
}

func TestE2E_PromptCacheReports_UnmatchedPathDoesNotSimulate(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 10_000,
	}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{
			"custom-a": "custom-a",
		},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {
				Enabled:                true,
				SimulateCache:          true,
				SynthesizeStablePrefix: true,
				TargetReadRatio:        0.90,
			},
		},
	}
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", client,
		WithCapture(true),
		WithPromptCacheReports(cfg),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{map[string]any{
			"type": "text",
			"text": strings.Repeat("stable system prompt ", 700),
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	resp := postMessages(t, srv.URL, string(bodyBytes))
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 0 {
		t.Fatalf("cache_creation_input_tokens = %d, want 0 for unmatched path", got)
	}
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 0 {
		t.Fatalf("cache_read_input_tokens = %d, want 0 for unmatched path", got)
	}
}

func TestE2E_PromptCacheReports_PreservesRawCacheUsageWhenNoSimulation(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	meta := mustJSON(map[string]any{
		"tokenUsage": map[string]any{
			"uncachedInputTokens":   50,
			"outputTokens":          20,
			"totalTokens":           120,
			"cacheReadInputTokens":  40,
			"cacheWriteInputTokens": 10,
		},
	})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1, "metadataEvent", meta},
		promptTokens: 500,
	}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{
			"custom-a": "custom-a",
		},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {
				Enabled:                true,
				SimulateCache:          true,
				SynthesizeStablePrefix: true,
				TargetReadRatio:        0.90,
			},
		},
	}
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", client,
		WithCapture(true),
		WithPromptCacheReports(cfg),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 40 {
		t.Fatalf("cache_read_input_tokens = %d, want raw 40", got)
	}
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 10 {
		t.Fatalf("cache_creation_input_tokens = %d, want raw 10", got)
	}
}

func TestE2E_PromptCacheReports_DisabledProfilePreservesRawCacheUsage(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "ok"})
	meta := mustJSON(map[string]any{
		"tokenUsage": map[string]any{
			"uncachedInputTokens":   50,
			"outputTokens":          20,
			"totalTokens":           120,
			"cacheReadInputTokens":  40,
			"cacheWriteInputTokens": 10,
		},
	})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1, "metadataEvent", meta},
		promptTokens: 500,
	}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{
			"nocache": "nocache",
		},
		Profiles: map[string]promptcache.ReportProfile{
			"nocache": {Enabled: false},
		},
	}
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", client,
		WithCapture(true),
		WithPromptCacheReports(cfg),
	)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessagesPath(t, srv.URL, "/api/nocache/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)
	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 40 {
		t.Fatalf("cache_read_input_tokens = %d, want raw 40", got)
	}
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 10 {
		t.Fatalf("cache_creation_input_tokens = %d, want raw 10", got)
	}
}

func assertNoUpstreamCachePoint(t *testing.T, payload *kiroproto.Payload) {
	t.Helper()
	if payload == nil {
		t.Fatal("payload is nil")
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.CachePoint != nil {
		t.Fatalf("current cachePoint must stay nil: %+v", current.CachePoint)
	}
	if current.UserInputMessageContext != nil {
		for i, tool := range current.UserInputMessageContext.Tools {
			if tool.CachePoint != nil {
				t.Fatalf("tool[%d] cachePoint must stay nil: %+v", i, tool.CachePoint)
			}
		}
	}
	for i, item := range payload.ConversationState.History {
		if item.UserInputMessage != nil && item.UserInputMessage.CachePoint != nil {
			t.Fatalf("history[%d] user cachePoint must stay nil: %+v", i, item.UserInputMessage.CachePoint)
		}
		if item.AssistantResponseMessage != nil && item.AssistantResponseMessage.CachePoint != nil {
			t.Fatalf("history[%d] assistant cachePoint must stay nil: %+v", i, item.AssistantResponseMessage.CachePoint)
		}
	}
}

func TestE2E_ToolDeduplication(t *testing.T) {
	tool1 := mustJSON(map[string]any{
		"name":      "read_file",
		"toolUseId": "tool_1",
		"input":     map[string]string{"path": "/tmp/a"},
		"stop":      true,
	})
	tool2 := mustJSON(map[string]any{
		"name":      "read_file",
		"toolUseId": "tool_1",
		"input":     map[string]string{"path": "/tmp/a"},
		"stop":      true,
	})
	client := &capturingClient{events: []any{"toolUseEvent", tool1, "toolUseEvent", tool2}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	content := result["content"].([]any)
	toolUseCount := 0
	for _, c := range content {
		block := c.(map[string]any)
		if block["type"] == "tool_use" {
			toolUseCount++
		}
	}
	if toolUseCount != 1 {
		t.Fatalf("tool_use count = %d, want 1 (dedup)", toolUseCount)
	}
}

func TestE2E_ToolInputMixed(t *testing.T) {
	// toolUseEvent with string input chunks (accumulated) then stop
	chunk1 := mustJSON(map[string]any{
		"name":      "write_file",
		"toolUseId": "tool_x",
		"input":     `{"path":`,
	})
	chunk2 := mustJSON(map[string]any{
		"input": `"/tmp/a"}`,
		"stop":  true,
	})
	client := &capturingClient{events: []any{"toolUseEvent", chunk1, "toolUseEvent", chunk2}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	content := result["content"].([]any)
	found := false
	for _, c := range content {
		block := c.(map[string]any)
		if block["type"] == "tool_use" {
			found = true
			if block["name"] != "write_file" {
				t.Fatalf("tool name = %v", block["name"])
			}
		}
	}
	if !found {
		t.Fatal("no tool_use block in response")
	}
}

func TestE2E_Truncation_Content(t *testing.T) {
	// Stream with text but no metadataEvent — response should still succeed.
	p1 := mustJSON(map[string]string{"content": "partial response"})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1}}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	// Response should still contain the partial text
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("empty content")
	}
	block := content[0].(map[string]any)
	if block["text"] != "partial response" {
		t.Fatalf("text = %v", block["text"])
	}
}

func TestE2E_PreCountedTokens_NonStreaming(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "hello"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 500,
	}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if int(usage["input_tokens"].(float64)) != 500 {
		t.Fatalf("input_tokens = %v, want 500", usage["input_tokens"])
	}
}

func TestE2E_PreCountedTokens_Streaming(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "hello"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 750,
	}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	body, _ := io.ReadAll(resp.Body)
	sseBody := string(body)
	if !strings.Contains(sseBody, `"input_tokens":750`) {
		t.Fatalf("expected input_tokens:750 in SSE stream, got: %s", sseBody)
	}
}

func TestE2E_PreCountedTokens_ZeroFallback(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "hello"})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1},
		promptTokens: 0, // simulates tokencount failure
	}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if int(usage["input_tokens"].(float64)) != 0 {
		t.Fatalf("input_tokens = %v, want 0 (fallback)", usage["input_tokens"])
	}
}

func TestE2E_MetadataOverridesPreCounted(t *testing.T) {
	p1 := mustJSON(map[string]string{"content": "hello"})
	meta := mustJSON(map[string]any{
		"tokenUsage": map[string]any{
			"uncachedInputTokens": 100,
			"outputTokens":        50,
			"totalTokens":         150,
		},
	})
	client := &capturingClient{
		events:       []any{"assistantResponseEvent", p1, "metadataEvent", meta},
		promptTokens: 999, // should be overridden by metadata
	}

	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	usage := result["usage"].(map[string]any)
	if int(usage["input_tokens"].(float64)) != 100 {
		t.Fatalf("input_tokens = %v, want 100 (metadata should override pre-counted)", usage["input_tokens"])
	}
}

func TestE2E_ClientError_Returns502(t *testing.T) {
	// When the kiro client returns an error, server should return 502
	errClient := &errorClient{err: io.EOF}

	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, nil, "", errClient)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestE2E_EmptyVisibleEndTurn_NonStreaming_RetrySucceeds(t *testing.T) {
	// First call: thinking-only via tags (empty visible end_turn).
	// Second call (retry): normal text response.
	thinkingOnly := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "<thinking>Let me think</thinking>"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		}),
	}
	normalResponse := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "Here is the answer"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 10, "totalTokens": 20},
		}),
	}
	client := &multiResponseClient{responses: [][]any{thinkingOnly, normalResponse}}
	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	content := result["content"].([]any)
	// Should contain the retry's text, not the thinking-only response.
	found := false
	for _, c := range content {
		block := c.(map[string]any)
		if block["type"] == "text" && block["text"] == "Here is the answer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected retry text in response, got content: %v", content)
	}
	// Should have been called twice.
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_EmptyVisibleEndTurn_NonStreaming_RetryAlsoFails(t *testing.T) {
	// Both calls return thinking-only → should return 502.
	thinkingOnly := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "<thinking>Let me think</thinking>"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		}),
	}
	client := &multiResponseClient{responses: [][]any{thinkingOnly, thinkingOnly}}
	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 502)
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_EmptyVisibleEndTurn_RetryClearsIDs(t *testing.T) {
	// Verify that retry clears ConversationID.
	thinkingOnly := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "<thinking>Let me think</thinking>"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		}),
	}
	normalResponse := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "Here is the answer"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 10, "totalTokens": 20},
		}),
	}
	client := &multiResponseClient{responses: [][]any{thinkingOnly, normalResponse}}
	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
	// Both payloads point to the same object (mutated in-place before retry),
	// so we verify the final state has cleared IDs.
	if client.payloads[1].ConversationState.ConversationID != "" {
		t.Fatalf("attempt-2 ConversationID should be empty, got %q", client.payloads[1].ConversationState.ConversationID)
	}
}

func TestE2E_EmptyVisibleEndTurn_Streaming_RetrySucceeds(t *testing.T) {
	// First call: thinking-only. Second call: normal text.
	thinkingOnly := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "<thinking>Let me think</thinking>"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		}),
	}
	normalResponse := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "Streamed answer"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 10, "totalTokens": 20},
		}),
	}
	client := &multiResponseClient{responses: [][]any{thinkingOnly, normalResponse}}
	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)

	body, _ := io.ReadAll(resp.Body)
	sseBody := string(body)
	// The retry response should contain the text.
	if !strings.Contains(sseBody, "Streamed answer") {
		t.Fatalf("expected retry text in SSE stream, got: %s", sseBody)
	}
	// The thinking-only first response should have been discarded (no thinking_delta in output).
	if strings.Contains(sseBody, "thinking_delta") {
		t.Fatalf("thinking from first attempt should have been discarded, got: %s", sseBody)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_EmptyVisibleEndTurn_Streaming_SavesFailureCapture(t *testing.T) {
	logBuf := setupCaptureTest(t)

	thinkingOnly := []any{
		"assistantResponseEvent", mustJSON(map[string]string{"content": "<thinking>Let me think</thinking>"}),
		"metadataEvent", mustJSON(map[string]any{
			"tokenUsage": map[string]any{"uncachedInputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		}),
	}
	client := &headerMultiResponseClient{
		responses: [][]any{thinkingOnly, thinkingOnly},
		headers: []http.Header{
			{"X-Amzn-RequestId": []string{"req-1"}},
			{"X-Amzn-RequestId": []string{"req-2"}},
		},
	}
	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6[1m]","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 502)

	// Parse log output and find capture records.
	logOutput := logBuf.String()
	var captureRecords []map[string]any
	for line := range strings.SplitSeq(logOutput, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["body"] == "upstream failure capture" {
			captureRecords = append(captureRecords, rec)
		}
	}
	if len(captureRecords) != 2 {
		t.Fatalf("capture record count = %d, want 2\nlog output:\n%s", len(captureRecords), logOutput)
	}

	attrs, ok := captureRecords[0]["attributes"].(map[string]any)
	if !ok {
		t.Fatal("capture record missing attributes")
	}
	if attrs["reason"] != "empty_visible_end_turn" {
		t.Errorf("reason = %v, want empty_visible_end_turn", attrs["reason"])
	}
	// response_headers should contain req-1 for attempt 1.
	headers, ok := attrs["response_headers"].(map[string]any)
	if !ok {
		t.Fatalf("response_headers should be a map, got %T", attrs["response_headers"])
	}
	headerJSON, _ := json.Marshal(headers)
	if !strings.Contains(string(headerJSON), "req-1") {
		t.Errorf("response_headers should contain req-1, got: %s", headerJSON)
	}
	// events should contain assistantResponseEvent.
	events, ok := attrs["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("events should be a non-empty array, got %T: %v", attrs["events"], attrs["events"])
	}
	eventsJSON, _ := json.Marshal(events)
	if !strings.Contains(string(eventsJSON), "assistantResponseEvent") {
		t.Errorf("events should contain assistantResponseEvent, got: %s", eventsJSON)
	}
	// request_body should be present and non-empty.
	if attrs["request_body"] == nil {
		t.Error("request_body should be present")
	}
	// Verify attempt 2 capture record has req-2 in response_headers.
	attrs2, ok := captureRecords[1]["attributes"].(map[string]any)
	if !ok {
		t.Fatal("capture record 2 missing attributes")
	}
	if attrs2["attempt"] != float64(2) {
		t.Errorf("attempt 2: attempt = %v, want 2", attrs2["attempt"])
	}
	headers2, ok := attrs2["response_headers"].(map[string]any)
	if !ok {
		t.Fatalf("attempt 2: response_headers should be a map, got %T", attrs2["response_headers"])
	}
	headerJSON2, _ := json.Marshal(headers2)
	if !strings.Contains(string(headerJSON2), "req-2") {
		t.Errorf("attempt 2: response_headers should contain req-2, got: %s", headerJSON2)
	}
}

func TestE2E_Success_DoesNotSaveFailureCapture(t *testing.T) {
	logBuf := setupCaptureTest(t)

	p1 := mustJSON(map[string]string{"content": "ok"})
	client := &capturingClient{events: []any{"assistantResponseEvent", p1}}
	srv := newE2EServer(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	_, _ = io.ReadAll(resp.Body)

	if strings.Contains(logBuf.String(), "upstream failure capture") {
		t.Fatal("capture log should not appear on success")
	}
}

func TestE2E_InvalidToolUseRetriesWithToolResult(t *testing.T) {
	badTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "recovered"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badTool},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "recovered") {
		t.Fatalf("final response missing recovered text: %s", body)
	}
	if strings.Contains(string(body), "invalid tool call") {
		t.Fatalf("warning should not be surfaced after successful retry: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
	if len(client.payloads) < 2 {
		t.Fatalf("payloads len = %d, want >=2", len(client.payloads))
	}
	second := client.payloads[1].ConversationState
	if second.ConversationID != "" {
		t.Fatalf("second payload conversation ID = %q, want empty", second.ConversationID)
	}
	ctx := second.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 {
		t.Fatalf("second payload missing error tool result: %#v", ctx)
	}
	if ctx.ToolResults[0].ToolUseID != "toolu_bad" || ctx.ToolResults[0].Status != "error" {
		t.Fatalf("unexpected tool result: %#v", ctx.ToolResults[0])
	}
	foundToolUseHistory := false
	for _, h := range second.History {
		if h.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range h.AssistantResponseMessage.ToolUses {
			if tu.ToolUseID == "toolu_bad" && tu.Name == "Write" {
				foundToolUseHistory = true
			}
		}
	}
	if !foundToolUseHistory {
		t.Fatal("second payload history missing invalid assistant tool_use")
	}
}

func TestE2E_InvalidToolUseRetriesWithToolResultStreaming(t *testing.T) {
	badTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad_stream",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "stream recovered"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badTool},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":true
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "stream recovered") {
		t.Fatalf("final stream missing recovered text: %s", body)
	}
	if strings.Contains(s, "invalid tool call") || strings.Contains(s, `"type":"tool_use"`) {
		t.Fatalf("invalid tool warning/tool_use should not be surfaced after retry: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
	if len(client.payloads) < 2 {
		t.Fatalf("payloads len = %d, want >=2", len(client.payloads))
	}
	if got := client.payloads[1].ConversationState.ConversationID; got != "" {
		t.Fatalf("second payload conversation ID = %q, want empty", got)
	}
}

func TestE2E_InvalidToolUseRetrySecondFailureReturnsToolUse(t *testing.T) {
	firstBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad_first",
		"input":     map[string]any{},
		"stop":      true,
	})
	secondBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad_second",
		"input":     map[string]any{},
		"stop":      true,
	})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", firstBadTool},
		{"toolUseEvent", secondBadTool},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	var result map[string]any
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", result["stop_reason"])
	}
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len = %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "toolu_bad_second" || block["name"] != "Write" {
		t.Fatalf("unexpected fallback block: %#v", block)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_InvalidToolUseRetrySecondFailureReturnsToolUseStreaming(t *testing.T) {
	firstBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad_stream_first",
		"input":     map[string]any{},
		"stop":      true,
	})
	secondBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_bad_stream_second",
		"input":     map[string]any{},
		"stop":      true,
	})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", firstBadTool},
		{"toolUseEvent", secondBadTool},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":true
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"type":"tool_use"`) || !strings.Contains(s, "toolu_bad_stream_second") {
		t.Fatalf("stream fallback missing tool_use: %s", body)
	}
	if strings.Contains(s, "invalid tool call") {
		t.Fatalf("warning should not be surfaced: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_ToolSearchInvalidToolUseRetriesWithToolResult(t *testing.T) {
	badTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_toolsearch_bad",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "toolsearch recovered"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badTool},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}
		],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "toolsearch recovered") {
		t.Fatalf("final response missing recovered text: %s", body)
	}
	if strings.Contains(string(body), "invalid tool call") {
		t.Fatalf("warning should not be surfaced after successful retry: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
	if got := client.payloads[1].ConversationState.ConversationID; got != "" {
		t.Fatalf("second payload conversation ID = %q, want empty", got)
	}
	ctx := client.payloads[1].ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 || ctx.ToolResults[0].Status != "error" {
		t.Fatalf("second payload missing error tool result: %#v", ctx)
	}
}

func TestE2E_ToolSearchInvalidToolUseRetrySecondFailureReturnsToolUse(t *testing.T) {
	firstBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_toolsearch_bad_first",
		"input":     map[string]any{},
		"stop":      true,
	})
	secondBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_toolsearch_bad_second",
		"input":     map[string]any{},
		"stop":      true,
	})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", firstBadTool},
		{"toolUseEvent", secondBadTool},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}
		],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	var result map[string]any
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "toolu_toolsearch_bad_second" {
		t.Fatalf("unexpected fallback block: %#v", block)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_ToolSearchInvalidInputReturnsToolResultError(t *testing.T) {
	badSearch := mustJSON(map[string]any{
		"name":      "ToolSearch",
		"toolUseId": "toolu_search_bad",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "after parse error"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badSearch},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"find a tool"}],
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"Write","description":"write file","defer_loading":true,"input_schema":{"type":"object"}}
		],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "tool_search_tool_result") || !strings.Contains(s, "invalid_input") {
		t.Fatalf("missing tool_search error result: %s", body)
	}
	if !strings.Contains(s, "after parse error") {
		t.Fatalf("final response missing after parse error: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_ToolSearchInvalidInputReturnsToolResultErrorStreaming(t *testing.T) {
	badSearch := mustJSON(map[string]any{
		"name":      "ToolSearch",
		"toolUseId": "toolu_search_bad_stream",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "after parse error stream"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badSearch},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"find a tool"}],
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"Write","description":"write file","defer_loading":true,"input_schema":{"type":"object"}}
		],
		"stream":true
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "tool_search_tool_result") || !strings.Contains(s, "invalid_input") {
		t.Fatalf("missing tool_search error result: %s", body)
	}
	if !strings.Contains(s, "after parse error stream") {
		t.Fatalf("final stream missing after parse error: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}

func TestE2E_ThinkingToolInvalidToolUseRetriesWithToolResult(t *testing.T) {
	t.Setenv("KIROCC_EXPERIMENT_THINKING_PROMPT", "tool")
	badTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_thinkingtool_bad",
		"input":     map[string]any{},
		"stop":      true,
	})
	finalText := mustJSON(map[string]string{"content": "thinkingtool recovered"})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", badTool},
		{"assistantResponseEvent", finalText},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"thinking":{"type":"enabled","budget_tokens":4000},
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "thinkingtool recovered") {
		t.Fatalf("final response missing recovered text: %s", body)
	}
	if strings.Contains(string(body), "invalid tool call") {
		t.Fatalf("warning should not be surfaced after successful retry: %s", body)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
	if got := client.payloads[1].ConversationState.ConversationID; got != "" {
		t.Fatalf("second payload conversation ID = %q, want empty", got)
	}
	ctx := client.payloads[1].ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 || ctx.ToolResults[0].Status != "error" {
		t.Fatalf("second payload missing error tool result: %#v", ctx)
	}
}

func TestE2E_ThinkingToolInvalidToolUseRetrySecondFailureReturnsToolUse(t *testing.T) {
	t.Setenv("KIROCC_EXPERIMENT_THINKING_PROMPT", "tool")
	firstBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_thinkingtool_bad_first",
		"input":     map[string]any{},
		"stop":      true,
	})
	secondBadTool := mustJSON(map[string]any{
		"name":      "Write",
		"toolUseId": "toolu_thinkingtool_bad_second",
		"input":     map[string]any{},
		"stop":      true,
	})
	client := &multiResponseClient{responses: [][]any{
		{"toolUseEvent", firstBadTool},
		{"toolUseEvent", secondBadTool},
	}}

	srv := newE2EServerWithClient(t, client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"write a file"}],
		"thinking":{"type":"enabled","budget_tokens":4000},
		"tools":[{"name":"Write","description":"write file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}],
		"stream":false
	}`)
	defer func() { _ = resp.Body.Close() }()

	requireStatus(t, resp, 200)
	var result map[string]any
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "toolu_thinkingtool_bad_second" {
		t.Fatalf("unexpected fallback block: %#v", block)
	}
	if client.callCount != 2 {
		t.Fatalf("callCount = %d, want 2", client.callCount)
	}
}
