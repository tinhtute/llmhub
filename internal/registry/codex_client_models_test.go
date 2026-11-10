package registry

import "testing"

func TestCodexClientModelUsesResponsesLite(t *testing.T) {
	if !CodexClientModelUsesResponsesLite("gpt-6-astra") {
		t.Fatal("gpt-6-astra should be marked use_responses_lite")
	}
	if !CodexClientModelUsesResponsesLite(" GPT-6-ASTRA ") {
		t.Fatal("lookup should trim and lowercase the slug")
	}
	if CodexClientModelUsesResponsesLite("gpt-5.4") {
		t.Fatal("gpt-5.4 must not be marked use_responses_lite")
	}
	if CodexClientModelUsesResponsesLite("") {
		t.Fatal("empty slug must not be marked use_responses_lite")
	}
}

func TestCodexClientModelThinkingLevelsReturnsDefensiveCopy(t *testing.T) {
	levels := CodexClientModelThinkingLevels("gpt-6-astra")
	if len(levels) == 0 {
		t.Fatal("expected supported reasoning levels for gpt-6-astra")
	}
	levels[0] = "mutated"
	if again := CodexClientModelThinkingLevels("gpt-6-astra"); again[0] == "mutated" {
		t.Fatal("thinking levels must be copied before returning")
	}
	if unknown := CodexClientModelThinkingLevels("no-such-model"); unknown != nil {
		t.Fatalf("unknown slug levels = %v, want nil", unknown)
	}
}
