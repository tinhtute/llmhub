package helps

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// Ported from upstream CLIProxyAPI helps/utls_proxy_pool_test.go (34a01b7602b3).
func TestUtlsFallbackProxyConnectionPool(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			ResetProxyTransports()
			t.Cleanup(ResetProxyTransports)
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			cfg := testConfig()
			auth := &cliproxyauth.Auth{ProxyURL: "direct"}
			if enabled {
				auth.ProxyConnectionPool = poolEnabled()
			}
			for i := 0; i < 2; i++ {
				client := NewUtlsHTTPClient(cfg, auth, 0)
				resp, err := client.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				if err = resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			want := int32(2)
			if enabled {
				want = 1
			}
			if got := connections.Load(); got != want {
				t.Fatalf("connections = %d, want %d", got, want)
			}
		})
	}
}

// TestUtlsLazyPoolsDoNotEvictUsedPool pins that a request only populates the pool
// kind it actually uses. Eagerly populating the pool on client construction consumed
// a cache slot per key even when the credential never took the fallback branch, so a
// large key set evicted its own live pools.
// Upstream CLIProxyAPI 6c231c23321b.
func TestUtlsLazyPoolsDoNotEvictUsedPool(t *testing.T) {
	ResetProxyTransports()
	t.Cleanup(ResetProxyTransports)

	var opened atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	cfg := testConfig()
	const keys = 300
	for round := 0; round < 2; round++ {
		for i := 0; i < keys; i++ {
			auth := poolOptInAuth("key-" + strconv.Itoa(i))
			auth.ProxyURL = "direct"
			client := NewUtlsHTTPClient(cfg, auth, 0)
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if err = resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := proxyTransportCache.Len(); got != keys {
		t.Fatalf("cache holds %d entries, want %d (one per key, no unused pools)", got, keys)
	}
	if got := opened.Load(); got != keys {
		t.Fatalf("connections = %d, want %d (second round must reuse every key's pool)", got, keys)
	}
}
