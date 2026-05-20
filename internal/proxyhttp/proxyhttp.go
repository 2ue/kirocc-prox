// Package proxyhttp caches *http.Client instances keyed by proxy URL so
// each pool credential can pin its auth-plane HTTP (token refresh / OAuth
// token exchange / getUsageLimits) through a fixed egress without
// rebuilding a transport on every call.
//
// Empty proxy URL falls back to http.DefaultTransport (which honors
// HTTPS_PROXY). Multiple credentials sharing the same URL share one
// client.
package proxyhttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const defaultTimeout = 30 * time.Second

// Pool is a thread-safe cache of *http.Client by proxy URL.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
	timeout time.Duration
}

// New returns an empty Pool. timeout <= 0 falls back to 30s.
func New(timeout time.Duration) *Pool {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Pool{
		clients: make(map[string]*http.Client),
		timeout: timeout,
	}
}

// ClientFor returns an *http.Client routed through proxyURL. The result
// is cached; repeated calls with the same URL return the same client.
// An empty proxyURL returns a "default" client using the process's
// default transport (which honors HTTPS_PROXY env).
//
// Supported schemes: http, https, socks5.
func (p *Pool) ClientFor(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	p.mu.RLock()
	if c, ok := p.clients[proxyURL]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check after acquiring write lock (race with another caller).
	if c, ok := p.clients[proxyURL]; ok {
		return c, nil
	}

	c, err := build(proxyURL, p.timeout)
	if err != nil {
		return nil, err
	}
	p.clients[proxyURL] = c
	return c, nil
}

// build creates a fresh *http.Client. Exposed for tests that want to
// bypass the cache.
func build(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if pwd, hasPwd := u.User.Password(); hasPwd {
			auth = &proxy.Auth{User: u.User.Username(), Password: pwd}
		} else if u.User != nil {
			auth = &proxy.Auth{User: u.User.Username()}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5)", u.Scheme)
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}
