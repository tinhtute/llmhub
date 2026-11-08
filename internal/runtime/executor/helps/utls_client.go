package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/therealtinhtute/llmhub/sdk/proxyutil"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains. The HTTP
// version follows the negotiated ALPN protocol: HTTP/2 connections are pooled
// per host, while HTTP/1.1 (including empty ALPN behind TLS-inspecting
// proxies) is served on single-use transports over the handshaken connection.
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
	// sessionCache is scoped to this round tripper, which is already keyed by
	// proxy, so resumption never crosses proxy boundaries.
	sessionCache tls.ClientSessionCache
	// dialTLS overrides the dial-plus-handshake step in tests.
	dialTLS func(host, addr string) (*tls.UConn, error)
}

// claudeCodeSessionCacheCapacity bounds the per-transport TLS session cache for
// the Anthropic inference plane.
const claudeCodeSessionCacheCapacity = 32

// newClaudeCodeTLSConfig builds the uTLS config for one inference-plane dial.
//
// OmitEmptyPsk keeps the pre_shared_key extension silent until a session is
// cached, so an unresumed ClientHello stays byte-identical to the captured
// native handshake. PreferSkipResumptionOnNilExtension turns uTLS's HelloCustom
// "resume without the matching extension" panic into a skipped resumption.
func newClaudeCodeTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{
		connections:  make(map[string]*http2.ClientConn),
		pending:      make(map[string]*sync.Cond),
		dialer:       dialer,
		sessionCache: tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity),
	}
}

func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
	}

	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	tlsConn, err := t.dialUtlsConnection(host, addr)
	if err != nil {
		return nil, err
	}

	switch protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol {
	case "h2":
		tr := &http2.Transport{}
		h2Conn, err := tr.NewClientConn(tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		return h2Conn, nil
	case "", "http/1.1":
		// Empty ALPN means HTTP/1.1, including when a TLS-inspecting proxy
		// omits ALPN. Hand the live connection back through the fallback
		// error so RoundTrip serves exactly one request on it instead of
		// pooling it as HTTP/2.
		return nil, &http1FallbackConn{conn: tlsConn}
	default:
		tlsConn.Close()
		return nil, fmt.Errorf("utls: unsupported negotiated protocol %q", protocol)
	}
}

// dialUtlsConnection dials upstream and completes the uTLS handshake.
func (t *utlsRoundTripper) dialUtlsConnection(host, addr string) (*tls.UConn, error) {
	if t.dialTLS != nil {
		return t.dialTLS(host, addr)
	}
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	// The uTLS path dials through proxy.Direct for SOCKS5, which leaves keepalive at
	// the OS default of hours; a silently dead peer would then pin the connection.
	// Upstream CLIProxyAPI fd02b720e6b3.
	conn = stampProxyKeepAlive(conn)

	tlsConfig := newClaudeCodeTLSConfig(host, t.sessionCache)
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	return tlsConn, nil
}

// http1FallbackConn reports a successful TLS handshake whose ALPN negotiation
// selected HTTP/1.1. It carries the live connection so the caller can serve
// one request on it without redialing; HTTP/1.1 connections are never pooled.
type http1FallbackConn struct {
	conn *tls.UConn
}

func (e *http1FallbackConn) Error() string {
	return "utls: negotiated HTTP/1.1, use single-use fallback transport"
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		var fallback *http1FallbackConn
		if !errors.As(err, &fallback) || fallback.conn == nil {
			return nil, err
		}
		return roundTripSingleUseHTTP1(req, fallback.conn)
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		return nil, err
	}

	return resp, nil
}

// roundTripSingleUseHTTP1 serves one request over an already-handshaken uTLS
// connection whose ALPN selected HTTP/1.1. A fresh non-pooling transport
// retains net/http's cancellation and request-body handling without changing
// the TLS fingerprint or redialing through another path.
func roundTripSingleUseHTTP1(req *http.Request, tlsConn *tls.UConn) (*http.Response, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return tlsConn, nil
		},
	}
	resp, err := transport.RoundTrip(req)
	transport.CloseIdleConnections()
	if err != nil {
		closeUtlsConn(tlsConn)
		return nil, err
	}
	if resp == nil {
		closeUtlsConn(tlsConn)
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &closeConnBody{
		ReadCloser: resp.Body,
		closeConn:  func() error { return closeUtlsConn(tlsConn) },
	}
	return resp, nil
}

// closeUtlsConn tolerates an already-closed connection: the HTTP/1.1
// transport may have closed its non-pooled connection after reading the
// response or canceling the request.
func closeUtlsConn(tlsConn *tls.UConn) error {
	if err := tlsConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// closeConnBody closes the response body and then the underlying connection.
type closeConnBody struct {
	io.ReadCloser
	closeConn func() error
	once      sync.Once
	err       error
}

func (b *closeConnBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		var errBody error
		if b.ReadCloser != nil {
			errBody = b.ReadCloser.Close()
		}
		var errConn error
		if b.closeConn != nil {
			errConn = b.closeConn()
		}
		b.err = errors.Join(errBody, errConn)
	})
	return b.err
}

// anthropicHosts contains the hosts that should use utls Chrome TLS fingerprint.
var anthropicHosts = map[string]struct{}{
	"api.anthropic.com": {},
}

// fallbackRoundTripper uses utls for Anthropic HTTPS hosts and falls back to
// standard transport for all other requests (non-HTTPS or non-Anthropic hosts).
//
// The fallback branch may be resolved lazily on first use: a single request only
// ever takes one branch, and building the pool up front would spend a cache slot per
// key even for credentials that only ever talk to Anthropic hosts.
// Upstream CLIProxyAPI 6c231c23321b.
type fallbackRoundTripper struct {
	mu         sync.Mutex
	utls       *utlsRoundTripper
	fallback   http.RoundTripper
	fallbackFn func() http.RoundTripper
}

func (f *fallbackRoundTripper) fallbackRoundTripper() http.RoundTripper {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fallbackFn != nil {
		if rt := f.fallbackFn(); rt != nil {
			f.fallback = rt
		}
		f.fallbackFn = nil
	}
	return f.fallback
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := anthropicHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallbackRoundTripper().RoundTrip(req)
}

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for Claude API requests to match real Claude Code's TLS behavior.
// Falls back to standard transport for non-HTTPS requests.
func NewUtlsHTTPClient(cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	utlsRT := newUtlsRoundTripper(proxyURL)

	var standardTransport http.RoundTripper = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	// A credential opt-in applies to this client too, but the pooled transport is only
	// built when a request actually takes the fallback branch.
	fallbackFn := func() http.RoundTripper {
		if transport := pooledProxyTransport(proxyURL, cfg, auth); transport != nil {
			return transport
		}
		return standardTransport
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls:       utlsRT,
			fallback:   standardTransport,
			fallbackFn: fallbackFn,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
