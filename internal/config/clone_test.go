package config

import (
	"reflect"
	"testing"
)

func TestCloneForRuntimeNil(t *testing.T) {
	var cfg *Config
	if got := cfg.CloneForRuntime(); got != nil {
		t.Fatalf("CloneForRuntime() = %#v, want nil", got)
	}
}

func TestCloneForRuntimeDeepCopiesConfig(t *testing.T) {
	enabled := true
	cfg := &Config{
		Host:                             "127.0.0.1",
		Port:                             8080,
		AuthDir:                          "/tmp/auth",
		Debug:                            true,
		AntigravitySignatureCacheEnabled: &enabled,
		GeminiKey: []GeminiKey{
			{APIKey: "key-1", BaseURL: "https://example.invalid", Models: []GeminiModel{{Name: "m", Alias: "a"}}},
		},
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"codex": {{Name: "upstream", Alias: "alias"}},
		},
	}
	cfg.SDKConfig.APIKeys = []string{"proxy-key"}

	clone := cfg.CloneForRuntime()
	if clone == nil {
		t.Fatal("CloneForRuntime() = nil")
	}
	if clone == cfg {
		t.Fatal("CloneForRuntime() returned original pointer")
	}

	clone.Host = "changed"
	clone.SDKConfig.APIKeys[0] = "mutated"
	clone.GeminiKey[0].APIKey = "mutated"
	clone.GeminiKey[0].Models[0].Alias = "mutated"
	clone.OAuthModelAlias["codex"][0].Alias = "mutated"
	*clone.AntigravitySignatureCacheEnabled = false

	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host shared with clone: %q", cfg.Host)
	}
	if cfg.SDKConfig.APIKeys[0] != "proxy-key" {
		t.Fatalf("APIKeys shared with clone: %q", cfg.SDKConfig.APIKeys[0])
	}
	if cfg.GeminiKey[0].APIKey != "key-1" || cfg.GeminiKey[0].Models[0].Alias != "a" {
		t.Fatalf("GeminiKey shared with clone: %+v", cfg.GeminiKey[0])
	}
	if cfg.OAuthModelAlias["codex"][0].Alias != "alias" {
		t.Fatalf("OAuthModelAlias shared with clone: %+v", cfg.OAuthModelAlias["codex"][0])
	}
	if !*cfg.AntigravitySignatureCacheEnabled {
		t.Fatal("pointer field shared with clone")
	}
}

func TestCloneForRuntimeParsedConfigRoundTrip(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
host: "127.0.0.1"
port: 8080
auth-dir: /tmp/auth
debug: true
credential-concurrency:
  lifecycle-config-revision: 7
api-keys:
  - proxy-key
gemini-api-key:
  - api-key: key-1
    base-url: https://example.invalid
combos:
  - name: combo-a
    models: ["gemini/gemini-2.0-flash"]
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	clone := cfg.CloneForRuntime()
	if clone == nil {
		t.Fatal("CloneForRuntime() = nil")
	}
	if !reflect.DeepEqual(cfg, clone) {
		t.Fatal("CloneForRuntime() differs from original")
	}

	clone.Combos[0].Models[0] = "mutated"
	clone.CredentialConcurrency.LifecycleConfigRevision = 999
	if cfg.Combos[0].Models[0] != "gemini/gemini-2.0-flash" {
		t.Fatalf("Combos shared with clone: %+v", cfg.Combos[0])
	}
	if cfg.CredentialConcurrency.LifecycleConfigRevision != 7 {
		t.Fatalf("CredentialConcurrency shared with clone: %+v", cfg.CredentialConcurrency)
	}
}

// TestParseConfigBytes_ProxyConnectionPool pins the per-credential connection-pool
// block on the legacy (non-v8) key schema and that CloneForRuntime never shares it.
// Ported from upstream CLIProxyAPI internal/config/clone_test.go (bf834311e6d6); the
// upstream variant exercises the v8 `api-keys:` group schema, which this fork does
// not have.
func TestParseConfigBytes_ProxyConnectionPool(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("codex-api-key:\n  - api-key: pool-key\n    base-url: https://ollama.com/v1\n    connection-pool:\n      enabled: true\n      idle-conn-timeout: 15s\n      max-idle-conns-per-host: 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("codex entries = %d, want 1", len(cfg.CodexKey))
	}
	pool := cfg.CodexKey[0].ConnectionPool
	if pool == nil || pool.Enabled == nil || !*pool.Enabled || pool.IdleConnTimeout != "15s" || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 4 {
		t.Fatalf("unexpected pool config: %+v", pool)
	}
	clone := cfg.CloneForRuntime()
	if clone.CodexKey[0].ConnectionPool == pool {
		t.Fatal("clone shares the pool block")
	}
	*clone.CodexKey[0].ConnectionPool.Enabled = false
	if !*cfg.CodexKey[0].ConnectionPool.Enabled {
		t.Fatal("clone shares enabled pointer")
	}
	if clone.CodexKey[0].ConnectionPool.MaxIdleConnsPerHost == pool.MaxIdleConnsPerHost {
		t.Fatal("clone shares max-idle-conns-per-host pointer")
	}
}

// TestParseConfigBytes_ConnectionPoolEmptyBlockStaysOptIn pins that a block with no
// fields still parses as a non-nil opt-in asking for the defaults.
// Upstream CLIProxyAPI 638f5f55e0d9.
func TestParseConfigBytes_ConnectionPoolEmptyBlockStaysOptIn(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("codex-api-key:\n  - api-key: empty-key\n    base-url: https://ollama.com/v1\n    connection-pool: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("codex entries = %d, want 1", len(cfg.CodexKey))
	}
	pool := cfg.CodexKey[0].ConnectionPool
	if pool == nil {
		t.Fatal("empty connection-pool block must stay an opt-in")
	}
	if pool.Enabled != nil || pool.IdleConnTimeout != "" || pool.MaxIdleConnsPerHost != nil {
		t.Fatalf("empty block = %+v, want unset fields", pool)
	}
}

// TestCredentialDeduplicationPreservesDistinctConnectionPools pins that two
// credentials differing only by their pool settings survive sanitization as separate
// entries: the pool block is part of the dedup key.
// Ported from upstream CLIProxyAPI internal/config/connection_pool_entry_test.go (8643ea56d520).
func TestCredentialDeduplicationPreservesDistinctConnectionPools(t *testing.T) {
	raw := []byte(`gemini-api-key:
  - api-key: "shared-gemini-key"
    base-url: "https://example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 2
  - api-key: "shared-gemini-key"
    base-url: "https://example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 5
  - api-key: "shared-gemini-key"
    base-url: "https://example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 5
vertex-api-key:
  - api-key: "shared-vertex-key"
    base-url: "https://vertex.example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 2
  - api-key: "shared-vertex-key"
    base-url: "https://vertex.example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 6
  - api-key: "shared-vertex-key"
    base-url: "https://vertex.example.invalid"
    connection-pool:
      enabled: true
      max-idle-conns-per-host: 6
`)
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if len(cfg.GeminiKey) != 2 {
		t.Fatalf("len(GeminiKey) = %d, want 2", len(cfg.GeminiKey))
	}
	if cfg.GeminiKey[0].ConnectionPool == nil || cfg.GeminiKey[0].ConnectionPool.MaxIdleConnsPerHost == nil || *cfg.GeminiKey[0].ConnectionPool.MaxIdleConnsPerHost != 2 {
		t.Fatalf("GeminiKey[0].ConnectionPool = %+v, want max-idle-conns-per-host=2", cfg.GeminiKey[0].ConnectionPool)
	}
	if cfg.GeminiKey[1].ConnectionPool == nil || cfg.GeminiKey[1].ConnectionPool.MaxIdleConnsPerHost == nil || *cfg.GeminiKey[1].ConnectionPool.MaxIdleConnsPerHost != 5 {
		t.Fatalf("GeminiKey[1].ConnectionPool = %+v, want max-idle-conns-per-host=5", cfg.GeminiKey[1].ConnectionPool)
	}
	if len(cfg.VertexCompatAPIKey) != 2 {
		t.Fatalf("len(VertexCompatAPIKey) = %d, want 2", len(cfg.VertexCompatAPIKey))
	}
	if cfg.VertexCompatAPIKey[0].ConnectionPool == nil || cfg.VertexCompatAPIKey[0].ConnectionPool.MaxIdleConnsPerHost == nil || *cfg.VertexCompatAPIKey[0].ConnectionPool.MaxIdleConnsPerHost != 2 {
		t.Fatalf("VertexCompatAPIKey[0].ConnectionPool = %+v, want max-idle-conns-per-host=2", cfg.VertexCompatAPIKey[0].ConnectionPool)
	}
	if cfg.VertexCompatAPIKey[1].ConnectionPool == nil || cfg.VertexCompatAPIKey[1].ConnectionPool.MaxIdleConnsPerHost == nil || *cfg.VertexCompatAPIKey[1].ConnectionPool.MaxIdleConnsPerHost != 6 {
		t.Fatalf("VertexCompatAPIKey[1].ConnectionPool = %+v, want max-idle-conns-per-host=6", cfg.VertexCompatAPIKey[1].ConnectionPool)
	}
}

// TestFormatConnectionPoolDedupID pins the serialization shape: nil is empty, and
// every field that changes the built transport changes the digest.
func TestFormatConnectionPoolDedupID(t *testing.T) {
	if got := FormatConnectionPoolDedupID(nil); got != "" {
		t.Fatalf("nil pool = %q, want empty", got)
	}
	enabled := true
	two := 2
	five := 5
	base := &ProxyConnectionPoolConfig{Enabled: &enabled, IdleConnTimeout: "300s", MaxIdleConnsPerHost: &two}
	if FormatConnectionPoolDedupID(base) == FormatConnectionPoolDedupID(nil) {
		t.Fatal("a pool block must not serialize like no block")
	}
	if FormatConnectionPoolDedupID(base) != FormatConnectionPoolDedupID(&ProxyConnectionPoolConfig{Enabled: &enabled, IdleConnTimeout: "300s", MaxIdleConnsPerHost: &two}) {
		t.Fatal("identical pool settings must serialize identically")
	}
	if FormatConnectionPoolDedupID(base) == FormatConnectionPoolDedupID(&ProxyConnectionPoolConfig{Enabled: &enabled, IdleConnTimeout: "300s", MaxIdleConnsPerHost: &five}) {
		t.Fatal("different max-idle-conns-per-host must serialize differently")
	}
	if FormatConnectionPoolDedupID(base) == FormatConnectionPoolDedupID(&ProxyConnectionPoolConfig{Enabled: &enabled, IdleConnTimeout: "60s", MaxIdleConnsPerHost: &two}) {
		t.Fatal("different idle-conn-timeout must serialize differently")
	}
}
