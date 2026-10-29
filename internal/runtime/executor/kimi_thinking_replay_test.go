package executor

import (
	"context"
	"testing"

	internalcache "github.com/therealtinhtute/llmhub/internal/cache"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

// R8 (upstream v8.0.42 ab139b45): the two K3.1 Pro variants are the same model
// family for thinking replay, so a session that switches between them keeps the
// signed reasoning it was already sent.
func TestKimiThinkingReplayModelFamily(t *testing.T) {
	for _, fixture := range []struct{ model, want string }{
		{model: "k3", want: "k3"},
		{model: "kimi-k3", want: "k3"},
		{model: "k3-256k", want: "k3"},
		{model: "kimi-k3-256k(high)", want: "k3"},
		{model: "k3.1-pro", want: "k3.1-pro"},
		{model: "k3.1-pro-highspeed", want: "k3.1-pro"},
		{model: "kimi-k3.1-pro", want: "k3.1-pro"},
		{model: "kimi-k3.1-pro-highspeed(max)", want: "k3.1-pro"},
		{model: "kimi-k2.8", want: "k2.8"},
	} {
		if got := kimiThinkingReplayModelFamily(fixture.model); got != fixture.want {
			t.Errorf("kimiThinkingReplayModelFamily(%q) = %q, want %q", fixture.model, got, fixture.want)
		}
	}
}

// A cached signed reasoning block must be replayed to the sibling model of the
// same family, and must not leak to a model of another family.
func TestKimiThinkingReplaySharedAcrossK31ProVariants(t *testing.T) {
	const cached = `[{"type":"thinking","thinking":"reasoning","signature":"kimi-signature"},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"README.md"}}]`
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"README.md"}}]}]}`)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "k31-family-switch",
		},
	}
	ctx := context.Background()
	for _, models := range [][2]string{
		{"kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"},
		{"kimi-k3.1-pro-highspeed", "kimi-k3.1-pro"},
	} {
		t.Run(models[0]+"_to_"+models[1], func(t *testing.T) {
			internalcache.ClearKimiThinkingReplayCache()
			t.Cleanup(internalcache.ClearKimiThinkingReplayCache)

			_, sourceScope := prepareKimiThinkingReplayRequest(ctx, cliproxyexecutor.Request{Model: models[0], Payload: payload}, opts)
			cacheKimiThinkingReplayContent(ctx, sourceScope, []byte(cached))

			prepared, scope := prepareKimiThinkingReplayRequest(ctx, cliproxyexecutor.Request{Model: models[1], Payload: payload}, opts)
			if scope.modelFamily != "k3.1-pro" || !scope.replayApplied {
				t.Fatalf("variant switch scope = %+v, want shared K3.1 replay", scope)
			}
			if !kimiJSONEqual([]byte(gjson.GetBytes(prepared.Payload, "messages.0.content").Raw), []byte(cached)) {
				t.Fatalf("variant switch did not preserve complete cached content: %s", prepared.Payload)
			}
			for _, isolatedModel := range []string{"kimi-k3", "kimi-k3-256k", "kimi-k2.8"} {
				_, isolatedScope := prepareKimiThinkingReplayRequest(ctx, cliproxyexecutor.Request{Model: isolatedModel, Payload: payload}, opts)
				if isolatedScope.replayApplied {
					t.Fatalf("%s received K3.1 replay", isolatedModel)
				}
			}
		})
	}
}
