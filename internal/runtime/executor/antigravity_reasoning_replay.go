package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	log "github.com/sirupsen/logrus"
	internalcache "github.com/therealtinhtute/llmhub/internal/cache"
	homekv "github.com/therealtinhtute/llmhub/internal/home"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	internalsignature "github.com/therealtinhtute/llmhub/internal/signature"
	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	cliproxysession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// antigravityReplayLogKey returns a short, non-reversible tag for a replay
// identifier. Session keys and tool call IDs are never logged verbatim.
func antigravityReplayLogKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

func antigravityGeminiClaudeToolUseID(callID, name, argsRaw string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	canonicalArgs := antigravityCanonicalReplayJSON([]byte(argsRaw))
	sum := sha256.Sum256([]byte(strings.TrimSpace(callID) + "\x00" + name + "\x00" + string(canonicalArgs)))
	return fmt.Sprintf("cpa_claude_tool_%x", sum[:8])
}

func antigravityIsGeminiClaudeToolUseID(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "cpa_claude_tool_")
}

type antigravityContentEdit struct {
	index       int64
	path        string
	start       int
	end         int
	replacement []byte
}

// applyAntigravityIndexedEdits splices collected JSON fragments into the original
// request with one body copy. Applying SJSON once per field made large histories
// scale with history size multiplied by the number of edits.
func applyAntigravityIndexedEdits(rawJSON []byte, edits []antigravityContentEdit, validOffsets bool) []byte {
	if len(edits) == 0 {
		return rawJSON
	}
	if !validOffsets {
		return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
	}

	finalSize := len(rawJSON)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		cursor = edit.end
	}
	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, rawJSON[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	return append(out, rawJSON[cursor:]...)
}

// applyAntigravityContentEditsWithSJSON preserves the legacy path semantics if
// a GJSON result cannot be proven to point into the original request bytes.
func applyAntigravityContentEditsWithSJSON(rawJSON []byte, edits []antigravityContentEdit) []byte {
	out := rawJSON
	for _, edit := range edits {
		path := edit.path
		if path == "" {
			path = fmt.Sprintf("request.contents.%d", edit.index)
		}
		if updated, errSet := sjson.SetRawBytes(out, path, edit.replacement); errSet == nil {
			out = updated
		}
	}
	return out
}

// repairAntigravityGeminiFunctionResponseNames copies missing or placeholder
// functionResponse names from the matching functionCall. Edits are applied to
// each part in isolation, then spliced into the request with one body copy.
// Ported from upstream commit d8f2dceef789.
func repairAntigravityGeminiFunctionResponseNames(rawJSON []byte) []byte {
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	callIDToName := make(map[string]string)
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			fc := part.Get("functionCall")
			if fc.Exists() {
				id := strings.TrimSpace(fc.Get("id").String())
				name := strings.TrimSpace(fc.Get("name").String())
				if id != "" && name != "" && name != "unknown" {
					callIDToName[id] = name
				}
			}
			return true
		})
		return true
	})
	if len(callIDToName) == 0 {
		return rawJSON
	}

	edits := make([]antigravityContentEdit, 0)
	validOffsets := true
	contents.ForEach(func(contentIdx, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(partIdx, part gjson.Result) bool {
			fr := part.Get("functionResponse")
			if !fr.Exists() {
				return true
			}
			id := strings.TrimSpace(fr.Get("id").String())
			name := strings.TrimSpace(fr.Get("name").String())
			if id == "" || (name != "" && name != "unknown") {
				return true
			}
			realName, ok := callIDToName[id]
			if !ok {
				return true
			}
			updatedPart, errSet := sjson.SetBytes([]byte(part.Raw), "functionResponse.name", realName)
			if errSet != nil {
				return true
			}
			start := part.Index
			end := start + len(part.Raw)
			if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(part.Raw)) {
				validOffsets = false
			}
			edits = append(edits, antigravityContentEdit{
				index:       contentIdx.Int(),
				path:        fmt.Sprintf("request.contents.%d.parts.%d", contentIdx.Int(), partIdx.Int()),
				start:       start,
				end:         end,
				replacement: updatedPart,
			})
			return true
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

func normalizeAntigravityGeminiFunctionResponseRoles(payload []byte) []byte {
	payload = repairAntigravityGeminiFunctionResponseNames(payload)
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	// Patch each response turn in isolation and splice all changed turns into the
	// request with one body copy. Applying SJSON once per content made large
	// histories scale with history size multiplied by the number of tool turns.
	contentItems := make([][]byte, 0, contents.Get("#").Int())
	changed := false
	contents.ForEach(func(_, content gjson.Result) bool {
		contentJSON := []byte(content.Raw)
		hasFunctionResponse := false
		parts := content.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionResponse").Exists() {
					hasFunctionResponse = true
					return false
				}
				return true
			})
		} else if parts.Type != gjson.Null {
			// Result.Array returns a non-array JSON value as one item.
			hasFunctionResponse = parts.Get("functionResponse").Exists()
		}
		if hasFunctionResponse && !strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "function") {
			contentJSON, _ = sjson.SetBytes(contentJSON, "role", "function")
			changed = true
		}
		contentItems = append(contentItems, contentJSON)
		return true
	})
	if !changed {
		return payload
	}
	out, _ := sjson.SetRawBytes(payload, "request.contents", translatorcommon.JoinRawArray(contentItems))
	return out
}

// antigravityCountClaudeToolProvenanceIDs reports how many reserved
// Claude-facing provenance IDs are still present in a Gemini-shaped payload.
func antigravityCountClaudeToolProvenanceIDs(payload []byte) int {
	count := 0
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return 0
	}
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		countPart := func(part gjson.Result) {
			for _, path := range []string{"functionCall.id", "functionResponse.id"} {
				if antigravityIsGeminiClaudeToolUseID(part.Get(path).String()) {
					count++
				}
			}
		}
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				countPart(part)
				return true
			})
		} else if parts.Type != gjson.Null {
			// Result.Array returns a non-array JSON value as one item.
			countPart(parts)
		}
		return true
	})
	return count
}

type antigravityReasoningReplayScope struct {
	modelName     string
	sessionKey    string
	cacheSnapshot internalcache.AntigravityReasoningReplaySnapshot
	// openAIChat marks an OpenAI chat-completions caller. Its replay history is
	// bound to the system instruction and preceding turns only, so a later turn
	// that changes tool_choice does not invalidate signatures cached earlier.
	openAIChat bool
}

func (s antigravityReasoningReplayScope) valid() bool {
	return strings.TrimSpace(s.modelName) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func antigravityReasoningReplayScopeFromPayload(modelName string, payload []byte) antigravityReasoningReplayScope {
	sessionID := antigravityReplaySessionIDFromPayload(payload)
	if sessionID == "" {
		if stable := strings.TrimSpace(generateStableSessionID(payload)); stable != "" {
			sessionID = strings.TrimPrefix(stable, "-")
			if sessionID == "" {
				sessionID = stable
			}
		}
	}
	if sessionID == "" {
		return antigravityReasoningReplayScope{}
	}
	return antigravityReasoningReplayScope{
		modelName:  strings.TrimSpace(modelName),
		sessionKey: "session:" + sessionID,
	}
}

func antigravityReasoningReplayScopeFromRequest(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) antigravityReasoningReplayScope {
	// Prefer an explicit downstream session over a provider sessionId synthesized
	// from request text. This keeps identical prompts in separate client sessions
	// from sharing an opaque Gemini reasoning chain.
	scope := antigravityReasoningReplayScope{modelName: modelName}
	if sessionKey := antigravityReasoningReplayClientSessionKey(ctx, req, opts); sessionKey != "" {
		scope.sessionKey = sessionKey
	} else {
		scope = antigravityReasoningReplayScopeFromPayload(modelName, payload)
		if !scope.valid() {
			scope = antigravityReasoningReplayScopeFromPayload(modelName, req.Payload)
		}
	}
	if scope.valid() {
		scope.openAIChat = opts.SourceFormat == sdktranslator.FormatOpenAI
		// Only trusted execution metadata can namespace the ledger. Never read a
		// caller identity from request JSON or downstream credential headers here.
		callerScope := antigravityMetadataMapString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
		if callerScope == "" {
			callerScope = antigravityMetadataMapString(req.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
		}
		if callerScope != "" {
			sum := sha256.Sum256([]byte(callerScope))
			scope.sessionKey = "caller:" + hex.EncodeToString(sum[:]) + ":" + scope.sessionKey
		}
	}
	return scope
}

func antigravityReasoningReplayClientSessionKey(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	_ = ctx
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Session-ID", "X-Session-Affinity", "X-Client-Request-Id"} {
		if value := strings.TrimSpace(opts.Headers.Get(name)); value != "" {
			if lane := antigravityClaudeReplaySystemLane(opts.OriginalRequest); lane != "" {
				return "header:" + name + ":" + value + ":context:" + lane
			}
			return "header:" + name + ":" + value
		}
	}
	for _, raw := range [][]byte{opts.OriginalRequest, req.Payload} {
		if len(raw) == 0 {
			continue
		}
		for _, path := range []string{"session_id", "sessionId", "metadata.session_id"} {
			if value := strings.TrimSpace(gjson.GetBytes(raw, path).String()); value != "" {
				return "responses:" + value
			}
		}
	}
	if value := antigravityMetadataMapString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	if value := antigravityMetadataMapString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	for _, raw := range [][]byte{opts.OriginalRequest, req.Payload} {
		if value := strings.TrimSpace(gjson.GetBytes(raw, "prompt_cache_key").String()); value != "" {
			return "prompt-cache:" + value
		}
	}
	// Reuse ingress extraction for explicit session aliases before falling back
	// to inferred roots. Keep the established session/execution priorities above.
	if info, ok := cliproxysession.ExtractSessionInfo(opts.Headers, nil, nil); ok && info.SessionID != "" {
		return "client:" + info.SessionID
	}
	if value := helps.DerivedSessionID(opts.Metadata, req.Metadata); value != "" {
		return "derived:" + value
	}
	return ""
}

func antigravityMetadataMapString(metadata map[string]any, key string) string {
	if metadata == nil || key == "" {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func antigravityClaudeReplaySystemLane(payload []byte) string {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return ""
	}
	var value any
	if errUnmarshal := json.Unmarshal([]byte(system.Raw), &value); errUnmarshal != nil {
		return ""
	}
	value = antigravityClaudeReplayNormalizeSystem(value)
	normalized, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return ""
	}
	sum := sha256.Sum256(normalized)
	return fmt.Sprintf("%x", sum[:16])
}

func antigravityClaudeReplayNormalizeSystem(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, child := range typed {
			if strings.EqualFold(strings.TrimSpace(key), "cache_control") {
				continue
			}
			normalized[key] = antigravityClaudeReplayNormalizeSystem(child)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, child := range typed {
			normalized[index] = antigravityClaudeReplayNormalizeSystem(child)
		}
		return normalized
	default:
		return value
	}
}

func antigravityReplaySessionIDFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"sessionId", "session_id", "request.sessionId", "request.session_id"} {
		if id := strings.TrimSpace(gjson.GetBytes(payload, path).String()); id != "" {
			return id
		}
	}
	return ""
}

func antigravityReasoningReplayPendingModelContentIndex(payload []byte) (contentIndex int, basePartIndex int) {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return 0, 0
	}
	arr := contents.Array()
	if len(arr) == 0 {
		return 0, 0
	}
	last := arr[len(arr)-1]
	if strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "model") {
		parts := last.Get("parts")
		hasFunctionResponse := false
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				hasFunctionResponse = hasFunctionResponse || part.Get("functionResponse").Exists()
				return !hasFunctionResponse
			})
		}
		if !hasFunctionResponse {
			base := 0
			if parts.IsArray() {
				base = len(parts.Array())
			}
			return len(arr) - 1, base
		}
	}
	return len(arr), 0
}

func antigravityReasoningReplayResolveContentIndex(payload []byte, cached int) int {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return cached
	}
	arr := contents.Array()
	if cached >= 0 && cached < len(arr) {
		return cached
	}
	return -1
}

// logAntigravityReasoningReplayDegraded reports that a replay-state operation
// failed and the request continued without it. A Home that predates the CAS
// command fails every call, and the Home client already warns once about that,
// so those are logged at debug level to avoid one warning per request.
func logAntigravityReasoningReplayDegraded(scope antigravityReasoningReplayScope, stage string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, homekv.ErrCompareAndSwapUnsupported) {
		log.Debugf("antigravity executor: reasoning replay %s unavailable on this Home (session=%s): %v",
			stage, antigravityReplayLogKey(scope.sessionKey), err)
		return
	}
	log.Warnf("antigravity executor: reasoning replay %s failed; continuing without replay (session=%s): %v",
		stage, antigravityReplayLogKey(scope.sessionKey), err)
}

func prepareAntigravityGeminiReasoningReplayPayload(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) ([]byte, antigravityReasoningReplayScope, error) {
	if !antigravityUsesReasoningReplayCache(modelName) {
		return payload, antigravityReasoningReplayScope{}, nil
	}
	updated, scope, replayApplied, errReplay := applyAntigravityReasoningReplayCache(ctx, modelName, req, opts, payload)
	if errReplay != nil {
		// Replay state is an optimization, not a correctness requirement: a ledger
		// miss is already a tolerated outcome below. Failing the request here would
		// surface as an untyped executor error, which MarkResult treats as a
		// credential fault and uses to mark every candidate credential unavailable.
		// Degrade to "no replay this turn" instead.
		logAntigravityReasoningReplayDegraded(scope, "read", errReplay)
		updated = payload
	}
	updated = normalizeAntigravityGeminiFunctionResponseRoles(updated)
	if antigravityPayloadHasClaudeToolProvenanceID(updated) {
		// The replay ledger could not resolve every tool ID — the session lane
		// changed, the entry expired, the process restarted, or a turn never
		// committed. Degrade those calls instead of killing the conversation.
		degradedPayload, degradedCount := degradeAntigravityClaudeToolProvenanceIDs(updated)
		log.Warnf("antigravity executor: replay state missing for %d tool ID(s); rewriting them to synthetic IDs and continuing without reasoning replay for those calls", degradedCount)
		updated = degradedPayload
	}
	// An identity-only restore drops the cached signature, which can leave a model
	// turn's first function call unsigned. Gemini rejects that, so re-assert the
	// invariant the pre-replay sanitizer established.
	updated = antigravityRepairUnsignedFirstFunctionCalls(updated)
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(updated); errPairing != nil {
		originalPairingValid := internalsignature.ValidateGeminiFunctionCallPairing(payload) == nil
		if replayApplied && originalPairingValid && scope.valid() {
			if _, errDelete := internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, scope.modelName, scope.sessionKey, scope.cacheSnapshot); errDelete != nil {
				// Invalidation is best-effort cleanup. Returning it here would replace
				// the pairing diagnosis below with an untyped error.
				logAntigravityReasoningReplayDegraded(scope, "invalidate", errDelete)
			}
			// Upstream b8e6ec0ae770: replay corrupted an otherwise valid history, so
			// drop the mutated payload and continue with the client's original one
			// instead of failing the request.
			log.Warnf("antigravity executor: reasoning replay broke Gemini function call pairing (%v); degrading to original payload", errPairing)
			return payload, scope, nil
		}
		return payload, scope, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("antigravity executor: invalid Gemini function call history: %v", errPairing)}
	}
	return updated, scope, nil
}

func clearAntigravityReasoningReplayOnInvalidSignature(ctx context.Context, scope antigravityReasoningReplayScope, statusCode int, body []byte) error {
	if !scope.valid() {
		return nil
	}
	if statusCode != http.StatusBadRequest {
		return nil
	}
	bodyText := strings.ToLower(string(body))
	if !strings.Contains(bodyText, "thoughtsignature") && !strings.Contains(bodyText, "thought_signature") && !strings.Contains(bodyText, "signature") {
		return nil
	}
	_, errDelete := internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, scope.modelName, scope.sessionKey, scope.cacheSnapshot)
	return errDelete
}

func applyAntigravityReasoningReplayCache(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) ([]byte, antigravityReasoningReplayScope, bool, error) {
	scope := antigravityReasoningReplayScopeFromRequest(ctx, modelName, req, opts, payload)
	if !scope.valid() {
		return payload, scope, false, nil
	}
	items, snapshot, ok, err := internalcache.GetAntigravityReasoningReplayItemsWithSnapshotRequired(ctx, scope.modelName, scope.sessionKey)
	scope.cacheSnapshot = snapshot
	reservedBefore := antigravityCountClaudeToolProvenanceIDs(payload)
	if err != nil || !ok || len(items) == 0 {
		// A ledger miss on a payload that still carries reserved provenance IDs is
		// the signature of a session/lane switch, cache expiry, or a turn that never
		// committed. Log it so the two failure families stay distinguishable.
		if reservedBefore > 0 {
			log.Debugf("antigravity replay: ledger miss with %d reserved tool provenance ID(s) present (session=%s found=%t)",
				reservedBefore, antigravityReplayLogKey(scope.sessionKey), ok)
		}
		return payload, scope, false, err
	}
	updated := payload
	changed := false
	var toolSchemas map[string]any
	if opts.SourceFormat.String() == "claude" {
		toolSchemas = antigravityReplayToolSchemasFromRequests(opts.OriginalRequest, req.Payload)
	}
	updated, changed = applyAntigravityReasoningReplayItems(updated, items, toolSchemas)
	if reservedBefore > 0 {
		log.Debugf("antigravity replay: ledger items=%d reserved before=%d after=%d applied=%t (session=%s)",
			len(items), reservedBefore, antigravityCountClaudeToolProvenanceIDs(updated), changed,
			antigravityReplayLogKey(scope.sessionKey))
	}
	if !changed {
		return payload, scope, false, nil
	}
	return updated, scope, true, nil
}

// applyAntigravityReasoningReplayItems applies cached replay items to the
// request payload. Consecutive items that only rewrite existing parts are
// batched into part-level replacements spliced once; structural or
// context-dependent items fall back to the sequential implementation, after
// which a fresh batch starts. Ported from upstream commit acf919ce50fb.
func applyAntigravityReasoningReplayItems(payload []byte, items [][]byte, toolSchemas map[string]any) ([]byte, bool) {
	updated := payload
	changed := false
	index := newAntigravityReplayRequestIndex(payload)
	for len(items) > 0 {
		batch := newAntigravityReplayBatch(index)
		handled := 0
		for handled < len(items) && batch.apply(items[handled], toolSchemas) {
			handled++
		}
		if handled > 0 {
			next, ok := batch.build(updated)
			if !ok {
				// A malformed or non-addressable part offset makes splicing unsafe.
				// Nothing has been committed yet, so retain the exact legacy behavior
				// for all remaining items.
				next, sequentialChanged := applyAntigravityReasoningReplayItemsSequential(updated, items, toolSchemas)
				return next, changed || sequentialChanged
			}
			updated = next
			changed = batch.applied || changed
			items = items[handled:]
			if len(items) == 0 {
				break
			}
			index = newAntigravityReplayRequestIndex(updated)
			// Retry the first unhandled item against the freshly flushed payload.
			// It may only have rejected the batch because a prior stable-ID restore
			// made the old context index stale.
			continue
		}

		// Apply one structural or context-dependent item with the original
		// sequential implementation, then start another safe batch. A rare
		// fallback therefore cannot make every preceding signature rewrite the
		// entire request again.
		next, itemChanged := applyAntigravityReasoningReplayItemsSequential(updated, items[:1], toolSchemas)
		updated = next
		changed = itemChanged || changed
		items = items[1:]
		if len(items) > 0 && itemChanged {
			index = newAntigravityReplayRequestIndex(updated)
		}
	}
	return updated, changed
}

// applyAntigravityReasoningReplayItemsSequential is the original per-item
// replay application: re-filter and re-apply each item against the latest
// payload so structural mutations stay visible to later items.
func applyAntigravityReasoningReplayItemsSequential(payload []byte, items [][]byte, toolSchemas map[string]any) ([]byte, bool) {
	updated := payload
	changed := false
	for _, item := range items {
		eligible := filterAntigravityReasoningReplayItemsForRequestWithSchemas(updated, [][]byte{item}, toolSchemas)
		if len(eligible) != 1 {
			continue
		}
		next, applied := insertAntigravityReasoningReplayItemsWithSchemas(updated, eligible, toolSchemas)
		if !applied {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

type antigravityReplayPartKey struct {
	contentIndex int
	partIndex    int
}

type antigravityReplayIndexedContent struct {
	role  string
	parts []gjson.Result
}

// antigravityReplayRequestIndex is an immutable, request-scoped view over one
// payload: parsed content parts (each retaining its byte offset for splicing),
// first-occurrence lookups by tool call ID, and cached context fingerprints.
type antigravityReplayRequestIndex struct {
	payload                       []byte
	validContents                 bool
	contents                      []antigravityReplayIndexedContent
	functionCallsByID             map[string]antigravityReplayPartKey
	functionCallCountsByID        map[string]int
	functionResponseContentByID   map[string]int
	functionResponsePartsByID     map[string][]antigravityReplayPartKey
	contextFingerprints           map[int]string
	openAIChatContextFingerprints map[int]string
}

func newAntigravityReplayRequestIndex(payload []byte) *antigravityReplayRequestIndex {
	index := &antigravityReplayRequestIndex{
		payload:                       payload,
		functionCallsByID:             make(map[string]antigravityReplayPartKey),
		functionCallCountsByID:        make(map[string]int),
		functionResponseContentByID:   make(map[string]int),
		functionResponsePartsByID:     make(map[string][]antigravityReplayPartKey),
		contextFingerprints:           make(map[int]string),
		openAIChatContextFingerprints: make(map[int]string),
	}
	contentsResult := gjson.GetBytes(payload, "request.contents")
	index.validContents = contentsResult.IsArray()
	if !index.validContents {
		return index
	}
	for contentIndex, content := range contentsResult.Array() {
		indexedContent := antigravityReplayIndexedContent{
			role: strings.TrimSpace(content.Get("role").String()),
		}
		if parts := content.Get("parts"); parts.IsArray() {
			indexedContent.parts = parts.Array()
		}
		for partIndex, part := range indexedContent.parts {
			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				callID := strings.TrimSpace(functionCall.Get("id").String())
				if callID != "" {
					index.functionCallCountsByID[callID]++
					if _, exists := index.functionCallsByID[callID]; !exists {
						index.functionCallsByID[callID] = antigravityReplayPartKey{contentIndex: contentIndex, partIndex: partIndex}
					}
				}
			}
			if functionResponse := part.Get("functionResponse"); functionResponse.Exists() {
				callID := strings.TrimSpace(functionResponse.Get("id").String())
				if callID != "" {
					if _, exists := index.functionResponseContentByID[callID]; !exists {
						index.functionResponseContentByID[callID] = contentIndex
					}
					index.functionResponsePartsByID[callID] = append(index.functionResponsePartsByID[callID], antigravityReplayPartKey{contentIndex: contentIndex, partIndex: partIndex})
				}
			}
		}
		index.contents = append(index.contents, indexedContent)
	}
	return index
}

func (i *antigravityReplayRequestIndex) part(key antigravityReplayPartKey) (gjson.Result, bool) {
	if i == nil || key.contentIndex < 0 || key.contentIndex >= len(i.contents) {
		return gjson.Result{}, false
	}
	parts := i.contents[key.contentIndex].parts
	if key.partIndex < 0 || key.partIndex >= len(parts) {
		return gjson.Result{}, false
	}
	return parts[key.partIndex], true
}

// contextFingerprint mirrors antigravityReplayContextFingerprint but caches the
// result per content index; the index never outlives the payload it describes.
func (i *antigravityReplayRequestIndex) contextFingerprint(beforeContentIndex int) string {
	if i == nil {
		return ""
	}
	if fingerprint, ok := i.contextFingerprints[beforeContentIndex]; ok {
		return fingerprint
	}
	fingerprint := antigravityReplayContextFingerprint(i.payload, beforeContentIndex)
	i.contextFingerprints[beforeContentIndex] = fingerprint
	return fingerprint
}

// openAIChatContextFingerprint mirrors antigravityOpenAIReplayContextFingerprint
// against cached values; the cache never outlives the payload it describes.
func (i *antigravityReplayRequestIndex) openAIChatContextFingerprint(contentIndex int) string {
	if i == nil {
		return ""
	}
	if fingerprint, ok := i.openAIChatContextFingerprints[contentIndex]; ok {
		return fingerprint
	}
	fingerprint := antigravityOpenAIReplayContextFingerprint(i.payload, contentIndex)
	i.openAIChatContextFingerprints[contentIndex] = fingerprint
	return fingerprint
}

// contextMatches mirrors antigravityReplayItemContextMatches against the cached
// context fingerprints.
func (i *antigravityReplayRequestIndex) contextMatches(itemResult gjson.Result, contentIndex int) bool {
	expected := strings.TrimSpace(itemResult.Get("contextHash").String())
	if strings.HasPrefix(expected, antigravityOpenAIReplayContextPrefix) {
		return expected == i.openAIChatContextFingerprint(contentIndex)
	}
	return expected == "" || expected == i.contextFingerprint(contentIndex)
}

// functionCallPartLocation mirrors antigravityFunctionCallPartLocation: the
// first function call carrying callID in document order.
func (i *antigravityReplayRequestIndex) functionCallPartLocation(callID string) (antigravityReplayPartKey, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return antigravityReplayPartKey{}, false
	}
	key, ok := i.functionCallsByID[callID]
	return key, ok
}

// functionCallPartLocationForReplayWithSchemas mirrors
// antigravityFunctionCallPartLocationForReplayWithSchemas against the cached index.
func (i *antigravityReplayRequestIndex) functionCallPartLocationForReplayWithSchemas(itemResult gjson.Result, toolSchemas map[string]any) (antigravityReplayPartKey, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		return antigravityReplayPartKey{}, false
	}
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	candidateIDs := []string{callID}
	if stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if candidateID == "" {
			continue
		}
		key, found := i.functionCallPartLocation(candidateID)
		if !found {
			continue
		}
		if i.contextMatches(itemResult, key.contentIndex) {
			part, okPart := i.part(key)
			if okPart && antigravityFunctionCallMatchesReplayItem(part.Get("functionCall"), itemResult, toolSchemas) {
				return key, true
			}
			log.Debugf("antigravity replay: located call %q at contents[%d].parts[%d] but name/args did not match ledger item (opaque_id=%t)",
				name, key.contentIndex, key.partIndex, antigravityIsGeminiClaudeToolUseID(candidateID))
			return antigravityReplayPartKey{}, false
		}
		// The candidate ID matched exactly, so callID+name+args are already proven
		// identical. Only the surrounding context drifted, which invalidates the
		// cached signature but not the tool identity.
		log.Debugf("antigravity replay: exact tool ID match for %q at contents[%d].parts[%d] rejected by context hash (opaque_id=%t)",
			name, key.contentIndex, key.partIndex, antigravityIsGeminiClaudeToolUseID(candidateID))
		return antigravityReplayPartKey{}, false
	}
	if !i.validContents {
		return antigravityReplayPartKey{}, false
	}
	cachedCI := int(itemResult.Get("contentIndex").Int())
	if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
		if cachedCI < 0 || cachedCI >= len(i.contents) || !i.contextMatches(itemResult, cachedCI) {
			return antigravityReplayPartKey{}, false
		}
		wantedOccurrence := int(targetOccurrence.Int())
		occurrence := 0
		for pi, part := range i.contents[cachedCI].parts {
			fc := part.Get("functionCall")
			if !fc.Exists() || (antigravityIsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != antigravityGeminiClaudeToolUseID(callID, name, args.Raw)) || !antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				continue
			}
			if occurrence == wantedOccurrence {
				return antigravityReplayPartKey{contentIndex: cachedCI, partIndex: pi}, true
			}
			occurrence++
		}
		return antigravityReplayPartKey{}, false
	}

	matches := make([]antigravityReplayPartKey, 0, 1)
	for ci, content := range i.contents {
		if !i.contextMatches(itemResult, ci) {
			continue
		}
		for pi, part := range content.parts {
			fc := part.Get("functionCall")
			if !fc.Exists() || (antigravityIsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != antigravityGeminiClaudeToolUseID(callID, name, args.Raw)) {
				continue
			}
			if antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				matches = append(matches, antigravityReplayPartKey{contentIndex: ci, partIndex: pi})
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return antigravityReplayPartKey{}, false
}

// functionCallProvenanceLocation mirrors antigravityFunctionCallProvenanceLocation
// against the cached index.
func (i *antigravityReplayRequestIndex) functionCallProvenanceLocation(itemResult gjson.Result, toolSchemas map[string]any) (antigravityReplayPartKey, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if name == "" || !args.Exists() || callID == "" {
		return antigravityReplayPartKey{}, false
	}
	stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw)
	if stableID == "" || stableID == callID {
		return antigravityReplayPartKey{}, false
	}
	key, found := i.functionCallPartLocation(stableID)
	if !found {
		return antigravityReplayPartKey{}, false
	}
	part, okPart := i.part(key)
	if !okPart || !antigravityFunctionCallMatchesReplayItem(part.Get("functionCall"), itemResult, toolSchemas) {
		return antigravityReplayPartKey{}, false
	}
	return key, true
}

// thoughtSignaturePartIndex mirrors antigravityThoughtSignatureReplayPartPath
// against the cached index, returning the part coordinates instead of a path.
func (i *antigravityReplayRequestIndex) thoughtSignaturePartIndex(itemResult gjson.Result) (antigravityReplayPartKey, bool) {
	ci := int(itemResult.Get("contentIndex").Int())
	if !i.validContents || ci < 0 || ci >= len(i.contents) {
		return antigravityReplayPartKey{}, false
	}
	indexedContent := i.contents[ci]
	if !strings.EqualFold(indexedContent.role, "model") {
		return antigravityReplayPartKey{}, false
	}
	partArr := indexedContent.parts
	if partArr == nil {
		return antigravityReplayPartKey{}, false
	}
	targetKind := strings.TrimSpace(itemResult.Get("targetKind").String())
	targetHash := strings.TrimSpace(itemResult.Get("targetHash").String())
	// A target hash pins the signature to a part whose own bytes are unchanged,
	// which is all Gemini validates: the signature's own integrity, never its
	// binding to the surrounding history. Drift elsewhere in the conversation
	// therefore costs this signature nothing, so it is deliberately not gated on
	// the context fingerprint. The fallback below has no such proof and stays
	// gated.
	if targetHash != "" {
		if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
			wanted := int(targetOccurrence.Int())
			occurrence := 0
			for pi, part := range partArr {
				kind, fingerprint := antigravityReplayPartFingerprint(part)
				if fingerprint != targetHash || (targetKind != "" && kind != targetKind) {
					continue
				}
				if occurrence == wanted {
					return antigravityReplayPartKey{contentIndex: ci, partIndex: pi}, true
				}
				occurrence++
			}
			return antigravityReplayPartKey{}, false
		}
		pi := int(itemResult.Get("partIndex").Int())
		if pi >= 0 && pi < len(partArr) {
			kind, fingerprint := antigravityReplayPartFingerprint(partArr[pi])
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return antigravityReplayPartKey{contentIndex: ci, partIndex: pi}, true
			}
		}
		for pi, part := range partArr {
			kind, fingerprint := antigravityReplayPartFingerprint(part)
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return antigravityReplayPartKey{contentIndex: ci, partIndex: pi}, true
			}
		}
		return antigravityReplayPartKey{}, false
	}

	// No target hash: nothing proves which part this signature belongs to, so
	// only a matching context fingerprint makes the positional guess safe.
	if !i.contextMatches(itemResult, ci) {
		return antigravityReplayPartKey{}, false
	}
	pi := int(itemResult.Get("partIndex").Int())
	if pi >= 0 && pi < len(partArr) && partArr[pi].Type != gjson.Null {
		if kind, _ := antigravityReplayPartFingerprint(partArr[pi]); kind != "" {
			return antigravityReplayPartKey{contentIndex: ci, partIndex: pi}, true
		}
	}
	// Legacy cache entries may point at a streamed signature-only part after
	// multiple text chunks. Attach them to the last semantic part in the same
	// model content, never to a different turn.
	for candidate := len(partArr) - 1; candidate >= 0; candidate-- {
		if kind, _ := antigravityReplayPartFingerprint(partArr[candidate]); kind != "" {
			return antigravityReplayPartKey{contentIndex: ci, partIndex: candidate}, true
		}
	}
	return antigravityReplayPartKey{}, false
}

// antigravityReplayBatch applies replay items to small part fragments
// and splices them into the full request once. Stable provenance IDs can be
// restored on this path because their identity does not depend on context. If
// that restore would make a later item depend on stale context, the current
// segment ends and the caller retries that item against the flushed payload.
type antigravityReplayBatch struct {
	index                     *antigravityReplayRequestIndex
	replacements              map[antigravityReplayPartKey][]byte
	functionResponsePartsByID map[string][]antigravityReplayPartKey
	applied                   bool
	identityChanged           bool
}

func newAntigravityReplayBatch(index *antigravityReplayRequestIndex) *antigravityReplayBatch {
	batch := &antigravityReplayBatch{
		index:                     index,
		replacements:              make(map[antigravityReplayPartKey][]byte),
		functionResponsePartsByID: make(map[string][]antigravityReplayPartKey),
	}
	if index != nil {
		for callID, keys := range index.functionResponsePartsByID {
			batch.functionResponsePartsByID[callID] = append([]antigravityReplayPartKey(nil), keys...)
		}
	}
	return batch
}

func (b *antigravityReplayBatch) apply(item []byte, toolSchemas map[string]any) bool {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "thought_signature":
		return b.applyThoughtSignature(itemResult)
	case "function_call_part":
		return b.applyFunctionCallSignature(itemResult, toolSchemas)
	default:
		return true
	}
}

func (b *antigravityReplayBatch) part(key antigravityReplayPartKey) (gjson.Result, bool) {
	if b == nil || b.index == nil || key.contentIndex < 0 || key.contentIndex >= len(b.index.contents) {
		return gjson.Result{}, false
	}
	parts := b.index.contents[key.contentIndex].parts
	if key.partIndex < 0 || key.partIndex >= len(parts) {
		return gjson.Result{}, false
	}
	if replacement, ok := b.replacements[key]; ok {
		return gjson.ParseBytes(replacement), true
	}
	return parts[key.partIndex], true
}

func (b *antigravityReplayBatch) setPart(key antigravityReplayPartKey, part []byte) bool {
	current, ok := b.part(key)
	changed := !ok || !bytes.Equal([]byte(current.Raw), part)
	b.replacements[key] = part
	return changed
}

func (b *antigravityReplayBatch) removeThoughtSignatureFromOtherParts(contentIndex int, signature string, keep antigravityReplayPartKey) bool {
	signature = strings.TrimSpace(signature)
	if signature == "" || b == nil || b.index == nil || contentIndex < 0 || contentIndex >= len(b.index.contents) {
		return false
	}
	changed := false
	for partIndex := range b.index.contents[contentIndex].parts {
		key := antigravityReplayPartKey{contentIndex: contentIndex, partIndex: partIndex}
		if key == keep {
			continue
		}
		part, ok := b.part(key)
		if !ok || antigravityNativePartThoughtSignature(part) != signature {
			continue
		}
		updated := []byte(part.Raw)
		for _, field := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
			updated, _ = sjson.DeleteBytes(updated, field)
		}
		changed = b.setPart(key, updated) || changed
	}
	return changed
}

func (b *antigravityReplayBatch) applyThoughtSignature(itemResult gjson.Result) bool {
	signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if signature == "" {
		return true
	}
	// Legacy positional items rely on the context fingerprint. Restoring a
	// stable function-call ID changes that fingerprint, while the immutable
	// batch index still describes the original payload.
	if b.identityChanged && strings.TrimSpace(itemResult.Get("targetHash").String()) == "" {
		return false
	}
	key, ok := b.index.thoughtSignaturePartIndex(itemResult)
	if !ok {
		return true
	}
	part, ok := b.part(key)
	if !ok {
		return false
	}
	if antigravityHasNativeThoughtSignature(part.Get("thoughtSignature").String()) {
		return true
	}
	updated, errSet := sjson.SetBytes([]byte(part.Raw), "thoughtSignature", signature)
	if errSet != nil {
		return false
	}
	b.removeThoughtSignatureFromOtherParts(key.contentIndex, signature, key)
	b.setPart(key, updated)
	// The sequential thought-signature path treats any successful Set as an
	// applied replay, even when a later item restores the original bytes.
	b.applied = true
	return true
}

func (b *antigravityReplayBatch) applyFunctionCallSignature(itemResult gjson.Result, toolSchemas map[string]any) bool {
	key, found := b.index.functionCallPartLocationForReplayWithSchemas(itemResult, toolSchemas)
	if !found {
		key, found = b.index.functionCallProvenanceLocation(itemResult, toolSchemas)
		if !found {
			return false
		}
	}
	part, ok := b.part(key)
	if !ok {
		return false
	}
	functionCall := part.Get("functionCall")
	nativeID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	currentID := strings.TrimSpace(functionCall.Get("id").String())
	nativeCall, okCall := antigravityNativeFunctionCallJSON(itemResult, nativeID)
	if !okCall {
		return false
	}
	sameNativeCall := currentID == nativeID && bytes.Equal(
		antigravityCanonicalReplayJSON([]byte(functionCall.Raw)),
		antigravityCanonicalReplayJSON(nativeCall),
	)
	stableID := antigravityGeminiClaudeToolUseID(nativeID, name, itemResult.Get("args").Raw)
	restoreStableID := currentID != nativeID && currentID == stableID
	if !sameNativeCall && !restoreStableID {
		return false
	}
	// The immutable lookup keeps the first location for an ID. If a malformed
	// history reuses a stable ID, sequential replay must rebuild after restoring
	// each occurrence so the next one becomes addressable.
	if restoreStableID && b.index.functionCallCountsByID[currentID] != 1 {
		return false
	}
	signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if sameNativeCall && (signature == "" || antigravityHasNativeThoughtSignature(part.Get("thoughtSignature").String())) {
		return true
	}
	// A native-ID item needs context to authorize a signature mutation. After
	// an earlier stable-ID restore that decision must be made against a rebuilt
	// index, so conservatively fall back to sequential replay.
	if b.identityChanged && currentID == nativeID {
		return false
	}
	if restoreStableID && !b.functionResponsesCanRestoreID(currentID, name) {
		return true
	}
	updated, errSet := sjson.SetRawBytes([]byte(part.Raw), "functionCall", nativeCall)
	if errSet != nil {
		return false
	}
	for _, field := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
		updated, _ = sjson.DeleteBytes(updated, field)
	}
	if signature != "" {
		updated, errSet = sjson.SetBytes(updated, "thoughtSignature", signature)
		if errSet != nil {
			return false
		}
	}

	// Stage every fragment before committing any of them. A function replay can
	// touch the call, matching responses, and duplicate signatures; a failed
	// staging attempt must not leak partial work into the preceding safe batch.
	pending := map[antigravityReplayPartKey][]byte{key: updated}
	if signature != "" {
		for partIndex := range b.index.contents[key.contentIndex].parts {
			otherKey := antigravityReplayPartKey{contentIndex: key.contentIndex, partIndex: partIndex}
			if otherKey == key {
				continue
			}
			otherPart, okPart := b.part(otherKey)
			if !okPart || antigravityNativePartThoughtSignature(otherPart) != signature {
				continue
			}
			otherUpdated := []byte(otherPart.Raw)
			for _, field := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
				otherUpdated, _ = sjson.DeleteBytes(otherUpdated, field)
			}
			pending[otherKey] = otherUpdated
		}
	}
	if restoreStableID {
		if !b.stageFunctionResponseIDRestores(pending, currentID, nativeID, name) {
			return false
		}
	}
	itemChanged := false
	for pendingKey, pendingPart := range pending {
		itemChanged = b.setPart(pendingKey, pendingPart) || itemChanged
	}
	if restoreStableID {
		keys := b.functionResponsePartsByID[currentID]
		if len(keys) > 0 {
			delete(b.functionResponsePartsByID, currentID)
			b.functionResponsePartsByID[nativeID] = append(b.functionResponsePartsByID[nativeID], keys...)
		}
		b.identityChanged = true
	}
	b.applied = itemChanged || b.applied
	return true
}

func (b *antigravityReplayBatch) functionResponsesCanRestoreID(currentID, nativeName string) bool {
	if currentID == "" {
		return true
	}
	for _, key := range b.functionResponsePartsByID[currentID] {
		part, ok := b.part(key)
		if !ok {
			return false
		}
		response := part.Get("functionResponse")
		if !response.Exists() || strings.TrimSpace(response.Get("id").String()) != currentID {
			return false
		}
		name := strings.TrimSpace(response.Get("name").String())
		if name != "" && name != "unknown" && name != nativeName {
			return false
		}
	}
	return true
}

func (b *antigravityReplayBatch) stageFunctionResponseIDRestores(
	pending map[antigravityReplayPartKey][]byte,
	currentID, nativeID, nativeName string,
) bool {
	if currentID == "" || nativeID == "" || nativeName == "" || currentID == nativeID {
		return true
	}
	for _, key := range b.functionResponsePartsByID[currentID] {
		part, ok := b.part(key)
		if replacement, exists := pending[key]; exists {
			part = gjson.ParseBytes(replacement)
			ok = true
		}
		if !ok {
			return false
		}
		response := part.Get("functionResponse")
		if !response.Exists() || strings.TrimSpace(response.Get("id").String()) != currentID {
			return false
		}
		updated, errSet := sjson.SetBytes([]byte(part.Raw), "functionResponse.id", nativeID)
		if errSet != nil {
			return false
		}
		updated, errSet = sjson.SetBytes(updated, "functionResponse.name", nativeName)
		if errSet != nil {
			return false
		}
		pending[key] = updated
	}
	return true
}

func (b *antigravityReplayBatch) build(payload []byte) ([]byte, bool) {
	if b == nil || b.index == nil || len(b.replacements) == 0 {
		return payload, true
	}
	outputSize := len(payload)
	replacementCount := 0
	for key, replacement := range b.replacements {
		original, ok := b.indexedPart(key)
		if !ok {
			return nil, false
		}
		if bytes.Equal(replacement, []byte(original.Raw)) {
			continue
		}
		outputSize += len(replacement) - len(original.Raw)
		replacementCount++
	}
	if replacementCount == 0 || outputSize < 0 {
		return payload, true
	}
	out := make([]byte, 0, outputSize)
	last := 0
	applied := 0
	for contentIndex, content := range b.index.contents {
		for partIndex, part := range content.parts {
			key := antigravityReplayPartKey{contentIndex: contentIndex, partIndex: partIndex}
			replacement, ok := b.replacements[key]
			if !ok || bytes.Equal(replacement, []byte(part.Raw)) {
				continue
			}
			start := part.Index
			end := start + len(part.Raw)
			if start <= last || start < 0 || end < start || end > len(payload) {
				return nil, false
			}
			out = append(out, payload[last:start]...)
			out = append(out, replacement...)
			last = end
			applied++
		}
	}
	if applied != replacementCount {
		return nil, false
	}
	out = append(out, payload[last:]...)
	return out, true
}

func (b *antigravityReplayBatch) indexedPart(key antigravityReplayPartKey) (gjson.Result, bool) {
	if b == nil || b.index == nil {
		return gjson.Result{}, false
	}
	return b.index.part(key)
}

func filterAntigravityReasoningReplayItemsForRequest(payload []byte, items [][]byte) [][]byte {
	return filterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
}

func filterAntigravityReasoningReplayItemsForRequestWithSchemas(payload []byte, items [][]byte, toolSchemas map[string]any) [][]byte {
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "function_call_part":
			signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if ci, pi, foundCall := antigravityFunctionCallPartLocationForReplayWithSchemas(payload, itemResult, toolSchemas); foundCall {
				part := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d", ci, pi))
				currentID := strings.TrimSpace(part.Get("functionCall.id").String())
				nativeID := strings.TrimSpace(itemResult.Get("call_id").String())
				needsNativeRestore := currentID != nativeID || !bytes.Equal(antigravityCanonicalReplayJSON([]byte(part.Get("functionCall.args").Raw)), antigravityCanonicalReplayJSON([]byte(itemResult.Get("args").Raw)))
				if !needsNativeRestore && (signature == "" || antigravityHasNativeThoughtSignature(part.Get("thoughtSignature").String())) {
					continue
				}
				break
			}
			// Even without a context match, an exact opaque ID match can still
			// restore the native call identity.
			if _, _, foundProvenance := antigravityFunctionCallProvenanceLocation(payload, itemResult, toolSchemas); foundProvenance {
				break
			}
			callID := strings.TrimSpace(itemResult.Get("call_id").String())
			if callID == "" {
				continue
			}
			responseIndex, _, foundResponse := antigravityFunctionResponseContentIndexForReplay(payload, itemResult)
			if !foundResponse {
				continue
			}
			contextMatches := antigravityReplayItemContextMatches(payload, itemResult, responseIndex)
			if !contextMatches && responseIndex > 0 {
				previousRole := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.role", responseIndex-1)).String()
				contextMatches = strings.EqualFold(strings.TrimSpace(previousRole), "model") && antigravityReplayItemContextMatches(payload, itemResult, responseIndex-1)
			}
			if !contextMatches {
				continue
			}
		case "thought_signature":
			if antigravityRequestHasThoughtSignatureAt(payload, itemResult) {
				continue
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func antigravityExistingToolCallKeys(payload []byte) map[string]bool {
	existing := make(map[string]bool)
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return existing
	}
	for _, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if fc := part.Get("functionCall"); fc.Exists() {
				for _, key := range antigravityReplayToolCallKeysFromPart(fc) {
					existing[key] = true
				}
			}
		}
	}
	return existing
}

func antigravityReplayToolCallKeys(itemResult gjson.Result) []string {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	name := strings.TrimSpace(itemResult.Get("name").String())
	if name == "" {
		return nil
	}
	args := itemResult.Get("args").Raw
	key := antigravityFunctionCallKey(name, args, callID)
	if key == "" {
		return nil
	}
	return []string{key}
}

func antigravityReplayToolCallKeysFromPart(fc gjson.Result) []string {
	return antigravityReplayToolCallKeys(gjson.Parse(fc.Raw))
}

func antigravityFunctionCallKey(name, argsRaw, callID string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.TrimSpace(argsRaw) != "" {
		argsRaw = string(antigravityCanonicalReplayJSON([]byte(argsRaw)))
	}
	h := sha256.Sum256([]byte(strings.Join([]string{name, argsRaw, callID}, "\x00")))
	return fmt.Sprintf("fc:%x", h[:8])
}

func antigravityAnyKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func antigravityNeedsSignatureReplayForExistingFunctionCall(payload []byte, itemResult gjson.Result) bool {
	if strings.TrimSpace(itemResult.Get("thoughtSignature").String()) == "" {
		return false
	}
	ci, pi, ok := antigravityFunctionCallPartLocationForReplay(payload, itemResult)
	if !ok {
		return false
	}
	pathSig := fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", ci, pi)
	return !antigravityHasNativeThoughtSignature(gjson.GetBytes(payload, pathSig).String())
}

func antigravityRequestHasMatchingFunctionResponse(payload []byte, itemResult gjson.Result) bool {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		return true
	}
	_, _, ok := antigravityFunctionResponseContentIndexForReplay(payload, itemResult)
	return ok
}

func antigravityFunctionResponseContentIndexForReplay(payload []byte, itemResult gjson.Result) (int, string, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	candidateIDs := []string{callID}
	if stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if contentIndex, ok := antigravityFunctionResponseContentIndex(payload, candidateID); ok {
			return contentIndex, candidateID, true
		}
	}
	return -1, "", false
}

func antigravityFunctionResponseContentIndex(payload []byte, callID string) (int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, false
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return -1, false
	}
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			fr := part.Get("functionResponse")
			if fr.Exists() && strings.TrimSpace(fr.Get("id").String()) == callID {
				return i, true
			}
		}
	}
	return -1, false
}

func restoreAntigravityFunctionResponseReplayIdentity(payload []byte, currentID, nativeID, nativeName string) []byte {
	currentID = strings.TrimSpace(currentID)
	nativeID = strings.TrimSpace(nativeID)
	nativeName = strings.TrimSpace(nativeName)
	if currentID == "" || nativeID == "" || nativeName == "" || currentID == nativeID {
		return payload
	}
	out := payload
	contents := gjson.GetBytes(out, "request.contents")
	contents.ForEach(func(contentKey, content gjson.Result) bool {
		content.Get("parts").ForEach(func(partKey, part gjson.Result) bool {
			response := part.Get("functionResponse")
			if !response.Exists() || strings.TrimSpace(response.Get("id").String()) != currentID {
				return true
			}
			responsePath := fmt.Sprintf("request.contents.%d.parts.%d.functionResponse", contentKey.Int(), partKey.Int())
			out, _ = sjson.SetBytes(out, responsePath+".id", nativeID)
			out, _ = sjson.SetBytes(out, responsePath+".name", nativeName)
			return true
		})
		return true
	})
	return out
}

func antigravityPayloadHasFunctionCallID(payload []byte, callID string) bool {
	_, _, ok := antigravityFunctionCallPartLocation(payload, callID)
	return ok
}

func antigravityFunctionCallPartLocation(payload []byte, callID string) (contentIndex int, partIndex int, ok bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, -1, false
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for pi, part := range parts.Array() {
			fc := part.Get("functionCall")
			if fc.Exists() && strings.TrimSpace(fc.Get("id").String()) == callID {
				return ci, pi, true
			}
		}
	}
	return -1, -1, false
}

func antigravityFunctionCallPartLocationForReplay(payload []byte, itemResult gjson.Result) (contentIndex int, partIndex int, ok bool) {
	return antigravityFunctionCallPartLocationForReplayWithSchemas(payload, itemResult, nil)
}

func antigravityFunctionCallPartLocationForReplayWithSchemas(payload []byte, itemResult gjson.Result, toolSchemas map[string]any) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		return -1, -1, false
	}
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	candidateIDs := []string{callID}
	if stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if candidateID == "" {
			continue
		}
		ci, pi, found := antigravityFunctionCallPartLocation(payload, candidateID)
		if !found {
			continue
		}
		if antigravityReplayItemContextMatches(payload, itemResult, ci) {
			fc := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
			if antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				return ci, pi, true
			}
			log.Debugf("antigravity replay: located call %q at contents[%d].parts[%d] but name/args did not match ledger item (opaque_id=%t)",
				name, ci, pi, antigravityIsGeminiClaudeToolUseID(candidateID))
			return -1, -1, false
		}
		// The candidate ID matched exactly, so callID+name+args are already proven
		// identical. Only the surrounding context drifted, which invalidates the
		// cached signature but not the tool identity.
		log.Debugf("antigravity replay: exact tool ID match for %q at contents[%d].parts[%d] rejected by context hash (opaque_id=%t)",
			name, ci, pi, antigravityIsGeminiClaudeToolUseID(candidateID))
		return -1, -1, false
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	contentArr := contents.Array()
	cachedCI := int(itemResult.Get("contentIndex").Int())
	if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
		if cachedCI < 0 || cachedCI >= len(contentArr) || !antigravityReplayItemContextMatches(payload, itemResult, cachedCI) {
			return -1, -1, false
		}
		wantedOccurrence := int(targetOccurrence.Int())
		occurrence := 0
		for pi, part := range contentArr[cachedCI].Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() || (antigravityIsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != antigravityGeminiClaudeToolUseID(callID, name, args.Raw)) || !antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				continue
			}
			if occurrence == wantedOccurrence {
				return cachedCI, pi, true
			}
			occurrence++
		}
		return -1, -1, false
	}

	matches := make([][2]int, 0, 1)
	for ci, content := range contentArr {
		if !antigravityReplayItemContextMatches(payload, itemResult, ci) {
			continue
		}
		for pi, part := range content.Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() || (antigravityIsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != antigravityGeminiClaudeToolUseID(callID, name, args.Raw)) {
				continue
			}
			if antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				matches = append(matches, [2]int{ci, pi})
			}
		}
	}
	if len(matches) == 1 {
		return matches[0][0], matches[0][1], true
	}
	return -1, -1, false
}

// antigravityFunctionCallProvenanceLocation locates the function call whose
// Claude-facing opaque ID was derived from this exact ledger item.
//
// The opaque ID is sha256(call_id, name, args), so an exact match already proves
// that the call ID, tool name and arguments are identical to the provider-native
// call. The surrounding context hash adds nothing to that proof; it only decides
// whether the cached thoughtSignature is still valid. Callers therefore use this
// to recover tool identity after the context has drifted, without replaying any
// signature.
func antigravityFunctionCallProvenanceLocation(payload []byte, itemResult gjson.Result, toolSchemas map[string]any) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if name == "" || !args.Exists() || callID == "" {
		return -1, -1, false
	}
	stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw)
	if stableID == "" || stableID == callID {
		return -1, -1, false
	}
	ci, pi, found := antigravityFunctionCallPartLocation(payload, stableID)
	if !found {
		return -1, -1, false
	}
	fc := gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
	if !antigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
		return -1, -1, false
	}
	return ci, pi, true
}

func insertAntigravityModelFunctionCallBeforeContent(payload []byte, beforeIndex int, name, callID, thoughtSig string, args gjson.Result) ([]byte, bool) {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return payload, false
	}
	arr := contents.Array()
	if beforeIndex < 0 || beforeIndex > len(arr) {
		return payload, false
	}
	fc := map[string]any{"name": name}
	if callID != "" {
		fc["id"] = callID
	}
	if args.Exists() {
		fc["args"] = args.Value()
	}
	part := map[string]any{"functionCall": fc}
	if thoughtSig == "" {
		thoughtSig = "skip_thought_signature_validator"
	}
	part["thoughtSignature"] = thoughtSig
	newContent := map[string]any{
		"role":  "model",
		"parts": []any{part},
	}
	newArr := make([]any, 0, len(arr)+1)
	for i := 0; i < beforeIndex; i++ {
		newArr = append(newArr, arr[i].Value())
	}
	newArr = append(newArr, newContent)
	for i := beforeIndex; i < len(arr); i++ {
		newArr = append(newArr, arr[i].Value())
	}
	updated, err := sjson.SetBytes(payload, "request.contents", newArr)
	if err != nil {
		return payload, false
	}
	return updated, true
}

func appendAntigravityFunctionCallToModelContent(payload []byte, contentIndex int, name, callID, thoughtSig string, args gjson.Result) ([]byte, bool) {
	contentPath := fmt.Sprintf("request.contents.%d", contentIndex)
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, contentPath+".role").String()), "model") || !gjson.GetBytes(payload, contentPath+".parts").IsArray() {
		return payload, false
	}
	fc := map[string]any{"name": name}
	if callID != "" {
		fc["id"] = callID
	}
	if args.Exists() {
		fc["args"] = args.Value()
	}
	part := map[string]any{"functionCall": fc}
	if thoughtSig == "" {
		hasFunctionCall := false
		gjson.GetBytes(payload, contentPath+".parts").ForEach(func(_, existingPart gjson.Result) bool {
			hasFunctionCall = existingPart.Get("functionCall").Exists()
			return !hasFunctionCall
		})
		if !hasFunctionCall {
			thoughtSig = "skip_thought_signature_validator"
		}
	}
	if thoughtSig != "" {
		part["thoughtSignature"] = thoughtSig
	}
	updated, errSet := sjson.SetBytes(payload, contentPath+".parts.-1", part)
	if errSet != nil {
		return payload, false
	}
	return updated, true
}

func antigravityRemoveThoughtSignatureFromOtherParts(payload []byte, contentIndex int, signature, keepPartPath string) []byte {
	signature = strings.TrimSpace(signature)
	partsPath := fmt.Sprintf("request.contents.%d.parts", contentIndex)
	parts := gjson.GetBytes(payload, partsPath)
	if signature == "" || !parts.IsArray() {
		return payload
	}
	out := payload
	for partIndex, part := range parts.Array() {
		partPath := fmt.Sprintf("%s.%d", partsPath, partIndex)
		if partPath == keepPartPath || antigravityNativePartThoughtSignature(part) != signature {
			continue
		}
		for _, field := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
			out, _ = sjson.DeleteBytes(out, partPath+"."+field)
		}
	}
	return out
}

func antigravityRequestHasThoughtSignatureAt(payload []byte, itemResult gjson.Result) bool {
	partPath, ok := antigravityThoughtSignatureReplayPartPath(payload, itemResult)
	if !ok {
		return false
	}
	return antigravityHasNativeThoughtSignature(gjson.GetBytes(payload, partPath+".thoughtSignature").String())
}

func antigravityHasNativeThoughtSignature(signature string) bool {
	signature = strings.TrimSpace(signature)
	return signature != "" && signature != "skip_thought_signature_validator"
}

func antigravityReplayPartFingerprint(part gjson.Result) (kind, fingerprint string) {
	if part.Get("functionCall").Exists() || part.Get("functionResponse").Exists() {
		return "", ""
	}
	text := part.Get("text")
	if !text.Exists() {
		return "", ""
	}
	kind = "text"
	if part.Get("thought").Bool() {
		kind = "thought"
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + text.String()))
	return kind, fmt.Sprintf("%x", sum[:])
}

func antigravityReplayPartOccurrence(parts []gjson.Result, targetPartIndex int, targetKind, targetHash string) int {
	occurrence := 0
	for partIndex := 0; partIndex < targetPartIndex && partIndex < len(parts); partIndex++ {
		kind, fingerprint := antigravityReplayPartFingerprint(parts[partIndex])
		if kind == targetKind && fingerprint == targetHash {
			occurrence++
		}
	}
	return occurrence
}

const antigravityOpenAIReplayContextPrefix = "openai-chat-v1:"

func antigravityReplayContextFingerprint(payload []byte, beforeContentIndex int) string {
	return antigravityReplayContextFingerprintWithPolicy(payload, beforeContentIndex, true)
}

// antigravityOpenAIReplayContextFingerprint binds OpenAI chat replay history to
// the system instruction and preceding turns only, not the current turn's tool
// declarations or tool choice. The version tag keeps legacy and non-OpenAI
// ledger matching unchanged.
func antigravityOpenAIReplayContextFingerprint(payload []byte, beforeContentIndex int) string {
	return antigravityOpenAIReplayContextPrefix + antigravityReplayContextFingerprintWithPolicy(payload, beforeContentIndex, false)
}

func antigravityReplayContextFingerprintWithPolicy(payload []byte, beforeContentIndex int, includeToolPolicy bool) string {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() || beforeContentIndex < 0 {
		return ""
	}
	contentArr := contents.Array()
	if beforeContentIndex > len(contentArr) {
		return ""
	}
	paths := []string{"request.systemInstruction"}
	if includeToolPolicy {
		paths = append(paths, "request.tools", "request.toolConfig")
	}
	var context strings.Builder
	for _, path := range paths {
		if value := gjson.GetBytes(payload, path); value.Exists() {
			context.WriteString(path)
			context.WriteByte('\x00')
			context.Write(antigravityCanonicalReplayJSON([]byte(value.Raw)))
			context.WriteByte('\x00')
		}
	}
	for ci := 0; ci < beforeContentIndex; ci++ {
		content := contentArr[ci]
		context.WriteString(strings.ToLower(strings.TrimSpace(content.Get("role").String())))
		context.WriteByte('\x00')
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			normalized := []byte(part.Raw)
			for _, signaturePath := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
				normalized, _ = sjson.DeleteBytes(normalized, signaturePath)
			}
			context.Write(antigravityCanonicalReplayJSON(normalized))
			context.WriteByte('\x00')
			return true
		})
	}
	if context.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(context.String()))
	return fmt.Sprintf("%x", sum[:])
}

func antigravityReplayToolSchemasFromRequests(rawRequests ...[]byte) map[string]any {
	toolSchemas := make(map[string]any)
	for _, raw := range rawRequests {
		if len(raw) == 0 {
			continue
		}
		tools := gjson.GetBytes(raw, "tools")
		if !tools.IsArray() {
			continue
		}
		for _, tool := range tools.Array() {
			candidates := []gjson.Result{tool}
			if function := tool.Get("function"); function.Exists() {
				candidates = append(candidates, function)
			}
			for _, candidate := range candidates {
				name := strings.TrimSpace(candidate.Get("name").String())
				if name == "" {
					continue
				}
				var schema gjson.Result
				for _, path := range []string{"input_schema", "parameters", "parametersJsonSchema"} {
					if value := candidate.Get(path); value.Exists() && value.IsObject() {
						schema = value
						break
					}
				}
				if !schema.Exists() {
					continue
				}
				var schemaValue any
				if json.Unmarshal([]byte(schema.Raw), &schemaValue) != nil {
					continue
				}
				if _, exists := toolSchemas[name]; !exists {
					toolSchemas[name] = schemaValue
				}
			}
		}
	}
	return toolSchemas
}

func antigravityReplayJSONValue(result gjson.Result) (any, bool) {
	raw := result.Raw
	if result.Type == gjson.String {
		raw = result.String()
	}
	var value any
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &value) != nil {
		return nil, false
	}
	return value, true
}

func antigravityNormalizeReplayToolValue(value, schema any) any {
	schemaObject, _ := schema.(map[string]any)
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		properties, _ := schemaObject["properties"].(map[string]any)
		for key, child := range typed {
			childSchema := properties[key]
			normalizedChild := antigravityNormalizeReplayToolValue(child, childSchema)
			if propertySchema, ok := childSchema.(map[string]any); ok {
				if defaultValue, hasDefault := propertySchema["default"]; hasDefault && reflect.DeepEqual(normalizedChild, antigravityNormalizeReplayToolValue(defaultValue, childSchema)) {
					continue
				}
			}
			normalized[key] = normalizedChild
		}
		return normalized
	case []any:
		itemSchema := schemaObject["items"]
		normalized := make([]any, len(typed))
		for index, child := range typed {
			normalized[index] = antigravityNormalizeReplayToolValue(child, itemSchema)
		}
		return normalized
	default:
		return value
	}
}

func antigravityFunctionCallMatchesReplayItem(functionCall, itemResult gjson.Result, toolSchemas map[string]any) bool {
	name := strings.TrimSpace(itemResult.Get("name").String())
	if name == "" || strings.TrimSpace(functionCall.Get("name").String()) != name {
		return false
	}
	currentArgs := functionCall.Get("args")
	nativeArgs := itemResult.Get("args")
	if !currentArgs.Exists() || !nativeArgs.Exists() {
		return false
	}
	if bytes.Equal(antigravityCanonicalReplayJSON([]byte(currentArgs.Raw)), antigravityCanonicalReplayJSON([]byte(nativeArgs.Raw))) {
		return true
	}
	schema, okSchema := toolSchemas[name]
	if !okSchema {
		return false
	}
	currentValue, okCurrent := antigravityReplayJSONValue(currentArgs)
	nativeValue, okNative := antigravityReplayJSONValue(nativeArgs)
	if !okCurrent || !okNative {
		return false
	}
	return reflect.DeepEqual(antigravityNormalizeReplayToolValue(currentValue, schema), antigravityNormalizeReplayToolValue(nativeValue, schema))
}

func antigravityPayloadHasClaudeToolProvenanceID(payload []byte) bool {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return false
	}
	found := false
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		hasReservedID := func(part gjson.Result) bool {
			for _, path := range []string{"functionCall.id", "functionResponse.id"} {
				if antigravityIsGeminiClaudeToolUseID(part.Get(path).String()) {
					return true
				}
			}
			return false
		}
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				found = hasReservedID(part)
				return !found
			})
		} else if parts.Type != gjson.Null {
			// Result.Array returns a non-array JSON value as one item.
			found = hasReservedID(parts)
		}
		return !found
	})
	return found
}

// antigravitySyntheticToolCallID derives a deterministic neutral call ID for a
// reserved Claude-facing provenance ID that could not be resolved back to its
// provider-native call. It is stable across turns and never lands in the reserved
// namespace, so call/response pairs stay consistent without impersonating a
// provider-issued ID.
func antigravitySyntheticToolCallID(reservedID string) string {
	sum := sha256.Sum256([]byte("antigravity-degraded-tool-call\x00" + reservedID))
	return fmt.Sprintf("call_%x", sum[:6])
}

// degradeAntigravityClaudeToolProvenanceIDs rewrites unresolved reserved tool
// provenance IDs to neutral synthetic IDs so a conversation survives a replay
// ledger miss instead of failing closed forever.
//
// The same reserved ID always maps to the same synthetic ID, so functionCall and
// functionResponse stay paired. Whatever signature the client carried in-band is
// kept: Gemini validates a thought signature's own integrity, not its binding to
// the call ID or the surrounding history, so rewriting the ID does not invalidate
// it. Calls left with no signature at all get the leading bypass sentinel from
// antigravityRepairUnsignedFirstFunctionCalls. Every other part is left alone,
// preserving the native "1 signed + N unsigned" parallel-call shape.
func degradeAntigravityClaudeToolProvenanceIDs(payload []byte) ([]byte, int) {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return payload, 0
	}
	// Mutate each small part independently, then splice all rewrites into the
	// request with one body copy. One sjson.SetBytes per part made large
	// histories scale with history size multiplied by the number of rewrites.
	type partReplacement struct {
		start int
		end   int
		data  []byte
	}
	replacements := make([]partReplacement, 0)
	for _, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if fc := part.Get("functionCall"); fc.Exists() {
				id := strings.TrimSpace(fc.Get("id").String())
				if !antigravityIsGeminiClaudeToolUseID(id) {
					continue
				}
				updatedPart, _ := sjson.SetBytes([]byte(part.Raw), "functionCall.id", antigravitySyntheticToolCallID(id))
				replacements = append(replacements, partReplacement{
					start: part.Index,
					end:   part.Index + len(part.Raw),
					data:  updatedPart,
				})
				continue
			}
			if fr := part.Get("functionResponse"); fr.Exists() {
				id := strings.TrimSpace(fr.Get("id").String())
				if !antigravityIsGeminiClaudeToolUseID(id) {
					continue
				}
				updatedPart, _ := sjson.SetBytes([]byte(part.Raw), "functionResponse.id", antigravitySyntheticToolCallID(id))
				replacements = append(replacements, partReplacement{
					start: part.Index,
					end:   part.Index + len(part.Raw),
					data:  updatedPart,
				})
			}
		}
	}
	if len(replacements) == 0 {
		return payload, 0
	}
	outputSize := len(payload)
	for _, replacement := range replacements {
		outputSize += len(replacement.data) - (replacement.end - replacement.start)
	}
	out := make([]byte, 0, outputSize)
	last := 0
	for _, replacement := range replacements {
		if replacement.start <= last || replacement.end > len(payload) {
			return payload, 0
		}
		out = append(out, payload[last:replacement.start]...)
		out = append(out, replacement.data...)
		last = replacement.end
	}
	out = append(out, payload[last:]...)
	return out, len(replacements)
}

// antigravityRepairUnsignedFirstFunctionCalls restores Gemini's bypass sentinel on
// the first function call of any model turn that replay left completely unsigned.
//
// Gemini rejects a model turn whose leading functionCall carries no
// thoughtSignature. The request-level sanitizer enforces that invariant, but it
// runs before reasoning replay, and replay can legitimately drop a signature
// afterwards: a degraded call loses one, and an identity-only restore on drifted
// context deliberately declines to replay one. Only a missing signature is filled
// in here, so native signatures are never touched.
func antigravityRepairUnsignedFirstFunctionCalls(payload []byte) []byte {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	out := payload
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "model") {
			return true
		}
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(partIndex, part gjson.Result) bool {
			if !part.Get("functionCall").Exists() {
				return true
			}
			if antigravityNativePartThoughtSignature(part) == "" {
				path := fmt.Sprintf(
					"request.contents.%d.parts.%d.thoughtSignature",
					contentIndex.Int(),
					partIndex.Int(),
				)
				out, _ = sjson.SetBytes(out, path, "skip_thought_signature_validator")
			}
			// Only the first function call of a turn needs a signature; siblings stay
			// unsigned to preserve the native parallel-call shape.
			return false
		})
		return true
	})
	return out
}

func antigravityCanonicalReplayJSON(raw []byte) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return bytes.TrimSpace(raw)
	}
	canonical, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return bytes.TrimSpace(raw)
	}
	return canonical
}

func antigravityReplayItemContextMatches(payload []byte, itemResult gjson.Result, contentIndex int) bool {
	expected := strings.TrimSpace(itemResult.Get("contextHash").String())
	if strings.HasPrefix(expected, antigravityOpenAIReplayContextPrefix) {
		return expected == antigravityOpenAIReplayContextFingerprint(payload, contentIndex)
	}
	return expected == "" || expected == antigravityReplayContextFingerprint(payload, contentIndex)
}

func antigravitySetReplayItemContextHash(item []byte, payload []byte, contentIndex int) []byte {
	if contextHash := antigravityReplayContextFingerprint(payload, contentIndex); contextHash != "" {
		item, _ = sjson.SetBytes(item, "contextHash", contextHash)
	}
	return item
}

func antigravitySetOpenAIReplayItemContextHash(item []byte, payload []byte, contentIndex int) []byte {
	if contextHash := antigravityOpenAIReplayContextFingerprint(payload, contentIndex); contextHash != "" {
		item, _ = sjson.SetBytes(item, "contextHash", contextHash)
	}
	return item
}

func antigravityThoughtSignatureReplayPartPath(payload []byte, itemResult gjson.Result) (string, bool) {
	ci := int(itemResult.Get("contentIndex").Int())
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return "", false
	}
	contentArr := contents.Array()
	if ci < 0 || ci >= len(contentArr) || !strings.EqualFold(strings.TrimSpace(contentArr[ci].Get("role").String()), "model") {
		return "", false
	}
	parts := contentArr[ci].Get("parts")
	if !parts.IsArray() {
		return "", false
	}
	partArr := parts.Array()
	targetKind := strings.TrimSpace(itemResult.Get("targetKind").String())
	targetHash := strings.TrimSpace(itemResult.Get("targetHash").String())
	// A target hash pins the signature to a part whose own bytes are unchanged,
	// which is all Gemini validates: the signature's own integrity, never its
	// binding to the surrounding history. Drift elsewhere in the conversation
	// therefore costs this signature nothing, so it is deliberately not gated on
	// the context fingerprint. The fallback below has no such proof and stays
	// gated.
	if targetHash != "" {
		if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
			wanted := int(targetOccurrence.Int())
			occurrence := 0
			for pi, part := range partArr {
				kind, fingerprint := antigravityReplayPartFingerprint(part)
				if fingerprint != targetHash || (targetKind != "" && kind != targetKind) {
					continue
				}
				if occurrence == wanted {
					return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
				}
				occurrence++
			}
			return "", false
		}
		pi := int(itemResult.Get("partIndex").Int())
		if pi >= 0 && pi < len(partArr) {
			kind, fingerprint := antigravityReplayPartFingerprint(partArr[pi])
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		for pi, part := range partArr {
			kind, fingerprint := antigravityReplayPartFingerprint(part)
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		return "", false
	}

	// No target hash: nothing proves which part this signature belongs to, so
	// only a matching context fingerprint makes the positional guess safe.
	if !antigravityReplayItemContextMatches(payload, itemResult, ci) {
		return "", false
	}
	pi := int(itemResult.Get("partIndex").Int())
	if pi >= 0 && pi < len(partArr) && partArr[pi].Type != gjson.Null {
		if kind, _ := antigravityReplayPartFingerprint(partArr[pi]); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
		}
	}
	// Legacy cache entries may point at a streamed signature-only part after
	// multiple text chunks. Attach them to the last semantic part in the same
	// model content, never to a different turn.
	for candidate := len(partArr) - 1; candidate >= 0; candidate-- {
		if kind, _ := antigravityReplayPartFingerprint(partArr[candidate]); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, candidate), true
		}
	}
	return "", false
}

func antigravityExistingReplayPartPath(payload []byte, contentIndex int, partIndex int) (string, bool) {
	if contentIndex < 0 || partIndex < 0 {
		return "", false
	}
	partsPath := fmt.Sprintf("request.contents.%d.parts", contentIndex)
	parts := gjson.GetBytes(payload, partsPath)
	if !parts.IsArray() {
		return "", false
	}
	arr := parts.Array()
	if partIndex >= len(arr) || arr[partIndex].Type == gjson.Null {
		return "", false
	}
	return fmt.Sprintf("%s.%d", partsPath, partIndex), true
}

func antigravityReplayPartWritePath(payload []byte, contentIndex int, partIndex int) string {
	if path, ok := antigravityExistingReplayPartPath(payload, contentIndex, partIndex); ok {
		return path
	}
	partsPath := fmt.Sprintf("request.contents.%d.parts", contentIndex)
	if gjson.GetBytes(payload, partsPath).IsArray() {
		return partsPath + ".-1"
	}
	return partsPath + ".0"
}

func insertAntigravityReasoningReplayItems(payload []byte, items [][]byte) ([]byte, bool) {
	return insertAntigravityReasoningReplayItemsWithSchemas(payload, items, nil)
}

func insertAntigravityReasoningReplayItemsWithSchemas(payload []byte, items [][]byte, toolSchemas map[string]any) ([]byte, bool) {
	out := payload
	changed := false
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "thought_signature":
			sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if sig == "" {
				continue
			}
			partPath, exists := antigravityThoughtSignatureReplayPartPath(out, itemResult)
			if !exists {
				continue
			}
			path := partPath + ".thoughtSignature"
			if antigravityHasNativeThoughtSignature(gjson.GetBytes(out, path).String()) {
				continue
			}
			ci := int(itemResult.Get("contentIndex").Int())
			out = antigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
			updated, err := sjson.SetBytes(out, path, sig)
			if err != nil {
				continue
			}
			out = updated
			changed = true
		case "function_call_part":
			updated, ok := mergeAntigravityFunctionCallPartReplayWithSchemas(out, itemResult, toolSchemas)
			if ok {
				out = updated
				changed = true
			}
		}
	}
	return out, changed
}

func antigravityNativeFunctionCallJSON(itemResult gjson.Result, fallbackID string) ([]byte, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		return nil, false
	}
	functionCall := []byte(`{"name":""}`)
	functionCall, _ = sjson.SetBytes(functionCall, "name", name)
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = fallbackID
	}
	if callID != "" {
		functionCall, _ = sjson.SetBytes(functionCall, "id", callID)
	}
	if args.Type == gjson.String {
		if parsed := gjson.Parse(args.String()); parsed.Exists() {
			functionCall, _ = sjson.SetRawBytes(functionCall, "args", []byte(parsed.Raw))
		} else {
			functionCall, _ = sjson.SetBytes(functionCall, "args", args.String())
		}
	} else {
		functionCall, _ = sjson.SetRawBytes(functionCall, "args", []byte(args.Raw))
	}
	return functionCall, true
}

func antigravityFunctionResponsesCanRestoreID(payload []byte, currentID, nativeName string) bool {
	if currentID == "" {
		return true
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return false
	}
	valid := true
	contents.ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			response := part.Get("functionResponse")
			if !response.Exists() || strings.TrimSpace(response.Get("id").String()) != currentID {
				return true
			}
			name := strings.TrimSpace(response.Get("name").String())
			valid = name == "" || name == "unknown" || name == nativeName
			return valid
		})
		return valid
	})
	return valid
}

// restoreAntigravityNativeFunctionCallReplay rewrites one function call part back
// to its provider-native identity. allowSignature reports whether the cached
// thoughtSignature may be replayed as well; identity-only restores pass false
// because the surrounding context no longer matches the one the signature was
// issued for.
func restoreAntigravityNativeFunctionCallReplay(payload []byte, contentIndex, partIndex int, itemResult gjson.Result, allowLegacyIDRestore, allowSignature bool) ([]byte, bool) {
	partPath := fmt.Sprintf("request.contents.%d.parts.%d", contentIndex, partIndex)
	currentCall := gjson.GetBytes(payload, partPath+".functionCall")
	if !currentCall.Exists() {
		return payload, false
	}
	currentID := strings.TrimSpace(currentCall.Get("id").String())
	nativeID := strings.TrimSpace(itemResult.Get("call_id").String())
	nativeName := strings.TrimSpace(itemResult.Get("name").String())
	restoreIdentity := currentID == nativeID || antigravityIsGeminiClaudeToolUseID(currentID) || allowLegacyIDRestore
	if !restoreIdentity {
		signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
		if !allowSignature || signature == "" || antigravityHasNativeThoughtSignature(gjson.GetBytes(payload, partPath+".thoughtSignature").String()) {
			return payload, false
		}
		payload = antigravityRemoveThoughtSignatureFromOtherParts(payload, contentIndex, signature, partPath)
		updated, errSet := sjson.SetBytes(payload, partPath+".thoughtSignature", signature)
		return updated, errSet == nil
	}
	if currentID != nativeID && !antigravityFunctionResponsesCanRestoreID(payload, currentID, nativeName) {
		return payload, false
	}
	nativeCall, okCall := antigravityNativeFunctionCallJSON(itemResult, currentID)
	if !okCall {
		return payload, false
	}
	out, errSet := sjson.SetRawBytes(payload, partPath+".functionCall", nativeCall)
	if errSet != nil {
		return payload, false
	}
	for _, field := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
		out, _ = sjson.DeleteBytes(out, partPath+"."+field)
	}
	if signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String()); allowSignature && signature != "" {
		out = antigravityRemoveThoughtSignatureFromOtherParts(out, contentIndex, signature, partPath)
		out, _ = sjson.SetBytes(out, partPath+".thoughtSignature", signature)
	}
	if currentID != "" && nativeID != "" && currentID != nativeID {
		contents := gjson.GetBytes(out, "request.contents")
		contents.ForEach(func(contentKey, content gjson.Result) bool {
			content.Get("parts").ForEach(func(partKey, part gjson.Result) bool {
				response := part.Get("functionResponse")
				if !response.Exists() || strings.TrimSpace(response.Get("id").String()) != currentID {
					return true
				}
				responsePath := fmt.Sprintf("request.contents.%d.parts.%d.functionResponse", contentKey.Int(), partKey.Int())
				out, _ = sjson.SetBytes(out, responsePath+".id", nativeID)
				out, _ = sjson.SetBytes(out, responsePath+".name", nativeName)
				return true
			})
			return true
		})
	}
	return out, !bytes.Equal(out, payload)
}

func mergeAntigravityFunctionCallPartReplay(payload []byte, itemResult gjson.Result) ([]byte, bool) {
	return mergeAntigravityFunctionCallPartReplayWithSchemas(payload, itemResult, nil)
}

func mergeAntigravityFunctionCallPartReplayWithSchemas(payload []byte, itemResult gjson.Result, toolSchemas map[string]any) ([]byte, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if name == "" || !args.Exists() {
		return payload, false
	}
	if ci, pi, exists := antigravityFunctionCallPartLocationForReplayWithSchemas(payload, itemResult, toolSchemas); exists {
		_, allowLegacyIDRestore := toolSchemas[name]
		return restoreAntigravityNativeFunctionCallReplay(payload, ci, pi, itemResult, allowLegacyIDRestore, true)
	}
	// The context drifted, but an exact opaque ID match still proves this call's
	// identity. Gemini validates a thought signature's own integrity and nothing
	// about the history around it, so the drift costs the signature nothing: restore
	// the native call and its signature rather than making the model re-reason.
	if ci, pi, exists := antigravityFunctionCallProvenanceLocation(payload, itemResult, toolSchemas); exists {
		return restoreAntigravityNativeFunctionCallReplay(payload, ci, pi, itemResult, false, true)
	}
	if callID != "" {
		stableID := antigravityGeminiClaudeToolUseID(callID, name, args.Raw)
		if antigravityPayloadHasFunctionCallID(payload, callID) || (stableID != "" && antigravityPayloadHasFunctionCallID(payload, stableID)) {
			// The call is already in the history under its native or Claude-facing
			// ID, and neither lookup above accepted it, so the client changed it.
			// Never replay an opaque signature onto that changed call, and never
			// insert a second copy of it further down.
			return payload, false
		}
		if frIndex, currentResponseID, ok := antigravityFunctionResponseContentIndexForReplay(payload, itemResult); ok {
			parallelModelIndex := frIndex - 1
			if parallelModelIndex >= 0 && strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, fmt.Sprintf("request.contents.%d.role", parallelModelIndex)).String()), "model") && antigravityReplayItemContextMatches(payload, itemResult, parallelModelIndex) {
				if updated, appended := appendAntigravityFunctionCallToModelContent(payload, parallelModelIndex, name, callID, sig, args); appended {
					return restoreAntigravityFunctionResponseReplayIdentity(updated, currentResponseID, callID, name), true
				}
			}
			if antigravityReplayItemContextMatches(payload, itemResult, frIndex) {
				if updated, inserted := insertAntigravityModelFunctionCallBeforeContent(payload, frIndex, name, callID, sig, args); inserted {
					return restoreAntigravityFunctionResponseReplayIdentity(updated, currentResponseID, callID, name), true
				}
			}
		}
	} else {
		// Without a native call ID, only an exact semantic match is safe. Never
		// put an opaque signature on a different call at the old numeric slot.
		return payload, false
	}

	ci := antigravityReasoningReplayResolveContentIndex(payload, int(itemResult.Get("contentIndex").Int()))
	if ci < 0 || !antigravityReplayItemContextMatches(payload, itemResult, ci) {
		return payload, false
	}
	pi := int(itemResult.Get("partIndex").Int())
	out := payload
	changed := false

	partPath, exists := antigravityExistingReplayPartPath(out, ci, pi)
	if !exists {
		fc := map[string]any{"name": name}
		if callID != "" {
			fc["id"] = callID
		}
		if args.Type == gjson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		part := map[string]any{"functionCall": fc}
		if sig != "" {
			part["thoughtSignature"] = sig
		}
		if updated, err := sjson.SetBytes(out, antigravityReplayPartWritePath(out, ci, pi), part); err == nil {
			return updated, true
		}
		return payload, false
	}

	pathSig := partPath + ".thoughtSignature"
	if sig != "" && !antigravityHasNativeThoughtSignature(gjson.GetBytes(out, pathSig).String()) {
		out = antigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
		if updated, err := sjson.SetBytes(out, pathSig, sig); err == nil {
			out = updated
			changed = true
		}
	}
	pathFC := partPath + ".functionCall"
	if !gjson.GetBytes(out, pathFC).Exists() {
		fc := map[string]any{"name": name}
		if callID != "" {
			fc["id"] = callID
		}
		if args.Type == gjson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		if updated, err := sjson.SetBytes(out, pathFC, fc); err == nil {
			out = updated
			changed = true
		}
	}
	return out, changed
}

type antigravityPendingThoughtSignature struct {
	signature  string
	targetKind string
}

type antigravityReasoningReplayAccumulator struct {
	scope                   antigravityReasoningReplayScope
	requestPayload          []byte
	items                   [][]byte
	seenFC                  map[string]bool
	seenSignatures          map[string]bool
	segmentOccurrences      map[string]int
	functionCallOccurrences map[string]int
	contentIndex            int
	nextPartIndex           int
	visibleText             strings.Builder
	thoughtText             strings.Builder
	visiblePartIndex        int
	thoughtPartIndex        int
	lastResponseKind        string
	pendingSignatures       []antigravityPendingThoughtSignature
	itemBytes               int
	overflow                bool
	terminal                bool
}

// stampContextHash binds one ledger item to this request's context fingerprint.
// OpenAI chat callers use the tool-policy-free variant so a later turn that
// changes tool_choice still replays signatures cached by an earlier turn.
func (a *antigravityReasoningReplayAccumulator) stampContextHash(item []byte, contentIndex int) []byte {
	if a == nil {
		return item
	}
	if a.scope.openAIChat {
		return antigravitySetOpenAIReplayItemContextHash(item, a.requestPayload, contentIndex)
	}
	return antigravitySetReplayItemContextHash(item, a.requestPayload, contentIndex)
}

func newAntigravityReasoningReplayAccumulator(scope antigravityReasoningReplayScope, requestPayload []byte) *antigravityReasoningReplayAccumulator {
	if !scope.valid() {
		return nil
	}
	contentIndex, basePartIndex := antigravityReasoningReplayPendingModelContentIndex(requestPayload)
	items := antigravityReasoningReplayItemsFromRequest(requestPayload)
	seenSignatures := make(map[string]bool, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		if signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String()); signature != "" {
			seenSignatures[signature] = true
		}
	}
	itemBytes := 0
	for _, item := range items {
		itemBytes += len(item)
	}
	segmentOccurrences := make(map[string]int)
	functionCallOccurrences := make(map[string]int)
	if parts := gjson.GetBytes(requestPayload, fmt.Sprintf("request.contents.%d.parts", contentIndex)); parts.IsArray() {
		parts.ForEach(func(_, part gjson.Result) bool {
			if fc := part.Get("functionCall"); fc.Exists() {
				key := antigravityFunctionCallKey(fc.Get("name").String(), fc.Get("args").Raw, "")
				if key != "" {
					functionCallOccurrences[key]++
				}
				return true
			}
			if kind, fingerprint := antigravityReplayPartFingerprint(part); fingerprint != "" {
				segmentOccurrences[kind+"\x00"+fingerprint]++
			}
			return true
		})
	}
	if scope.openAIChat {
		for itemIndex, item := range items {
			cachedContentIndex := int(gjson.GetBytes(item, "contentIndex").Int())
			items[itemIndex] = antigravitySetOpenAIReplayItemContextHash(item, requestPayload, cachedContentIndex)
		}
	}
	return &antigravityReasoningReplayAccumulator{
		scope:                   scope,
		requestPayload:          append([]byte(nil), requestPayload...),
		items:                   items,
		seenFC:                  make(map[string]bool),
		seenSignatures:          seenSignatures,
		segmentOccurrences:      segmentOccurrences,
		functionCallOccurrences: functionCallOccurrences,
		contentIndex:            contentIndex,
		nextPartIndex:           basePartIndex,
		visiblePartIndex:        -1,
		thoughtPartIndex:        -1,
		itemBytes:               itemBytes,
		overflow:                len(items) > internalcache.AntigravityReasoningReplayCacheMaxItemsPerEntry || itemBytes > internalcache.AntigravityReasoningReplayCacheMaxBytesPerEntry,
	}
}

func antigravityReasoningReplayItemsFromRequest(payload []byte) [][]byte {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return nil
	}
	items := make([][]byte, 0)
	contents.ForEach(func(contentKey, content gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "model") {
			return true
		}
		ci := int(contentKey.Int())
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		partArr := parts.Array()
		functionCallOccurrences := make(map[string]int)
		for pi, part := range partArr {
			signature := antigravityNativePartThoughtSignature(part)
			if !antigravityHasNativeThoughtSignature(signature) {
				signature = ""
			}
			if fc := part.Get("functionCall"); fc.Exists() {
				key := antigravityFunctionCallKey(fc.Get("name").String(), fc.Get("args").Raw, "")
				occurrence := functionCallOccurrences[key]
				if key != "" {
					functionCallOccurrences[key] = occurrence + 1
				}
				if item := buildAntigravityFunctionCallPartItem(ci, pi, occurrence, fc, signature); len(item) > 0 {
					items = append(items, antigravitySetReplayItemContextHash(item, payload, ci))
				}
				continue
			}
			if signature == "" {
				continue
			}
			targetPart := part
			targetPI := pi
			kind, fingerprint := antigravityReplayPartFingerprint(targetPart)
			if fingerprint == "" && pi > 0 {
				targetPI = pi - 1
				targetPart = partArr[targetPI]
				kind, fingerprint = antigravityReplayPartFingerprint(targetPart)
			}
			if fingerprint == "" {
				continue
			}
			item := buildAntigravityThoughtSignatureItem(ci, targetPI, signature, kind, fingerprint)
			item, _ = sjson.SetBytes(item, "targetOccurrence", antigravityReplayPartOccurrence(partArr, targetPI, kind, fingerprint))
			items = append(items, antigravitySetReplayItemContextHash(item, payload, ci))
		}
		return true
	})
	return items
}

func (a *antigravityReasoningReplayAccumulator) appendItem(item []byte) {
	if a == nil || len(item) == 0 || a.overflow {
		return
	}
	if len(a.items)+1 > internalcache.AntigravityReasoningReplayCacheMaxItemsPerEntry || a.itemBytes+len(item) > internalcache.AntigravityReasoningReplayCacheMaxBytesPerEntry {
		a.overflow = true
		return
	}
	a.items = append(a.items, item)
	a.itemBytes += len(item)
}

func (a *antigravityReasoningReplayAccumulator) attachDetachedSignatureToLastFunctionCall(signature string) {
	if a == nil || signature == "" {
		return
	}
	for itemIndex := len(a.items) - 1; itemIndex >= 0; itemIndex-- {
		item := gjson.ParseBytes(a.items[itemIndex])
		if item.Get("type").String() != "function_call_part" {
			continue
		}
		if strings.TrimSpace(item.Get("thoughtSignature").String()) != "" {
			return
		}
		updated, errSet := sjson.SetBytes(a.items[itemIndex], "thoughtSignature", signature)
		if errSet != nil {
			return
		}
		delta := len(updated) - len(a.items[itemIndex])
		if a.itemBytes+delta > internalcache.AntigravityReasoningReplayCacheMaxBytesPerEntry {
			a.overflow = true
			return
		}
		a.items[itemIndex] = updated
		a.itemBytes += delta
		return
	}
}

func (a *antigravityReasoningReplayAccumulator) ObserveSSELine(line []byte) {
	if a == nil {
		return
	}
	payload := helps.JSONPayload(line)
	if payload == nil {
		return
	}
	a.observeResponsePayload(payload)
}

func (a *antigravityReasoningReplayAccumulator) observeResponsePayload(payload []byte) {
	if finishReason := strings.TrimSpace(gjson.GetBytes(payload, "response.candidates.0.finishReason").String()); finishReason != "" {
		a.terminal = true
	}
	parts := gjson.GetBytes(payload, "response.candidates.0.content.parts")
	if !parts.IsArray() {
		return
	}
	parts.ForEach(func(_, part gjson.Result) bool {
		pi := a.nextPartIndex
		a.nextPartIndex++
		signature := antigravityNativePartThoughtSignature(part)
		if !antigravityHasNativeThoughtSignature(signature) {
			signature = ""
		}
		if fc := part.Get("functionCall"); fc.Exists() {
			if a.lastResponseKind == "text" || a.lastResponseKind == "thought" {
				a.flushPendingThoughtSignaturesForKind(a.lastResponseKind)
			}
			if signature != "" {
				remainingPending := a.pendingSignatures[:0]
				for _, pending := range a.pendingSignatures {
					if pending.targetKind != "" {
						remainingPending = append(remainingPending, pending)
					}
				}
				a.pendingSignatures = remainingPending
			}
			if signature == "" {
				for pendingIndex := len(a.pendingSignatures) - 1; pendingIndex >= 0; pendingIndex-- {
					if a.pendingSignatures[pendingIndex].targetKind == "" {
						signature = a.pendingSignatures[pendingIndex].signature
						a.pendingSignatures = append(a.pendingSignatures[:pendingIndex], a.pendingSignatures[pendingIndex+1:]...)
						break
					}
				}
			}
			keys := antigravityReplayToolCallKeysFromPart(fc)
			for _, key := range keys {
				dedupeKey := key + "\x00" + signature
				if signature == "" {
					dedupeKey = fmt.Sprintf("%s\x00part:%d", key, pi)
				}
				if a.seenFC[dedupeKey] {
					return true
				}
				a.seenFC[dedupeKey] = true
			}
			occurrenceKey := antigravityFunctionCallKey(fc.Get("name").String(), fc.Get("args").Raw, "")
			occurrence := a.functionCallOccurrences[occurrenceKey]
			if occurrenceKey != "" {
				a.functionCallOccurrences[occurrenceKey] = occurrence + 1
			}
			item := buildAntigravityFunctionCallPartItem(a.contentIndex, pi, occurrence, fc, signature)
			if len(item) > 0 {
				a.appendItem(a.stampContextHash(item, a.contentIndex))
				if signature != "" {
					a.seenSignatures[signature] = true
				}
			}
			a.lastResponseKind = "function_call"
			return true
		}

		targetKind := ""
		if part.Get("thought").Bool() {
			targetKind = "thought"
		}
		text := part.Get("text")
		hasSemanticText := text.Exists() && text.String() != ""
		signatureOnly := signature != "" && !hasSemanticText
		if signatureOnly && a.lastResponseKind == "function_call" {
			if !a.seenSignatures[signature] {
				a.attachDetachedSignatureToLastFunctionCall(signature)
				a.seenSignatures[signature] = true
			}
			return true
		}
		if hasSemanticText {
			if targetKind != "thought" {
				targetKind = "text"
			}
			if signature != "" {
				remainingPending := a.pendingSignatures[:0]
				for _, pending := range a.pendingSignatures {
					unboundPrefix := pending.targetKind == ""
					if pending.targetKind == targetKind {
						unboundPrefix = (targetKind == "text" && a.visibleText.Len() == 0) || (targetKind == "thought" && a.thoughtText.Len() == 0)
					}
					if unboundPrefix {
						if pending.signature == signature {
							delete(a.seenSignatures, signature)
						}
						continue
					}
					remainingPending = append(remainingPending, pending)
				}
				a.pendingSignatures = remainingPending
				for _, pending := range a.pendingSignatures {
					if pending.targetKind == targetKind && pending.signature != signature {
						a.flushPendingThoughtSignaturesForKind(targetKind)
						break
					}
				}
			}
			if a.lastResponseKind != "" && a.lastResponseKind != targetKind && (a.lastResponseKind == "text" || a.lastResponseKind == "thought") {
				a.flushPendingThoughtSignaturesForKind(a.lastResponseKind)
			}
			if targetKind == "thought" {
				if a.thoughtText.Len() == 0 {
					a.thoughtPartIndex = pi
				}
				a.thoughtText.WriteString(text.String())
			} else {
				if a.visibleText.Len() == 0 {
					a.visiblePartIndex = pi
				}
				a.visibleText.WriteString(text.String())
			}
			a.lastResponseKind = targetKind
		}
		acceptedSignature := false
		if signature != "" && !a.seenSignatures[signature] {
			if targetKind == "" {
				targetKind = a.lastResponseKind
			}
			unmatchedDetachedCarrier := signatureOnly && a.lastResponseKind == targetKind && ((targetKind == "text" && a.visibleText.Len() == 0) || (targetKind == "thought" && a.thoughtText.Len() == 0))
			if unmatchedDetachedCarrier {
				a.seenSignatures[signature] = true
			} else if len(a.pendingSignatures)+len(a.items)+1 > internalcache.AntigravityReasoningReplayCacheMaxItemsPerEntry || a.itemBytes+len(signature) > internalcache.AntigravityReasoningReplayCacheMaxBytesPerEntry {
				a.overflow = true
				a.seenSignatures[signature] = true
			} else {
				a.pendingSignatures = append(a.pendingSignatures, antigravityPendingThoughtSignature{signature: signature, targetKind: targetKind})
				a.seenSignatures[signature] = true
				acceptedSignature = true
			}
		}
		if acceptedSignature && (signatureOnly || hasSemanticText) {
			switch targetKind {
			case "text":
				if a.visibleText.Len() > 0 {
					a.flushPendingThoughtSignaturesForKind("text")
				}
			case "thought":
				if a.thoughtText.Len() > 0 {
					a.flushPendingThoughtSignaturesForKind("thought")
				}
			}
		}
		return true
	})
}

func buildAntigravityThoughtSignatureItem(contentIndex, partIndex int, signature, targetKind, targetHash string) []byte {
	item := []byte(fmt.Sprintf(`{"type":"thought_signature","thoughtSignature":%q,"contentIndex":%d,"partIndex":%d}`,
		signature, contentIndex, partIndex))
	if targetKind != "" {
		item, _ = sjson.SetBytes(item, "targetKind", targetKind)
	}
	if targetHash != "" {
		item, _ = sjson.SetBytes(item, "targetHash", targetHash)
	}
	return item
}

func buildAntigravityFunctionCallPartItem(contentIndex, partIndex, targetOccurrence int, fc gjson.Result, signature string) []byte {
	item := map[string]any{
		"type":             "function_call_part",
		"contentIndex":     contentIndex,
		"partIndex":        partIndex,
		"targetOccurrence": targetOccurrence,
		"name":             fc.Get("name").String(),
	}
	if id := strings.TrimSpace(fc.Get("id").String()); id != "" {
		item["call_id"] = id
	}
	if args := fc.Get("args"); args.Exists() {
		if args.Type == gjson.String {
			item["args"] = args.String()
		} else {
			item["args"] = json.RawMessage(args.Raw)
		}
	}
	if signature != "" {
		item["thoughtSignature"] = signature
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	return raw
}

func (a *antigravityReasoningReplayAccumulator) flushPendingThoughtSignaturesForKind(targetKind string) {
	if a == nil || (targetKind != "text" && targetKind != "thought") {
		return
	}
	text := a.visibleText.String()
	partIndex := a.visiblePartIndex
	if targetKind == "thought" {
		text = a.thoughtText.String()
		partIndex = a.thoughtPartIndex
	}
	targetHash := ""
	targetOccurrence := 0
	if text != "" {
		sum := sha256.Sum256([]byte(targetKind + "\x00" + text))
		targetHash = fmt.Sprintf("%x", sum[:])
		occurrenceKey := targetKind + "\x00" + targetHash
		targetOccurrence = a.segmentOccurrences[occurrenceKey]
		a.segmentOccurrences[occurrenceKey] = targetOccurrence + 1
	}
	remaining := a.pendingSignatures[:0]
	for _, pending := range a.pendingSignatures {
		if pending.targetKind != targetKind || targetHash == "" {
			remaining = append(remaining, pending)
			continue
		}
		item := buildAntigravityThoughtSignatureItem(a.contentIndex, partIndex, pending.signature, targetKind, targetHash)
		item, _ = sjson.SetBytes(item, "targetOccurrence", targetOccurrence)
		a.appendItem(a.stampContextHash(item, a.contentIndex))
	}
	a.pendingSignatures = remaining
	if targetKind == "thought" {
		a.thoughtText.Reset()
		a.thoughtPartIndex = -1
	} else {
		a.visibleText.Reset()
		a.visiblePartIndex = -1
	}
}

func (a *antigravityReasoningReplayAccumulator) appendPendingThoughtSignatures() {
	if a == nil {
		return
	}
	for index := range a.pendingSignatures {
		if a.pendingSignatures[index].targetKind != "" {
			continue
		}
		switch {
		case a.lastResponseKind == "text" && a.visibleText.Len() > 0:
			a.pendingSignatures[index].targetKind = "text"
		case a.lastResponseKind == "thought" && a.thoughtText.Len() > 0:
			a.pendingSignatures[index].targetKind = "thought"
		case a.visibleText.Len() > 0:
			a.pendingSignatures[index].targetKind = "text"
		case a.thoughtText.Len() > 0:
			a.pendingSignatures[index].targetKind = "thought"
		}
	}
	a.flushPendingThoughtSignaturesForKind("thought")
	a.flushPendingThoughtSignaturesForKind("text")
	a.pendingSignatures = nil
}

func (a *antigravityReasoningReplayAccumulator) Commit(ctx context.Context) {
	if a == nil || !a.scope.valid() {
		return
	}
	log.Debugf("antigravity replay: accumulator commit terminal=%t overflow=%t items=%d (session=%s)",
		a.terminal, a.overflow, len(a.items), antigravityReplayLogKey(a.scope.sessionKey))
	if !a.terminal {
		// No terminal finishReason means the stream never completed, so this turn
		// contributes nothing to the ledger and its tool IDs become unresolvable.
		return
	}
	if a.overflow {
		_, _ = internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, a.scope.modelName, a.scope.sessionKey, a.scope.cacheSnapshot)
		return
	}
	a.appendPendingThoughtSignatures()
	if a.overflow {
		_, _ = internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, a.scope.modelName, a.scope.sessionKey, a.scope.cacheSnapshot)
		return
	}
	if len(a.items) == 0 {
		_, _ = internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, a.scope.modelName, a.scope.sessionKey, a.scope.cacheSnapshot)
		return
	}
	if _, errReplace := internalcache.ReplaceAntigravityReasoningReplayItemsIfUnchanged(ctx, a.scope.modelName, a.scope.sessionKey, a.scope.cacheSnapshot, a.items); errReplace != nil {
		_, _ = internalcache.DeleteAntigravityReasoningReplayItemsIfUnchanged(ctx, a.scope.modelName, a.scope.sessionKey, a.scope.cacheSnapshot)
	}
}

func cacheAntigravityReasoningReplayFromResponse(ctx context.Context, scope antigravityReasoningReplayScope, requestPayload, body []byte) {
	if !scope.valid() || len(body) == 0 {
		return
	}
	acc := newAntigravityReasoningReplayAccumulator(scope, requestPayload)
	acc.observeResponsePayload(body)
	acc.Commit(ctx)
}

func applyAntigravityNativeSignatureReplayIfNeeded(modelName string, payload []byte) []byte {
	if antigravityUsesReasoningReplayCache(modelName) {
		return payload
	}
	// Native per-part signature replay is not on upstream/dev; Gemini uses HOME replay only.
	return payload
}

func antigravityUsesReasoningReplayCache(modelName string) bool {
	modelName = strings.ToLower(modelName)
	if strings.Contains(modelName, "claude") {
		return false
	}
	return strings.Contains(modelName, "gemini") || strings.Contains(modelName, "flash") || strings.Contains(modelName, "agent")
}

func antigravityNativePartThoughtSignature(part gjson.Result) string {
	for _, path := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
		if signature := strings.TrimSpace(part.Get(path).String()); signature != "" {
			return signature
		}
	}
	return ""
}
