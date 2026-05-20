package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/pool"
)

type fakeProvider struct {
	id      string
	prefix  string
	display string
}

func (f *fakeProvider) ID() string          { return f.id }
func (f *fakeProvider) DisplayName() string { return f.display }
func (f *fakeProvider) HandlesModel(m string) bool {
	return strings.HasPrefix(strings.ToLower(m), f.prefix)
}
func (f *fakeProvider) RefreshToken(_ context.Context, _ *pool.Credential) error {
	return nil
}
func (f *fakeProvider) FetchQuota(_ context.Context, _ *pool.Credential) (*pool.KiroQuotaSnapshot, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeProvider) SupportsOAuth() bool { return false }
func (f *fakeProvider) StartOAuth(_ context.Context, _ map[string]string) (*OAuthFlow, error) {
	return nil, ErrOAuthNotSupported
}
func (f *fakeProvider) CompleteOAuth(_ context.Context, _ url.Values, _ *OAuthFlow, _ map[string]string) (*pool.Credential, error) {
	return nil, ErrOAuthNotSupported
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("new registry should be empty, got len=%d", r.Len())
	}
	r.Register(&fakeProvider{id: "kiro", prefix: "claude", display: "Kiro"})
	r.Register(&fakeProvider{id: "codex", prefix: "gpt", display: "Codex"})
	if r.Len() != 2 {
		t.Fatalf("expected 2 providers, got %d", r.Len())
	}
	p, err := r.Get("codex")
	if err != nil || p.ID() != "codex" {
		t.Fatalf("Get(codex) returned %v / err %v", p, err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", err)
	}
}

func TestRegistry_RouteFor(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "kiro", prefix: "claude"})
	r.Register(&fakeProvider{id: "codex", prefix: "gpt"})

	cases := map[string]string{
		"claude-sonnet-4-6": "kiro",
		"claude-opus-4-7":   "kiro",
		"gpt-4o-mini":       "codex",
		"random-model":      "kiro", // first registered is fallback
	}
	for model, want := range cases {
		p := r.RouteFor(model)
		if p == nil || p.ID() != want {
			t.Errorf("RouteFor(%q) = %v, want %s", model, p, want)
		}
	}
}

func TestRegistry_SetFallback(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "kiro", prefix: "claude"})
	r.Register(&fakeProvider{id: "codex", prefix: "gpt"})

	if err := r.SetFallback("codex"); err != nil {
		t.Fatalf("SetFallback: %v", err)
	}
	if p := r.RouteFor("unknown-model"); p.ID() != "codex" {
		t.Errorf("fallback should be codex after SetFallback, got %s", p.ID())
	}
	if err := r.SetFallback("nope"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("expected ErrUnknownProvider for unknown fallback, got %v", err)
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "a"})
	r.Register(&fakeProvider{id: "b"})
	all := r.All()
	if len(all) != 2 || all[0].ID() != "a" || all[1].ID() != "b" {
		t.Fatalf("All() returned wrong order: %v", all)
	}
}

func TestRegistry_RegisterReplace(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "kiro", display: "Old"})
	r.Register(&fakeProvider{id: "kiro", display: "New"})
	if r.Len() != 1 {
		t.Fatalf("re-register should not duplicate, got len=%d", r.Len())
	}
	p, _ := r.Get("kiro")
	if p.DisplayName() != "New" {
		t.Errorf("expected DisplayName to be replaced, got %q", p.DisplayName())
	}
}
