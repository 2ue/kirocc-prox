package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/models"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	tu "github.com/niuma/kirocc-pro/internal/testutil"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// stubConductor returns a fixed Credential, or an error when acquireErr is
// set. Sufficient for the server tests that just need an upstream call to
// be made; pool routing logic is tested in internal/pool/.
type stubConductor struct {
	cred       *pool.Credential
	acquireErr error
}

func (s *stubConductor) Acquire(_ context.Context, _, _ string) (*pool.Credential, error) {
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	return s.cred, nil
}
func (s *stubConductor) Release(_ *pool.Credential, _ ...string) {}

// newStubCred returns the default test credential used across server tests.
func newStubCred() *pool.Credential {
	return &pool.Credential{
		ID: "default",
		Credentials: auth.Credentials{
			AccessToken: "test-token",
			ProfileARN:  "arn:test",
			Region:      "us-east-1",
		},
	}
}

// stubScheduler is a no-op implementation. The server tests don't observe
// scheduler state.
type stubScheduler struct{}

func (stubScheduler) Register(_ []*pool.Credential)                    {}
func (stubScheduler) Ready() []*pool.Credential                        { return nil }
func (stubScheduler) Lookup(_ string) *pool.Credential                 { return nil }
func (stubScheduler) All() []*pool.Credential                          { return nil }
func (stubScheduler) MarkSuccess(_, _ string, _ pool.Usage)            {}
func (stubScheduler) MarkRateLimit(_, _ string, _ time.Duration)       {}
func (stubScheduler) MarkAuthError(_, _ string)                        {}
func (stubScheduler) RefreshQuota(_ string, _ *pool.KiroQuotaSnapshot) {}
func (stubScheduler) RecordQuotaError(_, _ string)                     {}
func (stubScheduler) SetEnabled(_ string, _ bool) error                { return nil }
func (stubScheduler) Add(_ *pool.Credential) error                     { return nil }
func (stubScheduler) Remove(_ string) error                            { return nil }

// mockKiroClient implements kiroclient.Client for tests.
type mockKiroClient struct {
	handler func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error)
}

func (m *mockKiroClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	return m.handler(ctx, token, payload, region)
}

// buildEventStream builds a binary event stream body from event type/payload pairs.
func buildEventStream(events ...any) io.ReadCloser {
	var buf bytes.Buffer
	for i := 0; i < len(events); i += 2 {
		eventType := events[i].(string)
		payload := events[i+1].([]byte)
		buf.Write(tu.BuildFrame(eventType, payload))
	}
	return io.NopCloser(&buf)
}

func newTestServer(t *testing.T, apiKey string, client kiroclient.Client) *httptest.Server {
	t.Helper()
	return newTestServerWithOptions(t, apiKey, client)
}

func newTestServerWithOptions(t *testing.T, apiKey string, client kiroclient.Client, opts ...ServerOption) *httptest.Server {
	t.Helper()
	conductor := &stubConductor{cred: newStubCred()}
	if client == nil {
		client = &mockKiroClient{
			handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
				p, _ := json.Marshal(map[string]string{"content": "ok"})
				body := buildEventStream("assistantResponseEvent", p)
				return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
			},
		}
	}
	s := New(conductor, stubScheduler{}, nil, apiKey, client, opts...)
	return newTCP4TestServer(t, s.Handler())
}

func TestAuthRejectedMessagesAreRecordedInUsage(t *testing.T) {
	agg := usage.NewAggregator(usage.NewMemoryStore(10), nil)
	defer func() { _ = agg.Close() }()
	s := New(&stubConductor{cred: newStubCred()}, stubScheduler{}, agg, "secret", nil)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, http.StatusUnauthorized)
	_, _ = io.ReadAll(resp.Body)

	records, err := agg.Recent(context.Background(), usage.Filter{}, 10)
	if err != nil {
		t.Fatalf("recent usage: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].Status != usage.StatusAuthError || records[0].RequestPath != "/v1/messages" {
		t.Fatalf("record = %+v, want auth_error on /v1/messages", records[0])
	}

	resp2, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	requireStatus(t, resp2, http.StatusUnauthorized)
	_, _ = io.ReadAll(resp2.Body)
	records, _ = agg.Recent(context.Background(), usage.Filter{}, 10)
	if len(records) != 1 {
		t.Fatalf("records after /v1/models auth failure = %d, want still 1", len(records))
	}
}

func TestDynamicAPIKeyValidatorDisabledAllowsUnauthenticatedModels(t *testing.T) {
	called := false
	srv := newTestServerWithOptions(t, "", nil,
		WithAPIKeyValidator(func(string) (string, error) {
			called = true
			return "", errors.New("validator should be disabled")
		}),
		WithAPIKeyValidatorEnabled(func() bool { return false }),
	)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, http.StatusOK)
	if called {
		t.Fatal("dynamic validator was called while disabled")
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestGetModels(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	data := result["data"].([]any)
	if len(data) == 0 {
		t.Fatal("empty data")
	}
}

func TestGetModels_ContainsDefaultModel(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	data := result["data"].([]any)
	found := false
	for _, item := range data {
		m := item.(map[string]any)
		if m["id"] == models.DefaultModel {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("default model %q not found", models.DefaultModel)
	}
}

func TestCountTokens(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(
		srv.URL+"/v1/messages/count_tokens",
		"application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	tokens, ok := result["input_tokens"].(float64)
	if !ok || tokens <= 0 {
		t.Fatalf("input_tokens = %v, want > 0", result["input_tokens"])
	}
}

func TestCountTokens_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages/count_tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 405 {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestCountTokens_InvalidJSON(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader("bad"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCountTokens_EmptyMessages(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIKeyAuth_Missing(t *testing.T) {
	srv := newTestServer(t, "secret", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAPIKeyAuth_Valid(t *testing.T) {
	srv := newTestServer(t, "secret", nil)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestAPIKeyAuth_Invalid(t *testing.T) {
	srv := newTestServer(t, "secret", nil)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAPIKeyAuth_SkippedForHealth(t *testing.T) {
	srv := newTestServer(t, "secret", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCORSHeaders(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	req, _ := http.NewRequest("OPTIONS", srv.URL+"/v1/messages", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:3000")
	}
}

func TestPostMessages_InvalidJSON(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("bad"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostMessages_ProfilePrefixRoute(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{"content": "ok"})
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("assistantResponseEvent", p1)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}, PromptTokens: 10}, nil
	}}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{"custom-a": "custom-a"},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {Enabled: true},
		},
	}
	srv := newTestServerWithOptions(t, "", client, WithPromptCacheReports(cfg))
	defer srv.Close()

	resp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	legacyResp := postMessagesPath(t, srv.URL, "/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = legacyResp.Body.Close() }()
	if legacyResp.StatusCode != 404 {
		body, _ := io.ReadAll(legacyResp.Body)
		t.Fatalf("legacy status = %d, want 404, body = %s", legacyResp.StatusCode, body)
	}

	modelsResp, err := http.Get(srv.URL + "/api/custom-a/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = modelsResp.Body.Close() }()
	if modelsResp.StatusCode != 200 {
		body, _ := io.ReadAll(modelsResp.Body)
		t.Fatalf("models status = %d, body = %s", modelsResp.StatusCode, body)
	}

	modelsPostResp, err := http.Post(srv.URL+"/api/custom-a/v1/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = modelsPostResp.Body.Close() }()
	if modelsPostResp.StatusCode != 405 {
		body, _ := io.ReadAll(modelsPostResp.Body)
		t.Fatalf("models POST status = %d, want 405, body = %s", modelsPostResp.StatusCode, body)
	}

	countResp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages/count_tokens/", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = countResp.Body.Close() }()
	if countResp.StatusCode != 200 {
		body, _ := io.ReadAll(countResp.Body)
		t.Fatalf("count_tokens status = %d, body = %s", countResp.StatusCode, body)
	}

	trailingResp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages/", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = trailingResp.Body.Close() }()
	if trailingResp.StatusCode != 200 {
		body, _ := io.ReadAll(trailingResp.Body)
		t.Fatalf("trailing status = %d, body = %s", trailingResp.StatusCode, body)
	}
}

func TestPostMessages_UnconfiguredProfilePrefixRouteNotFound(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp := postMessagesPath(t, srv.URL, "/api/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestPromptCacheReportProviderControlsProfileRoutes(t *testing.T) {
	staticCfg := promptcache.ReportConfig{
		Routes: map[string]string{"custom-a": "custom-a"},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {Enabled: true},
		},
	}

	var (
		mu  sync.RWMutex
		cfg promptcache.ReportConfig
	)
	setConfig := func(next promptcache.ReportConfig) {
		mu.Lock()
		defer mu.Unlock()
		cfg = next
	}
	provider := func() promptcache.ReportConfig {
		mu.RLock()
		defer mu.RUnlock()
		return cfg
	}

	srv := newTestServerWithOptions(t, "", nil,
		WithPromptCacheReports(staticCfg),
		WithPromptCacheReportProvider(provider),
	)
	defer srv.Close()

	assertModelsStatus := func(want int) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/custom-a/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != want {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("models status = %d, want %d, body = %s", resp.StatusCode, want, body)
		}
	}

	assertModelsStatus(http.StatusNotFound)
	setConfig(staticCfg)
	assertModelsStatus(http.StatusOK)
	setConfig(promptcache.ReportConfig{})
	assertModelsStatus(http.StatusNotFound)
}

func TestPostMessages_CustomDashboardNamedRouteUsesAPIPrefix(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{"content": "ok"})
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("assistantResponseEvent", p1)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}, PromptTokens: 10}, nil
	}}
	cfg := promptcache.ReportConfig{
		Routes: map[string]string{"dashboard/custom-a": "custom-a"},
		Profiles: map[string]promptcache.ReportProfile{
			"custom-a": {Enabled: true},
		},
	}
	srv := newTestServerWithOptions(t, "", client, WithPromptCacheReports(cfg))
	defer srv.Close()

	resp := postMessagesPath(t, srv.URL, "/api/dashboard/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	frontendResp := postMessagesPath(t, srv.URL, "/dashboard/custom-a/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = frontendResp.Body.Close() }()
	if frontendResp.StatusCode != 404 {
		body, _ := io.ReadAll(frontendResp.Body)
		t.Fatalf("frontend status = %d, want 404, body = %s", frontendResp.StatusCode, body)
	}
}

func TestPostMessages_UnknownProfilePathNotFound(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/custom-a/not-messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPostMessages_EmptyMessages(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostMessages_AuthError(t *testing.T) {
	conductor := &stubConductor{acquireErr: io.EOF}
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		return nil, io.EOF
	}}
	s := New(conductor, stubScheduler{}, nil, "", client)
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 200 {
		t.Fatal("expected non-200 when auth fails")
	}
}

func TestPostMessages_NonStreaming(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{"content": "Hello!"})
	p2, _ := json.Marshal(map[string]any{"usage": map[string]any{"inputTokens": 10, "outputTokens": 5}})

	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("assistantResponseEvent", p1, "meteringEvent", p2)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)
	if result["type"] != "message" {
		t.Fatalf("type = %v", result["type"])
	}
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("empty content")
	}
	block := content[0].(map[string]any)
	if block["text"] != "Hello!" {
		t.Fatalf("text = %v", block["text"])
	}
}

func TestPostMessages_Streaming(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{"content": "Hello"})
	p2, _ := json.Marshal(map[string]string{"content": "Hello world"})

	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("assistantResponseEvent", p1, "assistantResponseEvent", p2)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	sseBody := string(body)
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(sseBody, want) {
			t.Errorf("missing %q in SSE body", want)
		}
	}
}

func TestPostMessages_InvalidState_PreStream(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{
		"reason":  "CONTENT_LENGTH_EXCEEDS_THRESHOLD",
		"message": "Too long",
	})

	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("invalidStateEvent", p1)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestPostMessages_InvalidState_MidStream(t *testing.T) {
	p1, _ := json.Marshal(map[string]string{"content": "partial"})
	p2, _ := json.Marshal(map[string]string{"message": "limit exceeded"})

	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		body := buildEventStream("assistantResponseEvent", p1, "invalidStateEvent", p2)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"invalid_state"`) {
		t.Fatalf("missing error event in SSE: %s", body)
	}
}

func TestPostMessages_PayloadPassthrough(t *testing.T) {
	var capturedPayload *kiroproto.Payload
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		capturedPayload = payload
		p, _ := json.Marshal(map[string]string{"content": "ok"})
		body := buildEventStream("assistantResponseEvent", p)
		return &kiroclient.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	if capturedPayload == nil {
		t.Fatal("payload not captured")
	}
	if capturedPayload.ConversationState.AgentTaskType != "vibe" {
		t.Fatalf("agentTaskType = %q", capturedPayload.ConversationState.AgentTaskType)
	}
	if capturedPayload.ConversationState.ChatTriggerType != "MANUAL" {
		t.Fatalf("chatTriggerType = %q", capturedPayload.ConversationState.ChatTriggerType)
	}
}

func TestPostMessages_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 405 {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestIsLocalhostOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"http://localhost:3000", true},
		{"https://localhost:8080", true},
		{"http://127.0.0.1:3000", true},
		{"https://127.0.0.1:443", true},
		{"http://[::1]:3000", true},
		{"", false},
		{"http://example.com", false},
		// Malformed origins that prefix matching would incorrectly accept:
		{"http://localhost:evil.com", false},
		{"http://localhost:3000/path@evil.com", false},
		// No port:
		{"http://localhost", true},
		{"http://127.0.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			got := isLocalhostOrigin(tt.origin)
			if got != tt.want {
				t.Errorf("isLocalhostOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
