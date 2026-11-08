package helps

import (
	"net/http"

	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// Pooling is configured per api-key credential, so tests opt in through the auth
// override instead of a global config block.
// Ported from upstream CLIProxyAPI helps/proxy_pool_test_helpers_test.go (34a01b7602b3).
func poolEnabled() *cliproxyauth.ProxyConnectionPoolOverride {
	enabled := true
	return &cliproxyauth.ProxyConnectionPoolOverride{Enabled: &enabled}
}

func poolOptInAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, ProxyConnectionPool: poolEnabled()}
}

func poolAuth(id string, override *cliproxyauth.ProxyConnectionPoolOverride) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, ProxyConnectionPool: override}
}

// poolBlock is the block with no fields: the opt-in that uses the defaults.
func poolBlock() *cliproxyauth.ProxyConnectionPoolOverride {
	return &cliproxyauth.ProxyConnectionPoolOverride{}
}

func testConfig() *config.Config { return &config.Config{} }

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
