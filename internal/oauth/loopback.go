// [fork] Ephemeral localhost HTTP listener for capturing OAuth redirects.
// This matches the desktop-app pattern used by cockpit-tools (Kiro) and
// CLIProxyAPI (Codex): the OAuth server hardcodes a small set of allowed
// loopback redirect URIs, so the proxy must bind on one of THOSE ports
// (not the admin port) for the auth flow to succeed.
//
// Usage:
//
//	lb, err := oauth.NewLoopback(ctx, []int{1455}, "/auth/callback")
//	authURL := buildURL(lb.RedirectURI(), ...)
//	openBrowser(authURL)
//	params, err := lb.Wait(10*time.Minute)
//	defer lb.Close()
//
// The Loopback responds to the captured request with a static HTML page
// so the user's browser shows a "you can close this tab" notice while
// the caller finishes the token exchange in the background.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultLoopbackTimeout is how long Wait blocks before giving up.
const DefaultLoopbackTimeout = 10 * time.Minute

// Loopback is an HTTP listener bound to 127.0.0.1:port. It captures the
// first request whose path matches expectedPath and surfaces the query
// parameters via Wait.
type Loopback struct {
	port    int
	path    string
	ln      net.Listener
	srv     *http.Server
	result  chan url.Values
	once    sync.Once
	closeMu sync.Mutex
	closed  bool

	// capturedPath is the actual URL.Path of the redirect that arrived
	// (set inside handle). Kiro picks this path dynamically — we need it
	// to rebuild the redirect_uri at token-exchange time.
	pathMu       sync.RWMutex
	capturedPath string
}

// NewLoopback tries each candidate port in order until one binds, then
// starts an HTTP server that listens for OAuth redirects.
//
// expectedPath is the URL path the provider redirects to (e.g.
// "/auth/callback" for Codex). Pass "" or "/" to accept any path —
// useful for Kiro which echoes back a server-chosen path.
func NewLoopback(ctx context.Context, candidatePorts []int, expectedPath string) (*Loopback, error) {
	if len(candidatePorts) == 0 {
		return nil, errors.New("oauth: no candidate ports")
	}

	var ln net.Listener
	var firstErr error
	for _, p := range candidatePorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln = l
			break
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("oauth: all candidate ports unavailable; first error: %w", firstErr)
	}
	// Read the actual bound port (matters when caller passed 0 for any).
	port := ln.Addr().(*net.TCPAddr).Port

	lb := &Loopback{
		port:   port,
		path:   normalizePath(expectedPath),
		ln:     ln,
		result: make(chan url.Values, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", lb.handle)
	lb.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = lb.srv.Serve(ln)
	}()
	return lb, nil
}

// Port returns the bound TCP port.
func (l *Loopback) Port() int { return l.port }

// CapturedPath returns the URL.Path of the captured redirect request.
// Empty until a request has been received. Kiro requires this verbatim
// path when rebuilding redirect_uri for the token-exchange call (the
// auth server chooses the path dynamically — e.g.
// "/oauth/kiro/idc/callback").
func (l *Loopback) CapturedPath() string {
	l.pathMu.RLock()
	defer l.pathMu.RUnlock()
	return l.capturedPath
}

// SetCapturedPath lets the admin manual-callback handler seed the
// captured path from the URL the user pasted, since in that flow the
// loopback never received the redirect itself. Idempotent: subsequent
// calls overwrite the prior value.
func (l *Loopback) SetCapturedPath(p string) {
	l.pathMu.Lock()
	defer l.pathMu.Unlock()
	l.capturedPath = p
}

// RedirectURI returns the absolute URL the OAuth provider should redirect
// to (without query string). The OAuth client sends this verbatim in the
// authorize call.
//
// When path is empty / root, we return "http://localhost:PORT" WITHOUT a
// trailing slash — matching cockpit-tools (Kiro). OAuth servers commonly
// do exact-string matching on redirect_uri, and a stray trailing slash
// fails that check, causing "An error was encountered with the requested
// page" on the auth portal.
func (l *Loopback) RedirectURI() string {
	if l.path == "" || l.path == "/" {
		return fmt.Sprintf("http://localhost:%d", l.port)
	}
	return fmt.Sprintf("http://localhost:%d%s", l.port, l.path)
}

// Wait blocks until the loopback captures a request or timeout elapses.
// On success returns the captured query parameters. The caller still
// needs to Close() the loopback afterwards.
func (l *Loopback) Wait(timeout time.Duration) (url.Values, error) {
	if timeout <= 0 {
		timeout = DefaultLoopbackTimeout
	}
	select {
	case v := <-l.result:
		return v, nil
	case <-time.After(timeout):
		return nil, errors.New("oauth: loopback wait timeout")
	}
}

// WaitContext behaves like Wait but also cancels on ctx.Done.
func (l *Loopback) WaitContext(ctx context.Context, timeout time.Duration) (url.Values, error) {
	if timeout <= 0 {
		timeout = DefaultLoopbackTimeout
	}
	select {
	case v := <-l.result:
		return v, nil
	case <-time.After(timeout):
		return nil, errors.New("oauth: loopback wait timeout")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the listener. Safe to call multiple times.
func (l *Loopback) Close() {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = l.srv.Shutdown(ctx)
}

// handle captures the first request matching expectedPath, sends its
// query params to the result channel, and renders a static HTML page.
// Subsequent requests get a "already completed" page.
func (l *Loopback) handle(w http.ResponseWriter, r *http.Request) {
	// Path match: empty path matches any; otherwise must equal exactly.
	if l.path != "" && l.path != "/" {
		if normalizePath(r.URL.Path) != l.path {
			http.NotFound(w, r)
			return
		}
	}

	q := r.URL.Query()

	// Race-free: only the first captured callback wins.
	captured := false
	l.once.Do(func() {
		l.pathMu.Lock()
		l.capturedPath = r.URL.Path
		l.pathMu.Unlock()
		l.result <- q
		captured = true
	})

	if captured {
		writeLandingPage(w, q)
	} else {
		writeLandingPage(w, url.Values{"error": []string{"already-captured"}})
	}
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// writeLandingPage renders the success/failure page shown in the user's
// browser after the OAuth provider redirects. The token exchange happens
// out-of-band in the caller; this page just signals the user they can
// close the tab.
func writeLandingPage(w http.ResponseWriter, q url.Values) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	ok := q.Get("error") == ""
	title := "授权成功"
	color := "#5b8c5a"
	body := "已捕获到授权码，正在后台完成 token 兑换。可关闭本页面，回到管理后台查看结果。"
	if !ok {
		title = "授权未完成"
		color = "#a05050"
		body = "回调里未携带 code 参数（" + html.EscapeString(q.Get("error")) + "）。"
	}
	page := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>` + title + `</title>
<style>body{font-family:system-ui,sans-serif;background:#FAF8F5;color:#2A2622;
display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.card{max-width:420px;padding:28px 32px;background:#fff;border:1px solid #E8E3DD;
border-radius:10px;box-shadow:0 1px 6px rgba(0,0,0,.04)}
h1{color:` + color + `;margin:0 0 12px;font-size:18px}
p{margin:0 0 8px;line-height:1.55;font-size:14px;color:#555}</style>
</head><body><div class="card"><h1>` + title + `</h1><p>` + body + `</p>
<p style="margin-top:14px"><a href="javascript:window.close()">关闭此页面</a></p>
</div></body></html>`
	_, _ = w.Write([]byte(page))
}
