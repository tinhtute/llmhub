package executor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func resetClaudeDeviceProfileCache() {
	helps.ResetClaudeDeviceProfileCache()
}

func newClaudeHeaderTestRequest(t *testing.T, incoming http.Header) *http.Request {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginReq := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	ginReq.Header = incoming.Clone()
	ginCtx.Request = ginReq

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	return req.WithContext(context.WithValue(req.Context(), "gin", ginCtx))
}

func assertClaudeFingerprint(t *testing.T, headers http.Header, userAgent, pkgVersion, runtimeVersion, osName, arch string) {
	t.Helper()

	if got := headers.Get("User-Agent"); got != userAgent {
		t.Fatalf("User-Agent = %q, want %q", got, userAgent)
	}
	if got := headers.Get("X-Stainless-Package-Version"); got != pkgVersion {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, pkgVersion)
	}
	if got := headers.Get("X-Stainless-Runtime-Version"); got != runtimeVersion {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, runtimeVersion)
	}
	if got := headers.Get("X-Stainless-Os"); got != osName {
		t.Fatalf("X-Stainless-Os = %q, want %q", got, osName)
	}
	if got := headers.Get("X-Stainless-Arch"); got != arch {
		t.Fatalf("X-Stainless-Arch = %q, want %q", got, arch)
	}
}

func TestApplyClaudeHeaders_UsesConfiguredBaselineFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			Timeout:                "900",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline",
		Attributes: map[string]string{
			"api_key":                            "key-baseline",
			"header:User-Agent":                  "evil-client/9.9",
			"header:X-Stainless-Os":              "Linux",
			"header:X-Stainless-Arch":            "x64",
			"header:X-Stainless-Package-Version": "9.9.9",
		},
	}
	incoming := http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	}

	req := newClaudeHeaderTestRequest(t, incoming)
	applyClaudeHeaders(req, auth, "key-baseline", false, nil, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "evil-client/9.9", "9.9.9", "v24.5.0", "Linux", "x64")
	if got := req.Header.Get("X-Stainless-Timeout"); got != "900" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q", got, "900")
	}
}

func TestApplyClaudeHeaders_TracksHighestClaudeCLIFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-upgrade",
		Attributes: map[string]string{
			"api_key": "key-upgrade",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-upgrade", false, nil, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-upgrade", false, nil, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	higherReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	})
	applyClaudeHeaders(higherReq, auth, "key-upgrade", false, nil, nil, cfg)
	assertClaudeFingerprint(t, higherReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-upgrade", false, nil, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DoesNotDowngradeConfiguredBaselineOnFirstClaudeClient(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline-floor",
		Attributes: map[string]string{
			"api_key": "key-baseline-floor",
		},
	}

	olderClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(olderClaudeReq, auth, "key-baseline-floor", false, nil, nil, cfg)
	assertClaudeFingerprint(t, olderClaudeReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	newerClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(newerClaudeReq, auth, "key-baseline-floor", false, nil, nil, cfg)
	assertClaudeFingerprint(t, newerClaudeReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_UpgradesCachedSoftwareFingerprintWhenBaselineAdvances(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	oldCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	newCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.77 (external, cli)",
			PackageVersion:         "0.87.0",
			RuntimeVersion:         "v24.8.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline-reload",
		Attributes: map[string]string{
			"api_key": "key-baseline-reload",
		},
	}

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-baseline-reload", false, nil, nil, oldCfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-baseline-reload", false, nil, nil, newCfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_LearnsOfficialFingerprintAfterCustomBaselineFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "my-gateway/1.0",
			PackageVersion:         "custom-pkg",
			RuntimeVersion:         "custom-runtime",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-custom-baseline-learning",
		Attributes: map[string]string{
			"api_key": "key-custom-baseline-learning",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-custom-baseline-learning", false, nil, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "my-gateway/1.0", "custom-pkg", "custom-runtime", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-custom-baseline-learning", false, nil, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")

	postLearningThirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(postLearningThirdPartyReq, auth, "key-custom-baseline-learning", false, nil, nil, cfg)
	assertClaudeFingerprint(t, postLearningThirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestResolveClaudeDeviceProfile_RechecksCacheBeforeStoringCandidate(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-racy-upgrade",
		Attributes: map[string]string{
			"api_key": "key-racy-upgrade",
		},
	}

	lowPaused := make(chan struct{})
	releaseLow := make(chan struct{})
	var pauseOnce sync.Once
	var releaseOnce sync.Once

	helps.ClaudeDeviceProfileBeforeCandidateStore = func(candidate helps.ClaudeDeviceProfile) {
		if candidate.UserAgent != "claude-cli/2.1.62 (external, cli)" {
			return
		}
		pauseOnce.Do(func() { close(lowPaused) })
		<-releaseLow
	}
	t.Cleanup(func() {
		helps.ClaudeDeviceProfileBeforeCandidateStore = nil
		releaseOnce.Do(func() { close(releaseLow) })
	})

	lowResultCh := make(chan helps.ClaudeDeviceProfile, 1)
	go func() {
		lowResultCh <- helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
			"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
			"X-Stainless-Package-Version": []string{"0.74.0"},
			"X-Stainless-Runtime-Version": []string{"v24.3.0"},
			"X-Stainless-Os":              []string{"Linux"},
			"X-Stainless-Arch":            []string{"x64"},
		}, cfg)
	}()

	select {
	case <-lowPaused:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate to pause before storing")
	}

	highResult := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	}, cfg)
	releaseOnce.Do(func() { close(releaseLow) })

	select {
	case lowResult := <-lowResultCh:
		if lowResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
			t.Fatalf("lowResult.UserAgent = %q, want %q", lowResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
		}
		if lowResult.PackageVersion != "0.75.0" {
			t.Fatalf("lowResult.PackageVersion = %q, want %q", lowResult.PackageVersion, "0.75.0")
		}
		if lowResult.OS != "MacOS" || lowResult.Arch != "arm64" {
			t.Fatalf("lowResult platform = %s/%s, want %s/%s", lowResult.OS, lowResult.Arch, "MacOS", "arm64")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate result")
	}

	if highResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("highResult.UserAgent = %q, want %q", highResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if highResult.OS != "MacOS" || highResult.Arch != "arm64" {
		t.Fatalf("highResult platform = %s/%s, want %s/%s", highResult.OS, highResult.Arch, "MacOS", "arm64")
	}

	cached := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	}, cfg)
	if cached.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("cached.UserAgent = %q, want %q", cached.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if cached.PackageVersion != "0.75.0" {
		t.Fatalf("cached.PackageVersion = %q, want %q", cached.PackageVersion, "0.75.0")
	}
	if cached.OS != "MacOS" || cached.Arch != "arm64" {
		t.Fatalf("cached platform = %s/%s, want %s/%s", cached.OS, cached.Arch, "MacOS", "arm64")
	}
}

func TestApplyClaudeHeaders_ThirdPartyBaselineThenOfficialUpgradeKeepsPinnedPlatform(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-third-party-then-official",
		Attributes: map[string]string{
			"api_key": "key-third-party-then-official",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-third-party-then-official", false, nil, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-third-party-then-official", false, nil, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DisableDeviceProfileStabilization(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-disable-stability",
		Attributes: map[string]string{
			"api_key": "key-disable-stability",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-disable-stability", false, nil, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-disable-stability", false, nil, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.60 (external, cli)", "0.10.0", "v18.0.0", "Windows", "x64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-disable-stability", false, nil, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.61 (external, cli)", "0.73.0", "v24.2.0", "Windows", "x64")
}

func TestApplyClaudeHeaders_LegacyModePreservesConfiguredUserAgentOverrideForClaudeClients(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-legacy-ua-override",
		Attributes: map[string]string{
			"api_key":           "key-legacy-ua-override",
			"header:User-Agent": "config-ua/1.0",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-ua-override", false, nil, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "config-ua/1.0", "0.74.0", "v24.3.0", "Linux", "x64")
}

func TestApplyClaudeHeaders_LegacyModeFallsBackToRuntimeOSArchWhenMissing(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-legacy-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-legacy-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-runtime-os-arch", false, nil, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestApplyClaudeHeaders_UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.60 (external, cli)",
			PackageVersion: "0.70.0",
			RuntimeVersion: "v22.0.0",
			OS:             "MacOS",
			Arch:           "arm64",
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-unset-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-unset-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-unset-runtime-os-arch", false, nil, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestClaudeDeviceProfileStabilizationEnabled_DefaultFalse(t *testing.T) {
	if helps.ClaudeDeviceProfileStabilizationEnabled(nil) {
		t.Fatal("expected nil config to default to disabled stabilization")
	}
	if helps.ClaudeDeviceProfileStabilizationEnabled(&config.Config{}) {
		t.Fatal("expected unset stabilize-device-profile to default to disabled stabilization")
	}
}

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestApplyClaudeToolPrefix_WithToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"}],"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"beta"},{"type":"tool_reference","tool_name":"proxy_gamma"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.tool_name").String(); got != "proxy_beta" {
		t.Fatalf("messages.0.content.0.tool_name = %q, want %q", got, "proxy_beta")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != "proxy_gamma" {
		t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, "proxy_gamma")
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinTools(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"my_custom_tool","input_schema":{"type":"object"}}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name should not be prefixed: tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_my_custom_tool" {
		t.Fatalf("custom tool should be prefixed: tools.1.name = %q, want %q", got, "proxy_my_custom_tool")
	}
}

func TestApplyClaudeToolPrefix_BuiltinToolSkipped(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}},
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_KnownBuiltinInHistoryOnly(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_CustomToolsPrefixed(t *testing.T) {
	body := []byte(`{
		"tools": [{"name": "Read"}, {"name": "Write"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}},
				{"type": "tool_use", "name": "Write", "id": "w1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Write" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Write")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Write" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Write")
	}
}

func TestApplyClaudeToolPrefix_ToolChoiceBuiltin(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search"},
			{"name": "Read"}
		],
		"tool_choice": {"type": "tool", "name": "web_search"}
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "web_search")
	}
}

func TestApplyClaudeToolPrefix_KnownFallbackBuiltinsRemainUnprefixed(t *testing.T) {
	for _, builtin := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		t.Run(builtin, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{
				"tools":[{"name":"Read"}],
				"tool_choice":{"type":"tool","name":%q},
				"messages":[{"role":"assistant","content":[{"type":"tool_use","name":%q,"id":"toolu_1","input":{}},{"type":"tool_reference","tool_name":%q},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":%q}]}]}]
			}`, builtin, builtin, builtin, builtin))
			out := applyClaudeToolPrefix(input, "proxy_")

			if got := gjson.GetBytes(out, "tool_choice.name").String(); got != builtin {
				t.Fatalf("tool_choice.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != builtin {
				t.Fatalf("messages.0.content.0.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.2.content.0.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.2.content.0.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
				t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
			}
		})
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromResponse_WithToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_reference","tool_name":"proxy_alpha"},{"type":"tool_reference","tool_name":"bravo"}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.tool_name").String(); got != "alpha" {
		t.Fatalf("content.0.tool_name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.tool_name").String(); got != "bravo" {
		t.Fatalf("content.1.tool_name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestStripClaudeToolPrefixFromStreamLine_WithToolReference(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_reference","tool_name":"proxy_beta"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.tool_name").String(); got != "beta" {
		t.Fatalf("content_block.tool_name = %q, want %q", got, "beta")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReference(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"mcp__nia__manage_resource"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "proxy_mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "proxy_mcp__nia__manage_resource")
	}
}

func TestClaudeExecutor_ReusesUserIDAcrossModelsWhenCacheEnabled(t *testing.T) {
	var userIDs []string
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userID := gjson.GetBytes(body, "metadata.user_id").String()
		model := gjson.GetBytes(body, "model").String()
		userIDs = append(userIDs, userID)
		requestModels = append(requestModels, model)
		t.Logf("HTTP Server received request: model=%s, user_id=%s, url=%s", model, userID, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	t.Logf("End-to-end test: Fake HTTP server started at %s", server.URL)

	cacheEnabled := true
	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "key-123",
				BaseURL: server.URL,
				Cloak: &config.CloakConfig{
					CacheUserID: &cacheEnabled,
				},
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	models := []string{"claude-3-5-sonnet", "claude-3-5-haiku"}
	for _, model := range models {
		t.Logf("Sending request for model: %s", model)
		modelPayload, _ := sjson.SetBytes(payload, "model", model)
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   model,
			Payload: modelPayload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute(%s) error: %v", model, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	t.Logf("user_id[0] (model=%s): %s", requestModels[0], userIDs[0])
	t.Logf("user_id[1] (model=%s): %s", requestModels[1], userIDs[1])
	if userIDs[0] != userIDs[1] {
		t.Fatalf("expected user_id to be reused across models, got %q and %q", userIDs[0], userIDs[1])
	}
	if !helps.IsValidUserID(userIDs[0]) {
		t.Fatalf("user_id %q is not valid", userIDs[0])
	}
	t.Logf("✓ End-to-end test passed: Same user_id (%s) was used for both models", userIDs[0])
}

func TestClaudeExecutor_GeneratesNewUserIDByDefault(t *testing.T) {
	var userIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userIDs = append(userIDs, gjson.GetBytes(body, "metadata.user_id").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for i := 0; i < 2; i++ {
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute call %d error: %v", i, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	if userIDs[0] == userIDs[1] {
		t.Fatalf("expected user_id to change when caching is not enabled, got identical values %q", userIDs[0])
	}
	if !helps.IsValidUserID(userIDs[0]) || !helps.IsValidUserID(userIDs[1]) {
		t.Fatalf("user_ids should be valid, got %q and %q", userIDs[0], userIDs[1])
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsEmptyClaudeStream(t *testing.T) {
	_, err := executeOpenAIChatCompletionThroughClaude(t, "")
	if err == nil {
		t.Fatal("Execute error = nil, want empty stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "empty stream response") {
		t.Fatalf("Execute error = %q, want empty stream response", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsClaudeErrorEvent(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want upstream error event")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("Execute error = %q, want upstream overloaded", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsIncompleteClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "ended before message completion") {
		t.Fatalf("Execute error = %q, want incomplete stream error", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamConvertsValidClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "msg_123" {
		t.Fatalf("response id = %q, want msg_123; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "model").String(); got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("response model = %q, want claude-3-5-sonnet-20241022", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3", got)
	}
}

func executeOpenAIChatCompletionThroughClaude(t *testing.T, upstreamBody string) (cliproxyexecutor.Response, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)

	return executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
}

func assertStatusErr(t *testing.T, err error, want int) {
	t.Helper()

	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", err)
	}
	if got := status.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func TestStripClaudeToolPrefixFromResponse_NestedToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"proxy_mcp__nia__manage_resource"}]}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")
	got := gjson.GetBytes(out, "content.0.content.0.tool_name").String()
	if got != "mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "mcp__nia__manage_resource")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReferenceWithStringContent(t *testing.T) {
	// tool_result.content can be a string - should not be processed
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"plain string result"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content").String()
	if got != "plain string result" {
		t.Fatalf("string content should remain unchanged = %q", got)
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"web_search"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "web_search" {
		t.Fatalf("built-in tool_reference should not be prefixed, got %q", got)
	}
}

func TestPrependToFirstUserMessage_KeepsToolResultBlocksFirst(t *testing.T) {
	payload := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"continue"}]}
	]}`)

	out := prependToFirstUserMessage(payload, "guidance")
	blocks := gjson.GetBytes(out, "messages.1.content")
	if got := blocks.Get("0.type").String(); got != "tool_result" {
		t.Fatalf("content.0.type = %q, want tool_result: %s", got, string(out))
	}
	last := blocks.Array()[len(blocks.Array())-1]
	if last.Get("type").String() != "text" || !strings.Contains(last.Get("text").String(), "guidance") {
		t.Fatalf("guidance block was not appended after leading tool_result blocks: %s", string(out))
	}
}

func TestPrependToFirstUserMessage_PrependsWhenNoLeadingToolResult(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := prependToFirstUserMessage(payload, "guidance")
	blocks := gjson.GetBytes(out, "messages.0.content")
	if got := blocks.Get("0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want text: %s", got, string(out))
	}
	if !strings.Contains(blocks.Get("0.text").String(), "guidance") {
		t.Fatalf("guidance was not prepended: %s", string(out))
	}
	if got := blocks.Get("1.text").String(); got != "hello" {
		t.Fatalf("content.1.text = %q, want hello: %s", got, string(out))
	}
}

func TestInjectToolsCacheControlSkipsDeferredTools(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		wantCacheIndex int
		wantCacheTTL   string
	}{
		{
			name: "trailing deferred tool",
			input: `{"tools":[
				{"name":"resident","defer_loading":false},
				{"name":"deferred","defer_loading":true}
			]}`,
			wantCacheIndex: 0,
		},
		{
			name: "multiple trailing deferred tools",
			input: `{"tools":[
				{"name":"resident"},
				{"name":"deferred_1","defer_loading":true},
				{"name":"deferred_2","defer_loading":true}
			]}`,
			wantCacheIndex: 0,
		},
		{
			name: "middle deferred tool",
			input: `{"tools":[
				{"name":"resident_1"},
				{"name":"deferred","defer_loading":true},
				{"name":"resident_2"}
			]}`,
			wantCacheIndex: 2,
		},
		{
			name: "all tools deferred",
			input: `{"tools":[
				{"name":"deferred_1","defer_loading":true},
				{"name":"deferred_2","defer_loading":true}
			]}`,
			wantCacheIndex: -1,
		},
		{
			name: "existing cache control",
			input: `{"tools":[
				{"name":"resident_1","cache_control":{"type":"ephemeral","ttl":"1h"}},
				{"name":"resident_2"}
			]}`,
			wantCacheIndex: 0,
			wantCacheTTL:   "1h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := injectToolsCacheControl([]byte(tt.input))
			tools := gjson.GetBytes(output, "tools").Array()
			cacheCount := 0

			for index, tool := range tools {
				cacheControl := tool.Get("cache_control")
				if cacheControl.Exists() {
					cacheCount++
					if index != tt.wantCacheIndex {
						t.Errorf("cache_control added to tool %d, want tool %d: %s", index, tt.wantCacheIndex, string(output))
					}
				}
				if tool.Get("defer_loading").Bool() && cacheControl.Exists() {
					t.Errorf("deferred tool %d must not have cache_control: %s", index, string(output))
				}
			}

			wantCacheCount := 1
			if tt.wantCacheIndex < 0 {
				wantCacheCount = 0
			}
			if cacheCount != wantCacheCount {
				t.Errorf("cache_control count = %d, want %d: %s", cacheCount, wantCacheCount, string(output))
			}
			if tt.wantCacheTTL != "" {
				path := fmt.Sprintf("tools.%d.cache_control.ttl", tt.wantCacheIndex)
				if got := gjson.GetBytes(output, path).String(); got != tt.wantCacheTTL {
					t.Errorf("cache_control TTL = %q, want %q: %s", got, tt.wantCacheTTL, string(output))
				}
			}
		})
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange(t *testing.T) {
	// Payload where no TTL normalization is needed (all blocks use 1h with no
	// preceding 5m block). The text intentionally contains HTML chars (<, >, &)
	// that json.Marshal would escape to \u003c etc., altering byte identity.
	payload := []byte(`{"tools":[{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"<system-reminder>foo & bar</system-reminder>","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeCacheControlTTL(payload)

	if !bytes.Equal(out, payload) {
		t.Fatalf("normalizeCacheControlTTL altered bytes when no change was needed.\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !gjson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !gjson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestClaudeExecutor_CountTokens_AppliesCacheControlGuards(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [
			{"type":"text","text":"s1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"text","text":"s2","cache_control":{"type":"ephemeral","ttl":"1h"}}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral","ttl":"1h"}}]}
		]
	}`)

	_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-haiku-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}

	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens request body to be captured")
	}
	if got := countCacheControls(seenBody); got > 4 {
		t.Fatalf("count_tokens body has %d cache_control blocks, want <= 4", got)
	}
	if hasTTLOrderingViolation(seenBody) {
		t.Fatalf("count_tokens body still has ttl ordering violations: %s", string(seenBody))
	}
}

func hasTTLOrderingViolation(payload []byte) bool {
	seen5m := false
	violates := false

	checkCC := func(cc gjson.Result) {
		if !cc.Exists() || violates {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl != "1h" {
			seen5m = true
			return
		}
		if seen5m {
			violates = true
		}
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			checkCC(tool.Get("cache_control"))
			return !violates
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			checkCC(item.Get("cache_control"))
			return !violates
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					checkCC(item.Get("cache_control"))
					return !violates
				})
			}
			return !violates
		})
	}

	return violates
}

func TestClaudeExecutor_Execute_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_ExecuteStream_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_CountTokens_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func testClaudeExecutorInvalidCompressedErrorBody(
	t *testing.T,
	invoke func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error,
) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not-a-valid-gzip-stream"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	err := invoke(executor, auth, payload)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to decode error response body") {
		t.Fatalf("expected decode failure message, got: %v", err)
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); !ok || statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected status code 400, got: %v", err)
	}
}

func TestEnsureModelMaxTokens_UsesRegisteredMaxCompletionTokens(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-max-completion-tokens-client"
	modelID := "test-claude-max-completion-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-max-completion-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d", got, 4096)
	}
}

func TestEnsureModelMaxTokens_DefaultsMissingValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:          modelID,
		Type:        "claude",
		OwnedBy:     "anthropic",
		Object:      "model",
		Created:     time.Now().Unix(),
		UserDefined: true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-default-max-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != defaultModelMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, defaultModelMaxTokens)
	}
}

func TestEnsureModelMaxTokens_PreservesExplicitValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-preserve-max-tokens-model","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
}

func TestEnsureModelMaxTokens_SkipsUnregisteredModel(t *testing.T) {
	input := []byte(`{"model":"test-claude-unregistered-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, "test-claude-unregistered-model")

	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset, got %s", gjson.GetBytes(out, "max_tokens").Raw)
	}
}

// TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding verifies that streaming
// requests use Accept-Encoding: identity so the upstream cannot respond with a
// compressed SSE body that would silently break the line scanner.
func TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "identity")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/event-stream")
	}
}

// TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding verifies that non-streaming
// requests keep the full accept-encoding to allow response compression (which
// decodeResponseBody handles correctly).
func TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotEncoding != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "gzip, deflate, br, zstd")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
}

// TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded verifies that a streaming
// HTTP 200 response with Content-Encoding: gzip is correctly decompressed before
// the line scanner runs, so SSE chunks are not silently dropped.
func TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected at least one chunk from gzip-encoded SSE body, got none (body was not decompressed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("expected SSE content in chunks, got: %q", combined.String())
	}
}

// TestDecodeResponseBody_MagicByteGzipNoHeader verifies that decodeResponseBody
// detects gzip-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteGzipNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(plaintext))
	_ = gz.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_MagicByteZstdNoHeader verifies that decodeResponseBody
// detects zstd-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteZstdNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	_, _ = enc.Write([]byte(plaintext))
	_ = enc.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_PlainTextNoHeader verifies that decodeResponseBody returns
// plain text untouched when Content-Encoding is absent and no magic bytes match.
func TestDecodeResponseBody_PlainTextNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"
	rc := io.NopCloser(strings.NewReader(plaintext))
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader verifies the full
// pipeline: when the upstream returns a gzip-compressed SSE body WITHOUT setting
// Content-Encoding (a misbehaving upstream), the magic-byte sniff in
// decodeResponseBody still decompresses it, so chunks reach the caller.
func TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected chunks from gzip body without Content-Encoding header, got none (magic-byte sniff failed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("unexpected chunk content: %q", combined.String())
	}
}

// TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader verifies that the
// error path (4xx) correctly decompresses a gzip body even when the upstream omits
// the Content-Encoding header.  This closes the gap left by PR #1771, which only
// fixed header-declared compression on the error path.
func TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

// TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader verifies
// the same for the streaming executor: 4xx gzip body without Content-Encoding is
// decoded and the error message is readable.
func TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"stream test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "stream test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

// TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity verifies that the
// streaming executor enforces Accept-Encoding: identity regardless of auth.Attributes override.
func TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity(t *testing.T) {
	var gotEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                "key-123",
		"base_url":               server.URL,
		"header:Accept-Encoding": "gzip, deflate, br, zstd",
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q; stream path must enforce identity regardless of auth.Attributes override", gotEncoding)
	}
}

func expectedClaudeCodeStaticPrompt() string {
	return strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
}

func expectedForwardedSystemReminder(text string) string {
	return fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)
}

// Test case 1: String system prompt is preserved by forwarding it to the first user message
func TestCheckSystemInstructionsWithMode_StringSystemPreserved(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	system := gjson.GetBytes(out, "system")
	if !system.IsArray() {
		t.Fatalf("system should be an array, got %s", system.Type)
	}

	blocks := system.Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}

	if !strings.HasPrefix(blocks[0].Get("text").String(), "x-anthropic-billing-header:") {
		t.Fatalf("blocks[0] should be billing header, got %q", blocks[0].Get("text").String())
	}
	if blocks[1].Get("text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("blocks[1] should be agent block, got %q", blocks[1].Get("text").String())
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if blocks[2].Get("cache_control").Exists() {
		t.Fatalf("blocks[2] should not have cache_control, got %s", blocks[2].Get("cache_control").Raw)
	}

	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("You are a helpful assistant.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded system prompt, got %q", got)
	}
}

// Test case 2: Strict mode keeps only the injected Claude Code system blocks
func TestCheckSystemInstructionsWithMode_StringSystemStrict(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, true)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("strict mode should produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("strict mode should not forward system prompt into messages, got %q", got)
	}
}

// Test case 3: Empty string system prompt does not alter the first user message
func TestCheckSystemInstructionsWithMode_EmptyStringSystemIgnored(t *testing.T) {
	payload := []byte(`{"system":"","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("empty string system should still produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("empty string system should not alter messages, got %q", got)
	}
}

// Test case 4: Array system prompt is forwarded to the first user message
func TestCheckSystemInstructionsWithMode_ArraySystemStillWorks(t *testing.T) {
	payload := []byte(`{"system":[{"type":"text","text":"Be concise."}],"messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("Be concise.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded array system prompt, got %q", got)
	}
}

// Test case 5: Special characters in string system prompt survive forwarding
func TestCheckSystemInstructionsWithMode_StringWithSpecialChars(t *testing.T) {
	payload := []byte(`{"system":"Use <xml> tags & \"quotes\" in output.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder(`Use <xml> tags & "quotes" in output.`)+"hi" {
		t.Fatalf("forwarded system prompt text mangled, got %q", got)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningDisabledByDefaultKeepsLegacyHeader(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}

	billingHeader := gjson.GetBytes(seenBody, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		t.Fatalf("system.0.text = %q, want billing header", billingHeader)
	}
	if strings.Contains(billingHeader, "cch=00000;") {
		t.Fatalf("legacy mode should not forward cch placeholder, got %q", billingHeader)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningOptInSkipsUnapprovedOrigin(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:                 "key-123",
			BaseURL:                server.URL,
			ExperimentalCCHSigning: true,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	const messageText = "please keep literal cch=00000 in this message"
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"please keep literal cch=00000 in this message"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if got := gjson.GetBytes(seenBody, "messages.0.content.0.text").String(); got != messageText {
		t.Fatalf("message text = %q, want %q", got, messageText)
	}

	billingHeader := gjson.GetBytes(seenBody, "system.0.text").String()
	if strings.Contains(billingHeader, "cch=") {
		t.Fatalf("unapproved origin should not receive cch signing, got %q", billingHeader)
	}
}

func TestExperimentalCCHSigningRequiresApprovedOrigin(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{
		APIKey:                 "key-123",
		BaseURL:                "https://api.anthropic.com",
		ExperimentalCCHSigning: true,
	}}}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": "https://api.anthropic.com",
	}}

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "first party", origin: "https://api.anthropic.com/v1/messages", want: true},
		{name: "explicit default port", origin: "https://api.anthropic.com:443/v1/messages", want: true},
		{name: "gateway", origin: "https://gateway.example/v1/messages"},
		{name: "http first party", origin: "http://api.anthropic.com/v1/messages"},
		{name: "lookalike host", origin: "https://api.anthropic.com.evil.example/v1/messages"},
		{name: "empty origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := experimentalCCHSigningEnabled(cfg, auth, tt.origin); got != tt.want {
				t.Fatalf("experimentalCCHSigningEnabled(%q) = %t, want %t", tt.origin, got, tt.want)
			}
		})
	}
}

func TestApplyCloaking_PreservesConfiguredStrictModeAndSensitiveWordsWhenModeOmitted(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "key-123",
			Cloak: &config.CloakConfig{
				StrictMode:     true,
				SensitiveWords: []string{"proxy"},
			},
		}},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123"}}
	payload := []byte(`{"system":"proxy rules","messages":[{"role":"user","content":[{"type":"text","text":"proxy access"}]}]}`)

	out, cloaked := applyCloaking(context.Background(), cfg, auth, payload, "claude-3-5-sonnet-20241022", "key-123", "")
	if !cloaked {
		t.Fatal("expected applyCloaking to report cloaked for a strict-mode cloak config")
	}

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected strict mode to keep the 3 injected Claude Code system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("strict mode should not prepend a forwarded system reminder block, got %d content blocks", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "\u200B") {
		t.Fatalf("expected configured sensitive word obfuscation to apply, got %q", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AdaptiveCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_EnabledCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"thinking":{"type":"enabled","budget_tokens":2048}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_NoThinkingLeavesTemperatureAlone(t *testing.T) {
	payload := []byte(`{"temperature":0,"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AfterForcedToolChoiceKeepsOriginalTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tool_choice":{"type":"any"}}`)
	out := disableThinkingIfToolChoiceForced(payload)
	out = normalizeClaudeTemperatureForThinking(out)

	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed when tool_choice forces tool use")
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestRemapOAuthToolNames_TitleCase_NoReverseNeeded(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body, context.Background())
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed, err := reverseRemapOAuthToolNames(resp, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNames error: %v", err)
	}
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "Bash")
	}
}

func TestRemapOAuthToolNames_Lowercase_ReverseApplied(t *testing.T) {
	body := []byte(`{"tools":[{"name":"bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body, context.Background())
	if reverseMap["Bash"] != "bash" {
		t.Fatalf("reverseMap = %v, want entry Bash->bash", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed, err := reverseRemapOAuthToolNames(resp, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNames error: %v", err)
	}
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "bash")
	}
}

// TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed is the regression
// test for a case where a single request contains both a TitleCase tool (which
// must pass through unchanged) and a lowercase tool that we forward-rename.
// Before the fix, triggering ANY forward rename caused the reverse pass to
// lowercase every TitleCase tool in the response using a global reverse map,
// corrupting tool names the client originally sent in TitleCase (for example,
// a client's `Bash`, which its registry lookup cannot find as `bash`).
func TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`]}`)

	out, reverseMap := remapOAuthToolNames(body, context.Background())

	// Forward: TitleCase `Bash` is not a forward-map key, must pass through.
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q (TitleCase tool must not be renamed)", got, "Bash")
	}
	// Forward: `glob` is a forward-map key, upstream sees `Glob`.
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "Glob" {
		t.Fatalf("tools.1.name = %q, want %q", got, "Glob")
	}

	// Reverse map records ONLY the rename that happened.
	if len(reverseMap) != 1 || reverseMap["Glob"] != "glob" {
		t.Fatalf("reverseMap = %v, want {Glob:glob}", reverseMap)
	}

	// Upstream responds with a `Bash` tool_use. Since we never renamed `Bash`,
	// reverseRemap MUST leave it alone.
	bashResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed, err := reverseRemapOAuthToolNames(bashResp, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNames error: %v", err)
	}
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q (Bash must be preserved; was never forward-renamed)", got, "Bash")
	}

	// Upstream responds with a `Glob` tool_use. Since we renamed `glob`→`Glob`,
	// reverseRemap MUST restore the original `glob`.
	globResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_02","name":"Glob","input":{"filePattern":"**/*.go"}}]}`)
	reversed, err = reverseRemapOAuthToolNames(globResp, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNames error: %v", err)
	}
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "glob" {
		t.Fatalf("content.0.name = %q, want %q (Glob must be restored to client's original `glob`)", got, "glob")
	}
}

// TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap guards the
// SSE streaming code path against the same mixed-case bug.
func TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	// Bash block was never renamed, must pass through as-is.
	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}`)
	out, err := reverseRemapOAuthToolNamesFromStreamLine(bashLine, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine error: %v", err)
	}
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	// Glob block IS in the reverseMap, must be restored to `glob`.
	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"Glob","input":{}}}`)
	out, err = reverseRemapOAuthToolNamesFromStreamLine(globLine, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNamesFromStreamLine error: %v", err)
	}
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

func TestPrepareClaudeOAuthToolNamesForUpstream_MixedCaseWithPrefix(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`],"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"glob","input":{}}` +
		`]}]}`)

	out, reverseMap := prepareClaudeOAuthToolNamesForUpstream(context.Background(), body, "proxy_", false)

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Bash")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Glob" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Glob")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Bash" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Bash")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Glob" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Glob")
	}
	if len(reverseMap) != 1 || reverseMap["Glob"] != "glob" {
		t.Fatalf("reverseMap = %v, want {Glob:glob}", reverseMap)
	}
}

func TestRestoreClaudeOAuthToolNamesFromResponse_MixedCaseWithPrefix(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}
	resp := []byte(`{"content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"proxy_Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"proxy_Glob","input":{}}` +
		`]}`)

	out, err := restoreClaudeOAuthToolNamesFromResponse(resp, "proxy_", false, reverseMap)
	if err != nil {
		t.Fatalf("restoreClaudeOAuthToolNamesFromResponse error: %v", err)
	}

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "Bash")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "glob" {
		t.Fatalf("content.1.name = %q, want %q", got, "glob")
	}
}

func TestRestoreClaudeOAuthToolNamesFromStreamLine_MixedCaseWithPrefix(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"proxy_Bash","input":{}}}`)
	out, err := restoreClaudeOAuthToolNamesFromStreamLine(bashLine, "proxy_", false, reverseMap)
	if err != nil {
		t.Fatalf("restoreClaudeOAuthToolNamesFromStreamLine error: %v", err)
	}
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"proxy_Glob","input":{}}}`)
	out, err = restoreClaudeOAuthToolNamesFromStreamLine(globLine, "proxy_", false, reverseMap)
	if err != nil {
		t.Fatalf("restoreClaudeOAuthToolNamesFromStreamLine error: %v", err)
	}
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

func TestRemapOAuthToolNames_ThirdPartyToolsAliased(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"search_web","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}` +
		`],"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"search_web","input":{"q":"x"}}` +
		`]}]}`)

	out, reverseMap := remapOAuthToolNames(body, context.Background())

	// Third-party tools get a Claude Code-style MCP word alias, not a prefix.
	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) || !strings.HasSuffix(alias, "_search_web") {
		t.Fatalf("tools.0.name = %q, want MCP-style alias ending in _search_web", alias)
	}
	// Historical references resolve through the same request-local map.
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != alias {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, alias)
	}
	if reverseMap[alias] != "search_web" {
		t.Fatalf("reverseMap[%q] = %q, want %q", alias, reverseMap[alias], "search_web")
	}

	// Reverse restore returns the original name to the client.
	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"` + alias + `","input":{"q":"x"}}]}`)
	reversed, err := reverseRemapOAuthToolNames(resp, reverseMap)
	if err != nil {
		t.Fatalf("reverseRemapOAuthToolNames error: %v", err)
	}
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "search_web" {
		t.Fatalf("content.0.name = %q, want %q", got, "search_web")
	}
}

func TestApplyClaudeHeaders_BetaAssemblyPerRequest(t *testing.T) {
	apiKeyAuth := &cliproxyauth.Auth{ID: "auth-1", Attributes: map[string]string{"api_key": "key-1"}}
	// advanced-tool-use-2025-11-20 requires a real advanced tool-use feature
	// (tool search, defer_loading, input_examples, allowed_callers) in
	// 2.1.280 — a plain tools array alone does not emit it (upstream d7052c96af78).
	body := []byte(`{"model":"claude-opus-5","tools":[{"name":"search_web","input_schema":{"type":"object"},"defer_loading":true}]}`)

	// API-key mode with tools: conditional betas present, OAuth betas absent,
	// and the old unconditional token-efficient-tools beta is gone (2.1.220
	// never sends it).
	req := newClaudeHeaderTestRequest(t, nil)
	applyClaudeHeaders(req, apiKeyAuth, "key-1", false, nil, body, nil)
	got := req.Header.Get("Anthropic-Beta")
	for _, want := range []string{"claude-code-20250219", "advanced-tool-use-2025-11-20", "mid-conversation-system-2026-04-07", "effort-2025-11-24"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Anthropic-Beta = %q, missing %q", got, want)
		}
	}
	for _, absent := range []string{"oauth-2025-04-20", "extended-cache-ttl-2025-04-11", "token-efficient-tools-2026-03-28", "context-1m-2025-08-07"} {
		if strings.Contains(got, absent) {
			t.Fatalf("Anthropic-Beta = %q, must not contain %q", got, absent)
		}
	}

	// OAuth path: oauth-2025-04-20 present and extended-cache-ttl is last.
	oauthAuth := &cliproxyauth.Auth{ID: "auth-2", Attributes: map[string]string{"api_key": "key-2"}}
	oauthReq := newClaudeHeaderTestRequest(t, nil)
	applyClaudeHeaders(oauthReq, oauthAuth, "sk-ant-oat-2", false, nil, body, nil)
	if got := oauthReq.Header.Get("Anthropic-Beta"); !strings.HasSuffix(got, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("OAuth Anthropic-Beta must end with extended-cache-ttl, got %q", got)
	}

	// fast-mode follows the body's speed field (extra inputs are rejected
	// without the beta).
	fastReq := newClaudeHeaderTestRequest(t, nil)
	applyClaudeHeaders(fastReq, apiKeyAuth, "key-1", false, nil, []byte(`{"model":"claude-sonnet-4-6","speed":"fast"}`), nil)
	if got := fastReq.Header.Get("Anthropic-Beta"); !strings.Contains(got, "fast-mode-2026-02-01") {
		t.Fatalf("Anthropic-Beta = %q, missing fast-mode", got)
	}

	// count_tokens uses the fixed small profile and omits X-Stainless-Timeout.
	ctReq := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens", nil)
	applyClaudeHeaders(ctReq, apiKeyAuth, "key-1", false, nil, []byte(`{"model":"claude-sonnet-4-6"}`), nil)
	if got := ctReq.Header.Get("Anthropic-Beta"); got != "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01" {
		t.Fatalf("count_tokens Anthropic-Beta = %q, want fixed profile", got)
	}
	if ctReq.Header.Get("X-Stainless-Timeout") != "" {
		t.Fatalf("count_tokens must omit X-Stainless-Timeout, got %q", ctReq.Header.Get("X-Stainless-Timeout"))
	}

	// Known (managed) caller betas land at captured positions; unmanaged caller
	// betas are newer-client features and forwarded verbatim so their features
	// keep working (upstream #5738, bd584a752329).
	callerIncoming := http.Header{"Anthropic-Beta": []string{"context-1m-2025-08-07,my-custom-beta"}}
	callerReq := newClaudeHeaderTestRequest(t, callerIncoming)
	applyClaudeHeaders(callerReq, apiKeyAuth, "key-1", false, nil, body, nil)
	got = callerReq.Header.Get("Anthropic-Beta")
	if !strings.Contains(got, "context-1m-2025-08-07") {
		t.Fatalf("Anthropic-Beta = %q, known caller beta context-1m must be placed", got)
	}
	if !strings.HasSuffix(got, ",my-custom-beta") {
		t.Fatalf("Anthropic-Beta = %q, unmanaged caller beta must be forwarded on Anthropic", got)
	}
}

func TestDecodeResponseBody_StackedEncodings(t *testing.T) {
	compressTwice := func(b []byte) []byte {
		var inner, middle, outer bytes.Buffer
		w1 := gzip.NewWriter(&inner)
		_, _ = w1.Write(b)
		_ = w1.Close()
		w2 := gzip.NewWriter(&middle)
		_, _ = w2.Write(inner.Bytes())
		_ = w2.Close()
		w3 := gzip.NewWriter(&outer)
		_, _ = w3.Write(middle.Bytes())
		_ = w3.Close()
		return outer.Bytes()
	}

	payload := []byte(`{"content":"stacked encodings decode"}`)
	stacked := compressTwice(payload)
	decoded, err := decodeResponseBody(io.NopCloser(bytes.NewReader(stacked)), "gzip, gzip, gzip")
	if err != nil {
		t.Fatalf("decodeResponseBody: %v", err)
	}
	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = decoded.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded = %q, want %q", got, payload)
	}
}

func TestDecodeResponseBody_ZlibWrappedDeflate(t *testing.T) {
	payload := []byte(`{"content":"zlib deflate"}`)
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write(payload)
	_ = w.Close()

	decoded, err := decodeResponseBody(io.NopCloser(bytes.NewReader(buf.Bytes())), "deflate")
	if err != nil {
		t.Fatalf("decodeResponseBody: %v", err)
	}
	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = decoded.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded = %q, want %q", got, payload)
	}
}

// --- Hybrid passthrough MCP tool restore (upstream 22392c537d95) ---

// TestRemapOAuthToolNames_RecordsPassthroughMCPTools covers the request-side
// half of the hybrid fix: caller-owned mcp__ tools are forwarded untouched but
// recorded into the reverse map so the resolver can recover hybrid names.
func TestRemapOAuthToolNames_RecordsPassthroughMCPTools(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"search_web","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}},` +
		`{"name":"mcp__acme__link_pull_request","input_schema":{"type":"object"}}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)

	out, reverseMap := remapOAuthToolNames(body, context.Background())

	// Third-party tool is aliased to a request-local MCP name.
	alias := gjson.GetBytes(out, "tools.0.name").String()
	if !helps.IsClaudeMCPToolName(alias) || !strings.HasSuffix(alias, "_search_web") {
		t.Fatalf("tools.0.name = %q, want MCP-style alias ending in _search_web", alias)
	}
	// Caller-owned MCP tool is forwarded untouched.
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "mcp__acme__link_pull_request" {
		t.Fatalf("tools.1.name = %q, want passthrough MCP name untouched", got)
	}
	// ...but recorded so the response resolver can recognize the caller's server.
	if got := reverseMap["mcp__acme__link_pull_request"]; got != "mcp__acme__link_pull_request" {
		t.Fatalf("reverseMap = %v, want passthrough identity entry", reverseMap)
	}
}

// TestRemapOAuthToolNames_PassthroughSkippedWhenNothingAliased mirrors upstream
// recordPassthroughMCPTools: when no client tool was aliased the reverse map
// stays free of passthrough entries and the restore path is a no-op.
func TestRemapOAuthToolNames_PassthroughSkippedWhenNothingAliased(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"mcp__acme__link_pull_request","input_schema":{"type":"object"}}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)

	_, reverseMap := remapOAuthToolNames(body, context.Background())
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty (nothing aliased)", reverseMap)
	}
}

// TestRestoreClaudeOAuthToolNames_HybridPassthroughMCP ports upstream
// TestReverseRemapOAuthToolNamesRestoresHybridPassthroughMCPTools: the model
// may emit a hybrid name that mixes the virtual server prefix with the
// caller's real MCP tool name; both drifted shapes must restore exactly.
func TestRestoreClaudeOAuthToolNames_HybridPassthroughMCP(t *testing.T) {
	const virtual = "mcp__ripple_middle__"
	reverseMap := map[string]string{
		virtual + "blanket_Bash":       "Bash",
		virtual + "brand_Read":         "Read",
		"mcp__acme__link_pull_request": "mcp__acme__link_pull_request",
		"mcp__acme__list_threads":      "mcp__acme__list_threads",
	}

	testCases := []struct {
		name       string
		hybridName string
		wantName   string
	}{
		{
			name:       "model replaced server with virtual prefix",
			hybridName: "mcp__ripple_middle__link_pull_request",
			wantName:   "mcp__acme__link_pull_request",
		},
		{
			name:       "model prepended virtual prefix to full passthrough name",
			hybridName: "mcp__ripple_middle__acme__link_pull_request",
			wantName:   "mcp__acme__link_pull_request",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Non-stream response
			resp := []byte(fmt.Sprintf(`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`, tc.hybridName))
			restored, err := restoreClaudeOAuthToolNamesFromResponse(resp, "", false, reverseMap)
			if err != nil {
				t.Fatalf("restoreClaudeOAuthToolNamesFromResponse() error = %v, want %q", err, tc.wantName)
			}
			if got := gjson.GetBytes(restored, "content.0.name").String(); got != tc.wantName {
				t.Fatalf("content.0.name = %q, want %q", got, tc.wantName)
			}

			// Streaming response
			line := []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`, tc.hybridName))
			restoredLine, errStream := restoreClaudeOAuthToolNamesFromStreamLine(line, "", false, reverseMap)
			if errStream != nil {
				t.Fatalf("restoreClaudeOAuthToolNamesFromStreamLine() error = %v, want %q", errStream, tc.wantName)
			}
			if got := gjson.GetBytes(helps.JSONPayload(restoredLine), "content_block.name").String(); got != tc.wantName {
				t.Fatalf("content_block.name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestRestoreClaudeOAuthToolNames_HybridPassthroughPrecedenceAndAmbiguity ports
// upstream TestReverseRemapOAuthToolNamesHybridPassthroughPrecedenceAndAmbiguity:
// client-tool alias matches win over passthrough recovery, and a passthrough
// suffix matching multiple declared tools is rejected as a request-scoped error.
func TestRestoreClaudeOAuthToolNames_HybridPassthroughPrecedenceAndAmbiguity(t *testing.T) {
	const virtual = "mcp__ripple_middle__"

	t.Run("client tool takes precedence over passthrough with same suffix", func(t *testing.T) {
		reverseMap := map[string]string{
			virtual + "blanket_Bash": "Bash",
			"mcp__shell__Bash":       "mcp__shell__Bash",
		}
		// mcp__<virtual>__Bash is a semantic suffix match for Bash, should restore
		// to Bash, NOT mcp__shell__Bash.
		resp := []byte(fmt.Sprintf(`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`, virtual+"Bash"))
		restored, err := restoreClaudeOAuthToolNamesFromResponse(resp, "", false, reverseMap)
		if err != nil {
			t.Fatalf("restoreClaudeOAuthToolNamesFromResponse() error = %v", err)
		}
		if got := gjson.GetBytes(restored, "content.0.name").String(); got != "Bash" {
			t.Fatalf("content.0.name = %q, want %q", got, "Bash")
		}
	})

	t.Run("ambiguous passthrough tool rejected", func(t *testing.T) {
		reverseMap := map[string]string{
			virtual + "blanket_other": "other",
			"mcp__srv1__query":        "mcp__srv1__query",
			"mcp__srv2__query":        "mcp__srv2__query",
		}
		// mcp__<virtual>__query matches both srv1 and srv2; cannot disambiguate.
		resp := []byte(fmt.Sprintf(`{"content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}]}`, virtual+"query"))
		_, err := restoreClaudeOAuthToolNamesFromResponse(resp, "", false, reverseMap)
		if err == nil {
			t.Fatal("restoreClaudeOAuthToolNamesFromResponse() expected error for ambiguous passthrough, got nil")
		}
		requestScoped, ok := err.(interface{ IsRequestScoped() bool })
		if !ok || !requestScoped.IsRequestScoped() {
			t.Fatalf("ambiguous restore error should be request-scoped, got %T: %v", err, err)
		}
	})
}

// --- Advisor history detection (upstream 7c2f6ce0 end-state + f86a33f72175) ---

// TestCheckSystemInstructionsWithMode_ServerToolUseAdvisorKeepsSystemTopLevel:
// a real Anthropic advisor invocation is a server_tool_use block named
// "advisor". Its encrypted result is bound to the conversation layout, so the
// caller's system prompt must stay in top-level system and messages[] must not
// be spliced.
func TestCheckSystemInstructionsWithMode_ServerToolUseAdvisorKeepsSystemTopLevel(t *testing.T) {
	payload := []byte(`{
		"model": "claude-opus-5",
		"system": [{"type": "text", "text": "caller guidance"}],
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "server_tool_use", "id": "srvtoolu_1", "name": "advisor", "input": {"query": "help"}}
			]},
			{"role": "user", "content": [
				{"type": "advisor_tool_result", "tool_use_id": "srvtoolu_1", "content": [{"type": "text", "text": "advice"}]}
			]}
		]
	}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 4 {
		t.Fatalf("system blocks count = %d, want 4 (cloak blocks + preserved caller block): %s", len(blocks), out)
	}
	if got := blocks[3].Get("text").String(); got != "caller guidance" {
		t.Fatalf("system[3] = %q, want caller guidance preserved at top level", got)
	}
	// messages[] must be untouched: no system-reminder splice.
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Fatalf("messages[0].content should be untouched, got %q", got)
	}
}

// TestCheckSystemInstructionsWithMode_ClientToolUseNamedAdvisorRelocatesSystemPrompt
// is the f86a33f72175 regression test: a client-side tool_use merely named
// "advisor" is NOT an Anthropic advisor invocation, so the normal relocation
// path still applies.
func TestCheckSystemInstructionsWithMode_ClientToolUseNamedAdvisorRelocatesSystemPrompt(t *testing.T) {
	payload := []byte(`{
		"model": "claude-opus-5",
		"system": [{"type": "text", "text": "caller guidance"}],
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_client1", "name": "advisor", "input": {"query": "help"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_client1", "content": "client advice text"}
			]}
		]
	}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("system blocks count = %d, want 3 (caller prompt must be relocated): %s", len(blocks), out)
	}
	for i, b := range blocks {
		if strings.Contains(b.Get("text").String(), "caller guidance") {
			t.Fatalf("system[%d] unexpectedly contains caller guidance: %s", i, b.Raw)
		}
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("caller guidance")+"hello" {
		t.Fatalf("messages[0].content should include forwarded system prompt, got %q", got)
	}
}

// TestCheckSystemInstructionsWithMode_AdvisorRedactedResultKeepsSystemTopLevel
// covers advisor_redacted_result nested inside a tool_result, which is also
// layout-bound (upstream 7c2f6ce0).
func TestCheckSystemInstructionsWithMode_AdvisorRedactedResultKeepsSystemTopLevel(t *testing.T) {
	payload := []byte(`{
		"model": "claude-opus-5",
		"system": "caller guidance",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "srvtoolu_1", "content": [
					{"type": "advisor_redacted_result", "data": "enc"}
				]}
			]}
		]
	}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 4 {
		t.Fatalf("system blocks count = %d, want 4 (cloak blocks + preserved caller block): %s", len(blocks), out)
	}
	if got := blocks[3].Get("text").String(); got != "caller guidance" {
		t.Fatalf("system[3] = %q, want caller guidance preserved at top level", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Fatalf("messages[0].content should be untouched, got %q", got)
	}
}

// TestClaudeHistoryHasAdvisorCallOrResult exercises the detector directly,
// including the f86a33f72175 restriction: client tool_use blocks named
// "advisor" do not count.
func TestClaudeHistoryHasAdvisorCallOrResult(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "server_tool_use advisor",
			payload: `{"messages":[{"role":"assistant","content":[{"type":"server_tool_use","name":"advisor","id":"s1"}]}]}`,
			want:    true,
		},
		{
			name:    "client tool_use advisor is ignored",
			payload: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"advisor","id":"t1"}]}]}`,
			want:    false,
		},
		{
			name:    "advisor_tool_result",
			payload: `{"messages":[{"role":"user","content":[{"type":"advisor_tool_result","tool_use_id":"s1"}]}]}`,
			want:    true,
		},
		{
			name:    "advisor_redacted_result nested in tool_result",
			payload: `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"advisor_redacted_result"}]}]}]}`,
			want:    true,
		},
		{
			name:    "server_tool_use named something else",
			payload: `{"messages":[{"role":"assistant","content":[{"type":"server_tool_use","name":"web_search","id":"s1"}]}]}`,
			want:    false,
		},
		{
			name:    "no advisor blocks",
			payload: `{"messages":[{"role":"user","content":"hi"}]}`,
			want:    false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeHistoryHasAdvisorCallOrResult([]byte(tc.payload)); got != tc.want {
				t.Fatalf("claudeHistoryHasAdvisorCallOrResult = %v, want %v", got, tc.want)
			}
		})
	}
}
func TestRemapOAuthToolNames_MidConversationToolChanges(t *testing.T) {
	const tools = `[` +
		`{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{}}},` +
		`{"name":"lookup_notes","description":"Look up notes","input_schema":{"type":"object","properties":{}},"defer_loading":true},` +
		`{"type":"web_search_20250305","name":"web_search"},` +
		`{"name":"mcp__context7__query-docs","input_schema":{"type":"object"}}` +
		`]`

	type nameCheck struct {
		path     string
		original string
		aliased  bool
	}
	tests := []struct {
		name     string
		messages string
		checks   []nameCheck
	}{
		{
			name:     "tool_addition reference to declared tool",
			messages: `[{"role":"user","content":"Reply with exactly: ok"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"lookup_notes"}}]}]`,
			checks:   []nameCheck{{path: "messages.1.content.0.tool.name", original: "lookup_notes", aliased: true}},
		},
		{
			name:     "tool_removal reference to declared tool",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"read_file"}}]}]`,
			checks:   []nameCheck{{path: "messages.1.content.0.tool.name", original: "read_file", aliased: true}},
		},
		{
			name:     "mixed with text and tool_use history",
			messages: `[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},{"role":"system","content":[{"type":"text","text":"Tools changed."},{"type":"tool_removal","tool":{"type":"tool_reference","name":"read_file"}},{"type":"tool_addition","tool":{"type":"tool_reference","name":"lookup_notes"}}]}]`,
			checks: []nameCheck{
				{path: "messages.1.content.0.name", original: "read_file", aliased: true},
				{path: "messages.3.content.0.text", original: "Tools changed."},
				{path: "messages.3.content.1.tool.name", original: "read_file", aliased: true},
				{path: "messages.3.content.2.tool.name", original: "lookup_notes", aliased: true},
			},
		},
		{
			name:     "undeclared server and caller MCP names unchanged",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"Bash"}},{"type":"tool_addition","tool":{"type":"tool_reference","name":"web_search"}},{"type":"tool_removal","tool":{"type":"tool_reference","name":"mcp__context7__query-docs"}}]}]`,
			checks: []nameCheck{
				{path: "messages.1.content.0.tool.name", original: "Bash"},
				{path: "messages.1.content.1.tool.name", original: "web_search"},
				{path: "messages.1.content.2.tool.name", original: "mcp__context7__query-docs"},
			},
		},
		{
			name:     "MCP connector references unchanged",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"mcp_tool_reference","server_name":"docs","name":"read_file"}},{"type":"tool_removal","tool":{"type":"mcp_toolset_reference","server_name":"read_file"}}]}]`,
			checks: []nameCheck{
				{path: "messages.1.content.0.tool.name", original: "read_file"},
				{path: "messages.1.content.1.tool.server_name", original: "read_file"},
			},
		},
		{
			name:     "inline definition redefining declared tool",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"lookup_notes","description":"v2","input_schema":{"type":"object"}}}}]}]`,
			checks:   []nameCheck{{path: "messages.1.content.0.tool.definition.name", original: "lookup_notes", aliased: true}},
		},
		{
			name:     "inline definitions of undeclared or server tools unchanged",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db_query","input_schema":{"type":"object"}}}},{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"web_search_20260209","name":"read_file"}}},{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar"}}}]}]`,
			checks: []nameCheck{
				{path: "messages.1.content.0.tool.definition.name", original: "db_query"},
				{path: "messages.1.content.1.tool.definition.name", original: "read_file"},
				{path: "messages.1.content.2.tool.definition.mcp_server_name", original: "calendar"},
			},
		},
		{
			name:     "malformed tool changes ignored",
			messages: `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition"},{"type":"tool_addition","tool":"lookup_notes"},{"type":"tool_addition","tool":{"name":"lookup_notes"}},{"type":"tool_addition","tool":{"type":"tool_reference","tool_name":"lookup_notes"}},{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"lookup_notes"}}},{"type":"tool_addition","tool":{"type":"tool_reference","name":""}}]}]`,
			checks: []nameCheck{
				{path: "messages.1.content.1.tool", original: "lookup_notes"},
				{path: "messages.1.content.2.tool.name", original: "lookup_notes"},
				{path: "messages.1.content.3.tool.tool_name", original: "lookup_notes"},
				{path: "messages.1.content.4.tool.definition.name", original: "lookup_notes"},
				{path: "messages.1.content.5.tool.name", original: ""},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"tools":` + tools + `,"messages":` + test.messages + `}`)
			remappedBody, reverseMap := remapOAuthToolNames(body, context.Background())

			for _, variant := range []struct {
				name       string
				body       []byte
				reverseMap map[string]string
			}{
				{name: "remap", body: remappedBody, reverseMap: reverseMap},
			} {
				declared := make(map[string]string)
				gjson.GetBytes(variant.body, "tools").ForEach(func(index, tool gjson.Result) bool {
					original := gjson.Get(tools, fmt.Sprintf("%d.name", index.Int())).String()
					declared[original] = tool.Get("name").String()
					return true
				})
				for _, check := range test.checks {
					want := check.original
					if check.aliased {
						want = declared[check.original]
						if want == check.original || !helps.IsClaudeMCPToolName(want) {
							t.Fatalf("%s: tools[] name for %q = %q, want an MCP alias", variant.name, check.original, want)
						}
						if got := variant.reverseMap[want]; got != check.original {
							t.Fatalf("%s: reverseMap[%q] = %q, want %q", variant.name, want, got, check.original)
						}
					}
					if got := gjson.GetBytes(variant.body, check.path).String(); got != want {
						t.Fatalf("%s: %s = %q, want %q", variant.name, check.path, got, want)
					}
				}
				if got := gjson.GetBytes(variant.body, "tools.2.name").String(); got != "web_search" {
					t.Fatalf("%s: server tool name = %q, want web_search", variant.name, got)
				}
				if got := gjson.GetBytes(variant.body, "tools.3.name").String(); got != "mcp__context7__query-docs" {
					t.Fatalf("%s: caller MCP tool name = %q, want unchanged", variant.name, got)
				}
			}
		})
	}
}

// preservedThinkingTestSignature is a syntactically valid Claude thinking
// signature sample that survives signature sanitization, so executor-level
// tests can exercise replayed-history handling without a real upstream.
const preservedThinkingTestSignature = "CAISqwIKiAEIEBgCKkBHRlRBsNiptQUWfPoOhuQKwi5LnncZVO9bB5jqOs76D7uBtgktML0zqJtNmLHXHHcgD6lk4MQu4QBXzFd1lbC3Mg5jbGF1ZGUtZmFibGUtNTgBQgh0aGlua2luZ1okZDk3NDM5NzUtNGJiMC00OTM2LTllMjgtZDViMGQyMWJkYzQ4EgxCGh+XVFFFeySAjtAaDL/A1LltGu6MMJ+eXSIwsN0oBpDrqLv22UBfkMnTotnIbkvkOyb9xZHgigG6OZVHaI3gThm+maLKmgO5PrFLKlDFYp+YZksy/wKwszJlnLTPzAK+NUlfzagOE1ymtZTXhAYK260XyFYmg/te/C231+Fr/hoX+EJoUBnrn0gD7hqMISOT+TaFEuOXYsN517GfaxgB"

func preservedThinkingReplayPayload(model string) []byte {
	return []byte(`{
		"model": "` + model + `",
		"thinking": {"type": "adaptive"},
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"earlier reasoning","signature":"` + preservedThinkingTestSignature + `"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"continue"}]}
		]
	}`)
}

func runClaudeExecutorCapture(t *testing.T, cfg *config.Config, cloakMode string, stream bool, model string, payload []byte) ([]byte, http.Header) {
	t.Helper()
	var seenBody []byte
	var seenHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		seenHeaders = r.Header.Clone()
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"ok","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	if cfg == nil {
		cfg = &config.Config{}
	}
	attrs := map[string]string{"api_key": "key-preserved-thinking", "base_url": server.URL}
	if cloakMode != "" {
		attrs["cloak_mode"] = cloakMode
	}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: attrs}
	req := cliproxyexecutor.Request{Model: model, Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	if stream {
		result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
			}
		}
	} else {
		if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	return seenBody, seenHeaders
}

func TestClaudeExecutor_PreservedThinkingReplayInjectsDropBlock(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			seenBody, seenHeaders := runClaudeExecutorCapture(t, nil, "", false, model, preservedThinkingReplayPayload(model))
			if got := gjson.GetBytes(seenBody, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
				t.Fatalf("thinking.block_binding.prefix_mismatch_behavior = %q, want drop_block; body=%s", got, seenBody)
			}
			if betas := seenHeaders.Get("Anthropic-Beta"); !strings.Contains(betas, claudeThinkingBindingBeta) {
				t.Fatalf("Anthropic-Beta = %q, want %s", betas, claudeThinkingBindingBeta)
			}
			if !strings.Contains(string(seenBody), preservedThinkingTestSignature) {
				t.Fatalf("replayed thinking signature must survive injection: %s", seenBody)
			}
		})
	}
}

func TestClaudeExecutor_PreservedThinkingReplayInjectsDropBlockOnStream(t *testing.T) {
	seenBody, seenHeaders := runClaudeExecutorCapture(t, nil, "", true, "claude-haiku-5-5", preservedThinkingReplayPayload("claude-haiku-5-5"))
	if got := gjson.GetBytes(seenBody, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
		t.Fatalf("stream thinking.block_binding.prefix_mismatch_behavior = %q, want drop_block; body=%s", got, seenBody)
	}
	if betas := seenHeaders.Get("Anthropic-Beta"); !strings.Contains(betas, claudeThinkingBindingBeta) {
		t.Fatalf("stream Anthropic-Beta = %q, want %s", betas, claudeThinkingBindingBeta)
	}
}

func TestClaudeExecutor_PreservedThinkingInjectionSkipsNonPreservedModel(t *testing.T) {
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-opus-5", preservedThinkingReplayPayload("claude-opus-5"))
	if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("non-preserved-thinking model must not gain block_binding: %s", seenBody)
	}
}

func TestClaudeExecutor_PreservedThinkingRequiresSignedBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{
			name:    "explicit adaptive thinking",
			payload: `{"model":"claude-haiku-5-5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:    "omitted thinking",
			payload: `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-haiku-5-5", []byte(tc.payload))
			if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
				t.Fatalf("request without replayed signed thinking must not gain block_binding: %s", seenBody)
			}
		})
	}
}

func TestClaudeExecutor_PreservedThinkingOmittedThinkingSynthesizesAdaptiveBinding(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		for _, tc := range []struct {
			name  string
			block string
		}{
			{
				name:  "signed_thinking",
				block: `{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}`,
			},
			{
				name:  "redacted_thinking",
				block: `{"type":"redacted_thinking","data":"redacted-payload"}`,
			},
		} {
			t.Run(model+"_"+tc.name, func(t *testing.T) {
				payload := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[` + tc.block + `,{"type":"text","text":"Answer"}]},{"role":"user","content":"continue"}]}`)
				seenBody, seenHeaders := runClaudeExecutorCapture(t, nil, "", false, model, payload)
				if got := gjson.GetBytes(seenBody, "thinking.type").String(); got != "adaptive" {
					t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, seenBody)
				}
				if got := gjson.GetBytes(seenBody, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
					t.Fatalf("thinking.block_binding.prefix_mismatch_behavior = %q, want drop_block; body=%s", got, seenBody)
				}
				if betas := seenHeaders.Get("Anthropic-Beta"); !strings.Contains(betas, claudeThinkingBindingBeta) {
					t.Fatalf("Anthropic-Beta = %q, want %s", betas, claudeThinkingBindingBeta)
				}
			})
		}
	}
}

func TestClaudeExecutor_PreservedThinkingTypelessThinkingObjectUntouched(t *testing.T) {
	payload := []byte(`{"model":"claude-haiku-5-5","thinking":{},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}]},{"role":"user","content":"continue"}]}`)
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-haiku-5-5", payload)
	if gjson.GetBytes(seenBody, "thinking.type").Exists() || gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("thinking object without type must stay untouched: %s", seenBody)
	}
}

func TestClaudeExecutor_PreservedThinkingForcedToolChoiceKeepsThinkingOff(t *testing.T) {
	for _, tc := range []struct {
		name       string
		thinking   string
		toolChoice string
	}{
		{
			name:       "omitted thinking with tool_choice any",
			toolChoice: `{"type":"any"}`,
		},
		{
			name:       "omitted thinking with tool_choice tool",
			toolChoice: `{"type":"tool","name":"lookup"}`,
		},
		{
			name:       "explicit adaptive thinking stripped by tool_choice any",
			thinking:   `"thinking":{"type":"adaptive"},`,
			toolChoice: `{"type":"any"}`,
		},
		{
			name:       "explicit adaptive thinking stripped by tool_choice tool",
			thinking:   `"thinking":{"type":"adaptive"},`,
			toolChoice: `{"type":"tool","name":"lookup"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"model":"claude-haiku-5-5",` + tc.thinking + `"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":` + tc.toolChoice + `,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}]},{"role":"user","content":"continue"}]}`)
			seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-haiku-5-5", payload)
			if gjson.GetBytes(seenBody, "thinking").Exists() {
				t.Fatalf("forced tool_choice must keep thinking off: %s", seenBody)
			}
		})
	}
}

func TestClaudeExecutor_PreservedThinkingInjectionKeepsCallerError(t *testing.T) {
	payload := []byte(`{"model":"claude-haiku-5-5","thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"error"}},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}]},{"role":"user","content":"continue"}]}`)
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-haiku-5-5", payload)
	if got := gjson.GetBytes(seenBody, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "error" {
		t.Fatalf("caller prefix_mismatch_behavior = %q, want error preserved; body=%s", got, seenBody)
	}
}

func TestClaudeExecutor_PayloadOverrideWinsOverPreservedThinkingInjection(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "claude-haiku-5-5"}},
			Params: map[string]any{"thinking.block_binding.prefix_mismatch_behavior": "error"},
		}},
	}}
	seenBody, _ := runClaudeExecutorCapture(t, cfg, "", false, "claude-haiku-5-5", preservedThinkingReplayPayload("claude-haiku-5-5"))
	if got := gjson.GetBytes(seenBody, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "error" {
		t.Fatalf("payload override must be the final barrier, got %q; body=%s", got, seenBody)
	}
}

func TestClaudeExecutor_PreservedThinkingInjectionSkipsDisabledHaikuThinking(t *testing.T) {
	payload := []byte(`{"model":"claude-haiku-5-5","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}]},{"role":"user","content":"continue"}]}`)
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-haiku-5-5", payload)
	if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("block_binding with disabled thinking is rejected upstream on haiku-5-5: %s", seenBody)
	}
}

func TestClaudeExecutor_PreservedThinkingInjectionSkipsBetweenToolsSonnetThinking(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"between_tools"},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier","signature":"` + preservedThinkingTestSignature + `"}]},{"role":"user","content":"continue"}]}`)
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-sonnet-5-5", payload)
	if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("block_binding with between_tools thinking is rejected upstream on sonnet-5-5: %s", seenBody)
	}
}

func TestClaudeExecutor_PreservedThinkingInjectionSkipsMythosModel(t *testing.T) {
	seenBody, _ := runClaudeExecutorCapture(t, nil, "", false, "claude-mythos-5-1", preservedThinkingReplayPayload("claude-mythos-5-1"))
	if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("mythos-5-1 does not run the prefix check and must not gain block_binding: %s", seenBody)
	}
}

func TestClaudeExecutor_PayloadFilterRemovesPreservedThinkingInjection(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Filter: []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{{Name: "claude-haiku-5-5"}},
			Params: []string{"thinking.block_binding"},
		}},
	}}
	seenBody, _ := runClaudeExecutorCapture(t, cfg, "", false, "claude-haiku-5-5", preservedThinkingReplayPayload("claude-haiku-5-5"))
	if gjson.GetBytes(seenBody, "thinking.block_binding").Exists() {
		t.Fatalf("payload filter must remove injected block_binding: %s", seenBody)
	}
}

func TestApplyClaudeHeaders_BlockBindingBodyAlwaysGetsBindingBeta(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-5-5","thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}},"messages":[{"role":"user","content":"hi"}]}`)
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		key  string
	}{
		{name: "cli_profile", auth: &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k", "fingerprint_profile": "claude-code-cli"}}, key: "k"},
		{name: "caller_owned", auth: &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k", "caller_owned": "true"}}, key: "k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newClaudeHeaderTestRequest(t, nil)
			applyClaudeHeaders(req, tc.auth, tc.key, false, nil, body, nil)
			betas := req.Header.Get("Anthropic-Beta")
			if !strings.Contains(betas, claudeThinkingBindingBeta) {
				t.Fatalf("%s: Anthropic-Beta = %q, want %s", tc.name, betas, claudeThinkingBindingBeta)
			}
		})
	}
}

// The internal Codex commentary marker is a forgeable literal, so the Claude
// executor restores it on every path that leaves for a Claude-compatible
// upstream -- including the model names whose signature policy is not Claude.
// Captured upstream bodies are the contract: the marker must be gone, its text
// must be a plain text block, and a genuine signature must still be forwarded.
func TestClaudeExecutor_RestoresClientSuppliedCommentaryMarkerBeforeUpstream(t *testing.T) {
	const marker = "cpa:commentary:v1"
	// claude-opus-5-5 is a preserved-thinking model: before the restore ran, a
	// marker counted as a signed thinking block and forced thinking.block_binding.
	const model = "claude-opus-5-5"
	payload := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[
		{"role":"user","content":[{"type":"text","text":"First turn"}]},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"` + marker + `"},
			{"type":"thinking","thinking":"REASON_SENTINEL","signature":"opaque-provider-id"},
			{"type":"text","text":"FINAL_SENTINEL"}
		]},
		{"role":"user","content":[{"type":"text","text":"Next turn"}]}
	]}`)

	cases := []struct {
		name    string
		invoke  func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth)
		respond string
	}{
		{
			name: "Execute",
			invoke: func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) {
				t.Helper()
				if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
					Model:   model,
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
					t.Fatalf("Execute error: %v", err)
				}
			},
			respond: `{"id":"msg_1","type":"message","model":"` + model + `","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name: "ExecuteStream",
			invoke: func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) {
				t.Helper()
				result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
					Model:   model,
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("unexpected chunk error: %v", chunk.Err)
					}
				}
			},
			respond: "data: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name: "CountTokens",
			invoke: func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) {
				t.Helper()
				if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
					Model:   model,
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
					t.Fatalf("CountTokens error: %v", err)
				}
			},
			respond: `{"input_tokens":10}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				if strings.HasSuffix(r.URL.Path, "/count_tokens") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.respond))
					return
				}
				if strings.HasPrefix(tc.respond, "data: ") {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = w.Write([]byte(tc.respond))
			}))
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":  "key-123",
				"base_url": server.URL,
			}}
			tc.invoke(t, executor, auth)

			if len(body) == 0 {
				t.Fatal("no upstream body captured")
			}
			if strings.Contains(string(body), marker) {
				t.Fatalf("internal commentary marker reached the upstream: %s", body)
			}
			content := gjson.GetBytes(body, "messages.1.content").Array()
			if len(content) != 3 {
				t.Fatalf("assistant content block count = %d, want 3: %s", len(content), body)
			}
			if got := content[0].Get("type").String(); got != "text" {
				t.Fatalf("commentary block type = %q, want text: %s", got, content[0].Raw)
			}
			if got := content[0].Get("text").String(); got != "COMMENTARY_SENTINEL" {
				t.Fatalf("commentary text = %q, want COMMENTARY_SENTINEL", got)
			}
			if content[0].Get("signature").Exists() {
				t.Fatalf("restored commentary text must not carry a signature: %s", content[0].Raw)
			}
			if got := content[1].Get("signature").String(); got != "opaque-provider-id" {
				t.Fatalf("genuine signature = %q, want it forwarded untouched: %s", got, content[1].Raw)
			}
		})
	}
}

// A history that carries only the internal marker must not be mistaken for signed
// history: the restore runs before the preserved-thinking binding is derived, so
// no thinking type or block_binding is injected for it.
func TestClaudeExecutor_CommentaryMarkerAloneDoesNotLookSigned(t *testing.T) {
	const marker = "cpa:commentary:v1"
	const model = "claude-opus-5-5"
	payload := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[
		{"role":"user","content":[{"type":"text","text":"First turn"}]},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"COMMENTARY_SENTINEL","signature":"` + marker + `"}
		]},
		{"role":"user","content":[{"type":"text","text":"Next turn"}]}
	]}`)

	for _, tc := range []struct {
		name   string
		invoke func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth)
		path   string
	}{
		{
			name: "Execute",
			invoke: func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) {
				t.Helper()
				if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
					t.Fatalf("Execute error: %v", err)
				}
			},
			path: "/v1/messages",
		},
		{
			name: "CountTokens",
			invoke: func(t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth) {
				t.Helper()
				if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
					t.Fatalf("CountTokens error: %v", err)
				}
			},
			path: "/v1/messages/count_tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "count_tokens") {
					_, _ = w.Write([]byte(`{"input_tokens":10}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"` + model + `","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
			tc.invoke(t, executor, auth)

			if strings.Contains(string(body), marker) {
				t.Fatalf("internal commentary marker reached the upstream: %s", body)
			}
			if gjson.GetBytes(body, "thinking.block_binding").Exists() || gjson.GetBytes(body, "thinking.type").Exists() {
				t.Fatalf("a marker-only history must not be treated as signed history: %s", body)
			}
			block := gjson.GetBytes(body, "messages.1.content.0")
			if block.Get("type").String() != "text" || block.Get("text").String() != "COMMENTARY_SENTINEL" {
				t.Fatalf("commentary block should be plain text: %s", block.Raw)
			}
		})
	}
}
