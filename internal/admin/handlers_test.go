package admin

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// --- Fakes -----------------------------------------------------------------

type fakeScheduler struct {
	mu    sync.Mutex
	creds []*pool.Credential
}

func newFakeScheduler(creds ...*pool.Credential) *fakeScheduler {
	return &fakeScheduler{creds: creds}
}

func (f *fakeScheduler) Register(creds []*pool.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = creds
}
func (f *fakeScheduler) Ready() []*pool.Credential { return f.All() }
func (f *fakeScheduler) Lookup(id string) *pool.Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creds {
		if c.ID == id {
			return c
		}
	}
	return nil
}
func (f *fakeScheduler) All() []*pool.Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pool.Credential, len(f.creds))
	copy(out, f.creds)
	return out
}
func (f *fakeScheduler) MarkSuccess(string, string, pool.Usage)            {}
func (f *fakeScheduler) MarkRateLimit(string, string, time.Duration)       {}
func (f *fakeScheduler) MarkAuthError(string, string)                       {}
func (f *fakeScheduler) RefreshQuota(id string, snap *pool.KiroQuotaSnapshot) {
	if c := f.Lookup(id); c != nil {
		c.Mu.Lock()
		c.LastQuota = snap
		c.LastQuotaAt = time.Now()
		c.Mu.Unlock()
	}
}
func (f *fakeScheduler) RecordQuotaError(string, string) {}
func (f *fakeScheduler) SetEnabled(id string, enabled bool) error {
	c := f.Lookup(id)
	if c == nil {
		return pool.ErrCredentialNotFound
	}
	c.Mu.Lock()
	c.Disabled = !enabled
	if !enabled && c.DisabledAt.IsZero() {
		c.DisabledAt = time.Now()
	}
	if enabled {
		c.DisabledReason = ""
		c.DisabledAt = time.Time{}
	}
	c.Mu.Unlock()
	return nil
}
func (f *fakeScheduler) Add(cred *pool.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creds {
		if c.ID == cred.ID {
			return pool.ErrDuplicateID
		}
	}
	f.creds = append(f.creds, cred)
	return nil
}
func (f *fakeScheduler) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.creds {
		if c.ID == id {
			f.creds = append(f.creds[:i], f.creds[i+1:]...)
			return nil
		}
	}
	return pool.ErrCredentialNotFound
}

// fakeAggregator records call counts so handler tests can assert routing.
// recentRecords is consulted by Recent() to return a canned slice for
// /admin/usage/recent assertions.
type fakeAggregator struct {
	queries       int
	mu            sync.Mutex
	agg           usage.Aggregate
	timeline      []usage.TimelineBucket
	recentRecords []usage.Record
}

func (f *fakeAggregator) Publish(usage.Record) {}
func (f *fakeAggregator) Query(_ context.Context, _ usage.Filter, w usage.Window) (usage.Aggregate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	out := f.agg
	if w.Bucket > 0 {
		out.Timeline = append([]usage.TimelineBucket(nil), f.timeline...)
	}
	return out, nil
}
func (f *fakeAggregator) Recent(_ context.Context, _ usage.Filter, _ int) ([]usage.Record, error) {
	return f.recentRecords, nil
}
func (f *fakeAggregator) Close() error { return nil }

type fakeCache struct {
	snap *pool.KiroQuotaSnapshot
	err  error
}

func (f *fakeCache) Fetch(context.Context, string, string, string, string) (*pool.KiroQuotaSnapshot, error) {
	return f.snap, f.err
}
func (f *fakeCache) FetchForce(context.Context, string, string, string, string) (*pool.KiroQuotaSnapshot, error) {
	return f.snap, f.err
}

// --- Helpers ---------------------------------------------------------------

func newTestServer(t *testing.T, sched pool.Scheduler, agg usage.Aggregator, cache *fakeCache) (*httptest.Server, func()) {
	t.Helper()
	s := NewServer("127.0.0.1", 0, "", "", sched, agg, cache)
	ts := httptest.NewServer(s.Handler())
	return ts, ts.Close
}

func mustCred(id, label string) *pool.Credential {
	return &pool.Credential{ID: id, Label: label, Priority: 100}
}

func mustCredWithQuota(id, label string, total, used float64, plan string) *pool.Credential {
	c := mustCred(id, label)
	c.LastQuota = &pool.KiroQuotaSnapshot{
		FetchedAt:    time.Now(),
		PlanName:     plan,
		CreditsTotal: total,
		CreditsUsed:  used,
	}
	c.LastQuotaAt = time.Now()
	return c
}

func decode[T any](t *testing.T, body io.Reader) T {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %T: %v (raw=%s)", v, err, string(data))
	}
	return v
}

// --- Tests -----------------------------------------------------------------

func TestMaskEmail(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		// Email-shaped labels.
		{"alice@example.com", "a***@example.com"},
		{"b@x.io", "b***@x.io"},
		{"alice@example.com (Pro)", "a***@example.com (Pro)"},
		{"@empty.com", "@empty.com"},
		// Plain identifiers — whole identifier masked.
		{"kiro-alice-001", "k*************"},
		{"alice", "a****"},
		// Identifier + descriptor — only the identifier is masked.
		{"Alice Pro", "A**** Pro"},
		{"alice (Pro)", "a**** (Pro)"},
		{"bob [team]", "b** [team]"},
		// Short / empty labels are returned unchanged.
		{"a", "a"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := maskEmail(tc.in); got != tc.want {
			t.Errorf("maskEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	t.Parallel()
	c1 := mustCredWithQuota("a1", "alice@x.com", 100, 30, "PRO")
	c2 := mustCredWithQuota("a2", "bob@x.com", 50, 50, "PRO")
	c3 := mustCred("a3", "carol@x.com")
	c3.Disabled = true
	sched := newFakeScheduler(c1, c2, c3)

	ts, cleanup := newTestServer(t, sched, &fakeAggregator{}, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/health")
	if err != nil {
		t.Fatalf("GET /admin/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	h := decode[healthResponse](t, resp.Body)
	if h.TotalAccounts != 3 {
		t.Errorf("TotalAccounts = %d, want 3", h.TotalAccounts)
	}
	if h.Active != 2 || h.Disabled != 1 {
		t.Errorf("Active=%d Disabled=%d, want 2/1", h.Active, h.Disabled)
	}
	if got, want := h.TotalCreditsRemaining, 70.0; got != want {
		t.Errorf("TotalCreditsRemaining = %v, want %v", got, want)
	}
	if h.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
}

func TestAccountsList_MasksAndStats(t *testing.T) {
	t.Parallel()
	c := mustCredWithQuota("k-1", "alice@example.com", 100, 25, "PRO")
	sched := newFakeScheduler(c)
	agg := &fakeAggregator{
		agg: usage.Aggregate{
			TotalRequests:     12,
			TotalInputTokens:  3400,
			TotalOutputTokens: 1200,
		},
	}
	ts, cleanup := newTestServer(t, sched, agg, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/accounts")
	if err != nil {
		t.Fatalf("GET /admin/accounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	rows := decode[[]accountRow](t, resp.Body)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Label != "a***@example.com" {
		t.Errorf("label = %q, want masked", r.Label)
	}
	if r.Status != "active" {
		t.Errorf("status = %q, want active", r.Status)
	}
	if r.PlanName != "PRO" {
		t.Errorf("plan = %q, want PRO", r.PlanName)
	}
	if r.Credits.Remaining != 75 {
		t.Errorf("credits.remaining = %v, want 75", r.Credits.Remaining)
	}
	if r.Stats24h.Requests != 12 || r.Stats24h.InputTokens != 3400 || r.Stats24h.OutputTokens != 1200 {
		t.Errorf("stats_24h = %+v", r.Stats24h)
	}

	// Ensure no token fields leak via raw JSON inspection.
	resp2, err := http.Get(ts.URL + "/admin/accounts")
	if err != nil {
		t.Fatalf("re-GET /admin/accounts: %v", err)
	}
	defer resp2.Body.Close()
	raw, _ := io.ReadAll(resp2.Body)
	for _, banned := range []string{"AccessToken", "RefreshToken", "Authorization", "accessToken"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("response contains forbidden field %q: %s", banned, string(raw))
		}
	}
}

func TestAccountGet_FullDetailNoMask(t *testing.T) {
	t.Parallel()
	c := mustCredWithQuota("k-1", "alice@example.com", 100, 25, "PRO")
	c.ProfileARN = "arn:aws:codewhisperer:us-east-1:000:profile/Demo"
	c.Region = "us-east-1"
	c.AuthType = "social"
	sched := newFakeScheduler(c)
	ts, cleanup := newTestServer(t, sched, &fakeAggregator{}, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/accounts/k-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	d := decode[accountDetail](t, resp.Body)
	if d.Label != "alice@example.com" {
		t.Errorf("detail label = %q, want unmasked", d.Label)
	}
	if d.Email != "alice@example.com" {
		t.Errorf("email = %q", d.Email)
	}
	if d.ProfileARN == "" {
		t.Error("ProfileARN missing in detail")
	}
}

func TestAccountGet_NotFound(t *testing.T) {
	t.Parallel()
	sched := newFakeScheduler()
	ts, cleanup := newTestServer(t, sched, &fakeAggregator{}, &fakeCache{})
	defer cleanup()
	resp, err := http.Get(ts.URL + "/admin/accounts/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDisableEnableFlow(t *testing.T) {
	t.Parallel()
	c := mustCred("k-1", "alice@example.com")
	sched := newFakeScheduler(c)
	ts, cleanup := newTestServer(t, sched, &fakeAggregator{}, &fakeCache{})
	defer cleanup()

	// Disable.
	resp, err := http.Post(ts.URL+"/admin/accounts/k-1/disable", "", nil)
	if err != nil {
		t.Fatalf("POST disable: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("disable status = %d", resp.StatusCode)
	}
	if !c.Disabled {
		t.Error("credential not disabled after POST /disable")
	}

	// Verify list reflects status.
	resp, _ = http.Get(ts.URL + "/admin/accounts")
	rows := decode[[]accountRow](t, resp.Body)
	resp.Body.Close()
	if rows[0].Status != "disabled" {
		t.Errorf("status after disable = %q", rows[0].Status)
	}

	// Re-enable.
	resp, err = http.Post(ts.URL+"/admin/accounts/k-1/enable", "", nil)
	if err != nil {
		t.Fatalf("POST enable: %v", err)
	}
	resp.Body.Close()
	if c.Disabled {
		t.Error("credential still disabled after POST /enable")
	}

	resp, _ = http.Get(ts.URL + "/admin/accounts")
	rows = decode[[]accountRow](t, resp.Body)
	resp.Body.Close()
	if rows[0].Status != "active" {
		t.Errorf("status after enable = %q", rows[0].Status)
	}
}

func TestRefreshCallsCacheFetchForce(t *testing.T) {
	t.Parallel()
	c := mustCred("k-1", "alice@example.com")
	c.AccessToken = "tok"
	c.ProfileARN = "arn:demo"
	c.Region = "us-east-1"
	sched := newFakeScheduler(c)
	cache := &fakeCache{snap: &pool.KiroQuotaSnapshot{
		FetchedAt:    time.Now(),
		PlanName:     "PRO",
		CreditsTotal: 200,
		CreditsUsed:  10,
	}}
	ts, cleanup := newTestServer(t, sched, &fakeAggregator{}, cache)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/admin/accounts/k-1/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if c.LastQuota == nil || c.LastQuota.PlanName != "PRO" {
		t.Errorf("LastQuota not written back: %+v", c.LastQuota)
	}
}

func TestUsageEndpoint_GroupByModel(t *testing.T) {
	t.Parallel()
	agg := &fakeAggregator{
		agg: usage.Aggregate{
			TotalRequests:     10,
			TotalInputTokens:  500,
			TotalOutputTokens: 300,
			ByCredModel: map[string]map[string]usage.CellStats{
				"k-1": {"claude-opus-4.7": {Requests: 7, Success: 7, InputTokens: 350, OutputTokens: 200}},
				"k-2": {"claude-opus-4.7": {Requests: 3, Success: 3, InputTokens: 150, OutputTokens: 100}},
			},
		},
	}
	sched := newFakeScheduler()
	ts, cleanup := newTestServer(t, sched, agg, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/usage?window=24h&group=model")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := decode[usageResponse](t, resp.Body)
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (flattened)", len(got.Rows))
	}
	if got.Rows[0].Requests != 10 {
		t.Errorf("flattened requests = %d, want 10", got.Rows[0].Requests)
	}
	if got.Totals.Requests != 10 {
		t.Errorf("totals.requests = %d", got.Totals.Requests)
	}
}

func TestUsageTimeline_DefaultsAndShape(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Minute)
	agg := &fakeAggregator{
		timeline: []usage.TimelineBucket{
			{Start: now.Add(-20 * time.Minute), Requests: 4},
			{Start: now.Add(-10 * time.Minute), Requests: 7},
			{Start: now, Requests: 2},
		},
	}
	ts, cleanup := newTestServer(t, newFakeScheduler(), agg, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/usage/timeline?bucket=10m&window=2h")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	got := decode[timelineResponse](t, resp.Body)
	if len(got.Timeline) != 3 {
		t.Errorf("timeline len = %d, want 3", len(got.Timeline))
	}
	if got.Bucket != "10m0s" && got.Bucket != "10m" {
		t.Errorf("bucket = %q", got.Bucket)
	}
}

func TestHTMLServing_ContentTypeAndBody(t *testing.T) {
	t.Parallel()
	ts, cleanup := newTestServer(t, newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	defer cleanup()

	for _, path := range []string{"/admin", "/admin/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		ct := resp.Header.Get("Content-Type")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s status = %d", path, resp.StatusCode)
		}
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
		if !strings.Contains(string(body), "kirocc-pro") {
			t.Errorf("GET %s body missing title", path)
		}
	}

	// CSS asset.
	resp, _ := http.Get(ts.URL + "/admin/assets/style.css")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("style.css Content-Type = %q", ct)
	}
	if !strings.Contains(string(body), "--accent") {
		t.Errorf("style.css body unexpected (no --accent)")
	}

	// JS asset.
	resp, _ = http.Get(ts.URL + "/admin/assets/app.js")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	resp.Body.Close()

	// Missing asset -> 404.
	resp, _ = http.Get(ts.URL + "/admin/assets/does-not-exist.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Path-traversal rejected even when sent raw (bypassing http client URL cleaning).
	// Build a request manually so the .. survives transmission.
	rawURL := ts.URL + "/admin/assets/"
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.URL.Opaque = "/admin/assets/..%2Fserver.go"
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("traversal status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestStartShutdown_RespectsContext(t *testing.T) {
	t.Parallel()
	s := NewServer("127.0.0.1", 0, "", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	// We cannot Start with port 0 via Start() (it doesn't listen on the inner
	// http.Server's Addr if zero), so just exercise Shutdown directly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown on unstarted server returned %v", err)
	}
}

func TestNewServerDefaults(t *testing.T) {
	t.Parallel()
	s := NewServer("", 0, "", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	if !strings.HasPrefix(s.Addr(), "127.0.0.1:") {
		t.Errorf("default Addr = %q", s.Addr())
	}
	if !strings.HasSuffix(s.Addr(), ":3457") {
		t.Errorf("default port wrong: %q", s.Addr())
	}
}
