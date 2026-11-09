package executor

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/clienterror"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude diagnostics + upstream request continuity state, ported from upstream
// CLIProxyAPI claude_executor_diagnostics.go (parity: 086ad91bd970, 4a5ab534f827).

type claudeDiagnosticsRequestState struct {
	key      string
	sequence uint64
	promptID string
}

func injectClaudeDiagnostics(body []byte, auth *cliproxyauth.Auth, sessionID string) ([]byte, claudeDiagnosticsRequestState) {
	key, sequence, previousMessageID, _, promptID := helps.BeginClaudeContinuity(claudeDiagnosticsCredentialIdentity(auth), sessionID, false, "")
	return injectClaudeDiagnosticsWithState(body, key, sequence, previousMessageID, promptID)
}

func injectClaudeDiagnosticsWithState(body []byte, key string, sequence uint64, previousMessageID string, promptIDs ...string) ([]byte, claudeDiagnosticsRequestState) {
	if key == "" {
		return body, claudeDiagnosticsRequestState{}
	}
	promptID := ""
	if len(promptIDs) > 0 {
		promptID = promptIDs[0]
	}
	value := `{"previous_message_id":null}`
	if previousMessageID != "" {
		value = `{"previous_message_id":` + marshalJSONStringWithoutHTMLEscape(previousMessageID) + `}`
	}

	if diagnostics := gjson.GetBytes(body, "diagnostics"); diagnostics.Exists() {
		updated, errSet := sjson.SetRawBytes(body, "diagnostics", []byte(value))
		if errSet == nil {
			return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence, promptID: promptID}
		}
	}
	if contextManagement := gjson.GetBytes(body, "context_management"); contextManagement.Exists() {
		start := contextManagement.Index
		insertAt := start + len(contextManagement.Raw)
		if start >= 0 && insertAt >= start && insertAt <= len(body) && bytes.Equal(body[start:insertAt], []byte(contextManagement.Raw)) {
			updated := make([]byte, 0, len(body)+len(value)+len(`,"diagnostics":`))
			updated = append(updated, body[:insertAt]...)
			updated = append(updated, `,"diagnostics":`...)
			updated = append(updated, value...)
			updated = append(updated, body[insertAt:]...)
			return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence, promptID: promptID}
		}
	}
	updated, errSet := sjson.SetRawBytes(body, "diagnostics", []byte(value))
	if errSet != nil {
		return body, claudeDiagnosticsRequestState{}
	}
	return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence, promptID: promptID}
}

func commitClaudeContinuity(state claudeDiagnosticsRequestState, messageID, requestID string) {
	helps.CommitClaudeContinuity(state.key, state.sequence, messageID, requestID, state.promptID)
}

func commitClaudeDiagnostics(state claudeDiagnosticsRequestState, messageID string) {
	commitClaudeContinuity(state, messageID, "")
}

// claudeDiagnosticsCredentialIdentity resolves the stable credential identity
// that scopes upstream request continuity. Ported from upstream; the local
// credential pool exposes auth ID/Index only (device-pool and account-UUID
// fallbacks do not exist in this tree).
func claudeDiagnosticsCredentialIdentity(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return "index:" + index
	}
	return ""
}

func claudeMessageIDFromResponse(data []byte) string {
	return strings.TrimSpace(gjson.GetBytes(data, "id").String())
}

func observeClaudeStreamJSONPayload(payload []byte, hasDataPrefix bool, messageID *string, completed *bool) {
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "message_start":
		if id := strings.TrimSpace(root.Get("message.id").String()); id != "" {
			*messageID = id
		}
	case "message_stop":
		*completed = true
	case "message":
		if !hasDataPrefix && strings.TrimSpace(root.Get("stop_reason").String()) != "" {
			if id := strings.TrimSpace(root.Get("id").String()); id != "" {
				*messageID = id
			}
			*completed = true
		}
	}
}

// observeClaudeSSEEvent tracks the upstream message id (message_start), whether
// the stream reached message_stop, and any error event, so continuity commits
// only complete responses and truncated streams cannot corrupt state
// (upstream 086ad91bd970).
func observeClaudeSSEEvent(rawEvent []byte, messageID *string, completed *bool, streamErr *error) {
	var dataLines [][]byte
	for _, line := range bytes.Split(rawEvent, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) > 0 {
			dataLines = append(dataLines, payload)
		}
	}
	if len(dataLines) == 0 {
		trimmed := bytes.TrimSpace(rawEvent)
		if bytes.HasPrefix(trimmed, []byte("{")) && gjson.ValidBytes(trimmed) {
			observeClaudeStreamJSONPayload(trimmed, false, messageID, completed)
		}
		return
	}
	joined := bytes.TrimSpace(bytes.Join(dataLines, []byte("\n")))
	if !gjson.ValidBytes(joined) {
		return
	}
	observeClaudeStreamJSONPayload(joined, true, messageID, completed)
	if streamErr != nil && *streamErr == nil {
		if gjson.ParseBytes(joined).Get("type").String() == "error" {
			*streamErr = statusErr{code: 502, msg: string(joined)}
		} else if refusalMsg, blocked := clienterror.ClaudePolicyRefusalMessage(joined); blocked {
			// A policy/cyber refusal is a request fault, not an upstream failure:
			// 400 keeps the credential out of cooldown (upstream c3938b72bdaa).
			*streamErr = statusErr{code: http.StatusBadRequest, msg: refusalMsg}
		}
	}
}

func claudeMessageIDFromSSE(data []byte) string {
	var messageID string
	completed := false
	var eventBuf bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		eventBuf.Write(line)
		eventBuf.WriteByte('\n')
		if len(bytes.TrimSpace(line)) == 0 {
			observeClaudeSSEEvent(eventBuf.Bytes(), &messageID, &completed, nil)
			eventBuf.Reset()
		}
	}
	if eventBuf.Len() > 0 {
		observeClaudeSSEEvent(eventBuf.Bytes(), &messageID, &completed, nil)
	}
	if !completed {
		return ""
	}
	return messageID
}

func isClaudeHaiku55Model(model string) bool {
	model = claudeCanonicalModel(model)
	return model == "claude-haiku-5-5" || strings.HasPrefix(model, "claude-haiku-5-5-") || strings.HasPrefix(model, "claude-haiku-5-5[") || model == "claude-haiku-5.5" || strings.HasPrefix(model, "claude-haiku-5.5-") || strings.HasPrefix(model, "claude-haiku-5.5[")
}

// isClaudeFable51OnlyModel reports Fable 5.1 specifically, excluding Mythos
// 5.1, which shares the fable family checks elsewhere but does not run the
// preserved-thinking prefix check.
func isClaudeFable51OnlyModel(model string) bool {
	m := claudeCanonicalModel(model)
	for _, target := range []string{"fable-5-1", "fable-5.1"} {
		idx := strings.Index(m, target)
		if idx != -1 {
			nextIdx := idx + len(target)
			if nextIdx >= len(m) || m[nextIdx] < '0' || m[nextIdx] > '9' {
				return true
			}
		}
	}
	return false
}

// isClaudePreservedThinkingModel reports the models whose signed thinking
// blocks Anthropic binds to the creation-time conversation prefix (system,
// tools, and every earlier message). Replaying those blocks under a reshaped
// prefix fails binding validation unless the request opts into dropping them.
// See https://platform.claude.com/docs/en/build-with-claude/preserved-thinking
func isClaudePreservedThinkingModel(model string) bool {
	return isClaudeOpus55Model(model) || isClaudeSonnet55Model(model) || isClaudeHaiku55Model(model) || isClaudeFable51OnlyModel(model)
}

func claudeToolChoiceForcesToolUse(body []byte) bool {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	return toolChoiceType == "any" || toolChoiceType == "tool"
}

// claudeThinkingTypeAcceptsBlockBinding reports whether the body's thinking
// mode may carry block_binding on this model. Haiku 5.5 and Sonnet 5.5 accept
// it only with adaptive thinking (disabled/between_tools return 400); the
// other preserved-thinking models also accept manual enabled thinking.
func claudeThinkingTypeAcceptsBlockBinding(model, thinkingType string) bool {
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "adaptive":
		return true
	case "enabled":
		return !isClaudeHaiku55Model(model) && !isClaudeSonnet55Model(model)
	default:
		return false
	}
}

// claudeBodyHasSignedThinkingBlock reports whether any assistant message
// carries a replayed thinking block with a signature or a redacted_thinking
// block, i.e. history Anthropic subjects to prefix binding validation.
func claudeBodyHasSignedThinkingBlock(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			switch strings.TrimSpace(block.Get("type").String()) {
			case "thinking":
				found = strings.TrimSpace(block.Get("signature").String()) != ""
			case "redacted_thinking":
				found = strings.TrimSpace(block.Get("data").String()) != ""
			}
			return !found
		})
		return !found
	})
	return found
}

// applyClaudePreservedThinkingBinding makes relayed or reshaped histories
// survivable on preserved-thinking models: when the request replays signed
// thinking blocks, default thinking.block_binding.prefix_mismatch_behavior to
// drop_block so a prefix mismatch (client reconnect resending history, a
// changed tools list, reconstructed server-tool blocks) drops the bound
// blocks instead of failing the whole request with a 400. On preserved-thinking
// models, an omitted thinking field runs adaptive thinking and is equivalent to
// {"type":"adaptive"}, so synthesize that adaptive thinking object unless
// tool_choice forces tool use. A thinking object without a type and any
// caller-supplied prefix_mismatch_behavior stay untouched. This must run before
// user payload rules, which stay the final semantic barrier and may override or
// filter the injected value.
func applyClaudePreservedThinkingBinding(body []byte) []byte {
	model := gjson.GetBytes(body, "model").String()
	if len(body) == 0 || !isClaudePreservedThinkingModel(model) || claudeToolChoiceForcesToolUse(body) {
		return body
	}
	thinking := gjson.GetBytes(body, "thinking")
	if thinking.Exists() {
		if !claudeThinkingTypeAcceptsBlockBinding(model, thinking.Get("type").String()) || thinking.Get("block_binding.prefix_mismatch_behavior").Exists() {
			return body
		}
	}
	if !claudeBodyHasSignedThinkingBlock(body) {
		return body
	}
	if !thinking.Exists() {
		body, _ = sjson.SetBytes(body, "thinking.type", "adaptive")
	}
	body, _ = sjson.SetBytes(body, "thinking.block_binding.prefix_mismatch_behavior", "drop_block")
	return body
}
