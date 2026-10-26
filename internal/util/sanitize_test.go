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
		{"parameterless empty properties", `{"reason":"Entering plan mode"}`, "EnterPlanMode", origRequest, "{}", nil},
		{"parameterless missing properties", `{"reason":"List jobs"}`, "CronList", origRequest, "{}", nil},
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
		{"case collision exact match parameterless", `{"reason":"x"}`, "Read", caseCollisionRequest, "{}", nil},
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

func TestMapClaudeToolName(t *testing.T) {
	origReq := []byte(`{
		"tools": [
			{"name": "Read", "input_schema": {"type": "object"}},
			{"name": "mcp/server/read", "input_schema": {"type": "object"}},
			{"name": "duplicate_case", "input_schema": {"type": "object"}},
			{"name": "Duplicate_Case", "input_schema": {"type": "object"}}
		]
	}`)
	toolMap := map[string]string{
		"read":            "Read",
		"mcp_server_read": "mcp/server/read",
	}

	tests := []struct {
		name     string
		toolName string
		want     string
	}{
		{"exact match in request", "Read", "Read"},
		{"canonical map match", "read", "Read"},
		{"namespaced tool exact", "mcp/server/read", "mcp/server/read"},
		{"namespaced tool mapped from sanitized", "mcp_server_read", "mcp/server/read"},
		{"ambiguous collision bypassed", "DUPLICATE_CASE", "DUPLICATE_CASE"},
		{"empty name", "", ""},
		{"unmatched fallback", "unknown_tool", "unknown_tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MapClaudeToolName(toolMap, origReq, tt.toolName); got != tt.want {
				t.Errorf("MapClaudeToolName(%q) = %q, want %q", tt.toolName, got, tt.want)
			}
		})
	}
}

func TestClaudeToolIsParameterless(t *testing.T) {
	origReq := []byte(`{
		"tools": [
			{"name": "EmptyProperties", "input_schema": {"type": "object", "properties": {}}},
			{"name": "MissingProperties", "input_schema": {"type": "object"}},
			{"name": "NoSchema"},
			{"name": "HasProperties", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}}},
			{"name": "DynamicTool", "input_schema": {"type": "object", "properties": {}, "additionalProperties": true}},
			{"name": "PatternTool", "input_schema": {"type": "object", "patternProperties": {"^x-": {"type": "string"}}}}
		]
	}`)

	tests := []struct {
		name     string
		toolName string
		want     bool
	}{
		{"empty properties", "EmptyProperties", true},
		{"missing properties", "MissingProperties", true},
		{"no schema", "NoSchema", true},
		{"has properties", "HasProperties", false},
		{"dynamic tool", "DynamicTool", false},
		{"pattern tool", "PatternTool", false},
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

// TestUnambiguousToolNameMapMatchesMapClaudeToolName pins the collision policy
// MapClaudeToolName encodes by re-scanning the request: every name it would keep
// or rewrite must come out identical through MapToolName over the precomputed
// map (plan v8040-p3, upstream 80d04e1).
func TestUnambiguousToolNameMapMatchesMapClaudeToolName(t *testing.T) {
	requests := [][]byte{
		nil,
		[]byte(``),
		[]byte(`{`),
		[]byte(`{}`),
		[]byte(`{"tools":[]}`),
		[]byte(`{"tools":[{"name":"Read"}]}`),
		[]byte(`{"tools":[{"name":"Read"},{"name":"read"}]}`),
		[]byte(`{"tools":[{"name":"Read"},{"name":"Read"}]}`),
		[]byte(`{"tools":[{"name":"_Read"},{"name":"read"},{"name":"READ"}]}`),
		[]byte(`{"tools":[{"function":{"name":"Write"}}]}`),
		[]byte(`{"tools":[{"name":"  Spaced  "},{"name":"spaced"}]}`),
		[]byte(`{"tools":[{"name":""},{"name":"Empty"}]}`),
	}

	// Deterministic pseudo-random fixtures: no seeding dependency on the runtime.
	state := uint64(1)
	next := func(n int) int {
		state = state*6364136223846793005 + 1442695040888963407
		return int((state >> 33) % uint64(n))
	}
	pool := []string{"Read", "read", "READ", "_Read", "Write", "write", "spaced", "  Spaced  ", "Run", "run", "mcp__x__y", ""}
	for i := 0; i < 500; i++ {
		var b strings.Builder
		b.WriteString(`{"tools":[`)
		for j, count := 0, 1+next(4); j < count; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"name":"` + pool[next(len(pool))] + `"}`)
		}
		b.WriteString(`]}`)
		requests = append(requests, []byte(b.String()))
	}

	names := []string{"", "Read", "read", "READ", "_Read", "__read", "Write", "write", "Spaced", "spaced", "  Spaced  ", "Missing", "read_file", "mcp__x__y"}
	for _, req := range requests {
		for _, name := range names {
			legacy := MapClaudeToolName(ToolNameMapFromClaudeRequest(req), req, name)
			fast := MapToolName(UnambiguousToolNameMapFromClaudeRequest(req), name)
			if legacy != fast {
				t.Fatalf("map mismatch for request %q name %q: legacy=%q fast=%q", req, name, legacy, fast)
			}
		}
	}
}
