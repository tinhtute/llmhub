package auth

import "testing"

// TestIndexSeedIncludesConnectionPool pins that two credentials that differ only by
// their connection-pool settings produce different auth indexes; otherwise the
// management auth-index mapping would collapse them onto one credential.
// Upstream CLIProxyAPI 8643ea56d520.
func TestIndexSeedIncludesConnectionPool(t *testing.T) {
	enabled := true
	two := 2
	five := 5
	base := func(pool *ProxyConnectionPoolOverride) *Auth {
		return &Auth{
			Provider:            "gemini",
			Attributes:          map[string]string{"base_url": "https://example.invalid", "api_key": "shared"},
			ProxyConnectionPool: pool,
		}
	}
	without := base(nil)
	first := base(&ProxyConnectionPoolOverride{Enabled: &enabled, MaxIdleConnsPerHost: &two})
	second := base(&ProxyConnectionPoolOverride{Enabled: &enabled, MaxIdleConnsPerHost: &five})

	if without.indexSeed() == first.indexSeed() {
		t.Fatal("a pool block must change the index seed")
	}
	if first.indexSeed() == second.indexSeed() {
		t.Fatal("different pool settings must produce different index seeds")
	}
	if first.indexSeed() != base(&ProxyConnectionPoolOverride{Enabled: &enabled, MaxIdleConnsPerHost: &two}).indexSeed() {
		t.Fatal("identical pool settings must produce an identical index seed")
	}
}

// TestAuthCloneDeepCopiesConnectionPool pins that auth snapshots never share the
// mutable pool override.
func TestAuthCloneDeepCopiesConnectionPool(t *testing.T) {
	enabled := true
	two := 2
	original := &Auth{ID: "pool-clone", ProxyConnectionPool: &ProxyConnectionPoolOverride{Enabled: &enabled, IdleConnTimeout: "300s", MaxIdleConnsPerHost: &two}}
	clone := original.Clone()
	if clone.ProxyConnectionPool == original.ProxyConnectionPool {
		t.Fatal("clone shares the pool override pointer")
	}
	*clone.ProxyConnectionPool.Enabled = false
	*clone.ProxyConnectionPool.MaxIdleConnsPerHost = 9
	if !*original.ProxyConnectionPool.Enabled || *original.ProxyConnectionPool.MaxIdleConnsPerHost != 2 {
		t.Fatal("clone shares pool override fields")
	}
}
