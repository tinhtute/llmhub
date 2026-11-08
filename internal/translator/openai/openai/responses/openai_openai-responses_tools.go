package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponsesToolDeclaration preserves the client identity of one callable tool
// while recording the flat name sent to providers without namespace support.
type ResponsesToolDeclaration struct {
	Namespace     string
	Name          string
	Type          string
	EffectiveName string
}

// ResponsesToolDeclarationTable is scoped to one Responses request.
type ResponsesToolDeclarationTable struct {
	declarations         []ResponsesToolDeclaration
	byEffective          map[string]ResponsesToolDeclaration
	byCallItemID         map[string]ResponsesToolDeclaration
	customCallArgs       map[string]*strings.Builder
	customCallInputState map[string]*responsesCustomToolInputState
}

// ResponsesToolNameCollisionError reports distinct client declarations that
// flatten to the same outbound name.
type ResponsesToolNameCollisionError struct {
	EffectiveName string
	First         ResponsesToolDeclaration
	Second        ResponsesToolDeclaration
}

func (e *ResponsesToolNameCollisionError) Error() string {
	return fmt.Sprintf("tool name collision: %q and %q both map to %q", responsesDeclarationIdentity(e.First), responsesDeclarationIdentity(e.Second), e.EffectiveName)
}

// BuildResponsesToolDeclarationTable collects callable declarations from both
// top-level tools and Codex Desktop additional_tools input items. Effective
// names are namespace-qualified, capped to the Chat Completions function-name
// limit, and disambiguated when two distinct declarations flatten onto the
// same name (upstream disambiguateResponsesChatToolNames).
func BuildResponsesToolDeclarationTable(requestRawJSON []byte) (*ResponsesToolDeclarationTable, error) {
	table := &ResponsesToolDeclarationTable{
		byEffective:          make(map[string]ResponsesToolDeclaration),
		byCallItemID:         make(map[string]ResponsesToolDeclaration),
		customCallArgs:       make(map[string]*strings.Builder),
		customCallInputState: make(map[string]*responsesCustomToolInputState),
	}
	root := gjson.ParseBytes(requestRawJSON)

	var collected []ResponsesToolDeclaration
	byRawIdentity := make(map[string]ResponsesToolDeclaration)
	var collectErr error
	collect := func(tools gjson.Result) bool {
		var collectTools func(gjson.Result, string) bool
		collectTools = func(items gjson.Result, namespace string) bool {
			if !items.Exists() || !items.IsArray() {
				return true
			}
			proceed := true
			items.ForEach(func(_, tool gjson.Result) bool {
				toolType := tool.Get("type").String()
				if strings.TrimSpace(toolType) == "namespace" {
					proceed = collectTools(tool.Get("tools"), tool.Get("name").String())
					return proceed
				}
				if !responsesCallableToolType(toolType) {
					return true
				}
				name := responsesToolName(tool)
				if strings.TrimSpace(name) == "" {
					return true
				}
				declaration := ResponsesToolDeclaration{
					Namespace:     namespace,
					Name:          name,
					Type:          toolType,
					EffectiveName: qualifyResponsesNamespaceToolName(namespace, name),
				}
				// Distinct declarations qualifying to the same raw name before
				// the cap keep the local collision contract: a flat tool and a
				// namespaced child flattening to one outbound name would be
				// indistinguishable to the upstream. Exact duplicate deliveries
				// (same tool through "tools" and "additional_tools") dedupe.
				rawIdentity := rawResponsesNamespaceQualifiedName(namespace, name)
				if existing, ok := byRawIdentity[rawIdentity]; ok {
					if !sameResponsesDeclarationIdentity(existing, declaration) {
						collectErr = &ResponsesToolNameCollisionError{EffectiveName: rawIdentity, First: existing, Second: declaration}
						proceed = false
						return false
					}
					return true
				}
				byRawIdentity[rawIdentity] = declaration
				collected = append(collected, declaration)
				return true
			})
			return proceed
		}
		return collectTools(tools, "")
	}

	if proceed := collect(root.Get("tools")); proceed {
		if input := root.Get("input"); input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				switch item.Get("type").String() {
				case "additional_tools":
					proceed = collect(item.Get("tools"))
				case util.ResponsesToolSearchOutputType:
					// Tools loaded by a client tool search stay declared on
					// every later request, so the model can call them.
					if util.IsResponsesClientToolSearchItem(item) {
						proceed = collect(item.Get("tools"))
					}
				}
				return proceed
			})
		}
	}
	if collectErr != nil {
		return nil, collectErr
	}

	disambiguateResponsesChatToolNames(collected)
	for _, declaration := range collected {
		if _, exists := table.byEffective[declaration.EffectiveName]; !exists {
			table.byEffective[declaration.EffectiveName] = declaration
		}
		table.declarations = append(table.declarations, declaration)
	}
	return table, nil
}

// NormalizeResponsesToolsForCodex flattens namespace declarations, promotes
// additional_tools, and rewrites request references to exact effective names.
func NormalizeResponsesToolsForCodex(requestRawJSON []byte) []byte {
	table, err := BuildResponsesToolDeclarationTable(requestRawJSON)
	if err != nil {
		return requestRawJSON
	}

	root := gjson.ParseBytes(requestRawJSON)
	result := requestRawJSON
	toolsOut := []byte(`[]`)
	hasTools := false
	appendTools := func(tools gjson.Result) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			for _, normalized := range normalizeResponsesToolForCodex(tool, "", table) {
				toolsOut, _ = sjson.SetRawBytes(toolsOut, "-1", normalized)
				hasTools = true
			}
			return true
		})
	}
	appendTools(root.Get("tools"))

	if input := root.Get("input"); input.Exists() && input.IsArray() {
		inputOut := []byte(`[]`)
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				appendTools(item.Get("tools"))
				return true
			}
			normalized := table.normalizeCallReference([]byte(item.Raw), "")
			inputOut, _ = sjson.SetRawBytes(inputOut, "-1", normalized)
			return true
		})
		result, _ = sjson.SetRawBytes(result, "input", inputOut)
	}

	if hasTools {
		result, _ = sjson.SetRawBytes(result, "tools", toolsOut)
	}
	result = table.normalizeCallReference(result, "tool_choice")
	if choices := gjson.GetBytes(result, "tool_choice.tools"); choices.IsArray() {
		for i := range choices.Array() {
			result = table.normalizeCallReference(result, fmt.Sprintf("tool_choice.tools.%d", i))
		}
	}
	return result
}

// RestoreResponsesToolCalls restores declarations on direct Responses events
// and complete response payloads using exact effective-name lookup only.
func (t *ResponsesToolDeclarationTable) RestoreResponsesToolCalls(rawJSON []byte) []byte {
	events := t.RestoreResponsesToolCallEvents(rawJSON)
	if len(events) == 0 {
		return rawJSON
	}
	return events[len(events)-1]
}

// RestoreResponsesToolCallEvents restores one upstream event and may prepend a
// buffered custom-input delta when the terminal event resolves wrapper ambiguity.
func (t *ResponsesToolDeclarationTable) RestoreResponsesToolCallEvents(rawJSON []byte) [][]byte {
	if t == nil || len(t.byEffective) == 0 || len(rawJSON) == 0 {
		return [][]byte{rawJSON}
	}
	result := rawJSON
	var pending []byte
	eventType := gjson.GetBytes(result, "type").String()
	if strings.HasPrefix(eventType, "response.output_item.") {
		result = t.restoreCallItem(result, "item")
	} else if strings.HasPrefix(eventType, "response.function_call_arguments.") {
		result, pending = t.restoreCustomCallInputEvent(result)
	}
	for _, path := range []string{"response.output", "output"} {
		items := gjson.GetBytes(result, path)
		if !items.IsArray() {
			continue
		}
		for i := range items.Array() {
			result = t.restoreCallItem(result, fmt.Sprintf("%s.%d", path, i))
		}
	}
	if len(pending) > 0 {
		return [][]byte{pending, result}
	}
	return [][]byte{result}
}

func (t *ResponsesToolDeclarationTable) restoreCallItem(item []byte, itemPath string) []byte {
	prefix := ""
	if itemPath != "" {
		prefix = itemPath + "."
	}
	name := gjson.GetBytes(item, prefix+"name").String()
	declaration, ok := t.byEffective[name]
	if !ok {
		return item
	}
	if id := gjson.GetBytes(item, prefix+"id").String(); id != "" {
		t.byCallItemID[id] = declaration
	}
	item, _ = sjson.SetBytes(item, prefix+"name", declaration.Name)
	if declaration.Namespace != "" {
		item, _ = sjson.SetBytes(item, prefix+"namespace", declaration.Namespace)
	} else {
		item, _ = sjson.DeleteBytes(item, prefix+"namespace")
	}
	if responsesDeclarationKind(declaration.Type) == "custom" {
		item, _ = sjson.SetBytes(item, prefix+"type", "custom_tool_call")
		arguments := gjson.GetBytes(item, prefix+"arguments")
		if arguments.Exists() && !gjson.GetBytes(item, prefix+"input").Exists() {
			item, _ = sjson.SetBytes(item, prefix+"input", unwrapCustomToolInput(arguments.String()))
		}
		item, _ = sjson.DeleteBytes(item, prefix+"arguments")
		if id := gjson.GetBytes(item, prefix+"id").String(); strings.HasPrefix(id, "fc_") {
			item, _ = sjson.SetBytes(item, prefix+"id", "ctc_"+strings.TrimPrefix(id, "fc_"))
		}
	} else {
		item, _ = sjson.SetBytes(item, prefix+"type", "function_call")
	}
	return item
}

func (t *ResponsesToolDeclarationTable) restoreCustomCallInputEvent(event []byte) ([]byte, []byte) {
	itemID := gjson.GetBytes(event, "item_id").String()
	declaration, ok := t.byCallItemID[itemID]
	if !ok || responsesDeclarationKind(declaration.Type) != "custom" {
		return event, nil
	}

	eventType := gjson.GetBytes(event, "type").String()
	suffix := strings.TrimPrefix(eventType, "response.function_call_arguments.")
	event, _ = sjson.SetBytes(event, "type", "response.custom_tool_call_input."+suffix)
	clientItemID := itemID
	if strings.HasPrefix(itemID, "fc_") {
		clientItemID = "ctc_" + strings.TrimPrefix(itemID, "fc_")
		event, _ = sjson.SetBytes(event, "item_id", clientItemID)
	}
	state := t.customCallInputState[itemID]
	if state == nil {
		state = &responsesCustomToolInputState{}
		t.customCallInputState[itemID] = state
	}
	if suffix == "done" {
		arguments := gjson.GetBytes(event, "arguments")
		terminalArguments := ""
		if arguments.Exists() {
			terminalArguments = arguments.String()
		} else if args := t.customCallArgs[itemID]; args != nil {
			terminalArguments = args.String()
		}
		inputDelta := responsesCustomToolInputDelta(terminalArguments, state, true)
		event, _ = sjson.SetBytes(event, "input", unwrapCustomToolInput(terminalArguments))
		event, _ = sjson.DeleteBytes(event, "arguments")
		if inputDelta == "" {
			return event, nil
		}
		pending := []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"","output_index":0,"delta":""}`)
		pending, _ = sjson.SetBytes(pending, "item_id", clientItemID)
		if outputIndex := gjson.GetBytes(event, "output_index"); outputIndex.Exists() {
			pending, _ = sjson.SetBytes(pending, "output_index", outputIndex.Int())
		}
		pending, _ = sjson.SetBytes(pending, "delta", inputDelta)
		return event, pending
	}
	if suffix == "delta" {
		delta := gjson.GetBytes(event, "delta")
		if delta.Exists() {
			args := t.customCallArgs[itemID]
			if args == nil {
				args = &strings.Builder{}
				t.customCallArgs[itemID] = args
			}
			args.WriteString(delta.String())
			event, _ = sjson.SetBytes(event, "delta", responsesCustomToolInputDelta(args.String(), state, false))
		}
	}
	return event, nil
}

// normalizeCallReference rewrites one call reference (a replayed
// function_call/custom_tool_call input item, tool_choice, or a
// tool_choice.tools entry) to the exact effective name emitted for its
// declaration, deleting any namespace fields. Resolution mirrors upstream
// canonicalResponsesToolName/chatNameForResponsesNamespaceToolCall: declared
// identities map to their disambiguated effective name, namespaced unknowns
// qualify and cap without landing on a declared alias, and bare names resolve
// through the emitted alias, the uncapped qualified identity, then a unique
// local name before falling back to a capped non-alias.
func (t *ResponsesToolDeclarationTable) normalizeCallReference(rawJSON []byte, itemPath string) []byte {
	prefix := ""
	if itemPath != "" {
		prefix = itemPath + "."
	}
	namePath := ""
	for _, candidate := range []string{prefix + "function.name", prefix + "custom.name", prefix + "name"} {
		if gjson.GetBytes(rawJSON, candidate).Exists() {
			namePath = candidate
			break
		}
	}
	if namePath == "" {
		return rawJSON
	}
	name := gjson.GetBytes(rawJSON, namePath).String()
	namespace := ""
	for _, candidate := range []string{prefix + "namespace", prefix + "function.namespace", prefix + "custom.namespace"} {
		if ns := gjson.GetBytes(rawJSON, candidate); ns.Exists() {
			namespace = ns.String()
			break
		}
	}
	kind := responsesCallReferenceKind(gjson.GetBytes(rawJSON, prefix+"type").String())
	resolved, ok := t.resolveChatCallName(namespace, name, kind)
	if !ok {
		return rawJSON
	}
	rawJSON, _ = sjson.SetBytes(rawJSON, namePath, resolved)
	for _, candidate := range []string{prefix + "namespace", prefix + "function.namespace", prefix + "custom.namespace"} {
		rawJSON, _ = sjson.DeleteBytes(rawJSON, candidate)
	}
	return rawJSON
}

// resolveChatCallName resolves one call reference against the declaration
// table. The second result is false for non-call items that match no
// declaration, so unrelated payloads carrying a name field stay untouched.
func (t *ResponsesToolDeclarationTable) resolveChatCallName(namespace, name, kind string) (string, bool) {
	if declaration, ok := t.findOriginal(namespace, name, kind); ok {
		return declaration.EffectiveName, true
	}
	if kind != "" {
		// A replayed call may carry the wrong kind marker for a declared tool;
		// identity (namespace + local name) still wins, as upstream's
		// kind-agnostic declaration lookup does.
		if declaration, ok := t.findOriginal(namespace, name, ""); ok {
			return declaration.EffectiveName, true
		}
	}
	if kind == "" {
		return "", false
	}
	if namespace != "" {
		// An identity no current declaration backs (history from an older
		// build, or a foreign client) still needs a chat-legal name, but not
		// one that a real declaration owns.
		return t.avoidDeclaredAliases(qualifyResponsesNamespaceToolName(namespace, name)), true
	}
	if _, ok := t.byEffective[name]; ok {
		return name, true
	}
	// A replayed call may carry the fully-qualified uncapped name of a long
	// declaration; resolve it to that declaration's emitted chat name before
	// the bare local-name lookup and the blind cap (upstream ordering).
	for _, declaration := range t.declarations {
		if rawResponsesNamespaceQualifiedName(declaration.Namespace, declaration.Name) == name {
			return declaration.EffectiveName, true
		}
	}
	candidate := ""
	ambiguous := false
	for _, declaration := range t.declarations {
		if declaration.Name != name {
			continue
		}
		if candidate != "" && candidate != declaration.EffectiveName {
			ambiguous = true
		}
		candidate = declaration.EffectiveName
	}
	if candidate != "" && !ambiguous {
		return candidate, true
	}
	// Unresolved or ambiguous names still get capped, but never onto a
	// declared alias — that would attribute the call to the wrong tool.
	return t.avoidDeclaredAliases(capResponsesChatToolName(name)), true
}

// avoidDeclaredAliases keeps a fallback name from colliding with any alias
// the declaration table actually emits (upstream
// avoidResponsesDeclaredChatAliases).
func (t *ResponsesToolDeclarationTable) avoidDeclaredAliases(candidate string) string {
	if _, taken := t.byEffective[candidate]; !taken {
		return candidate
	}
	for suffix := 1; ; suffix++ {
		variant := capResponsesChatToolName(candidate + "_" + strconv.Itoa(suffix))
		if _, taken := t.byEffective[variant]; !taken {
			return variant
		}
	}
}

func (t *ResponsesToolDeclarationTable) findOriginal(namespace, name, kind string) (ResponsesToolDeclaration, bool) {
	for _, declaration := range t.declarations {
		if declaration.Namespace != namespace || declaration.Name != name {
			continue
		}
		if kind != "" && responsesDeclarationKind(declaration.Type) != kind {
			continue
		}
		return declaration, true
	}
	return ResponsesToolDeclaration{}, false
}

func normalizeResponsesToolForCodex(tool gjson.Result, namespace string, table *ResponsesToolDeclarationTable) [][]byte {
	toolType := tool.Get("type").String()
	if strings.TrimSpace(toolType) == "namespace" {
		namespaceName := tool.Get("name").String()
		children := tool.Get("tools")
		if !children.IsArray() {
			return nil
		}
		var out [][]byte
		children.ForEach(func(_, child gjson.Result) bool {
			out = append(out, normalizeResponsesToolForCodex(child, namespaceName, table)...)
			return true
		})
		return out
	}
	if !responsesCallableToolType(toolType) {
		return [][]byte{[]byte(tool.Raw)}
	}
	name := responsesToolName(tool)
	if strings.TrimSpace(name) == "" {
		return nil
	}
	// The emitted name is the declaration's effective name, so tools carry the
	// same cap and disambiguation suffixes that call references resolve to.
	effectiveName := qualifyResponsesNamespaceToolName(namespace, name)
	if table != nil {
		if declaration, ok := table.findOriginal(namespace, name, ""); ok {
			effectiveName = declaration.EffectiveName
		}
	}
	normalized := []byte(tool.Raw)
	if tool.Get("function").IsObject() {
		normalized = []byte(`{"type":"function","name":"","description":"","parameters":{}}`)
		normalized, _ = sjson.SetBytes(normalized, "name", effectiveName)
		if description := responsesToolDescription(tool); description != "" {
			normalized, _ = sjson.SetBytes(normalized, "description", description)
		}
		if parameters := responsesToolParameters(tool); parameters.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "parameters", sanitizeResponsesToolParametersForCodex([]byte(parameters.Raw)))
		}
		return [][]byte{normalized}
	}
	if strings.TrimSpace(toolType) == "" {
		normalized, _ = sjson.SetBytes(normalized, "type", "function")
	}
	normalized, _ = sjson.SetBytes(normalized, "name", effectiveName)
	normalized, _ = sjson.DeleteBytes(normalized, "namespace")
	for _, path := range []string{"parameters", "parametersJsonSchema", "input_schema"} {
		if parameters := tool.Get(path); parameters.Exists() && parameters.IsObject() {
			normalized, _ = sjson.SetRawBytes(normalized, path, sanitizeResponsesToolParametersForCodex([]byte(parameters.Raw)))
		}
	}
	return [][]byte{normalized}
}

// responsesNumericBoundKeywords lists the JSON Schema numeric bound keywords
// the Codex backend rejects with HTTP 500 when set to an integer outside the
// signed 64-bit range (upstream 57d005fe6c20, issue #6579).
var responsesNumericBoundKeywords = [...]string{
	"minimum",
	"maximum",
	"exclusiveMinimum",
	"exclusiveMaximum",
}

// sanitizeResponsesToolParametersForCodex removes schema fragments the Codex
// backend rejects: regex patterns with unsupported Unicode property escapes
// (\p{...} / \P{...}), patternProperties entries keyed by such patterns, and
// integer numeric bounds outside int64. The walk is schema-aware: only
// subschemas under known JSON Schema keyword locations
// (util.SchemaMapKeywords / util.SchemaValueKeywords) are visited, so keys
// inside user data (e.g. description, default, enum) are preserved. The input
// is returned unchanged when nothing was removed.
func sanitizeResponsesToolParametersForCodex(raw []byte) []byte {
	rawStr := string(raw)
	// "inimum" and "aximum" match minimum, maximum, exclusiveMinimum, and
	// exclusiveMaximum. `\u` covers patterns using JSON Unicode escapes
	// (e.g. \u005c for '\') that decode to unsupported property escapes.
	if !strings.Contains(rawStr, `\p{`) && !strings.Contains(rawStr, `\P{`) && !strings.Contains(rawStr, `\u`) &&
		!strings.Contains(rawStr, `\\0`) && !strings.Contains(rawStr, "inimum") && !strings.Contains(rawStr, "aximum") {
		return raw
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil || root == nil {
		return raw
	}
	var dummy any
	if err := dec.Decode(&dummy); err != io.EOF {
		return raw
	}
	if !stripResponsesIncompatibleSchemaFragments(root) {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return raw
	}
	return bytes.TrimSpace(buf.Bytes())
}

func stripResponsesIncompatibleSchemaFragments(v any) bool {
	changed := false
	switch schema := v.(type) {
	case map[string]any:
		if patternVal, ok := schema["pattern"].(string); ok && util.HasUnsupportedUnicodePropertyEscape(patternVal) {
			delete(schema, "pattern")
			changed = true
		}
		for _, boundKey := range responsesNumericBoundKeywords {
			if boundVal, ok := schema[boundKey].(json.Number); ok && isResponsesNumericBoundOutOfInt64(boundVal) {
				delete(schema, boundKey)
				changed = true
			}
		}
		if patternProps, ok := schema["patternProperties"].(map[string]any); ok {
			for patternKey, subSchema := range patternProps {
				if util.HasUnsupportedUnicodePropertyEscape(patternKey) {
					delete(patternProps, patternKey)
					changed = true
				} else if stripResponsesIncompatibleSchemaFragments(subSchema) {
					changed = true
				}
			}
		}
		for _, mapKey := range util.SchemaMapKeywords {
			if mapKey == "patternProperties" {
				continue
			}
			if subMap, ok := schema[mapKey].(map[string]any); ok {
				for _, subSchema := range subMap {
					if stripResponsesIncompatibleSchemaFragments(subSchema) {
						changed = true
					}
				}
			}
		}
		for _, valKey := range util.SchemaValueKeywords {
			if val, exists := schema[valKey]; exists {
				switch sub := val.(type) {
				case map[string]any:
					if stripResponsesIncompatibleSchemaFragments(sub) {
						changed = true
					}
				case []any:
					for _, item := range sub {
						if stripResponsesIncompatibleSchemaFragments(item) {
							changed = true
						}
					}
				}
			}
		}
	case []any:
		for _, item := range schema {
			if stripResponsesIncompatibleSchemaFragments(item) {
				changed = true
			}
		}
	}
	return changed
}

// isResponsesNumericBoundOutOfInt64 reports whether a JSON numeric bound
// denotes an integer outside the signed 64-bit range. Fractional values and
// non-integral scientific notation are always in range for Codex; only exact
// integers beyond [math.MinInt64, math.MaxInt64] are stripped. Adapted from
// upstream isNumericBoundOutOfInt64 (57d005fe6c20).
func isResponsesNumericBoundOutOfInt64(number json.Number) bool {
	raw := strings.TrimSpace(number.String())
	if raw == "" {
		return false
	}
	if !strings.ContainsAny(raw, ".eE") {
		_, errParse := strconv.ParseInt(raw, 10, 64)
		return errors.Is(errParse, strconv.ErrRange)
	}

	s := raw
	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}

	mantissa := s
	var exp int64
	if idx := strings.IndexAny(s, "eE"); idx >= 0 {
		mantissa = s[:idx]
		expStr := s[idx+1:]
		parsedExp, errExp := strconv.ParseInt(expStr, 10, 64)
		if errExp != nil {
			if errors.Is(errExp, strconv.ErrRange) {
				return !strings.HasPrefix(expStr, "-") && strings.Trim(mantissa, "0.") != ""
			}
			return false
		}
		exp = parsedExp
	}

	intPart := mantissa
	fracPart := ""
	if dotIdx := strings.IndexByte(mantissa, '.'); dotIdx >= 0 {
		intPart = mantissa[:dotIdx]
		fracPart = mantissa[dotIdx+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return false
		}
	}
	if intPart == "" && fracPart == "" {
		return false
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return false
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return false
		}
	}

	allDigits := intPart + fracPart
	sig := strings.TrimLeft(allDigits, "0")
	if sig == "" {
		return false
	}
	if exp < -int64(len(raw)) {
		return false
	}
	if exp > int64(len(raw))+20 {
		return true
	}
	trimmed := strings.TrimRight(sig, "0")
	trailingZeros := int64(len(sig) - len(trimmed))
	effectiveExp := exp - int64(len(fracPart)) + trailingZeros
	if effectiveExp < 0 {
		return false
	}
	if effectiveExp > 19 || int64(len(trimmed))+effectiveExp > 19 {
		return true
	}

	intStr := trimmed + strings.Repeat("0", int(effectiveExp))
	if negative {
		intStr = "-" + intStr
	}
	_, errParse := strconv.ParseInt(intStr, 10, 64)
	return errors.Is(errParse, strconv.ErrRange)
}

func convertResponsesToolToOpenAIChatTools(tool gjson.Result) [][]byte {
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "", "function":
		if tJSON, ok := convertResponsesFunctionToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	case "namespace":
		return convertResponsesNamespaceToolToOpenAIChat(tool)
	case "custom":
		if tJSON, ok := convertResponsesCustomToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	}
	return nil
}

func convertResponsesNamespaceToolToOpenAIChat(tool gjson.Result) [][]byte {
	namespaceName := tool.Get("name").String()
	children := tool.Get("tools")
	if !children.IsArray() {
		return nil
	}
	var out [][]byte
	children.ForEach(func(_, child gjson.Result) bool {
		name := qualifyResponsesNamespaceToolName(namespaceName, responsesToolName(child))
		switch strings.TrimSpace(child.Get("type").String()) {
		case "", "function":
			if converted, ok := convertResponsesFunctionToolToOpenAIChat(child, name); ok {
				out = append(out, converted)
			}
		case "custom":
			if converted, ok := convertResponsesCustomToolToOpenAIChat(child, name); ok {
				out = append(out, converted)
			}
		}
		return true
	})
	return out
}

func convertResponsesFunctionToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := overrideName
	if name == "" {
		name = responsesToolName(tool)
	}
	if strings.TrimSpace(name) == "" {
		return nil, false
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	if parameters := responsesToolParameters(tool); parameters.Exists() {
		chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", []byte(parameters.Raw))
	}
	return chatTool, true
}

func convertResponsesCustomToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := overrideName
	if name == "" {
		name = responsesToolName(tool)
	}
	if strings.TrimSpace(name) == "" {
		return nil, false
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	return chatTool, true
}

// responsesToolOutputText flattens a tool output payload to plain text
// (upstream openai_openai-responses_tools.go).
func responsesToolOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var b strings.Builder
		output.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				return true
			}
			if text := part.Get("text"); text.Exists() {
				b.WriteString(text.String())
			}
			return true
		})
		return b.String()
	}
	if output.Exists() {
		return output.Raw
	}
	return ""
}

func responsesToolName(tool gjson.Result) string {
	if name := tool.Get("name"); name.Exists() {
		return name.String()
	}
	return tool.Get("function.name").String()
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description"); description.Exists() {
		return description.String()
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{"parameters", "parametersJsonSchema", "input_schema", "function.parameters", "function.parametersJsonSchema"} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

func responsesCustomToolNames(requestRawJSON []byte) map[string]struct{} {
	names := make(map[string]struct{})
	table, err := BuildResponsesToolDeclarationTable(requestRawJSON)
	if err != nil {
		return names
	}
	for _, declaration := range table.declarations {
		if responsesDeclarationKind(declaration.Type) == "custom" {
			names[declaration.EffectiveName] = struct{}{}
		}
	}
	return names
}

func responsesSingleCustomToolName(requestRawJSON []byte) (string, bool) {
	table, err := BuildResponsesToolDeclarationTable(requestRawJSON)
	if err != nil || len(table.declarations) != 1 || responsesDeclarationKind(table.declarations[0].Type) != "custom" {
		return "", false
	}
	return table.declarations[0].EffectiveName, true
}

type responsesCustomToolInputState struct {
	raw       bool
	sent      int
	finalized bool
}

func unwrapCustomToolInput(arguments string) string {
	if input, ok := exactCustomToolInputWrapper(arguments); ok {
		return input
	}
	return arguments
}

func responsesCustomToolInputDelta(arguments string, state *responsesCustomToolInputState, final bool) string {
	if state == nil || state.finalized {
		return ""
	}
	if state.raw {
		if state.sent > len(arguments) {
			state.sent = 0
		}
		delta := arguments[state.sent:]
		state.sent = len(arguments)
		if final {
			state.finalized = true
		}
		return delta
	}
	if final {
		state.finalized = true
		if input, ok := exactCustomToolInputWrapper(arguments); ok {
			return input
		}
		state.raw = true
		state.sent = len(arguments)
		return arguments
	}
	if customToolInputWrapperPrefix(arguments) {
		return ""
	}
	state.raw = true
	state.sent = len(arguments)
	return arguments
}

func exactCustomToolInputWrapper(arguments string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') || !decoder.More() {
		return "", false
	}
	key, err := decoder.Token()
	if err != nil || key != "input" {
		return "", false
	}
	var rawInput json.RawMessage
	if err = decoder.Decode(&rawInput); err != nil || decoder.More() {
		return "", false
	}
	trimmedInput := bytes.TrimSpace(rawInput)
	if len(trimmedInput) == 0 || trimmedInput[0] != '"' {
		return "", false
	}
	var input string
	if err = json.Unmarshal(trimmedInput, &input); err != nil {
		return "", false
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return "", false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return "", false
	}
	return input, true
}

func customToolInputWrapperPrefix(arguments string) bool {
	i := skipCustomToolJSONWhitespace(arguments, 0)
	if i >= len(arguments) {
		return true
	}
	if arguments[i] != '{' {
		return false
	}
	i = skipCustomToolJSONWhitespace(arguments, i+1)
	if i >= len(arguments) {
		return true
	}
	keyStart := i
	keyEnd, complete, valid := scanCustomToolJSONString(arguments, i)
	if !valid {
		return false
	}
	if !complete {
		return true
	}
	var key string
	if err := json.Unmarshal([]byte(arguments[keyStart:keyEnd]), &key); err != nil || key != "input" {
		return false
	}
	i = skipCustomToolJSONWhitespace(arguments, keyEnd)
	if i >= len(arguments) {
		return true
	}
	if arguments[i] != ':' {
		return false
	}
	i = skipCustomToolJSONWhitespace(arguments, i+1)
	if i >= len(arguments) {
		return true
	}
	valueEnd, complete, valid := scanCustomToolJSONString(arguments, i)
	if !valid {
		return false
	}
	if !complete {
		return true
	}
	i = skipCustomToolJSONWhitespace(arguments, valueEnd)
	if i >= len(arguments) {
		return true
	}
	if arguments[i] != '}' {
		return false
	}
	i = skipCustomToolJSONWhitespace(arguments, i+1)
	return i == len(arguments)
}

func skipCustomToolJSONWhitespace(value string, start int) int {
	for start < len(value) {
		switch value[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start
		}
	}
	return start
}

func scanCustomToolJSONString(value string, start int) (end int, complete bool, valid bool) {
	if start >= len(value) || value[start] != '"' {
		return start, false, false
	}
	for i := start + 1; i < len(value); i++ {
		switch value[i] {
		case '"':
			return i + 1, true, true
		case '\\':
			i++
			if i >= len(value) {
				return len(value), false, true
			}
			switch value[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for j := 0; j < 4; j++ {
					i++
					if i >= len(value) {
						return len(value), false, true
					}
					if !strings.ContainsRune("0123456789abcdefABCDEF", rune(value[i])) {
						return i, false, false
					}
				}
			default:
				return i, false, false
			}
		default:
			if value[i] < 0x20 {
				return i, false, false
			}
		}
	}
	return len(value), false, true
}

// responsesChatToolNameLimit is the Chat Completions function name limit
// enforced by strict upstreams (e.g. z-ai/glm). Responses namespace tools
// routinely flatten to names longer than this.
const responsesChatToolNameLimit = 64

func qualifyResponsesNamespaceToolName(namespaceName, childName string) string {
	return capResponsesChatToolName(rawResponsesNamespaceQualifiedName(namespaceName, childName))
}

// rawResponsesNamespaceQualifiedName is qualifyResponsesNamespaceToolName
// without the length cap, so disambiguation can tell a genuine name apart
// from a truncation-induced collision (upstream
// rawResponsesNamespaceQualifiedName).
func rawResponsesNamespaceQualifiedName(namespaceName, childName string) string {
	childName = strings.TrimSpace(childName)
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if strings.HasPrefix(childName, namespaceName) {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

// capResponsesChatToolName truncates a flattened Responses tool name to the
// Chat Completions limit while keeping the tail, which carries the most
// identifying part of the name (the tool's local name). Namespace-qualified
// names share a long "mcp__<server>" prefix, so keeping the tail preserves
// more usable signal than keeping the head. Truncation can leave a partial
// "_"/"-" run at the start; leading separators are stripped because some
// strict upstreams reject names that do not begin with an alphanumeric
// character. This is a pure function of the input name, so every path that
// derives a chat function name stays consistent with every other (upstream
// capResponsesChatToolName).
func capResponsesChatToolName(name string) string {
	if len(name) <= responsesChatToolNameLimit {
		return name
	}
	truncated := name[len(name)-responsesChatToolNameLimit:]
	if trimmed := strings.TrimLeft(truncated, "_-"); trimmed != "" {
		return trimmed
	}
	return truncated
}

// disambiguateResponsesChatToolNames rewrites flattened names in place when
// distinct declarations collapse onto the same capped Chat Completions name.
// Identity is the pre-cap qualified name: declarations that qualified to the
// same name before the cap (one tool delivered through both "tools" and
// "additional_tools", or a flat tool colliding with a namespace child) are the
// same upstream tool and keep the shared first-wins name, while distinct
// names that only collide through truncation get "_1"-style suffixes so the
// deduplication downstream never silently drops a real tool.
//
// Qualified names that fit the cap unchanged are claimed before any
// truncation alias is assigned (equal raw names are one identity, so those
// claims cannot conflict). Local names that fit the cap are reserved the same
// way: a replayed call or tool_choice that omits the namespace carries the
// local name, and local-name recovery resolves it to the declaration, so a
// capped alias occupying that name would win the earlier exact-emitted-alias
// match and attribute those calls to the wrong tool. A long declaration whose
// capped tail lands on any reserved name therefore takes the suffix itself.
// Suffixed variants stay within the name cap, and every variant is claimed in
// the same pass so a later declaration cannot resurrect a collision.
//
// A local name carried by more than one distinct identity is ambiguous: no
// namespace-less call naming it can be resolved, so the name is burned
// instead of being awarded to whichever declaration came first. Burning
// matters even when the name is also a declaration's capped alias — that
// alias would be emitted verbatim, win the exact-emitted-alias match, and
// silently route the other namespace's calls to the first declaration.
// (upstream disambiguateResponsesChatToolNames)
func disambiguateResponsesChatToolNames(declarations []ResponsesToolDeclaration) {
	claimed := make(map[string]string, len(declarations))
	claim := func(candidate, identity string) bool {
		if ownerClaim, taken := claimed[candidate]; !taken {
			claimed[candidate] = identity
			return true
		} else {
			return ownerClaim == identity
		}
	}
	longDeclarations := make([]int, 0)
	identities := make([]string, len(declarations))
	// localName → the single identity that declares it, or "" once a second,
	// distinct identity shows the name is ambiguous.
	localOwners := make(map[string]string)
	ambiguousLocalNames := make(map[string]struct{})
	for i := range declarations {
		identity := rawResponsesNamespaceQualifiedName(declarations[i].Namespace, declarations[i].Name)
		identities[i] = identity
		if len(identity) > responsesChatToolNameLimit {
			longDeclarations = append(longDeclarations, i)
		} else {
			claim(identity, identity)
		}
		local := declarations[i].Name
		if local == "" || local == identity || len(local) > responsesChatToolNameLimit {
			continue
		}
		if ownerLocal, seen := localOwners[local]; !seen {
			localOwners[local] = identity
		} else if ownerLocal != "" && ownerLocal != identity {
			localOwners[local] = ""
		}
	}
	for local, ownerLocal := range localOwners {
		// Reserving under any identity keeps the name out of every later
		// truncation alias; ambiguous names additionally never get emitted.
		claim(local, ownerLocal)
		if ownerLocal == "" {
			ambiguousLocalNames[local] = struct{}{}
		}
	}
	isAmbiguous := func(name string) bool {
		_, ambiguous := ambiguousLocalNames[name]
		return ambiguous
	}
	for _, i := range longDeclarations {
		identity := identities[i]
		name := declarations[i].EffectiveName
		if !isAmbiguous(name) && claim(name, identity) {
			continue
		}
		for suffix := 1; ; suffix++ {
			candidate := capResponsesChatToolName(name + "_" + strconv.Itoa(suffix))
			if isAmbiguous(candidate) {
				continue
			}
			if claim(candidate, identity) {
				declarations[i].EffectiveName = candidate
				break
			}
		}
	}
}

func pickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

func applyResponsesFunctionCallNamespaceFields(item []byte, requestRawJSON []byte, qualifiedName string, itemPath string) []byte {
	table, err := BuildResponsesToolDeclarationTable(requestRawJSON)
	if err != nil {
		return item
	}
	prefix := ""
	if itemPath != "" {
		prefix = itemPath + "."
	}
	item, _ = sjson.SetBytes(item, prefix+"name", qualifiedName)
	return table.restoreCallItem(item, itemPath)
}

func responsesCallableToolType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case "", "function", "custom":
		return true
	default:
		return false
	}
}

func responsesDeclarationKind(toolType string) string {
	if strings.TrimSpace(toolType) == "custom" {
		return "custom"
	}
	return "function"
}

func responsesCallReferenceKind(callType string) string {
	switch strings.TrimSpace(callType) {
	case "custom", "custom_tool_call":
		return "custom"
	case "function", "function_call":
		return "function"
	default:
		return ""
	}
}

func sameResponsesDeclarationIdentity(a, b ResponsesToolDeclaration) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name && a.Type == b.Type
}

func responsesDeclarationIdentity(declaration ResponsesToolDeclaration) string {
	return declaration.Namespace + "\x00" + declaration.Name + "\x00" + declaration.Type
}
