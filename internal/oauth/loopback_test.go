package oauth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLoopback_BindEphemeral(t *testing.T) {
	lb, err := NewLoopback(context.Background(), []int{0}, "/cb")
	if err != nil {
		t.Fatalf("NewLoopback: %v", err)
	}
	defer lb.Close()
	if lb.Port() == 0 {
		t.Fatalf("expected non-zero bound port")
	}
	if !strings.HasPrefix(lb.RedirectURI(), "http://localhost:") {
		t.Errorf("unexpected redirect URI: %q", lb.RedirectURI())
	}
	if !strings.HasSuffix(lb.RedirectURI(), "/cb") {
		t.Errorf("expected /cb suffix in redirect URI: %q", lb.RedirectURI())
	}
}

func TestLoopback_CaptureSingleRequest(t *testing.T) {
	lb, err := NewLoopback(context.Background(), []int{0}, "/cb")
	if err != nil {
		t.Fatalf("NewLoopback: %v", err)
	}
	defer lb.Close()

	// Fire the simulated browser redirect in a goroutine so Wait can
	// receive it.
	go func() {
		resp, err := http.Get(lb.RedirectURI() + "?code=abc&state=xyz")
		if err != nil {
			t.Errorf("GET: %v", err)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "授权成功") {
			t.Errorf("landing page missing success text: %s", b)
		}
	}()

	params, err := lb.Wait(2 * time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if params.Get("code") != "abc" || params.Get("state") != "xyz" {
		t.Errorf("captured params wrong: %v", params)
	}
}

func TestLoopback_PathMismatchIsIgnored(t *testing.T) {
	lb, err := NewLoopback(context.Background(), []int{0}, "/cb")
	if err != nil {
		t.Fatalf("NewLoopback: %v", err)
	}
	defer lb.Close()

	// A request to the wrong path should NOT trigger Wait.
	resp, err := http.Get("http://localhost:" + itoa(lb.Port()) + "/other")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for wrong path, got %d", resp.StatusCode)
	}

	// Now hit the right path.
	go func() {
		_, _ = http.Get(lb.RedirectURI() + "?code=ok")
	}()
	params, err := lb.Wait(2 * time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if params.Get("code") != "ok" {
		t.Errorf("expected code=ok, got %q", params.Get("code"))
	}
}

func TestLoopback_Timeout(t *testing.T) {
	lb, err := NewLoopback(context.Background(), []int{0}, "/cb")
	if err != nil {
		t.Fatalf("NewLoopback: %v", err)
	}
	defer lb.Close()
	if _, err := lb.Wait(50 * time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestLoopback_AnyPath(t *testing.T) {
	// path="" or "/" means any path matches (Kiro pattern: server picks
	// path dynamically).
	lb, err := NewLoopback(context.Background(), []int{0}, "")
	if err != nil {
		t.Fatalf("NewLoopback: %v", err)
	}
	defer lb.Close()
	go func() {
		_, _ = http.Get("http://localhost:" + itoa(lb.Port()) + "/whatever/path?code=x")
	}()
	params, err := lb.Wait(2 * time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if params.Get("code") != "x" {
		t.Errorf("expected code=x, got %q", params.Get("code"))
	}
}

func itoa(n int) string {
	// quick local itoa for test paths
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
