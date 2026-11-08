package helps

import (
	"net/http"
	"testing"
)

type cacheKey struct {
	scope string
	proxy string
}

// TestTransportCacheBuilderPanicReleasesMutex pins that a panicking builder cannot
// wedge the cache: Get holds the mutex across build, and the deferred unlock must
// still run while the panic propagates.
// Ported from upstream CLIProxyAPI helps/transport_cache_test.go (8a82bed5825e).
func TestTransportCacheBuilderPanicReleasesMutex(t *testing.T) {
	cache := NewTransportCache[cacheKey](4)
	key := cacheKey{"auth-panic", ""}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic from builder to propagate")
			}
		}()
		_, _ = cache.Get(key, func() (*http.Transport, error) {
			panic("builder boom")
		})
	}()

	if got := cache.Len(); got != 0 {
		t.Fatalf("Len() after panicked build = %d, want 0", got)
	}

	transport, err := cache.Get(key, func() (*http.Transport, error) {
		return &http.Transport{}, nil
	})
	if err != nil || transport == nil {
		t.Fatalf("Get() after recovered builder panic = (%p, %v), want usable cache", transport, err)
	}
}

// TestTransportCachePurgeRetiresEntries pins that Purge marks removed entries retired,
// so a caller still holding one cannot repopulate its idle pool.
// Ported from upstream CLIProxyAPI helps/transport_cache_test.go (8a82bed5825e).
func TestTransportCachePurgeRetiresEntries(t *testing.T) {
	cache := NewTransportCache[cacheKey](4)
	key := cacheKey{"auth-purge", ""}

	_, err := cache.Get(key, func() (*http.Transport, error) {
		return &http.Transport{}, nil
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	entry := cache.items[key].Value.(*transportCacheEntry[cacheKey])
	cache.Purge()
	if !entry.retired.Load() {
		t.Fatal("expected Purge() to mark removed entry as retired")
	}
}
