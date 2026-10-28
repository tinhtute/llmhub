package util

import (
	"strings"
	"testing"
)

func TestSanitizeFunctionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Normal", "valid_name", "valid_name"},
		{"With Dots", "name.with.dots", "name.with.dots"},
		{"With Colons", "name:with:colons", "name:with:colons"},
		{"With Dashes", "name-with-dashes", "name-with-dashes"},
		{"Mixed Allowed", "name.with_dots:colons-dashes", "name.with_dots:colons-dashes"},
		{"Invalid Characters", "name!with@invalid#chars", "name_with_invalid_chars"},
		{"Spaces", "name with spaces", "name_with_spaces"},
		{"Non-ASCII", "name_with_你好_chars", "name_with____chars"},
		{"Starts with digit", "123name", "_123name"},
		{"Starts with dot", ".name", "_.name"},
		{"Starts with colon", ":name", "_:name"},
		{"Starts with dash", "-name", "_-name"},
		{"Starts with invalid char", "!name", "_name"},
		{"Exactly 64 chars", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact"},
		{"Too long (65 chars)", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charactX", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact"},
		{"Very long", "this_is_a_very_long_name_that_exceeds_the_sixty_four_character_limit_for_function_names", "this_is_a_very_long_name_that_exceeds_the_sixty_four_character_l"},
		{"Starts with digit (64 chars total)", "1234567890123456789012345678901234567890123456789012345678901234", "_123456789012345678901234567890123456789012345678901234567890123"},
		{"Starts with invalid char (64 chars total)", "!234567890123456789012345678901234567890123456789012345678901234", "_234567890123456789012345678901234567890123456789012345678901234"},
		{"Empty", "", ""},
		{"Single character invalid", "@", "_"},
		{"Single character valid", "a", "a"},
		{"Single character digit", "1", "_1"},
		{"Single character underscore", "_", "_"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeFunctionName(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeFunctionName(%q) = %v, want %v", tt.input, got, tt.expected)
			}
			// Verify Gemini compliance
			if len(got) > 64 {
				t.Errorf("SanitizeFunctionName(%q) result too long: %d", tt.input, len(got))
			}
			if len(got) > 0 {
				first := got[0]
				if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
					t.Errorf("SanitizeFunctionName(%q) result starts with invalid char: %c", tt.input, first)
				}
			}
		})
	}
}

func TestSanitizedToolNameMap(t *testing.T) {
	t.Run("returns map for tools needing sanitization", func(t *testing.T) {
		raw := []byte(`{"tools":[
			{"name":"valid_tool","input_schema":{}},
			{"name":"mcp/server/read","input_schema":{}},
			{"name":"tool@v2","input_schema":{}}
		]}`)
		m := SanitizedToolNameMap(raw)
		if m == nil {
			t.Fatal("expected non-nil map")
		}
		if m["mcp_server_read"] != "mcp/server/read" {
			t.Errorf("expected mcp_server_read → mcp/server/read, got %q", m["mcp_server_read"])
		}
		if m["tool_v2"] != "tool@v2" {
			t.Errorf("expected tool_v2 → tool@v2, got %q", m["tool_v2"])
		}
		if _, exists := m["valid_tool"]; exists {
			t.Error("valid_tool should not be in the map (no sanitization needed)")
		}
	})

	t.Run("returns nil when no tools need sanitization", func(t *testing.T) {
		raw := []byte(`{"tools":[{"name":"Read","input_schema":{}},{"name":"Write","input_schema":{}}]}`)
		m := SanitizedToolNameMap(raw)
		if m != nil {
			t.Errorf("expected nil, got %v", m)
		}
	})

	t.Run("returns nil for empty/missing tools", func(t *testing.T) {
		if m := SanitizedToolNameMap([]byte(`{}`)); m != nil {
			t.Error("expected nil for no tools")
		}
		if m := SanitizedToolNameMap(nil); m != nil {
			t.Error("expected nil for nil input")
		}
	})

	t.Run("collision keeps first mapping", func(t *testing.T) {
		raw := []byte(`{"tools":[
			{"name":"read/file","input_schema":{}},
			{"name":"read@file","input_schema":{}}
		]}`)
		m := SanitizedToolNameMap(raw)
		if m == nil {
			t.Fatal("expected non-nil map")
		}
		if m["read_file"] != "read/file" {
			t.Errorf("expected first mapping read/file, got %q", m["read_file"])
		}
	})
}

func TestRestoreSanitizedToolName(t *testing.T) {
	m := map[string]string{
		"mcp_server_read": "mcp/server/read",
		"tool_v2":         "tool@v2",
	}

	if got := RestoreSanitizedToolName(m, "mcp_server_read"); got != "mcp/server/read" {
		t.Errorf("expected mcp/server/read, got %q", got)
	}
	if got := RestoreSanitizedToolName(m, "unknown"); got != "unknown" {
		t.Errorf("expected passthrough for unknown, got %q", got)
	}
	if got := RestoreSanitizedToolName(nil, "name"); got != "name" {
		t.Errorf("expected passthrough for nil map, got %q", got)
	}
	if got := RestoreSanitizedToolName(m, ""); got != "" {
		t.Errorf("expected empty for empty name, got %q", got)
	}
}
func TestSanitizeClaudeFunctionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Normal", "valid_name", "valid_name"},
		{"With Dots", "name.with.dots", "name_with_dots"},
		{"With Colons", "name:with:colons", "name_with_colons"},
		{"With Dashes", "name-with-dashes", "name-with-dashes"},
		{"MCP Tool with Dots and Colons", "mcp.server.special:get_time", "mcp_server_special_get_time"},
		{"With Slashes", "server/action", "server_action"},
		{"Invalid Characters", "name!with@invalid#chars", "name_with_invalid_chars"},
		{"Spaces", "name with spaces", "name_with_spaces"},
		{"Non-ASCII", "name_with_你好_chars", "name_with____chars"},
		{"Empty", "", ""},
		{"Single character valid", "a", "a"},
		{"Single character invalid", "@", "_"},
		{"Starts with digit", "123name", "123name"},
		{"Starts with dash", "-name", "-name"},
		{"Exactly 64 chars", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact"},
		{"Too long (65 chars)", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charactX", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeClaudeFunctionName(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeClaudeFunctionName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
			if len(got) > 64 {
				t.Errorf("SanitizeClaudeFunctionName(%q) result too long: %d", tt.input, len(got))
			}
		})
	}
}

func TestCleanClaudeToolArguments(t *testing.T) {
	origRequest := []byte(`{
		"tools": [
			{"name": "EnterPlanMode", "input_schema": {"type": "object", "properties": {}}},
			{"name": "CronList", "input_schema": {"type": "object"}},
			{"name": "Bash", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}},
			{"name": "CustomToolWithReason", "input_schema": {"type": "object", "properties": {"reason": {"type": "string"}}}},
			{"name": "DynamicTool", "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}, "additionalProperties": true}},
			{"name": "UnionTool", "input_schema": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}}]}}
		]
	}`)

	dynamicRequest := []byte(`{
		"tools": [
			{"name": "MapTool", "input_schema": {"type": "object", "additionalProperties": true}},
			{"name": "TypedMapTool", "input_schema": {"type": "object", "properties": {}, "additionalProperties": {"type": "string"}}}
		]
	}`)
	caseCollisionRequest := []byte(`{
		"tools": [
			{"name": "Read", "input_schema": {"type": "object", "properties": {}}},
			{"name": "read", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}}
		]
	}`)
	composedRequest := []byte(`{
		"tools": [
			{"name": "AllOfEmpty", "input_schema": {"type": "object", "allOf": [{"type": "object", "properties": {}}]}},
			{"name": "AllOfArgs", "input_schema": {"type": "object", "allOf": [{"type": "object", "properties": {"path": {"type": "string"}}}]}},
			{"name": "RefEmpty", "input_schema": {"$ref": "#/$defs/empty", "$defs": {"empty": {"type": "object", "properties": {}}}}},
			{"name": "NestedTool", "input_schema": {"type": "object", "properties": {"opts": {"type": "object", "properties": {}}, "cfg": {"type": "object", "properties": {"mode": {"type": "string"}}}}}}
		]
	}`)
	transferredRequest := []byte(`{
		"model": "gemini-3.8-flash-high",
		"request": {"tools": [{"functionDeclarations": [{"name": "Bash", "parametersJsonSchema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}]}]}
	}`)
	translatedReasonRequired := []byte(`{
		"model": "gemini-3.8-flash-high",
		"request": {"tools": [{"functionDeclarations": [{"name": "Bash", "parametersJsonSchema": {"type": "object", "properties": {"command": {"type": "string"}, "reason": {"type": "string"}}, "required": ["command", "reason"]}}]}]}
	}`)
	uniqueItemsRequest := []byte(`{
		"tools": [
			{"name": "BatchUnique", "input_schema": {"type": "object", "properties": {"rows": {"type": "array", "uniqueItems": true, "items": {"type": "object", "properties": {"tag": {"type": "string"}}}}}, "required": ["rows"], "additionalProperties": false}}
		]
	}`)
	translatedUniqueItems := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "BatchUnique", "parametersJsonSchema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {"tag": {"type": "string"}, "_": {"type": "boolean"}}, "required": ["_"]}}}, "required": ["rows"]}}
		]}]}
	}`)
	minPropsRequest := []byte(`{
		"tools": [
			{"name": "StoreMetadata", "input_schema": {"type": "object", "properties": {}, "minProperties": 1}}
		]
	}`)
	translatedMinProps := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "StoreMetadata", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"], "minProperties": 1}}
		]}]}
	}`)
	siblingAllOfRequest := []byte(`{
		"tools": [
			{"name": "ConfigureSibling", "input_schema": {"type": "object", "required": ["cfg"], "anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {}}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "properties": {"other": {"type": "string"}}, "required": ["other"], "additionalProperties": false}], "allOf": [{"properties": {"cfg": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}]}}
		]
	}`)
	translatedSiblingAllOf := []byte(`{
		"model": "gemini-2.5-pro",
		"tools": [{"functionDeclarations": [
			{"name": "ConfigureSibling", "parametersJsonSchema": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}, "required": ["cfg"]}}
		]}]
	}`)
	arrayContainsRequest := []byte(`{
		"tools": [
			{"name": "BatchContains", "input_schema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {}}, "contains": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}, "required": ["rows"], "additionalProperties": false}}
		]
	}`)
	translatedArrayContains := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "BatchContains", "parametersJsonSchema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}, "required": ["rows"]}}
		]}]}
	}`)
	arrayEnumRequest := []byte(`{
		"tools": [
			{"name": "BatchEnum", "input_schema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {}}, "enum": [[{"reason": "business"}]]}}, "required": ["rows"], "additionalProperties": false}}
		]
	}`)
	translatedArrayEnum := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "BatchEnum", "parametersJsonSchema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}, "required": ["rows"]}}
		]}]}
	}`)
	enumUnionRequest := []byte(`{
		"tools": [
			{"name": "ExplainFixed", "input_schema": {"type": "object", "enum": [{"reason": "business"}], "anyOf": [{"type": "object", "properties": {}}]}}
		]
	}`)
	enumRequest := []byte(`{
		"tools": [
			{"name": "ExplainFixed", "input_schema": {"type": "object", "enum": [{"reason": "business"}]}},
			{"name": "ConstTool", "input_schema": {"type": "object", "const": {"reason": "fixed"}}}
		]
	}`)
	translatedEnum := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ExplainFixed", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}},
			{"name": "ConstTool", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}
		]}]}
	}`)
	collisionRequest := []byte(`{
		"tools": [
			{"name": "1", "input_schema": {"type": "object", "properties": {}, "required": ["reason"]}},
			{"name": "_1", "input_schema": {"type": "object", "properties": {}}}
		]
	}`)
	round15Request := []byte(`{
		"tools": [
			{"name": "Batch", "input_schema": {"type": "object", "anyOf": [{"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {}, "additionalProperties": false}}}, "required": ["rows"], "additionalProperties": false}, {"type": "object", "properties": {"rows": {}}, "required": ["rows"], "additionalProperties": false}]}},
			{"name": "BatchStrict", "input_schema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {}, "additionalProperties": false}}}, "required": ["rows"]}}
		]
	}`)
	translatedRound15 := []byte(`{
		"model": "gemini-2.5-pro",
		"tools": [{"functionDeclarations": [
			{"name": "Batch", "parametersJsonSchema": {"type": "object", "anyOf": [{"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {}}}}, "required": ["rows"]}, {"type": "object", "properties": {"rows": {}}, "required": ["rows"]}]}},
			{"name": "BatchStrict", "parametersJsonSchema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}, "required": ["rows"]}}
		]}]
	}`)
	round14Request := []byte(`{
		"tools": [
			{"name": "ConfigureAltFree", "input_schema": {"type": "object", "anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {}, "additionalProperties": false}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "properties": {"cfg": {}}, "required": ["cfg"], "additionalProperties": false}]}}
		]
	}`)
	translatedRound14 := []byte(`{
		"model": "gemini-2.5-pro",
		"tools": [{"functionDeclarations": [
			{"name": "ConfigureAltFree", "parametersJsonSchema": {"type": "object", "anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {}}}, "required": ["cfg"]}, {"type": "object", "properties": {"cfg": {}}, "required": ["cfg"]}]}}
		]}]
	}`)
	round13Request := []byte(`{
		"tools": [
			{"name": "DeepAlt", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {"opts": {"type": "object", "properties": {"mode": {"type": "string"}}, "additionalProperties": false}}, "required": ["opts"], "additionalProperties": false}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"tag": {"type": "string"}}, "required": ["tag"]}}, "required": ["cfg"], "additionalProperties": false}]}}, "required": ["choice"], "additionalProperties": false}}
		]
	}`)
	translatedRound13 := []byte(`{
		"model": "gemini-3-pro-high",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "DeepAlt", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"opts": {"type": "object", "properties": {"mode": {"type": "string"}, "_": {"type": "boolean"}}, "required": ["_"]}}, "required": ["opts"]}}, "required": ["cfg"]}}, "required": ["choice"]}}
		]}]}
	}`)
	round12Request := []byte(`{
		"tools": [
			{"name": "ConfigureAlt", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {}, "additionalProperties": false}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}, "required": ["mode"]}}, "required": ["cfg"], "additionalProperties": false}]}}, "required": ["choice"], "additionalProperties": false}}
		]
	}`)
	translatedRound12 := []byte(`{
		"model": "gemini-3-pro-high",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ConfigureAlt", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}, "required": ["cfg"]}, {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}, "required": ["mode"]}}, "required": ["cfg"]}]}}, "required": ["choice"]}}
		]}]}
	}`)
	round11Request := []byte(`{
		"tools": [
			{"name": "ChooseImplicit", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {}, "additionalProperties": false}, {"type": "object", "properties": {"tag": {"type": "string"}}, "required": ["tag"]}]}}, "required": ["choice"]}}
		]
	}`)
	translatedRound11 := []byte(`{
		"model": "gemini-3-pro-high",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ChooseImplicit", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}, {"type": "object", "properties": {"tag": {"type": "string"}}, "required": ["tag"]}]}}, "required": ["choice"]}}
		]}]}
	}`)
	round10Request := []byte(`{
		"tools": [
			{"name": "ImplicitBranch", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}, "additionalProperties": false}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "properties": {"tag": {"type": "string"}}, "required": ["tag"]}]}}, "required": ["choice"], "additionalProperties": false}}
		]
	}`)
	translatedRound10 := []byte(`{
		"model": "gemini-3.1-pro",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ImplicitBranch", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}, "_": {"type": "boolean"}}, "required": ["_"]}}, "required": ["cfg"]}}, "required": ["choice"]}}
		]}]}
	}`)
	round9Request := []byte(`{
		"tools": [
			{"name": "ConfigureNested", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}, "additionalProperties": false}}, "required": ["cfg"], "additionalProperties": false}, {"type": "object", "patternProperties": {"^cfg$": {"type": "object", "additionalProperties": true}}, "additionalProperties": false}]}}, "required": ["choice"]}}
		]
	}`)
	translatedRound9 := []byte(`{
		"model": "gemini-3.1-pro",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ConfigureNested", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}, "_": {"type": "boolean"}}, "required": ["_"]}}, "required": ["cfg"]}}, "required": ["choice"]}}
		]}]}
	}`)
	round8Request := []byte(`{
		"tools": [
			{"name": "ExplainPattern", "input_schema": {"type": "object", "properties": {}, "patternProperties": {"^reason$": {"type": "string"}}, "additionalProperties": false}},
			{"name": "PatternData", "input_schema": {"type": "object", "properties": {}, "patternProperties": {"^x-": {"type": "string"}}, "additionalProperties": false}}
		]
	}`)
	translatedRound8 := []byte(`{
		"model": "gemini-3.1-pro",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "ExplainPattern", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}},
			{"name": "PatternData", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}
		]}]}
	}`)
	round7Request := []byte(`{
		"tools": [
			{"name": "Configure", "input_schema": {"type": "object", "properties": {"cfg": {"anyOf": [{"type": "object", "properties": {}, "additionalProperties": false}, {"type": "object", "additionalProperties": true}]}}, "required": ["cfg"]}}
		]
	}`)
	translatedRound7 := []byte(`{
		"model": "gemini-3.1-pro",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "Configure", "parametersJsonSchema": {"type": "object", "properties": {"cfg": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}, {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}]}}, "required": ["cfg"]}}
		]}]}
	}`)
	round6Request := []byte(`{
		"tools": [
			{"name": "PlainOptions", "input_schema": {"type": "object", "properties": {"flag": {"type": "string"}}}},
			{"name": "StrictOptions", "input_schema": {"type": "object", "properties": {"flag": {"type": "string"}}, "additionalProperties": false}},
			{"name": "BranchChoice", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "additionalProperties": true}}, "required": ["cfg"]}, {"type": "object", "properties": {"cfg": {"type": "object", "properties": {}, "additionalProperties": false}}, "required": ["cfg"]}]}}, "required": ["choice"]}}
		]
	}`)
	translatedRound6 := []byte(`{
		"model": "gemini-2.5-pro",
		"tools": [{"functionDeclarations": [
			{"name": "PlainOptions", "parametersJsonSchema": {"type": "object", "properties": {"flag": {"type": "string"}}}},
			{"name": "StrictOptions", "parametersJsonSchema": {"type": "object", "properties": {"flag": {"type": "string"}}}},
			{"name": "BranchChoice", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"cfg": {"type": "object", "additionalProperties": true}}, "required": ["cfg"]}, {"type": "object", "properties": {"cfg": {"type": "object", "properties": {}}}, "required": ["cfg"]}]}}, "required": ["choice"]}}
		]}]
	}`)
	round5Request := []byte(`{
		"tools": [
			{"name": "Explain", "input_schema": {"type": "object", "required": ["reason"], "additionalProperties": true}},
			{"name": "Choose", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"opts": {"type": "object", "properties": {}, "additionalProperties": false}}, "required": ["opts"]}, {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}]}}, "required": ["choice"]}},
			{"name": "Options", "input_schema": {"type": "object", "properties": {"flag": {"type": "string"}}, "additionalProperties": true}}
		]
	}`)
	translatedRound5 := []byte(`{
		"model": "gemini-3.8-flash-high",
		"tools": [{"functionDeclarations": [
			{"name": "Explain", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}},
			{"name": "Choose", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"type": "object", "properties": {"opts": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}, "required": ["opts"]}}, "required": ["choice"]}},
			{"name": "Options", "parametersJsonSchema": {"type": "object", "properties": {"flag": {"type": "string"}}, "additionalProperties": true}}
		]}]
	}`)
	round4Request := []byte(`{
		"tools": [
			{"name": "UnionChoice", "input_schema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}, {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}]}}, "required": ["choice"]}},
			{"name": "AllOfChildMerge", "input_schema": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}}}, "allOf": [{"properties": {"cfg": {"type": "object", "properties": {}}}}]}},
			{"name": "NestedRefTool", "input_schema": {"type": "object", "properties": {"opts": {"$ref": "#/$defs/Empty"}}, "$defs": {"Empty": {"type": "object", "properties": {}, "additionalProperties": false}}}},
			{"name": "WrappedPlaceholder", "input_schema": {"type": "object", "properties": {"flag": {"type": "string"}}, "additionalProperties": false}}
		]
	}`)
	translatedRound4 := []byte(`{
		"model": "gemini-3.8-flash-high",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "UnionChoice", "parametersJsonSchema": {"type": "object", "properties": {"choice": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}, {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}]}}, "required": ["choice"]}},
			{"name": "AllOfChildMerge", "parametersJsonSchema": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}}}}},
			{"name": "NestedRefTool", "parametersJsonSchema": {"type": "object", "properties": {"opts": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}},
			{"name": "WrappedPlaceholder", "parametersJsonSchema": {"type": "object", "properties": {"flag": {"type": "string"}}}}
		]}]}
	}`)
	extraRequest := []byte(`{
		"tools": [
			{"name": "AllOfDynamicConflict", "input_schema": {"type": "object", "properties": {}, "additionalProperties": false, "allOf": [{"additionalProperties": true}]}},
			{"name": "MultiUnionEmpty", "input_schema": {"anyOf": [{"type": "object", "properties": {}, "additionalProperties": false}, {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"], "additionalProperties": false}]}},
			{"name": "DupTool", "input_schema": {"type": "object", "properties": {}}},
			{"name": "DupTool", "input_schema": {"type": "object", "properties": {}}},
			{"name": "ConflictingDup", "input_schema": {"type": "object", "properties": {}}},
			{"name": "ConflictingDup", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}},
			{"name": "MatrixTool", "input_schema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "array", "items": {"type": "object", "properties": {}}}}}}}
		]
	}`)
	translatedExtra := []byte(`{
		"model": "gemini-3.8-flash-high",
		"request": {"tools": [{"functionDeclarations": [
			{"name": "AllOfDynamicConflict", "parametersJsonSchema": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}},
			{"name": "MultiUnionEmpty", "parametersJsonSchema": {"anyOf": [{"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}, {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]}]}},
			{"name": "MatrixTool", "parametersJsonSchema": {"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "array", "items": {"type": "object", "properties": {"reason": {"type": "string"}}, "required": ["reason"]}}}}}}
		]}]}
	}`)
	deepRequest := []byte(`{
		"tools": [
			{"name": "FreeFormNested", "input_schema": {"type": "object", "properties": {"payload": {}}, "required": ["payload"]}},
			{"name": "AllOfMerge", "input_schema": {"type": "object", "properties": {"cfg": {"type": "object", "properties": {"mode": {"type": "string"}}}}, "allOf": [{"type": "object", "properties": {"cfg": {"type": "object", "properties": {"reason": {"type": "string"}}}}}]}},
			{"name": "SingleUnionEmpty", "input_schema": {"anyOf": [{"type": "object", "properties": {}}]}},
			{"name": "DynamicParent", "input_schema": {"type": "object", "properties": {"opts": {"type": "object", "properties": {}}}, "additionalProperties": true}},
			{"name": "RefArray", "input_schema": {"type": "object", "properties": {"items": {"$ref": "#/$defs/list"}}, "$defs": {"list": {"type": "array", "items": {"type": "object", "properties": {"v": {"type": "string"}}}}}}},
			{"name": "PropRef", "input_schema": {"type": "object", "properties": {"template": {"type": "object", "properties": {}}, "copy": {"$ref": "#/properties/template"}}}}
		]
	}`)

	tests := []struct {
		name        string
		rawArgs     string
		toolName    string
		origReq     []byte
		want        string
		transferred []byte
	}{
		{"injected placeholder stripped on implicitly open schema", `{"reason":"Entering plan mode"}`, "EnterPlanMode", origRequest, "{}", nil},
		{"injected placeholder stripped when properties omitted", `{"reason":"List jobs"}`, "CronList", origRequest, "{}", nil},
		{"declared args preserved", `{"command":"ls -la"}`, "Bash", origRequest, `{"command":"ls -la"}`, nil},
		{"undeclared reason stripped", `{"command":"ls -la","reason":"why"}`, "Bash", origRequest, `{"command":"ls -la"}`, nil},
		{"reason kept when translated schema lacks placeholder", `{"command":"ls -la","reason":"why"}`, "Bash", origRequest, `{"command":"ls -la","reason":"why"}`, transferredRequest},
		{"reason stripped when translated schema marks placeholder", `{"command":"ls -la","reason":"why"}`, "Bash", origRequest, `{"command":"ls -la"}`, translatedReasonRequired},
		{"undeclared underscore stripped", `{"command":"ls -la","_":true}`, "Bash", origRequest, `{"command":"ls -la"}`, nil},
		{"declared reason preserved", `{"reason":"valid"}`, "CustomToolWithReason", origRequest, `{"reason":"valid"}`, nil},
		{"additional properties preserved", `{"name":"x","reason":"dynamic"}`, "DynamicTool", origRequest, `{"name":"x","reason":"dynamic"}`, nil},
		{"union schema passthrough", `{"reason":"x"}`, "UnionTool", origRequest, `{"reason":"x"}`, nil},
		{"unknown tool passthrough", `{"reason":"x"}`, "EnterPlanMode", nil, `{"reason":"x"}`, nil},
		{"empty args", "", "EnterPlanMode", origRequest, "{}", nil},
		{"empty object args", "{}", "EnterPlanMode", origRequest, "{}", nil},
		{"dynamic map tool preserved", `{"key":"value"}`, "MapTool", dynamicRequest, `{"key":"value"}`, nil},
		{"typed map tool preserved", `{"key":"value"}`, "TypedMapTool", dynamicRequest, `{"key":"value"}`, nil},
		{"case collision exact match keeps args", `{"path":"/tmp/a"}`, "read", caseCollisionRequest, `{"path":"/tmp/a"}`, nil},
		{"case collision exact match placeholder stripped", `{"reason":"x"}`, "Read", caseCollisionRequest, "{}", nil},
		{"allOf empty normalized", `{"reason":"x"}`, "AllOfEmpty", composedRequest, "{}", nil},
		{"allOf declared args preserved", `{"path":"/tmp/a"}`, "AllOfArgs", composedRequest, `{"path":"/tmp/a"}`, nil},
		{"ref to empty normalized", `{"reason":"x"}`, "RefEmpty", composedRequest, "{}", nil},
		{"nested placeholders stripped", `{"opts":{"reason":"x"},"cfg":{"mode":"fast","_":true}}`, "NestedTool", composedRequest, `{"opts":{},"cfg":{"mode":"fast"}}`, nil},
		{"free-form nested preserved", `{"payload":{"value":1}}`, "FreeFormNested", deepRequest, `{"payload":{"value":1}}`, nil},
		{"allOf deep merge keeps declared reason", `{"cfg":{"mode":"a","reason":"b"}}`, "AllOfMerge", deepRequest, `{"cfg":{"mode":"a","reason":"b"}}`, nil},
		{"single union empty normalized", `{"reason":"x"}`, "SingleUnionEmpty", deepRequest, "{}", nil},
		{"dynamic parent still cleans declared child", `{"extra":1,"opts":{"reason":"x"}}`, "DynamicParent", deepRequest, `{"extra":1,"opts":{}}`, nil},
		{"ref array items cleaned", `{"items":[{"v":"a","_":true}]}`, "RefArray", deepRequest, `{"items":[{"v":"a"}]}`, nil},
		{"property ref cleaned", `{"copy":{"reason":"x"},"template":{}}`, "PropRef", deepRequest, `{"copy":{},"template":{}}`, nil},
		{"allOf dynamic conflict uses translated schema", `{"reason":"x"}`, "AllOfDynamicConflict", extraRequest, "{}", translatedExtra},
		{"multi-branch union uses translated schema", `{"reason":"x"}`, "MultiUnionEmpty", extraRequest, "{}", translatedExtra},
		{"identical duplicates cleaned", `{"reason":"x"}`, "DupTool", extraRequest, "{}", nil},
		{"conflicting duplicates passthrough", `{"path":"/tmp/a"}`, "ConflictingDup", extraRequest, `{"path":"/tmp/a"}`, nil},
		{"conflicting duplicates keep first-declared cleaning", `{"reason":"x"}`, "ConflictingDup", extraRequest, `{"reason":"x"}`, nil},
		{"matrix nested arrays cleaned", `{"rows":[[{"reason":"x"}]]}`, "MatrixTool", extraRequest, `{"rows":[[{}]]}`, translatedExtra},
		{"union declared required reason preserved", `{"choice":{"reason":"maintenance"}}`, "UnionChoice", round4Request, `{"choice":{"reason":"maintenance"}}`, translatedRound4},
		{"allOf child merge keeps mode", `{"cfg":{"mode":"fast"}}`, "AllOfChildMerge", round4Request, `{"cfg":{"mode":"fast"}}`, translatedRound4},
		{"nested ref placeholder cleaned", `{"opts":{"reason":"initialize"}}`, "NestedRefTool", round4Request, `{"opts":{}}`, translatedRound4},
		{"required-only reason preserved", `{"reason":"maintenance"}`, "Explain", round5Request, `{"reason":"maintenance"}`, translatedRound5},
		{"union branch nested placeholder cleaned", `{"choice":{"opts":{"reason":"initialize"}}}`, "Choose", round5Request, `{"choice":{"opts":{}}}`, translatedRound5},
		{"gemini route keeps dynamic underscore", `{"flag":"x","_":true}`, "Options", round5Request, `{"flag":"x","_":true}`, translatedRound5},
		{"implicit additional properties keeps underscore", `{"flag":"x","_":true}`, "PlainOptions", round6Request, `{"flag":"x","_":true}`, translatedRound6},
		{"closed object still strips executor underscore", `{"flag":"x","_":true}`, "StrictOptions", round6Request, `{"flag":"x"}`, translatedRound6},
		{"alternative branch dynamic data preserved", `{"choice":{"cfg":{"value":1}}}`, "BranchChoice", round6Request, `{"choice":{"cfg":{"value":1}}}`, translatedRound6},
		{"alternative branch dynamic reason preserved", `{"cfg":{"reason":"business"}}`, "Configure", round7Request, `{"cfg":{"reason":"business"}}`, translatedRound7},
		{"pattern property reason preserved", `{"reason":"business"}`, "ExplainPattern", round8Request, `{"reason":"business"}`, translatedRound8},
		{"pattern property non-placeholder data preserved", `{"x-note":"keep"}`, "PatternData", round8Request, `{"x-note":"keep"}`, translatedRound8},
		{"union branch pattern child preserved", `{"choice":{"cfg":{"mode":"fast","_":true}}}`, "ConfigureNested", round9Request, `{"choice":{"cfg":{"mode":"fast","_":true}}}`, translatedRound9},
		{"implicit open branch child preserved", `{"choice":{"tag":"keep","cfg":{"mode":"fast","_":true}}}`, "ImplicitBranch", round10Request, `{"choice":{"tag":"keep","cfg":{"mode":"fast","_":true}}}`, translatedRound10},
		{"implicit open branch field preserved", `{"choice":{"tag":"keep","reason":"business"}}`, "ChooseImplicit", round11Request, `{"choice":{"tag":"keep","reason":"business"}}`, translatedRound11},
		{"implicit open branch non-matching object cleaned", `{"choice":{"reason":"x"}}`, "ChooseImplicit", round11Request, `{"choice":{}}`, translatedRound11},
		{"alternative child candidates keep matched data", `{"choice":{"cfg":{"mode":"fast","reason":"business"}}}`, "ConfigureAlt", round12Request, `{"choice":{"cfg":{"mode":"fast","reason":"business"}}}`, translatedRound12},
		{"alternative child candidates clean unmatched placeholder", `{"choice":{"cfg":{"reason":"x"}}}`, "ConfigureAlt", round12Request, `{"choice":{"cfg":{}}}`, translatedRound12},
		{"deep alternative candidates keep matched subtree", `{"choice":{"cfg":{"tag":"keep","opts":{"mode":"fast","_":true}}}}`, "DeepAlt", round13Request, `{"choice":{"cfg":{"tag":"keep","opts":{"mode":"fast","_":true}}}}`, translatedRound13},
		{"deep alternative candidates clean unmatched subtree", `{"choice":{"cfg":{"opts":{"mode":"fast","_":true}}}}`, "DeepAlt", round13Request, `{"choice":{"cfg":{"opts":{"mode":"fast"}}}}`, translatedRound13},
		{"free-form alternative child preserved", `{"cfg":{"reason":"business","value":1}}`, "ConfigureAltFree", round14Request, `{"cfg":{"reason":"business","value":1}}`, translatedRound14},
		{"free-form array candidate preserved", `{"rows":[{"value":1}]}`, "Batch", round15Request, `{"rows":[{"value":1}]}`, translatedRound15},
		{"strict array placeholder cleaned", `{"rows":[{"reason":"x"}]}`, "BatchStrict", round15Request, `{"rows":[{}]}`, translatedRound15},
		{"fallback keeps required-only reason", `{"reason":"business"}`, "1", collisionRequest, `{"reason":"business"}`, nil},
		{"object enum value preserved", `{"reason":"business"}`, "ExplainFixed", enumRequest, `{"reason":"business"}`, translatedEnum},
		{"object enum with union preserved", `{"reason":"business"}`, "ExplainFixed", enumUnionRequest, `{"reason":"business"}`, translatedEnum},
		{"array enum elements preserved", `{"rows":[{"reason":"business"}]}`, "BatchEnum", arrayEnumRequest, `{"rows":[{"reason":"business"}]}`, translatedArrayEnum},
		{"array enum elements preserved fallback", `{"rows":[{"reason":"business"}]}`, "BatchEnum", arrayEnumRequest, `{"rows":[{"reason":"business"}]}`, nil},
		{"array contains elements preserved", `{"rows":[{"reason":"business"}]}`, "BatchContains", arrayContainsRequest, `{"rows":[{"reason":"business"}]}`, translatedArrayContains},
		{"array contains elements preserved fallback", `{"rows":[{"reason":"business"}]}`, "BatchContains", arrayContainsRequest, `{"rows":[{"reason":"business"}]}`, nil},
		{"sibling allOf declaration preserved", `{"cfg":{"reason":"business"}}`, "ConfigureSibling", siblingAllOfRequest, `{"cfg":{"reason":"business"}}`, translatedSiblingAllOf},
		{"minProperties object preserved", `{"reason":"store","label":"keep"}`, "StoreMetadata", minPropsRequest, `{"reason":"store","label":"keep"}`, translatedMinProps},
		{"minProperties object preserved fallback", `{"reason":"store","label":"keep"}`, "StoreMetadata", minPropsRequest, `{"reason":"store","label":"keep"}`, nil},
		{"uniqueItems elements preserved", `{"rows":[{"tag":"same","_":true},{"tag":"same","_":false}]}`, "BatchUnique", uniqueItemsRequest, `{"rows":[{"tag":"same","_":true},{"tag":"same","_":false}]}`, translatedUniqueItems},
		{"uniqueItems elements preserved fallback", `{"rows":[{"tag":"same","_":true},{"tag":"same","_":false}]}`, "BatchUnique", uniqueItemsRequest, `{"rows":[{"tag":"same","_":true},{"tag":"same","_":false}]}`, nil},
		{"object enum value preserved fallback", `{"reason":"business"}`, "ExplainFixed", enumRequest, `{"reason":"business"}`, nil},
		{"object const value preserved", `{"reason":"fixed"}`, "ConstTool", enumRequest, `{"reason":"fixed"}`, translatedEnum},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanClaudeToolArgumentsWithTranslated(tt.rawArgs, tt.toolName, tt.origReq, tt.transferred); got != tt.want {
				t.Errorf("CleanClaudeToolArguments(%q, %q) = %q, want %q", tt.rawArgs, tt.toolName, got, tt.want)
			}
		})
	}
}

func TestMapToolNameRestoresDeclaredName(t *testing.T) {
	// The response translators restore client-facing names through this pair:
	// build the map once per stream, then map per tool call.
	origReq := []byte(`{
		"tools": [
			{"name": "Read", "input_schema": {"type": "object"}},
			{"name": "duplicate_case", "input_schema": {"type": "object"}},
			{"name": "Duplicate_Case", "input_schema": {"type": "object"}}
		]
	}`)
	toolMap := UnambiguousToolNameMapFromClaudeRequest(origReq)

	tests := []struct {
		name     string
		toolName string
		want     string
	}{
		{"exact declared name", "Read", "Read"},
		{"canonical match restores declaration", "read", "Read"},
		{"ambiguous collision bypassed", "DUPLICATE_CASE", "DUPLICATE_CASE"},
		{"ambiguous collision keeps declared spelling", "duplicate_case", "duplicate_case"},
		{"empty name", "", ""},
		{"unmatched fallback", "unknown_tool", "unknown_tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MapToolName(toolMap, tt.toolName); got != tt.want {
				t.Errorf("MapToolName(%q) = %q, want %q", tt.toolName, got, tt.want)
			}
		})
	}
}

func TestClaudeToolIsParameterless(t *testing.T) {
	// An object that merely omits additionalProperties permits dynamic properties by
	// JSON Schema default, so it is not a parameterless declaration: only a missing
	// schema or an explicitly closed one (additionalProperties: false) is. The two
	// implicit-open rows below used to pin `true`; that expectation was the defect this
	// test now guards against (upstream cliproxyapi v8.0.41 e1fb25219c09).
	origReq := []byte(`{
		"tools": [
			{"name": "ImplicitOpenProperties", "input_schema": {"type": "object", "properties": {}}},
			{"name": "ImplicitOpenNoProperties", "input_schema": {"type": "object"}},
			{"name": "NoSchema"},
			{"name": "HasProperties", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}}},
			{"name": "DynamicTool", "input_schema": {"type": "object", "properties": {}, "additionalProperties": true}},
			{"name": "PatternTool", "input_schema": {"type": "object", "patternProperties": {"^x-": {"type": "string"}}}},
			{"name": "ClosedEmptyProperties", "input_schema": {"type": "object", "properties": {}, "additionalProperties": false}},
			{"name": "ClosedNoProperties", "input_schema": {"type": "object", "additionalProperties": false}}
		]
	}`)

	tests := []struct {
		name     string
		toolName string
		want     bool
	}{
		{"implicitly open empty properties", "ImplicitOpenProperties", false},
		{"implicitly open missing properties", "ImplicitOpenNoProperties", false},
		{"no schema", "NoSchema", true},
		{"has properties", "HasProperties", false},
		{"dynamic tool", "DynamicTool", false},
		{"pattern tool", "PatternTool", false},
		{"explicitly closed empty properties", "ClosedEmptyProperties", true},
		{"explicitly closed missing properties", "ClosedNoProperties", true},
		{"unknown tool", "NonExistent", false},
		{"empty tool name", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudeToolIsParameterless(tt.toolName, origReq); got != tt.want {
				t.Errorf("ClaudeToolIsParameterless(%q) = %v, want %v", tt.toolName, got, tt.want)
			}
		})
	}
}

// TestUnambiguousToolNameMapCollisionPolicy pins the rule the precomputed map
// encodes, independent of the implementation: a canonical key declared once
// restores that spelling; a key declared with two different spellings keeps the
// upstream name for every spelling; anything undeclared is returned unchanged.
// The curated fixtures cover the edge shapes and 500 deterministic pseudo-random
// declarations exercise the rule broadly (plan v8040-p3, upstream 80d04e1).
func TestUnambiguousToolNameMapCollisionPolicy(t *testing.T) {
	canonical := func(name string) string {
		return strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "_"))
	}
	// expectedName is the rule restated: the declared spelling for name, or name
	// itself when the canonical key is ambiguous or undeclared. Declarations are
	// stored trimmed, so a padded declaration restores its trimmed spelling.
	expectedName := func(declared []string, name string) string {
		key := canonical(name)
		if key == "" {
			return name
		}
		match := ""
		for _, d := range declared {
			trimmed := strings.TrimSpace(d)
			if canonical(trimmed) != key {
				continue
			}
			if match != "" && trimmed != match {
				return name
			}
			match = trimmed
		}
		if match == "" {
			return name
		}
		return match
	}

	names := []string{"", "Read", "read", "READ", "_Read", "__read", "Write", "write", "Spaced", "spaced", "  Spaced  ", "Missing", "read_file", "mcp__x__y"}
	declaredSets := [][]string{
		nil,
		{},
		{"Read"},
		{"Read", "read"},
		{"Read", "Read"},
		{"_Read", "read", "READ"},
		{"Write"},
		{"  Spaced  ", "spaced"},
		{"Empty"},
		{"__"},
	}

	// Deterministic pseudo-random fixtures: no seeding dependency on the runtime.
	state := uint64(1)
	next := func(n int) int {
		state = state*6364136223846793005 + 1442695040888963407
		return int((state >> 33) % uint64(n))
	}
	pool := []string{"Read", "read", "READ", "_Read", "Write", "write", "spaced", "  Spaced  ", "Run", "run", "mcp__x__y", "", "__"}
	for i := 0; i < 500; i++ {
		set := make([]string, 0, 4)
		for j, count := 0, 1+next(4); j < count; j++ {
			set = append(set, pool[next(len(pool))])
		}
		declaredSets = append(declaredSets, set)
	}

	for _, declared := range declaredSets {
		var b strings.Builder
		b.WriteString(`{"tools":[`)
		for j, name := range declared {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"name":"` + name + `"}`)
		}
		b.WriteString(`]}`)
		request := []byte(b.String())
		toolMap := UnambiguousToolNameMapFromClaudeRequest(request)
		for _, name := range names {
			if got, want := MapToolName(toolMap, name), expectedName(declared, name); got != want {
				t.Fatalf("MapToolName(%q) over declared %q = %q, want %q", name, declared, got, want)
			}
		}
	}

	// Declarations may carry the name under "function.name".
	functionShape := UnambiguousToolNameMapFromClaudeRequest([]byte(`{"tools":[{"function":{"name":"Write"}}]}`))
	if got := MapToolName(functionShape, "write"); got != "Write" {
		t.Fatalf("function.name declaration mapped %q, want Write", got)
	}

	// A request with no usable tools yields a nil map, and every name passes through.
	for _, request := range [][]byte{nil, []byte(``), []byte(`{`), []byte(`{}`), []byte(`{"tools":[]}`)} {
		if got := MapToolName(UnambiguousToolNameMapFromClaudeRequest(request), "Read"); got != "Read" {
			t.Fatalf("MapToolName over %q = %q, want passthrough", request, got)
		}
	}
}

// TestCleanClaudeToolArguments_ImplicitOpenObjectPreservesArguments pins the whole-object
// argument rule for every entry point: a schema that only *permits* dynamic properties
// (additionalProperties omitted or true) is not a parameterless declaration, so real
// arguments survive; only a schema that explicitly forbids them (additionalProperties:
// false) or omits the schema entirely is normalized to "{}". Nested objects, array items,
// allOf branches and local $ref targets follow the same open/closed split.
func TestCleanClaudeToolArguments_ImplicitOpenObjectPreservesArguments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		schema     string
		want       string
		wantNoArgs bool
	}{
		{"additionalProperties_omitted", `{"type":"object","properties":{}}`, `{"user":"v"}`, false},
		{"additionalProperties_omitted_no_properties", `{"type":"object"}`, `{"user":"v"}`, false},
		{"additionalProperties_true", `{"type":"object","properties":{},"additionalProperties":true}`, `{"user":"v"}`, false},
		{"additionalProperties_false", `{"type":"object","properties":{},"additionalProperties":false}`, `{}`, true},
		{"additionalProperties_false_no_properties", `{"type":"object","additionalProperties":false}`, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(`{"tools":[{"name":"echo","input_schema":` + tc.schema + `}]}`)
			cleanedSchema := CleanJSONSchemaForGeminiJSONSchema(tc.schema)
			translated := []byte(`{"tools":[{"functionDeclarations":[{"name":"echo","parametersJsonSchema":` + cleanedSchema + `}]}]}`)
			args := `{"user":"v"}`
			direct := CleanClaudeToolArguments(args, "echo", original)
			withTranslated := CleanClaudeToolArgumentsWithTranslated(args, "echo", original, translated)
			noArgs := ClaudeToolIsParameterless("echo", original)
			if direct != tc.want || withTranslated != tc.want || noArgs != tc.wantNoArgs {
				t.Errorf("want direct/translated=%s parameterless=%v; got direct=%s translated=%s parameterless=%v",
					tc.want, tc.wantNoArgs, direct, withTranslated, noArgs)
			}
		})
	}

	t.Run("implicit_open_strips_injected_placeholder_but_keeps_user_arg", func(t *testing.T) {
		schema := `{"type":"object","properties":{}}`
		original := []byte(`{"tools":[{"name":"echo","input_schema":` + schema + `}]}`)
		antigravitySchema := CleanJSONSchemaForAntigravity(schema)
		translated := []byte(`{"tools":[{"functionDeclarations":[{"name":"echo","parametersJsonSchema":` + antigravitySchema + `}]}]}`)
		args := `{"user":"v","reason":"Brief explanation"}`
		if got := CleanClaudeToolArguments(args, "echo", original); got != `{"user":"v"}` {
			t.Errorf("CleanClaudeToolArguments = %s, want {\"user\":\"v\"}", got)
		}
		if got := CleanClaudeToolArgumentsWithTranslated(args, "echo", original, translated); got != `{"user":"v"}` {
			t.Errorf("CleanClaudeToolArgumentsWithTranslated = %s, want {\"user\":\"v\"}", got)
		}
	})

	for _, tc := range []struct {
		name       string
		schema     string
		args       string
		want       string
		wantNoArgs bool
	}{
		{
			"nested_implicit_open_preserves_user_arg",
			`{"type":"object","properties":{"opts":{"type":"object","properties":{}}}}`,
			`{"opts":{"user":"v"}}`,
			`{"opts":{"user":"v"}}`,
			false,
		},
		{
			"nested_explicit_closed_erases_user_arg",
			`{"type":"object","properties":{"opts":{"type":"object","properties":{},"additionalProperties":false}}}`,
			`{"opts":{"user":"v"}}`,
			`{"opts":{}}`,
			false,
		},
		{
			"array_item_implicit_open_preserves_user_arg",
			`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{}}}}}`,
			`{"items":[{"user":"v"}]}`,
			`{"items":[{"user":"v"}]}`,
			false,
		},
		{
			"array_item_explicit_closed_erases_user_arg",
			`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{},"additionalProperties":false}}}}`,
			`{"items":[{"user":"v"}]}`,
			`{"items":[{}]}`,
			false,
		},
		{
			"allOf_implicit_open_preserves_user_arg",
			`{"type":"object","allOf":[{"type":"object","properties":{}}]}`,
			`{"user":"v"}`,
			`{"user":"v"}`,
			false,
		},
		{
			"allOf_explicit_closed_erases_user_arg",
			`{"type":"object","allOf":[{"type":"object","properties":{},"additionalProperties":false}]}`,
			`{"user":"v"}`,
			`{}`,
			true,
		},
		{
			"local_ref_implicit_open_preserves_user_arg",
			`{"$ref":"#/$defs/open","$defs":{"open":{"type":"object","properties":{}}}}`,
			`{"user":"v"}`,
			`{"user":"v"}`,
			false,
		},
		{
			"local_ref_explicit_closed_erases_user_arg",
			`{"$ref":"#/$defs/closed","$defs":{"closed":{"type":"object","properties":{},"additionalProperties":false}}}`,
			`{"user":"v"}`,
			`{}`,
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(`{"tools":[{"name":"echo","input_schema":` + tc.schema + `}]}`)
			cleanedSchema := CleanJSONSchemaForGeminiJSONSchema(tc.schema)
			translated := []byte(`{"tools":[{"functionDeclarations":[{"name":"echo","parametersJsonSchema":` + cleanedSchema + `}]}]}`)
			direct := CleanClaudeToolArguments(tc.args, "echo", original)
			withTranslated := CleanClaudeToolArgumentsWithTranslated(tc.args, "echo", original, translated)
			noArgs := ClaudeToolIsParameterless("echo", original)
			if direct != tc.want || withTranslated != tc.want || noArgs != tc.wantNoArgs {
				t.Errorf("want direct/translated=%s parameterless=%v; got direct=%s translated=%s parameterless=%v",
					tc.want, tc.wantNoArgs, direct, withTranslated, noArgs)
			}
		})
	}
}
