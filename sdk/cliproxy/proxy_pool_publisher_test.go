package cliproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// TestServiceProxyPoolFenceIsPerService pins that building or reloading one service
// never retires the connection pools another service is still serving. The fence used
// to be a single process-wide counter, so creating a second SDK service permanently
// degraded the first one to a fresh transport per request.
// Ported from upstream CLIProxyAPI sdk/cliproxy/proxy_pool_publisher_test.go (638f5f55e0d9).
func TestServiceProxyPoolFenceIsPerService(t *testing.T) {
	helps.ResetProxyTransports()
	t.Cleanup(helps.ResetProxyTransports)

	build := func() *Service {
		t.Helper()
		dir := t.TempDir()
		service, errBuild := NewBuilder().
			WithConfig(&config.Config{AuthDir: dir}).
			WithConfigPath(filepath.Join(dir, "config.yaml")).
			Build()
		if errBuild != nil {
			t.Fatalf("build service: %v", errBuild)
		}
		return service
	}

	auth := &coreauth.Auth{
		ID:                  "service-pool-key",
		ProxyURL:            "direct",
		ProxyConnectionPool: &coreauth.ProxyConnectionPoolOverride{},
	}
	transportFor := func(service *Service) *http.Transport {
		t.Helper()
		client := helps.NewProxyAwareHTTPClient(context.Background(), service.currentConfig(), auth, 0)
		transport, ok := client.Transport.(*http.Transport)
		if !ok || transport == nil {
			t.Fatalf("transport = %T, want a pooled *http.Transport", client.Transport)
		}
		return transport
	}

	first := build()
	if first.proxyPoolPublisher == 0 {
		t.Fatal("service did not reserve a pool publisher")
	}
	firstTransport := transportFor(first)
	if got := transportFor(first); got != firstTransport {
		t.Fatal("first service did not reuse its pool")
	}

	// A second, independent service publishes its own config.
	second := build()
	if second.proxyPoolPublisher == first.proxyPoolPublisher {
		t.Fatal("services share one pool publisher")
	}
	if got := transportFor(first); got != firstTransport {
		t.Fatal("building another service retired the active service's pool")
	}

	// Reloading the second service must not touch the first service's pools either.
	second.applyConfigUpdate(&config.Config{AuthDir: second.currentConfig().AuthDir})
	if got := transportFor(first); got != firstTransport {
		t.Fatal("reloading another service retired the active service's pool")
	}

	// Shutting down the second service retires its publisher and fences its snapshot
	// without touching the first service's pool.
	// Upstream CLIProxyAPI fd02b720e6b3.
	secondTransport := transportFor(second)
	secondCfgBeforeShutdown := second.currentConfig()
	if errShutdown := second.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("shutdown second service: %v", errShutdown)
	}
	if got := transportFor(first); got != firstTransport {
		t.Fatal("shutting down another service retired the active service's pool")
	}
	afterShutdownClient := helps.NewProxyAwareHTTPClient(context.Background(), secondCfgBeforeShutdown, auth, 0)
	afterShutdownTransport, ok := afterShutdownClient.Transport.(*http.Transport)
	if !ok || afterShutdownTransport == nil || afterShutdownTransport == secondTransport || !afterShutdownTransport.DisableKeepAlives {
		t.Fatalf("post-shutdown snapshot must be fenced into an unpooled transport, got %#v", afterShutdownClient.Transport)
	}
}
