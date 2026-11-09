package executor

import (
	"context"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// Ported from upstream CLIProxyAPI internal/runtime/executor/devin_effort_directive_test.go
// (061ffe6fbc22). The upstream "top-level effort only" case expects the top-level
// effort to reach the resolver, but no local Claude->Interactions translator
// exists: TranslateRequest falls back to the raw Claude payload and
// parseInteractionsPayload reads only generation_config.thinking_level, so the
// top-level effort never reaches ResolveDevinChatModelUID. That case asserts the
// current default variant instead of pretending the gap is closed.
func TestDevinExecutorUsesClaudeEffortDirective(t *testing.T) {
	const model = "devin/claude-opus-5-5"
	const topLevelOnly = `{"model":"` + model + `","max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	const withDirectives = `{"model":"` + model + `","max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},` +
		`"messages":[{"role":"system","content":[],"output_config":{"effort":"medium"}},{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"ok"},{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"user","content":"hi"}]}`
	const disabledWithDirective = `{"model":"` + model + `","max_tokens":1024,"thinking":{"type":"disabled"},` +
		`"messages":[{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"user","content":"hi"}]}`

	wantDefault := helps.ResolveDevinChatModelUID(model, "", 0)
	wantMedium := helps.ResolveDevinChatModelUID(model, "medium", 0)
	wantHigh := helps.ResolveDevinChatModelUID(model, "high", 0)
	if wantDefault == wantHigh || wantMedium == wantHigh {
		t.Fatalf("fixture model does not separate default %q, medium %q and high %q", wantDefault, wantMedium, wantHigh)
	}

	exec := NewDevinExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "devin-effort-directive", Provider: "devin", Attributes: map[string]string{"api_key": "test-key"}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	for _, tc := range []struct {
		name    string
		model   string
		payload string
		want    string
	}{
		// No local translator: the top-level effort is dropped before the resolver.
		{name: "top-level effort only does not reach the resolver", model: model, payload: topLevelOnly, want: wantDefault},
		{name: "last directive wins over the top-level placeholder", model: model, payload: withDirectives, want: wantHigh},
		{name: "model suffix wins over directive", model: model + "(medium)", payload: withDirectives, want: wantMedium},
		// Disabled thinking suppresses the directive; without a translator the
		// payload carries no thinking level, so the default variant is kept.
		{name: "disabled thinking keeps a historical directive from selecting a variant", model: model, payload: disabledWithDirective, want: wantDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.payload)}
			_, chatModelUID, _, err := exec.prepareDevinHTTPRequest(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("prepareDevinHTTPRequest: %v", err)
			}
			if chatModelUID != tc.want {
				t.Fatalf("chat model uid = %q, want %q", chatModelUID, tc.want)
			}
		})
	}
}
