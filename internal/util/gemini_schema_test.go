package util

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCleanJSONSchemaForAntigravity_ConstToEnum(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"const": "InsightVizNode"
			}
		}
	}`

	expected := `{
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"enum": ["InsightVizNode"]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_TypeFlattening_Nullable(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"name": {
				"type": ["string", "null"]
			},
			"other": {
				"type": "string"
			}
		},
		"required": ["name", "other"]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "(nullable)"
			},
			"other": {
				"type": "string"
			}
		},
		"required": ["other"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_ConstraintsToDescription(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"tags": {
				"type": "array",
				"description": "List of tags",
				"minItems": 1
			},
			"name": {
				"type": "string",
				"description": "User name",
				"minLength": 3
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// minItems should be REMOVED and moved to description
	if strings.Contains(result, `"minItems"`) {
		t.Errorf("minItems keyword should be removed")
	}
	if !strings.Contains(result, "minItems: 1") {
		t.Errorf("minItems hint missing in description")
	}

	// minLength should be moved to description
	if !strings.Contains(result, "minLength: 3") {
		t.Errorf("minLength hint missing in description")
	}
	if strings.Contains(result, `"minLength":`) || strings.Contains(result, `"minLength" :`) {
		t.Errorf("minLength keyword should be removed")
	}
}

func TestCleanJSONSchemaForAntigravity_AnyOfFlattening_SmartSelection(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"query": {
				"anyOf": [
					{ "type": "null" },
					{
						"type": "object",
						"properties": {
							"kind": { "type": "string" }
						}
					}
				]
			}
		}
	}`

	expected := `{
		"type": "object",
		"properties": {
			"query": {
				"type": "object",
				"nullable": true,
				"description": "Accepts: null | object",
				"properties": {
					"_": { "type": "boolean" },
					"kind": { "type": "string" }
				},
				"required": ["_"]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_OneOfMultiBranchPreserved(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"config": {
				"oneOf": [
					{ "type": "string" },
					{ "type": "integer" }
				]
			}
		}
	}`

	// Multi-branch unions are preserved instead of flattened so non-first
	// branches are not lost; oneOf is normalized to anyOf on native carriers.
	expected := `{
		"type": "object",
		"properties": {
			"config": {
				"anyOf": [
					{ "type": "string" },
					{ "type": "integer" }
				]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_MultiBranchAnyOfPreserved(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"target": {
				"anyOf": [
					{
						"type": "object",
						"properties": { "path": { "type": "string" } },
						"required": ["path"]
					},
					{
						"type": "object",
						"properties": { "id": { "type": "integer" } },
						"required": ["id"]
					},
					{ "type": "null" }
				]
			},
			"single": {
				"anyOf": [
					{ "type": "null" },
					{ "type": "string" }
				]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(result)

	// Multi-branch union keeps both object branches; null becomes nullable.
	branches := parsed.Get("properties.target.anyOf").Array()
	if len(branches) != 2 {
		t.Fatalf("target.anyOf branch count = %d, want 2; cleaned: %s", len(branches), result)
	}
	if branches[0].Get("properties.path.type").String() != "string" {
		t.Errorf("target.anyOf[0].properties.path.type not preserved: %s", branches[0].Raw)
	}
	if branches[1].Get("properties.id.type").String() != "integer" {
		t.Errorf("target.anyOf[1].properties.id.type not preserved: %s", branches[1].Raw)
	}
	if !parsed.Get("properties.target.nullable").Exists() {
		t.Errorf("target.nullable missing for null branch: %s", result)
	}

	// Single non-null branch unions still flatten to the branch itself.
	if got := parsed.Get("properties.single.type").String(); got != "string" {
		t.Errorf("single.type = %q, want string (flattened); cleaned: %s", got, result)
	}
	if parsed.Get("properties.single.anyOf").Exists() {
		t.Errorf("single.anyOf should have been flattened: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_AllOfMerging(t *testing.T) {
	input := `{
		"type": "object",
		"allOf": [
			{
				"properties": {
					"a": { "type": "string" }
				},
				"required": ["a"]
			},
			{
				"properties": {
					"b": { "type": "integer" }
				},
				"required": ["b"]
			}
		]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"a": { "type": "string" },
			"b": { "type": "integer" }
		},
		"required": ["a", "b"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

// TestCleanJSONSchemaForAntigravity_RefHandling pins the inlining contract R4 introduces: a local
// reference is expanded in place, so the referenced definition's fields reach the request instead
// of being replaced by a "See: ..." hint, and no definition container or dangling $ref survives.
func TestCleanJSONSchemaForAntigravity_RefHandling(t *testing.T) {
	input := `{
		"definitions": {
			"User": {
				"type": "object",
				"properties": {
					"name": { "type": "string" }
				},
				"required": ["name"]
			}
		},
		"type": "object",
		"properties": {
			"customer": { "$ref": "#/definitions/User" }
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(result)

	if got := parsed.Get("properties.customer.properties.name.type").String(); got != "string" {
		t.Errorf("referenced property was not inlined, name type = %q: %s", got, result)
	}
	if got := parsed.Get("properties.customer.required.0").String(); got != "name" {
		t.Errorf("referenced required entry was lost, got %q: %s", got, result)
	}
	if parsed.Get("definitions").Exists() {
		t.Errorf("definitions were not stripped after inlining: %s", result)
	}
	if strings.Contains(result, `"$ref"`) {
		t.Errorf("a resolvable local reference survived cleaning: %s", result)
	}
	if strings.Contains(result, "See: User") {
		t.Errorf("a resolvable local reference degraded to a hint: %s", result)
	}
}

// TestCleanJSONSchemaForAntigravity_RefHandling_DescriptionEscaping verifies that a sibling
// description on the referencing node wins over the referenced definition while the definition's
// fields are still inlined.
func TestCleanJSONSchemaForAntigravity_RefHandling_DescriptionEscaping(t *testing.T) {
	input := `{
		"definitions": {
			"User": {
				"type": "object",
				"properties": {
					"name": { "type": "string" }
				}
			}
		},
		"type": "object",
		"properties": {
			"customer": {
				"description": "He said \"hi\"\\nsecond line",
				"$ref": "#/definitions/User"
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(result)

	if got := parsed.Get("properties.customer.properties.name.type").String(); got != "string" {
		t.Errorf("referenced property was not inlined, name type = %q: %s", got, result)
	}
	if got, want := parsed.Get("properties.customer.description").String(), "He said \"hi\"\\nsecond line"; got != want {
		t.Errorf("sibling description = %q, want %q: %s", got, want, result)
	}
	if strings.Contains(result, "See: User") {
		t.Errorf("a resolvable local reference degraded to a hint: %s", result)
	}
}

// TestCleanJSONSchemaForAntigravity_CyclicRefDefaults verifies the cycle guard: a self-referential
// definition terminates, the outer level is expanded, and only the re-entrant reference degrades to
// a hint instead of recursing without bound.
func TestCleanJSONSchemaForAntigravity_CyclicRefDefaults(t *testing.T) {
	input := `{
		"definitions": {
			"Node": {
				"type": "object",
				"properties": {
					"label": { "type": "string" },
					"child": { "$ref": "#/definitions/Node" }
				}
			}
		},
		"$ref": "#/definitions/Node"
	}`

	result := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(result)

	if got := parsed.Get("type").String(); got != "object" {
		t.Errorf("root ref target type = %q, want object: %s", got, result)
	}
	if got := parsed.Get("properties.label.type").String(); got != "string" {
		t.Errorf("outer level was not expanded, label type = %q: %s", got, result)
	}
	if got := parsed.Get("properties.child.description").String(); !strings.Contains(got, "Node") {
		t.Errorf("re-entrant reference should degrade to a hint, child description = %q: %s", got, result)
	}
	if len(result) >= 8*1024 {
		t.Errorf("cyclic schema output = %d bytes, want bounded: %s", len(result), result)
	}
}

func TestCleanJSONSchemaForAntigravity_RequiredCleanup(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"a": {"type": "string"},
			"b": {"type": "string"}
		},
		"required": ["a", "b", "c"]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"a": {"type": "string"},
			"b": {"type": "string"}
		},
		"required": ["a", "b"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_AllOfMerging_DotKeys(t *testing.T) {
	input := `{
		"type": "object",
		"allOf": [
			{
				"properties": {
					"my.param": { "type": "string" }
				},
				"required": ["my.param"]
			},
			{
				"properties": {
					"b": { "type": "integer" }
				},
				"required": ["b"]
			}
		]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"my.param": { "type": "string" },
			"b": { "type": "integer" }
		},
		"required": ["my.param", "b"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_PropertyNameCollision(t *testing.T) {
	// A tool has an argument named "pattern" - should NOT be treated as a constraint
	input := `{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "The regex pattern"
			}
		},
		"required": ["pattern"]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "The regex pattern"
			}
		},
		"required": ["pattern"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)

	var resMap map[string]interface{}
	json.Unmarshal([]byte(result), &resMap)
	props, _ := resMap["properties"].(map[string]interface{})
	if _, ok := props["description"]; ok {
		t.Errorf("Invalid 'description' property injected into properties map")
	}
}

func TestCleanJSONSchemaForAntigravity_DotKeys(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"my.param": {
				"type": "string",
				"$ref": "#/definitions/MyType"
			}
		},
		"definitions": {
			"MyType": { "type": "string" }
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	var resMap map[string]interface{}
	if err := json.Unmarshal([]byte(result), &resMap); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	props, ok := resMap["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties missing")
	}

	if val, ok := props["my.param"]; !ok {
		t.Fatalf("Key 'my.param' is missing. Result: %s", result)
	} else {
		valMap, _ := val.(map[string]interface{})
		if _, hasRef := valMap["$ref"]; hasRef {
			t.Errorf("Key 'my.param' still contains $ref")
		}
		if _, ok := props["my"]; ok {
			t.Errorf("Artifact key 'my' created by sjson splitting")
		}
	}
}

func TestCleanJSONSchemaForAntigravity_AnyOfAlternativeHints(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"value": {
				"anyOf": [
					{ "type": "string" },
					{ "type": "integer" },
					{ "type": "null" }
				]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(result)

	// Multi-branch unions are preserved; null projects to nullable:true.
	branches := parsed.Get("properties.value.anyOf").Array()
	if len(branches) != 2 {
		t.Fatalf("anyOf branch count = %d, want 2; cleaned: %s", len(branches), result)
	}
	if branches[0].Get("type").String() != "string" || branches[1].Get("type").String() != "integer" {
		t.Errorf("branches were not preserved, got: %s", result)
	}
	if !parsed.Get("properties.value.nullable").Exists() {
		t.Errorf("Expected nullable marker for null branch, got: %s", result)
	}
	if desc := parsed.Get("properties.value.description").String(); !strings.Contains(desc, "(nullable)") {
		t.Errorf("Expected (nullable) hint, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_NullableHint(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"name": {
				"type": ["string", "null"],
				"description": "User name"
			}
		},
		"required": ["name"]
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if !strings.Contains(result, "(nullable)") {
		t.Errorf("Expected nullable hint, got: %s", result)
	}
	if !strings.Contains(result, "User name") {
		t.Errorf("Expected original description to be preserved, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_TypeFlattening_Nullable_DotKey(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"my.param": {
				"type": ["string", "null"]
			},
			"other": {
				"type": "string"
			}
		},
		"required": ["my.param", "other"]
	}`

	expected := `{
		"type": "object",
		"properties": {
			"my.param": {
				"type": "string",
				"description": "(nullable)"
			},
			"other": {
				"type": "string"
			}
		},
		"required": ["other"]
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_EnumHint(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"status": {
				"type": "string",
				"enum": ["active", "inactive", "pending"],
				"description": "Current status"
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if !strings.Contains(result, "Allowed:") {
		t.Errorf("Expected enum values hint, got: %s", result)
	}
	if !strings.Contains(result, "active") || !strings.Contains(result, "inactive") {
		t.Errorf("Expected enum values in hint, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_AdditionalPropertiesHint(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"name": { "type": "string" }
		},
		"additionalProperties": false
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if !strings.Contains(result, "No extra properties allowed") {
		t.Errorf("Expected additionalProperties hint, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_AnyOfFlattening_PreservesDescription(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"config": {
				"description": "Parent desc",
				"anyOf": [
					{ "type": "string", "description": "Child desc" },
					{ "type": "integer" }
				]
			}
		}
	}`

	expected := `{
		"type": "object",
		"properties": {
			"config": {
				"description": "Parent desc",
				"anyOf": [
					{ "type": "string", "description": "Child desc" },
					{ "type": "integer" }
				]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)
	compareJSON(t, expected, result)
}

func TestCleanJSONSchemaForAntigravity_SingleEnumNoHint(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"enum": ["fixed"]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if strings.Contains(result, "Allowed:") {
		t.Errorf("Single value enum should not add Allowed hint, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_MultipleNonNullTypes(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"value": {
				"type": ["string", "integer", "boolean"]
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if !strings.Contains(result, "Accepts:") {
		t.Errorf("Expected multiple types hint, got: %s", result)
	}
	if !strings.Contains(result, "string") || !strings.Contains(result, "integer") || !strings.Contains(result, "boolean") {
		t.Errorf("Expected all types in hint, got: %s", result)
	}
}

func compareJSON(t *testing.T, expectedJSON, actualJSON string) {
	var expMap, actMap map[string]interface{}
	errExp := json.Unmarshal([]byte(expectedJSON), &expMap)
	errAct := json.Unmarshal([]byte(actualJSON), &actMap)

	if errExp != nil || errAct != nil {
		t.Fatalf("JSON Unmarshal error. Exp: %v, Act: %v", errExp, errAct)
	}

	if !reflect.DeepEqual(expMap, actMap) {
		expBytes, _ := json.MarshalIndent(expMap, "", "  ")
		actBytes, _ := json.MarshalIndent(actMap, "", "  ")
		t.Errorf("JSON mismatch:\nExpected:\n%s\n\nActual:\n%s", string(expBytes), string(actBytes))
	}
}

// ============================================================================
// Empty Schema Placeholder Tests
// ============================================================================

func TestCleanJSONSchemaForAntigravity_EmptySchemaPlaceholder(t *testing.T) {
	// Empty object schema with no properties should get a placeholder
	input := `{
		"type": "object"
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Should have placeholder property added
	if !strings.Contains(result, `"reason"`) {
		t.Errorf("Empty schema should have 'reason' placeholder property, got: %s", result)
	}
	if !strings.Contains(result, `"required"`) {
		t.Errorf("Empty schema should have 'required' with 'reason', got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_EmptyPropertiesPlaceholder(t *testing.T) {
	// Object with empty properties object
	input := `{
		"type": "object",
		"properties": {}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Should have placeholder property added
	if !strings.Contains(result, `"reason"`) {
		t.Errorf("Empty properties should have 'reason' placeholder, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_NonEmptySchemaUnchanged(t *testing.T) {
	// Schema with properties should NOT get placeholder
	input := `{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"required": ["name"]
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Should NOT have placeholder property
	if strings.Contains(result, `"reason"`) {
		t.Errorf("Non-empty schema should NOT have 'reason' placeholder, got: %s", result)
	}
	// Original properties should be preserved
	if !strings.Contains(result, `"name"`) {
		t.Errorf("Original property 'name' should be preserved, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_NestedEmptySchema(t *testing.T) {
	// Nested empty object in items should also get placeholder
	input := `{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"items": {
					"type": "object"
				}
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Nested empty object should also get placeholder
	// Check that the nested object has a reason property
	parsed := gjson.Parse(result)
	nestedProps := parsed.Get("properties.items.items.properties")
	if !nestedProps.Exists() || !nestedProps.Get("reason").Exists() {
		t.Errorf("Nested empty object should have 'reason' placeholder, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_EmptySchemaWithDescription(t *testing.T) {
	// Empty schema with description should preserve description and add placeholder
	input := `{
		"type": "object",
		"description": "An empty object"
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Should have both description and placeholder
	if !strings.Contains(result, `"An empty object"`) {
		t.Errorf("Description should be preserved, got: %s", result)
	}
	if !strings.Contains(result, `"reason"`) {
		t.Errorf("Empty schema should have 'reason' placeholder, got: %s", result)
	}
}

// ============================================================================
// Format field handling (ad-hoc patch removal)
// ============================================================================

func TestCleanJSONSchemaForAntigravity_FormatFieldRemoval(t *testing.T) {
	// format:"uri" should be removed and added as hint
	input := `{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"format": "uri",
				"description": "A URL"
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// format should be removed
	if strings.Contains(result, `"format"`) {
		t.Errorf("format field should be removed, got: %s", result)
	}
	// hint should be added to description
	if !strings.Contains(result, "format: uri") {
		t.Errorf("format hint should be added to description, got: %s", result)
	}
	// original description should be preserved
	if !strings.Contains(result, "A URL") {
		t.Errorf("Original description should be preserved, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_FormatFieldNoDescription(t *testing.T) {
	// format without description should create description with hint
	input := `{
		"type": "object",
		"properties": {
			"email": {
				"type": "string",
				"format": "email"
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// format should be removed
	if strings.Contains(result, `"format"`) {
		t.Errorf("format field should be removed, got: %s", result)
	}
	// hint should be added
	if !strings.Contains(result, "format: email") {
		t.Errorf("format hint should be added, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_MultipleFormats(t *testing.T) {
	// Multiple format fields should all be handled
	input := `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "format": "uri"},
			"email": {"type": "string", "format": "email"},
			"date": {"type": "string", "format": "date-time"}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// All format fields should be removed
	if strings.Contains(result, `"format"`) {
		t.Errorf("All format fields should be removed, got: %s", result)
	}
	// All hints should be added
	if !strings.Contains(result, "format: uri") {
		t.Errorf("uri format hint should be added, got: %s", result)
	}
	if !strings.Contains(result, "format: email") {
		t.Errorf("email format hint should be added, got: %s", result)
	}
	if !strings.Contains(result, "format: date-time") {
		t.Errorf("date-time format hint should be added, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_NumericEnumToString(t *testing.T) {
	// Gemini API requires enum values to be strings, not numbers
	input := `{
		"type": "object",
		"properties": {
			"priority": {"type": "integer", "enum": [0, 1, 2]},
			"level": {"type": "number", "enum": [1.5, 2.5, 3.5]},
			"status": {"type": "string", "enum": ["active", "inactive"]}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Numeric enum values should be converted to strings
	if strings.Contains(result, `"enum":[0,1,2]`) {
		t.Errorf("Integer enum values should be converted to strings, got: %s", result)
	}
	if strings.Contains(result, `"enum":[1.5,2.5,3.5]`) {
		t.Errorf("Float enum values should be converted to strings, got: %s", result)
	}
	// Should contain string versions
	if !strings.Contains(result, `"0"`) || !strings.Contains(result, `"1"`) || !strings.Contains(result, `"2"`) {
		t.Errorf("Integer enum values should be converted to string format, got: %s", result)
	}
	// String enum values should remain unchanged
	if !strings.Contains(result, `"active"`) || !strings.Contains(result, `"inactive"`) {
		t.Errorf("String enum values should remain unchanged, got: %s", result)
	}
}

func TestCleanJSONSchemaForAntigravity_BooleanEnumToString(t *testing.T) {
	// Boolean enum values should also be converted to strings
	input := `{
		"type": "object",
		"properties": {
			"enabled": {"type": "boolean", "enum": [true, false]}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	// Boolean enum values should be converted to strings
	if strings.Contains(result, `"enum":[true,false]`) {
		t.Errorf("Boolean enum values should be converted to strings, got: %s", result)
	}
	// Should contain string versions "true" and "false"
	if !strings.Contains(result, `"true"`) || !strings.Contains(result, `"false"`) {
		t.Errorf("Boolean enum values should be converted to string format, got: %s", result)
	}
}

func TestCleanJSONSchemaForGemini_RemovesGeminiUnsupportedMetadataFields(t *testing.T) {
	input := `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"$id": "root-schema",
		"type": "object",
		"properties": {
			"payload": {
				"type": "object",
				"prefill": "hello",
				"properties": {
					"mode": {
						"type": "string",
						"enum": ["a", "b"],
						"enumTitles": ["A", "B"]
					}
				},
				"patternProperties": {
					"^x-": {"type": "string"}
				}
			},
			"$id": {
				"type": "string",
				"description": "property name should not be removed"
			}
		}
	}`

	expected := `{
		"type": "object",
		"properties": {
			"payload": {
				"type": "object",
				"properties": {
					"mode": {
						"type": "string",
						"enum": ["a", "b"],
						"description": "Allowed: a, b"
					}
				}
			},
			"$id": {
				"type": "string",
				"description": "property name should not be removed"
			}
		}
	}`

	result := CleanJSONSchemaForGemini(input)
	compareJSON(t, expected, result)
}

func TestRemoveExtensionFields(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "removes x- fields at root",
			input: `{
				"type": "object",
				"x-custom-meta": "value",
				"properties": {
					"foo": { "type": "string" }
				}
			}`,
			expected: `{
				"type": "object",
				"properties": {
					"foo": { "type": "string" }
				}
			}`,
		},
		{
			name: "removes x- fields in nested properties",
			input: `{
				"type": "object",
				"properties": {
					"foo": {
						"type": "string",
						"x-internal-id": 123
					}
				}
			}`,
			expected: `{
				"type": "object",
				"properties": {
					"foo": {
						"type": "string"
					}
				}
			}`,
		},
		{
			name: "does NOT remove properties named x-",
			input: `{
				"type": "object",
				"properties": {
					"x-data": { "type": "string" },
					"normal": { "type": "number", "x-meta": "remove" }
				},
				"required": ["x-data"]
			}`,
			expected: `{
				"type": "object",
				"properties": {
					"x-data": { "type": "string" },
					"normal": { "type": "number" }
				},
				"required": ["x-data"]
			}`,
		},
		{
			name: "does NOT remove $schema and other meta fields (as requested)",
			input: `{
				"$schema": "http://json-schema.org/draft-07/schema#",
				"$id": "test",
				"type": "object",
				"properties": {
					"foo": { "type": "string" }
				}
			}`,
			expected: `{
				"$schema": "http://json-schema.org/draft-07/schema#",
				"$id": "test",
				"type": "object",
				"properties": {
					"foo": { "type": "string" }
				}
			}`,
		},
		{
			name: "handles properties named $schema",
			input: `{
				"type": "object",
				"properties": {
					"$schema": { "type": "string" }
				}
			}`,
			expected: `{
				"type": "object",
				"properties": {
					"$schema": { "type": "string" }
				}
			}`,
		},
		{
			name: "handles escaping in paths",
			input: `{
				"type": "object",
				"properties": {
					"foo.bar": {
						"type": "string",
						"x-meta": "remove"
					}
				},
				"x-root.meta": "remove"
			}`,
			expected: `{
				"type": "object",
				"properties": {
					"foo.bar": {
						"type": "string"
					}
				}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := removeExtensionFields(tt.input)
			compareJSON(t, tt.expected, actual)
		})
	}
}

// uniqueItems should be stripped and moved to description hint (#2123).
func TestCleanJSONSchemaForAntigravity_UniqueItemsStripped(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"ids": {
				"type": "array",
				"description": "Unique identifiers",
				"items": {"type": "string"},
				"uniqueItems": true
			}
		}
	}`

	result := CleanJSONSchemaForAntigravity(input)

	if strings.Contains(result, `"uniqueItems"`) {
		t.Errorf("uniqueItems should be removed from schema")
	}
	if !strings.Contains(result, "uniqueItems: true") {
		t.Errorf("uniqueItems hint missing in description")
	}
}

func TestCleanJSONSchemaStripsEncryptedMetadata(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"credential": {
				"type": "string",
				"description": "API credential",
				"encrypted": true
			},
			"timeout": {
				"type": "integer",
				"encrypted": false
			},
			"nested": {
				"type": "object",
				"properties": {
					"nested_value": {
						"type": "string",
						"encrypted": true
					}
				}
			},
			"encrypted": {
				"type": "boolean",
				"description": "Whether the payload is encrypted",
				"encrypted": true
			}
		},
		"required": ["credential", "encrypted"]
	}`

	for cleaner, clean := range map[string]func(string) string{
		"gemini":      CleanJSONSchemaForGemini,
		"antigravity": CleanJSONSchemaForAntigravity,
	} {
		t.Run(cleaner, func(t *testing.T) {
			got := clean(input)
			parsed := gjson.Parse(got)

			for _, path := range []string{
				"properties.credential.encrypted",
				"properties.timeout.encrypted",
				"properties.nested.properties.nested_value.encrypted",
				"properties.encrypted.encrypted",
			} {
				if parsed.Get(path).Exists() {
					t.Errorf("%s survived cleaning: %s", path, got)
				}
			}
			if parsed.Get("properties.credential.type").String() != "string" ||
				parsed.Get("properties.credential.description").String() != "API credential" {
				t.Errorf("credential schema was corrupted: %s", got)
			}
			if parsed.Get("properties.nested.properties.nested_value.type").String() != "string" {
				t.Errorf("nested nested_value schema was corrupted: %s", got)
			}
			if parsed.Get("properties.encrypted.type").String() != "boolean" {
				t.Errorf("property named encrypted was removed or corrupted: %s", got)
			}
		})
	}
}

// TestCleanJSONSchema_BarePropertyMapNormalized covers Issue #5178:
// MCP tools (e.g. Asana) emit bare property maps missing type:object and properties wrappers,
// plus boolean required: true on child properties.
func TestCleanJSONSchema_BarePropertyMapNormalized(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"parent": { "type": "string", "required": true },
				"insert_after": { "type": "string" },
				"insert_before": { "type": "string" }
			},
			"opts": {
				"opt_fields": { "type": "string" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		// data must be normalized into an object schema with properties
		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.properties.parent.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.parent.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.parent.type").String(), got)
		}
		if parsed.Get("properties.data.properties.insert_after.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.insert_after.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.insert_after.type").String(), got)
		}
		if parsed.Get("properties.data.properties.insert_before.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.insert_before.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.insert_before.type").String(), got)
		}
		// parent required: true must be promoted to data.required array
		var dataReq []string
		for _, r := range parsed.Get("properties.data.required").Array() {
			dataReq = append(dataReq, r.String())
		}
		if !contains(dataReq, "parent") {
			t.Errorf("%s: properties.data.required = %v, want 'parent' included; got schema: %s", cleaner, dataReq, got)
		}
		// boolean required on parent node must be stripped
		if parsed.Get("properties.data.properties.parent.required").Exists() {
			t.Errorf("%s: properties.data.properties.parent.required survived; got schema: %s", cleaner, got)
		}

		// opts must also be normalized into an object schema
		if parsed.Get("properties.opts.type").String() != "object" {
			t.Errorf("%s: properties.opts.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.opts.type").String(), got)
		}
		if parsed.Get("properties.opts.properties.opt_fields.type").String() != "string" {
			t.Errorf("%s: properties.opts.properties.opt_fields.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.opts.properties.opt_fields.type").String(), got)
		}
	}
}

// TestCleanJSONSchema_NestedBarePropertyMap tests recursive normalization of multi-level bare property maps.
func TestCleanJSONSchema_NestedBarePropertyMap(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"workspace": { "type": "string", "required": true },
				"task": {
					"name": { "type": "string", "required": true },
					"notes": { "type": "string" }
				}
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.properties.workspace.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.workspace.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.workspace.type").String(), got)
		}

		// Nested task should also be normalized to an object
		if parsed.Get("properties.data.properties.task.type").String() != "object" {
			t.Errorf("%s: properties.data.properties.task.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.properties.task.type").String(), got)
		}
		if parsed.Get("properties.data.properties.task.properties.name.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.task.properties.name.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.task.properties.name.type").String(), got)
		}

		// Required promotion at both levels
		var dataReq []string
		for _, r := range parsed.Get("properties.data.required").Array() {
			dataReq = append(dataReq, r.String())
		}
		if !contains(dataReq, "workspace") {
			t.Errorf("%s: properties.data.required = %v, want 'workspace'; got schema: %s", cleaner, dataReq, got)
		}

		var taskReq []string
		for _, r := range parsed.Get("properties.data.properties.task.required").Array() {
			taskReq = append(taskReq, r.String())
		}
		if !contains(taskReq, "name") {
			t.Errorf("%s: properties.data.properties.task.required = %v, want 'name'; got schema: %s", cleaner, taskReq, got)
		}
	}
}

// TestCleanJSONSchema_BarePropertyMapWithKeywordNames tests that bare property maps with fields
// named like schema keywords (title, description, format, type) are correctly normalized.
func TestCleanJSONSchema_BarePropertyMapWithKeywordNames(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"title": { "type": "string", "required": true },
				"description": { "type": "string" },
				"format": { "type": "string" },
				"type": { "type": "string" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.properties.title.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.title.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.title.type").String(), got)
		}
		if parsed.Get("properties.data.properties.description.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.description.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.description.type").String(), got)
		}
		if parsed.Get("properties.data.properties.type.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.type.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.type.type").String(), got)
		}

		var dataReq []string
		for _, r := range parsed.Get("properties.data.required").Array() {
			dataReq = append(dataReq, r.String())
		}
		if !contains(dataReq, "title") {
			t.Errorf("%s: properties.data.required = %v, want 'title'; got schema: %s", cleaner, dataReq, got)
		}
	}
}

// TestCleanJSONSchema_ArrayItemsBarePropertyMap tests bare property map normalization inside array items.
func TestCleanJSONSchema_ArrayItemsBarePropertyMap(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"items": {
					"id": { "type": "string", "required": true },
					"label": { "type": "string" }
				}
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.tasks.items.type").String() != "object" {
			t.Errorf("%s: properties.tasks.items.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.tasks.items.type").String(), got)
		}
		if parsed.Get("properties.tasks.items.properties.id.type").String() != "string" {
			t.Errorf("%s: properties.tasks.items.properties.id.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.tasks.items.properties.id.type").String(), got)
		}
		var itemsReq []string
		for _, r := range parsed.Get("properties.tasks.items.required").Array() {
			itemsReq = append(itemsReq, r.String())
		}
		if !contains(itemsReq, "id") {
			t.Errorf("%s: properties.tasks.items.required = %v, want 'id'; got schema: %s", cleaner, itemsReq, got)
		}
	}
}

// TestCleanJSONSchema_BooleanRequiredPromoted tests that boolean required: true is promoted
// and boolean required: false is stripped without being added to the required array.
func TestCleanJSONSchema_BooleanRequiredPromoted(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"existing": { "type": "string" },
			"name": { "type": "string", "required": true },
			"age": { "type": "integer", "required": false },
			"tag": { "type": "string" }
		},
		"required": ["existing"]
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		var req []string
		for _, r := range parsed.Get("required").Array() {
			req = append(req, r.String())
		}

		if !contains(req, "existing") || !contains(req, "name") {
			t.Errorf("%s: required = %v, want both 'existing' and 'name'; got schema: %s", cleaner, req, got)
		}
		if contains(req, "age") || contains(req, "tag") {
			t.Errorf("%s: required = %v, should not contain 'age' or 'tag'; got schema: %s", cleaner, req, got)
		}

		if parsed.Get("properties.name.required").Exists() {
			t.Errorf("%s: properties.name.required survived; got schema: %s", cleaner, got)
		}
		if parsed.Get("properties.age.required").Exists() {
			t.Errorf("%s: properties.age.required survived; got schema: %s", cleaner, got)
		}
	}
}

// TestCleanJSONSchema_PreservesLargeNumberPrecision tests that numbers are not corrupted by float64 precision loss.
func TestCleanJSONSchema_PreservesLargeNumberPrecision(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"big_int": {
				"type": "integer",
				"minimum": 9007199254740993
			},
			"bare_child": {
				"sub": { "type": "string" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		result := clean(input)
		// minimum is moved to description hint
		if !strings.Contains(result, "9007199254740993") {
			t.Errorf("%s: large integer precision was lost: %s", cleaner, result)
		}
	}
}

// TestCleanJSONSchema_BarePropertyMapWithRequestAndToolsNames tests that property names like
// "request", "tools", "headers", "messages" inside bare property maps are correctly normalized.
func TestCleanJSONSchema_BarePropertyMapWithRequestAndToolsNames(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"request": {
					"method": { "type": "string", "required": true },
					"url": { "type": "string" }
				},
				"headers": {
					"authorization": { "type": "string" }
				},
				"tools": {
					"name": { "type": "string" }
				}
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.properties.headers.type").String() != "object" {
			t.Errorf("%s: properties.data.properties.headers.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.properties.headers.type").String(), got)
		}
		if parsed.Get("properties.data.properties.tools.type").String() != "object" {
			t.Errorf("%s: properties.data.properties.tools.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.properties.tools.type").String(), got)
		}
		if parsed.Get("properties.data.properties.request.type").String() != "object" {
			t.Errorf("%s: properties.data.properties.request.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.properties.request.type").String(), got)
		}
		if parsed.Get("properties.data.properties.request.properties.method.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.request.properties.method.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.request.properties.method.type").String(), got)
		}
		var reqReq []string
		for _, r := range parsed.Get("properties.data.properties.request.required").Array() {
			reqReq = append(reqReq, r.String())
		}
		if !contains(reqReq, "method") {
			t.Errorf("%s: request.required = %v, want 'method'; got schema: %s", cleaner, reqReq, got)
		}
	}
}

// TestCleanJSONSchema_BarePropertyMapWithSiblingDescription tests bare property maps with sibling
// annotations (e.g. description, title, required) alongside child property definitions.
func TestCleanJSONSchema_BarePropertyMapWithSiblingDescription(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"description": "Task payload",
				"parent": { "type": "string", "required": true },
				"insert_after": { "type": "string" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.description").String() != "Task payload" {
			t.Errorf("%s: properties.data.description = %q, want 'Task payload'; got schema: %s", cleaner, parsed.Get("properties.data.description").String(), got)
		}
		if parsed.Get("properties.data.properties.parent.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.parent.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.parent.type").String(), got)
		}
		if parsed.Get("properties.data.properties.insert_after.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.insert_after.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.insert_after.type").String(), got)
		}
		var dataReq []string
		for _, r := range parsed.Get("properties.data.required").Array() {
			dataReq = append(dataReq, r.String())
		}
		if !contains(dataReq, "parent") {
			t.Errorf("%s: properties.data.required = %v, want 'parent'; got schema: %s", cleaner, dataReq, got)
		}
	}
}

// TestCleanJSONSchema_SingleKeySchemaWrapper tests that the {"schema": ...} wrapper
// is unwrapped, normalized, and re-wrapped by the malformed-schema preprocessing pass.
func TestCleanJSONSchema_SingleKeySchemaWrapper(t *testing.T) {
	inner := `{
		"type": "object",
		"properties": {
			"data": {
				"parent": { "type": "string", "required": true }
			}
		}
	}`
	wrapped := `{"schema": ` + inner + `}`

	result := CleanJSONSchemaForGemini(wrapped)
	parsed := gjson.Parse(result)

	if !parsed.Get("schema").Exists() {
		t.Fatalf("wrapper key 'schema' was lost: %s", result)
	}
	if parsed.Get("schema.properties.data.type").String() != "object" {
		t.Errorf("schema.properties.data.type = %q, want object; got: %s", parsed.Get("schema.properties.data.type").String(), result)
	}
	if parsed.Get("schema.properties.data.properties.parent.type").String() != "string" {
		t.Errorf("schema.properties.data.properties.parent.type = %q, want string; got: %s", parsed.Get("schema.properties.data.properties.parent.type").String(), result)
	}
	var dataReq []string
	for _, r := range parsed.Get("schema.properties.data.required").Array() {
		dataReq = append(dataReq, r.String())
	}
	if !contains(dataReq, "parent") {
		t.Errorf("schema.properties.data.required = %v, want 'parent'; got: %s", dataReq, result)
	}
}

// TestCleanJSONSchema_BarePropertyMapWithExplicitTypeObject tests that nodes declaring
// type: "object" but omitting properties wrapper are correctly normalized.
func TestCleanJSONSchema_BarePropertyMapWithExplicitTypeObject(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"type": "object",
				"parent": { "type": "string", "required": true },
				"insert_after": { "type": "string" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.data.type").String() != "object" {
			t.Errorf("%s: properties.data.type = %q, want object; got schema: %s", cleaner, parsed.Get("properties.data.type").String(), got)
		}
		if parsed.Get("properties.data.properties.parent.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.parent.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.parent.type").String(), got)
		}
		if parsed.Get("properties.data.properties.insert_after.type").String() != "string" {
			t.Errorf("%s: properties.data.properties.insert_after.type = %q, want string; got schema: %s", cleaner, parsed.Get("properties.data.properties.insert_after.type").String(), got)
		}
		var dataReq []string
		for _, r := range parsed.Get("properties.data.required").Array() {
			dataReq = append(dataReq, r.String())
		}
		if !contains(dataReq, "parent") {
			t.Errorf("%s: properties.data.required = %v, want 'parent'; got schema: %s", cleaner, dataReq, got)
		}
	}
}

// TestCleanJSONSchema_BarePropertyMapWithNullable tests bare property maps with nullable: true.
func TestCleanJSONSchema_BarePropertyMapWithNullable(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"nullable": true,
				"description": "Task payload",
				"parent": { "type": "string" }
			}
		}
	}`

	got := CleanJSONSchemaForGemini(input)
	parsed := gjson.Parse(got)

	if parsed.Get("properties.data.type").String() != "object" {
		t.Errorf("properties.data.type = %q, want object; got schema: %s", parsed.Get("properties.data.type").String(), got)
	}
	if parsed.Get("properties.data.properties.parent.type").String() != "string" {
		t.Errorf("properties.data.properties.parent.type = %q, want string; got schema: %s", parsed.Get("properties.data.properties.parent.type").String(), got)
	}
}

// TestCleanJSONSchema_PreservesHTMLCharactersWithoutEscaping tests that < > & in descriptions
// are not converted into HTML entities (\u003c, \u003e, \u0026).
func TestCleanJSONSchema_PreservesHTMLCharactersWithoutEscaping(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"data": {
				"description": "Uses <tag> & symbols > threshold",
				"parent": { "type": "string" }
			}
		}
	}`

	result := CleanJSONSchemaForGemini(input)
	if strings.Contains(result, `\u003c`) || strings.Contains(result, `\u003e`) || strings.Contains(result, `\u0026`) {
		t.Errorf("HTML characters were escaped: %s", result)
	}
	if !strings.Contains(result, "<tag>") || !strings.Contains(result, "& symbols >") {
		t.Errorf("Original description with HTML characters was corrupted: %s", result)
	}
}

// TestCleanJSONSchema_VendorExtensionOnEnumNotWrappedIntoProperties tests that vendor extensions
// on non-object types (e.g. x-google-enum-descriptions on a string enum) are not wrapped into properties.
func TestCleanJSONSchema_VendorExtensionOnEnumNotWrappedIntoProperties(t *testing.T) {
	input := `{
		"type": "string",
		"enum": ["FOO", "BAR"],
		"x-google-enum-descriptions": {
			"FOO": "Foo option",
			"BAR": "Bar option"
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties").Exists() {
			t.Errorf("%s: string enum gained unexpected properties: %s", cleaner, got)
		}
		if parsed.Get("type").String() != "string" {
			t.Errorf("%s: string type corrupted: %s", cleaner, got)
		}
	}
}

// TestCleanJSONSchema_ObjectDefaultNotWrappedIntoProperties tests that object-typed default
// is not wrapped into properties as an orphan bare property.
func TestCleanJSONSchema_ObjectDefaultNotWrappedIntoProperties(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"settings": {
				"type": "object",
				"default": { "theme": "dark", "lang": "en" }
			}
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		// settings must not gain properties.default.properties.theme
		if parsed.Get("properties.settings.properties.default").Exists() {
			t.Errorf("%s: default was converted to property: %s", cleaner, got)
		}
	}
}

// TestCleanJSONSchema_MixedPropertiesAndOrphanBareProperty tests that orphan bare property maps
// alongside an existing properties object are collected into properties.
func TestCleanJSONSchema_MixedPropertiesAndOrphanBareProperty(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"foo": { "type": "string" }
		},
		"bar": {
			"type": "integer",
			"required": true
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		if parsed.Get("properties.foo.type").String() != "string" {
			t.Errorf("%s: foo corrupted: %s", cleaner, got)
		}
		if parsed.Get("properties.bar.type").String() != "integer" {
			t.Errorf("%s: orphan bar was not moved to properties: %s", cleaner, got)
		}
		var req []string
		for _, r := range parsed.Get("required").Array() {
			req = append(req, r.String())
		}
		if !contains(req, "bar") {
			t.Errorf("%s: bar required not promoted: %s", cleaner, got)
		}
		if parsed.Get("bar").Exists() {
			t.Errorf("%s: top-level bar survived: %s", cleaner, got)
		}
	}
}

// TestCleanJSONSchema_PreservesAdditionalPropertiesObjectSchema tests that a standalone
// additionalProperties schema is recognized as a structural keyword and not wrapped as a property.
func TestCleanJSONSchema_PreservesAdditionalPropertiesObjectSchema(t *testing.T) {
	input := `{
		"additionalProperties": {
			"type": "string"
		}
	}`

	got := CleanJSONSchemaForGemini(input)
	parsed := gjson.Parse(got)
	// Should not be wrapped as properties.additionalProperties
	if parsed.Get("properties.additionalProperties").Exists() {
		t.Errorf("additionalProperties was wrapped into properties: %s", got)
	}
}

func TestCleanJSONSchemaForAntigravityResponse_AnyOfRequiredOnlyBranches(t *testing.T) {
	input := `{
		"type": "object",
		"anyOf": [
			{"required": ["left"]},
			{"required": ["right"]}
		],
		"properties": {
			"left": {"type": "integer"},
			"right": {"type": "integer"}
		},
		"additionalProperties": false
	}`

	got := CleanJSONSchemaForAntigravity(input)
	parsed := gjson.Parse(got)

	if parsed.Get("type").String() != "object" {
		t.Fatalf("type = %q, want object; cleaned: %s", parsed.Get("type").String(), got)
	}
	if !parsed.Get("properties.left").Exists() || !parsed.Get("properties.right").Exists() {
		t.Fatalf("properties were wiped out; cleaned: %s", got)
	}
	// Required-only branches are materialized into typed object branches
	// using the parent properties so the union is preserved.
	branches := parsed.Get("anyOf").Array()
	if len(branches) != 2 {
		t.Fatalf("anyOf branch count = %d, want 2; cleaned: %s", len(branches), got)
	}
	if branches[0].Get("type").String() != "object" || !branches[0].Get("properties.left").Exists() || branches[0].Get("required.0").String() != "left" {
		t.Fatalf("anyOf[0] was not materialized into valid object branch: %s", branches[0].Raw)
	}
	if branches[1].Get("type").String() != "object" || !branches[1].Get("properties.right").Exists() || branches[1].Get("required.0").String() != "right" {
		t.Fatalf("anyOf[1] was not materialized into valid object branch: %s", branches[1].Raw)
	}
}

func TestCleanJSONSchema_ToolArraysMissingItems(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"params": { "type": "array" },
			"values": { "type": ["array", "null"], "description": "no items" },
			"existing": { "type": "array", "items": { "type": "number" } }
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity": CleanJSONSchemaForAntigravity,
		"gemini":      CleanJSONSchemaForGemini,
	} {
		t.Run(cleaner, func(t *testing.T) {
			got := gjson.Parse(clean(input))

			for _, path := range []string{"properties.params.items.type", "properties.values.items.type"} {
				if itemType := got.Get(path).String(); itemType != "string" {
					t.Errorf("%s = %q, want string; got schema: %s", path, itemType, got.Raw)
				}
			}
			if itemType := got.Get("properties.existing.items.type").String(); itemType != "number" {
				t.Errorf("existing items type = %q, want number; got schema: %s", itemType, got.Raw)
			}

			rootArray := gjson.Parse(clean(`{"type":"array"}`))
			if itemType := rootArray.Get("items.type").String(); itemType != "string" {
				t.Errorf("root items type = %q, want string; got schema: %s", itemType, rootArray.Raw)
			}
		})
	}
}

// TestCleanJSONSchema_RemovesDraft04IdAndSchemaIdentifierKeywords covers Issue #5888:
// Draft-04 schema identifier "id" and Draft 2019-09/2020-12 identifier keywords ($anchor, $vocabulary,
// $dynamicRef, $dynamicAnchor) should be stripped from schema nodes while preserving properties legitimately named "id".
// Ported from upstream f668ac417d97 (CleanJSONSchemaForAntigravityTool has no local equivalent).
func TestCleanJSONSchema_RemovesDraft04IdAndSchemaIdentifierKeywords(t *testing.T) {
	// Repro case from Issue #5888: MCP tool property schema containing "id": "ContentType"
	input := `{
		"id": "http://example.com/root.json",
		"$anchor": "rootAnchor",
		"$vocabulary": {"https://json-schema.org/draft/2020-12/vocab/core": true},
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"enum": ["short", "video"],
				"id": "ContentType",
				"$anchor": "contentTypeAnchor",
				"$dynamicAnchor": "dynAnchor",
				"$dynamicRef": "#dynAnchor",
				"description": "Kind"
			},
			"id": {
				"type": "string",
				"description": "Property legitimately named id should survive"
			}
		},
		"required": ["kind"]
	}`

	for cleaner, clean := range map[string]func(string) string{
		"gemini":              CleanJSONSchemaForGemini,
		"antigravity":         CleanJSONSchemaForAntigravity,
		"antigravityResponse": CleanJSONSchemaForAntigravityResponse,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		// Root keywords should be stripped
		if parsed.Get("id").Exists() {
			t.Errorf("%s: root 'id' was not removed: %s", cleaner, got)
		}
		if parsed.Get("$anchor").Exists() {
			t.Errorf("%s: root '$anchor' was not removed: %s", cleaner, got)
		}
		if parsed.Get("$vocabulary").Exists() {
			t.Errorf("%s: root '$vocabulary' was not removed: %s", cleaner, got)
		}

		// Keywords inside property schema should be stripped
		if parsed.Get("properties.kind.id").Exists() {
			t.Errorf("%s: 'properties.kind.id' was not removed: %s", cleaner, got)
		}
		if parsed.Get("properties.kind.$anchor").Exists() {
			t.Errorf("%s: 'properties.kind.$anchor' was not removed: %s", cleaner, got)
		}
		if parsed.Get("properties.kind.$dynamicAnchor").Exists() {
			t.Errorf("%s: 'properties.kind.$dynamicAnchor' was not removed: %s", cleaner, got)
		}
		if parsed.Get("properties.kind.$dynamicRef").Exists() {
			t.Errorf("%s: 'properties.kind.$dynamicRef' was not removed: %s", cleaner, got)
		}

		// Property named "id" must be preserved
		if !parsed.Get("properties.id").Exists() {
			t.Errorf("%s: property named 'id' was incorrectly removed: %s", cleaner, got)
		}
		if parsed.Get("properties.id.type").String() != "string" {
			t.Errorf("%s: property named 'id' type corrupted: %s", cleaner, got)
		}
	}

	// Real-world MCP repro: definition carrying "id": "ContentType" referenced
	// via $ref. The cleaners inline the definition (see TestIssue6732_*), so the
	// referencing property must carry its fields while the definition block and
	// the unsupported "id" keyword are stripped.
	refInput := `{
		"definitions": {
			"ContentType": {
				"type": "string",
				"enum": ["short", "video"],
				"id": "ContentType",
				"description": "Kind"
			}
		},
		"type": "object",
		"properties": {
			"kind": { "$ref": "#/definitions/ContentType" }
		},
		"required": ["kind"]
	}`
	for cleaner, clean := range map[string]func(string) string{
		"antigravity":         CleanJSONSchemaForAntigravity,
		"antigravityResponse": CleanJSONSchemaForAntigravityResponse,
	} {
		got := clean(refInput)
		parsed := gjson.Parse(got)
		if !parsed.Get("properties.kind").Exists() {
			t.Errorf("%s: $ref property 'properties.kind' missing: %s", cleaner, got)
		}
		if gotType := parsed.Get("properties.kind.type").String(); gotType != "string" {
			t.Errorf("%s: inlined 'properties.kind.type' = %q, want string: %s", cleaner, gotType, got)
		}
		if !parsed.Get("properties.kind.enum").Exists() {
			t.Errorf("%s: inlined 'properties.kind.enum' missing: %s", cleaner, got)
		}
		if parsed.Get("properties.kind.id").Exists() {
			t.Errorf("%s: 'properties.kind.id' was not removed: %s", cleaner, got)
		}
		if parsed.Get("definitions").Exists() {
			t.Errorf("%s: 'definitions' was not removed: %s", cleaner, got)
		}
	}
}

// Ported from upstream CLIProxyAPI commit b532db9c (preserve constraints and
// additionalProperties in gemini parametersJsonSchema, issue #5959).

func TestCleanJSONSchemaForGeminiJSONSchema_PreservesAdditionalPropertiesAndPattern_Issue5959(t *testing.T) {
	input := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"title": "SubmitTool",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"recipient": {
				"type": "string",
				"pattern": "^(alice|bob)$",
				"minLength": 3,
				"maxLength": 10
			},
			"amount": {
				"type": "number",
				"minimum": 1
			},
			"nested": {
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"tag": {
						"type": "string",
						"pattern": "^[a-z]+$"
					}
				}
			},
			"items_list": {
				"type": "array",
				"items": {
					"type": "string",
					"pattern": "^[0-9]+$"
				}
			}
		},
		"required": ["recipient", "amount", "non_existent"]
	}`

	cleaned := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(cleaned)

	if got := parsed.Get("additionalProperties"); !got.Exists() || got.Type != gjson.False {
		t.Fatalf("additionalProperties should be preserved as false, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.recipient.pattern"); !got.Exists() || got.String() != "^(alice|bob)$" {
		t.Fatalf("pattern should be preserved, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.recipient.minLength").Int(); got != 3 {
		t.Fatalf("minLength should be preserved as 3, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.recipient.maxLength").Int(); got != 10 {
		t.Fatalf("maxLength should be preserved as 10, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.amount.minimum").Int(); got != 1 {
		t.Fatalf("minimum should be preserved as 1, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.nested.additionalProperties"); !got.Exists() || got.Type != gjson.False {
		t.Fatalf("nested additionalProperties should be preserved as false, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.nested.properties.tag.pattern"); !got.Exists() || got.String() != "^[a-z]+$" {
		t.Fatalf("nested tag pattern should be preserved, got: %v. Cleaned: %s", got, cleaned)
	}
	if got := parsed.Get("properties.items_list.items.pattern"); !got.Exists() || got.String() != "^[0-9]+$" {
		t.Fatalf("array items pattern should be preserved, got: %v. Cleaned: %s", got, cleaned)
	}
	if parsed.Get("title").Exists() {
		t.Fatalf("title should be removed. Cleaned: %s", cleaned)
	}
	if parsed.Get("$schema").Exists() {
		t.Fatalf("$schema should be removed. Cleaned: %s", cleaned)
	}
	if parsed.Get("description").Exists() && parsed.Get("description").String() == "No extra properties allowed" {
		t.Fatalf("additionalProperties: false should not be converted to description hint. Cleaned: %s", cleaned)
	}
	if got := parsed.Get("properties.recipient.description"); got.Exists() && strings.Contains(got.String(), "pattern:") {
		t.Fatalf("pattern should not be converted to description hint. Cleaned: %s", cleaned)
	}
	if got := parsed.Get("properties.nested.properties.tag.description"); got.Exists() && strings.Contains(got.String(), "pattern:") {
		t.Fatalf("nested pattern should not be converted to description hint. Cleaned: %s", cleaned)
	}
	if got := parsed.Get("properties.items_list.items.description"); got.Exists() && strings.Contains(got.String(), "pattern:") {
		t.Fatalf("array items pattern should not be converted to description hint. Cleaned: %s", cleaned)
	}
	// non_existent should be removed from required because it's not in properties
	required := parsed.Get("required").Array()
	if len(required) != 2 {
		t.Fatalf("required length = %d, want 2. Cleaned: %s", len(required), cleaned)
	}

	// Compare with legacy CleanJSONSchemaForGemini to verify backward compatibility
	legacyCleaned := CleanJSONSchemaForGemini(input)
	legacyParsed := gjson.Parse(legacyCleaned)
	if legacyParsed.Get("additionalProperties").Exists() {
		t.Fatalf("legacy cleaner should strip additionalProperties. Cleaned: %s", legacyCleaned)
	}
	if !strings.Contains(legacyParsed.Get("description").String(), "No extra properties allowed") {
		t.Fatalf("legacy cleaner should add description hint for additionalProperties. Cleaned: %s", legacyCleaned)
	}
	if legacyParsed.Get("properties.recipient.pattern").Exists() {
		t.Fatalf("legacy cleaner should strip pattern. Cleaned: %s", legacyCleaned)
	}
}

func TestCleanJSONSchemaForGeminiJSONSchema_PreservesSchemaValuedAdditionalProperties(t *testing.T) {
	input := `{
		"type": "object",
		"additionalProperties": {
			"type": "string",
			"pattern": "^[a-z]+$",
			"minLength": 2
		}
	}`

	cleaned := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(cleaned)

	ap := parsed.Get("additionalProperties")
	if !ap.Exists() || !ap.IsObject() {
		t.Fatalf("additionalProperties object should be preserved, got: %s", cleaned)
	}
	if got := ap.Get("type").String(); got != "string" {
		t.Fatalf("additionalProperties.type = %q, want string", got)
	}
	if got := ap.Get("pattern").String(); got != "^[a-z]+$" {
		t.Fatalf("additionalProperties.pattern = %q, want ^[a-z]+$", got)
	}
	if got := ap.Get("minLength").Int(); got != 2 {
		t.Fatalf("additionalProperties.minLength = %d, want 2", got)
	}
	if ap.Get("description").Exists() && strings.Contains(ap.Get("description").String(), "pattern:") {
		t.Fatalf("additionalProperties pattern should not be converted to description hint: %s", cleaned)
	}

	// Legacy cleaner should strip schema-valued additionalProperties
	legacyCleaned := CleanJSONSchemaForGemini(input)
	if gjson.Get(legacyCleaned, "additionalProperties").Exists() {
		t.Fatalf("legacy CleanJSONSchemaForGemini should strip additionalProperties schema: %s", legacyCleaned)
	}
}

// Ported from upstream CLIProxyAPI commit c93978c4 (normalize true boolean
// subschemas and strip unsupported keywords, issue #3551).

func TestCleanJSONSchema_TrueBooleanSubschemas(t *testing.T) {
	// Issue #3551: Antigravity rejects OpenAI function tools containing `true` JSON Schema subschemas.
	input := `{
		"type": "object",
		"properties": {
			"screenshot_id": true,
			"filename": true,
			"file_size": true,
			"source_file_checksum": true,
			"disabled_field": false
		},
		"additionalProperties": true
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity":         CleanJSONSchemaForAntigravity,
		"geminiJSONSchema":    CleanJSONSchemaForGeminiJSONSchema,
		"antigravityResponse": CleanJSONSchemaForAntigravityResponse,
		"gemini":              CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		// true subschemas under properties must be converted to empty object schemas {}
		for _, prop := range []string{"screenshot_id", "filename", "file_size", "source_file_checksum"} {
			val := parsed.Get("properties." + prop)
			if !val.Exists() {
				t.Fatalf("%s: expected property %q to exist in %s", cleaner, prop, got)
			}
			if val.Type != gjson.JSON || val.Raw != "{}" {
				t.Errorf("%s: property %q should be normalized to {}, got %s (type %v)", cleaner, prop, val.Raw, val.Type)
			}
		}

		// false subschema must NOT be converted to {} to preserve rejection semantics
		disabledVal := parsed.Get("properties.disabled_field")
		if !disabledVal.Exists() {
			t.Fatalf("%s: expected property 'disabled_field' to exist in %s", cleaner, got)
		}
		if disabledVal.Type != gjson.False {
			t.Errorf("%s: property 'disabled_field' should remain false, got %s", cleaner, disabledVal.Raw)
		}
	}
}

func TestCleanJSONSchema_NestedTrueBooleanSubschemas(t *testing.T) {
	// Issue #3551: Verify boolean true subschema normalization in nested schema positions.
	input := `{
		"type": "object",
		"properties": {
			"tags": {
				"type": "array",
				"items": true
			},
			"tuple": {
				"type": "array",
				"items": [true, {"type": "string"}],
				"additionalItems": true
			},
			"union": {
				"anyOf": [true, {"type": "string"}]
			},
			"combination": {
				"allOf": [true, {"type": "object", "properties": {"opt": true}}]
			},
			"metadata": {
				"type": "object",
				"properties": {
					"nested_true": true,
					"nested_false": false
				}
			},
			"large_int": 9007199254740993
		},
		"$defs": {
			"custom_schema": true
		}
	}`

	for cleaner, clean := range map[string]func(string) string{
		"antigravity":         CleanJSONSchemaForAntigravity,
		"geminiJSONSchema":    CleanJSONSchemaForGeminiJSONSchema,
		"antigravityResponse": CleanJSONSchemaForAntigravityResponse,
		"gemini":              CleanJSONSchemaForGemini,
	} {
		got := clean(input)
		parsed := gjson.Parse(got)

		// tags.items: true -> {}
		if val := parsed.Get("properties.tags.items"); val.Exists() && val.Type == gjson.True {
			t.Errorf("%s: tags.items should not be boolean true: %s", cleaner, got)
		}

		// nested properties: true -> {}, false preserved
		if val := parsed.Get("properties.metadata.properties.nested_true"); !val.Exists() || val.Type != gjson.JSON || val.Raw != "{}" {
			t.Errorf("%s: nested_true should be {}, got %s", cleaner, val.Raw)
		}
		if val := parsed.Get("properties.metadata.properties.nested_false"); !val.Exists() || val.Type != gjson.False {
			t.Errorf("%s: nested_false should remain false, got %s", cleaner, val.Raw)
		}

		// tuple items: true in list should be normalized to {}
		if val := parsed.Get("properties.tuple.items.0"); val.Exists() && val.Type == gjson.True {
			t.Errorf("%s: tuple items[0] should not be boolean true: %s", cleaner, got)
		}

		// union / combination inner property
		if val := parsed.Get("properties.combination.properties.opt"); val.Exists() {
			if val.Type == gjson.True {
				t.Errorf("%s: combination.properties.opt should not be boolean true: %s", cleaner, got)
			}
		}

		// large integer preservation
		if val := parsed.Get("properties.large_int"); !val.Exists() || val.Raw != "9007199254740993" {
			t.Errorf("%s: large_int corrupted, got %s", cleaner, val.Raw)
		}
	}
}

func TestCleanJSONSchema_RootAndWrappedTrue(t *testing.T) {
	// Verify root boolean true normalization to {}
	for cleaner, clean := range map[string]func(string) string{
		"antigravity":         CleanJSONSchemaForAntigravity,
		"geminiJSONSchema":    CleanJSONSchemaForGeminiJSONSchema,
		"antigravityResponse": CleanJSONSchemaForAntigravityResponse,
		"gemini":              CleanJSONSchemaForGemini,
	} {
		got := clean("true")
		if got != "{}" {
			t.Errorf("%s: root true should normalize to {}, got %s", cleaner, got)
		}

		// Verify wrapped {"schema": true} normalization
		wrapped := `{"schema": true}`
		gotWrapped := clean(wrapped)
		parsed := gjson.Parse(gotWrapped)
		if parsed.Get("schema").Exists() && parsed.Get("schema").Type == gjson.True {
			t.Errorf("%s: wrapped schema true should normalize to {}, got %s", cleaner, gotWrapped)
		}
	}
}

// Ported from upstream 2eb8dd11d248 (issue #6011 class): uppercase schema type
// declarations must behave like their lowercase forms. The local rewrite never
// stripped `items`, but the case-sensitive helpers skipped repair for
// "ARRAY"/"OBJECT" — missing items were not added and bare property maps were
// not folded.
func TestCleanJSONSchema_UppercaseTypeRepair(t *testing.T) {
	t.Run("UppercaseArrayGetsMissingItems", func(t *testing.T) {
		input := `{
			"type": "OBJECT",
			"properties": {
				"brands": {"type": "ARRAY"}
			}
		}`
		for name, clean := range map[string]func(string) string{
			"Antigravity": CleanJSONSchemaForAntigravity,
			"Gemini":      CleanJSONSchemaForGemini,
		} {
			got := clean(input)
			items := gjson.Parse(got).Get("properties.brands.items")
			if !items.Exists() {
				t.Fatalf("[%s] uppercase ARRAY without items was not repaired: %s", name, got)
			}
		}
	})

	t.Run("UppercaseArrayPreservesItems", func(t *testing.T) {
		input := `{
			"type": "OBJECT",
			"properties": {
				"summary": {"type": "STRING"},
				"brands": {
					"type": "ARRAY",
					"items": {"type": "STRING"}
				},
				"catalog": {
					"type": "OBJECT",
					"properties": {
						"items": {
							"type": "ARRAY",
							"items": {
								"type": "OBJECT",
								"properties": {"id": {"type": "STRING"}}
							}
						}
					}
				}
			}
		}`
		cleaners := map[string]func(string) string{
			"AntigravityResponse": CleanJSONSchemaForAntigravityResponse,
			"Antigravity":         CleanJSONSchemaForAntigravity,
			"Gemini":              CleanJSONSchemaForGemini,
			"GeminiJSONSchema":    CleanJSONSchemaForGeminiJSONSchema,
		}
		for name, clean := range cleaners {
			got := clean(input)
			parsed := gjson.Parse(got)
			if !parsed.Get("properties.brands.items").Exists() {
				t.Fatalf("[%s] properties.brands.items was lost for uppercase ARRAY: %s", name, got)
			}
			if typeStr := parsed.Get("properties.brands.type").String(); !strings.EqualFold(typeStr, "array") {
				t.Fatalf("[%s] properties.brands.type corrupted: %s", name, got)
			}
			if !parsed.Get("properties.catalog.properties.items.items").Exists() {
				t.Fatalf("[%s] properties.catalog.properties.items.items was lost: %s", name, got)
			}
		}
	})

	t.Run("UppercaseObjectFoldsBareProperties", func(t *testing.T) {
		// A node declared "OBJECT" with a bare property map child must fold the
		// child into "properties" exactly like lowercase "object".
		input := `{
			"type": "OBJECT",
			"nested": {"kind": {"type": "STRING"}}
		}`
		got := CleanJSONSchemaForGemini(input)
		parsed := gjson.Parse(got)
		if !parsed.Get("properties.nested.properties.kind").Exists() && !parsed.Get("properties.nested.kind").Exists() {
			t.Fatalf("uppercase OBJECT bare property map was not folded: %s", got)
		}
	})
}

func TestCleanJSONSchemaForAntigravityClaudeTool_FlattensNestedUnions_Issue3662(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"item": {
				"anyOf": [
					{
						"type": "object",
						"properties": {
							"image": {"type": "string"}
						}
					},
					{
						"type": "object",
						"properties": {
							"audio": {"type": "string"}
						}
					},
					{"type": "string"}
				]
			}
		}
	}`

	flattened := gjson.Parse(CleanJSONSchemaForAntigravityClaudeTool(input, false))
	item := flattened.Get("properties.item")
	if item.Get("anyOf").Exists() || item.Get("oneOf").Exists() {
		t.Fatalf("Antigravity Claude tool union survived cleaning: %s", flattened.Raw)
	}
	if got := item.Get("type").String(); got != "object" {
		t.Fatalf("Antigravity Claude flattened item type = %q, want object (strongest branch): %s", got, flattened.Raw)
	}
	if !item.Get("properties.image").Exists() {
		t.Fatalf("Antigravity Claude flattened item lost the first object branch: %s", flattened.Raw)
	}
	if desc := item.Get("description").String(); !strings.Contains(desc, "Accepts:") || !strings.Contains(desc, "string") {
		t.Fatalf("Antigravity Claude flattened item lost the dropped-branch hint: %s", flattened.Raw)
	}

	// Idempotency: cleaning twice preserves same shape
	flattenedTwice := CleanJSONSchemaForAntigravityClaudeTool(flattened.Raw, false)
	compareJSON(t, flattened.Raw, flattenedTwice)

	// Nullable multi-branch unions must keep nullability through translator -> executor chain
	nullableUnionInput := `{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]}}}`
	directFlattened := CleanJSONSchemaForAntigravityClaudeTool(nullableUnionInput, false)
	directValue := gjson.Parse(directFlattened).Get("properties.value")
	if !directValue.Get("nullable").Bool() || directValue.Get("type").String() != "string" {
		t.Fatalf("direct Claude flattening lost nullability: %s", directFlattened)
	}
	chainedFlattened := CleanJSONSchemaForAntigravityClaudeTool(CleanJSONSchemaForAntigravity(nullableUnionInput), false)
	chainedValue := gjson.Parse(chainedFlattened).Get("properties.value")
	if !chainedValue.Get("nullable").Bool() || chainedValue.Get("type").String() != "string" || chainedValue.Get("anyOf").Exists() {
		t.Fatalf("translator -> executor chain lost nullability while flattening: %s", chainedFlattened)
	}
	compareJSON(t, chainedFlattened, CleanJSONSchemaForAntigravityClaudeTool(chainedFlattened, false))
}

func TestCleanJSONSchema_RemovesDiscriminatorKeyword_Issue6639(t *testing.T) {
	input := `{
		"type": "object",
		"properties": {
			"target": {
				"anyOf": [
					{"type": "object", "properties": {"kind": {"const": "a"}, "url": {"type": "string"}}},
					{"type": "object", "properties": {"kind": {"const": "b"}, "path": {"type": "string"}}}
				],
				"discriminator": {"propertyName": "kind"}
			},
			"deliverables": {
				"type": "array",
				"items": {
					"anyOf": [
						{"type": "object", "properties": {"kind": {"const": "file"}, "path": {"type": "string"}}},
						{"type": "object", "properties": {"kind": {"const": "link"}, "url": {"type": "string"}}}
					],
					"discriminator": {"propertyName": "kind"}
				}
			},
			"polymorphicObject": {
				"type": "object",
				"properties": {
					"kind": {"type": "string"}
				},
				"oneOf": [
					{"properties": {"a": {"type": "string"}}},
					{"properties": {"b": {"type": "number"}}}
				],
				"discriminator": {
					"propertyName": "kind",
					"mapping": {"a": "#/definitions/A", "b": "#/definitions/B"}
				}
			},
			"discriminator": {
				"type": "string",
				"description": "Legitimate property named discriminator"
			},
			"nestedDiscriminatorProp": {
				"type": "object",
				"properties": {
					"discriminator": {
						"type": "object",
						"properties": {
							"kind": {"type": "string"}
						},
						"discriminator": {"propertyName": "kind"}
					}
				}
			}
		},
		"discriminator": {"propertyName": "target"}
	}`

	cleaners := map[string]func(string) string{
		"Antigravity":           CleanJSONSchemaForAntigravity,
		"AntigravityTool":       func(s string) string { return CleanJSONSchemaForAntigravityTool(s, false) },
		"AntigravityClaudeTool": func(s string) string { return CleanJSONSchemaForAntigravityClaudeTool(s, false) },
		"AntigravityResponse":   CleanJSONSchemaForAntigravityResponse,
		"Gemini":                CleanJSONSchemaForGemini,
		"GeminiJSONSchema":      CleanJSONSchemaForGeminiJSONSchema,
	}

	for name, clean := range cleaners {
		t.Run(name, func(t *testing.T) {
			got := clean(input)
			parsed := gjson.Parse(got)

			for _, forbiddenPath := range []string{
				"discriminator",
				"properties.target.discriminator",
				"properties.deliverables.items.discriminator",
				"properties.polymorphicObject.discriminator",
				"properties.nestedDiscriminatorProp.properties.discriminator.discriminator",
			} {
				if parsed.Get(forbiddenPath).Exists() {
					t.Errorf("[%s] %s was not removed: %s", name, forbiddenPath, got)
				}
			}

			if name != "AntigravityClaudeTool" {
				if gotBranches := len(parsed.Get("properties.target.anyOf").Array()); gotBranches != 2 {
					t.Errorf("[%s] properties.target.anyOf branch count = %d, want 2: %s", name, gotBranches, got)
				}
				if gotBranches := len(parsed.Get("properties.deliverables.items.anyOf").Array()); gotBranches != 2 {
					t.Errorf("[%s] properties.deliverables.items.anyOf branch count = %d, want 2: %s", name, gotBranches, got)
				}
			}

			if !parsed.Get("properties.discriminator").Exists() {
				t.Errorf("[%s] legitimate property \"discriminator\" was removed: %s", name, got)
			}
			if !parsed.Get("properties.nestedDiscriminatorProp.properties.discriminator.properties.kind").Exists() {
				t.Errorf("[%s] nested discriminator property was damaged: %s", name, got)
			}
		})
	}
}

func TestCleanJSONSchema_RemovesContentMediaTypeKeyword_Issue6699(t *testing.T) {
	input := `{
		"type": "object",
		"contentMediaType": "application/json",
		"contentEncoding": "utf-8",
		"properties": {
			"value": {
				"anyOf": [
					{ "type": "object", "properties": {} },
					{ "type": "string", "contentMediaType": "application/json", "contentEncoding": "base64" }
				]
			},
			"directPayload": {
				"type": "string",
				"contentMediaType": "application/json",
				"contentEncoding": "base64"
			},
			"contentMediaType": {
				"type": "string",
				"contentMediaType": "text/plain",
				"description": "Legitimate property named contentMediaType"
			},
			"contentEncoding": {
				"type": "string",
				"contentEncoding": "base64",
				"description": "Legitimate property named contentEncoding"
			}
		}
	}`

	cleaners := map[string]func(string) string{
		"Antigravity":           CleanJSONSchemaForAntigravity,
		"AntigravityTool":       func(s string) string { return CleanJSONSchemaForAntigravityTool(s, false) },
		"AntigravityClaudeTool": func(s string) string { return CleanJSONSchemaForAntigravityClaudeTool(s, false) },
		"AntigravityResponse":   CleanJSONSchemaForAntigravityResponse,
		"Gemini":                CleanJSONSchemaForGemini,
		"GeminiJSONSchema":      CleanJSONSchemaForGeminiJSONSchema,
	}

	for name, clean := range cleaners {
		t.Run(name, func(t *testing.T) {
			got := clean(input)
			parsed := gjson.Parse(got)

			for _, forbiddenPath := range []string{
				"contentMediaType",
				"contentEncoding",
				"properties.value.anyOf.1.contentMediaType",
				"properties.value.anyOf.1.contentEncoding",
				"properties.directPayload.contentMediaType",
				"properties.directPayload.contentEncoding",
				"properties.contentMediaType.contentMediaType",
				"properties.contentEncoding.contentEncoding",
			} {
				if parsed.Get(forbiddenPath).Exists() {
					t.Errorf("[%s] %s was not removed: %s", name, forbiddenPath, got)
				}
			}

			if name != "AntigravityClaudeTool" {
				if gotBranches := len(parsed.Get("properties.value.anyOf").Array()); gotBranches != 2 {
					t.Errorf("[%s] properties.value.anyOf branch count = %d, want 2: %s", name, gotBranches, got)
				}
				if gotType := parsed.Get("properties.value.anyOf.1.type").String(); gotType != "string" {
					t.Errorf("[%s] properties.value.anyOf.1.type = %q, want \"string\": %s", name, gotType, got)
				}
			}

			if !parsed.Get("properties.contentMediaType").Exists() {
				t.Errorf("[%s] legitimate property \"contentMediaType\" was removed: %s", name, got)
			}
			if !parsed.Get("properties.contentEncoding").Exists() {
				t.Errorf("[%s] legitimate property \"contentEncoding\" was removed: %s", name, got)
			}
		})
	}
}

func TestSanitizeAntigravityRequestSchemasScalesLinearlyWithDeclarations(t *testing.T) {
	const declCount = 256

	var input, want strings.Builder
	input.WriteString(`{"request":{"tools":[{"functionDeclarations":[`)
	want.WriteString(`{"request":{"tools":[{"functionDeclarations":[`)
	for i := range declCount {
		if i > 0 {
			input.WriteByte(',')
			want.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"name":"tool_%d","description":"%s","parametersJsonSchema":{"type":"object","$id":"drop","properties":{"q":{"type":"string"}}}}`, i, strings.Repeat("x", 4096))
		fmt.Fprintf(&want, `{"name":"tool_%d","description":"%s","parameters":{"type":"object","properties":{"q":{"type":"string"},"_":{"type":"boolean"}},"required":["_"]}}`, i, strings.Repeat("x", 4096))
	}
	input.WriteString(`]}]}}`)
	want.WriteString(`]}]}}`)

	payload := input.String()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := sanitizeAntigravityRequestSchemasForModel(payload, true, false)
	runtime.ReadMemStats(&after)

	if got != want.String() {
		t.Fatalf("sanitized tools diverged from the expected bytes\nfirst declaration want: %s\nfirst declaration got:  %s",
			gjson.Get(want.String(), "request.tools.0.functionDeclarations.0").Raw,
			gjson.Get(got, "request.tools.0.functionDeclarations.0").Raw)
	}

	const allowedCopies = 32
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(allowedCopies*len(payload)) {
		t.Fatalf("sanitizing %d declarations allocated %d bytes for a %d byte payload, want at most %d (cost must be linear in the tools size, not per declaration)",
			declCount, allocated, len(payload), allowedCopies*len(payload))
	}
}

func TestSanitizeAntigravityRequestSchemasPreservesToolArrayWhitespace(t *testing.T) {
	input := "{\n" +
		"  \"request\": {\n" +
		"    \"tools\": [\n" +
		"      {\"functionDeclarations\": [\n" +
		"        {\"name\": \"a\", \"parametersJsonSchema\": {\"type\": \"object\", \"$id\": \"drop\", \"properties\": {\"q\": {\"type\": \"string\"}}}} ,\n" +
		"        {\"name\": \"b\", \"parameters\": {\"type\": \"object\", \"required\": [\"q\"], \"properties\": {\"q\": {\"type\": \"string\"}}}} ,\n" +
		"        {\"name\": \"c\"}\n" +
		"      ]} ,\n" +
		"      {\"googleSearch\": {}}\n" +
		"    ]\n" +
		"  }\n" +
		"}\n"

	want := strings.Replace(input,
		`{"name": "a", "parametersJsonSchema": {"type": "object", "$id": "drop", "properties": {"q": {"type": "string"}}}}`,
		`{"name": "a","parameters":{"type": "object", "properties": {"q": {"type": "string"},"_":{"type":"boolean"}},"required":["_"]}}`, 1)
	if want == input {
		t.Fatal("test fixture is broken: declaration a was not replaced in the expected value")
	}

	if got := sanitizeAntigravityRequestSchemasForModel(input, true, false); got != want {
		t.Fatalf("pretty-printed tools lost whitespace outside the cleaned declaration\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCleanJSONSchema_RemovesReadOnlyWriteOnlyKeywords_Issue6778 covers upstream v8.0.35
// 154d5144fde6 (issue #6778): the JSON Schema access annotations readOnly/writeOnly, which Google
// Discovery-derived MCP tools such as Firestore emit, are rejected by the Gemini/Antigravity
// backend with 400 INVALID_ARGUMENT ("Unknown name \"readOnly\"") and must be stripped on every
// cleaner, while legitimate tool properties named readOnly/writeOnly keep their schema.
func TestCleanJSONSchema_RemovesReadOnlyWriteOnlyKeywords_Issue6778(t *testing.T) {
	input := `{
		"type": "object",
		"$schema": "http://json-schema.org/draft-04/schema#",
		"readOnly": false,
		"writeOnly": false,
		"properties": {
			"parent": { "type": "string", "description": "Parent resource name" },
			"database": {
				"type": "object",
				"properties": {
					"name":       { "type": "string", "description": "Database resource name", "readOnly": true },
					"uid":        { "type": "string", "readOnly": true },
					"createTime": { "type": "string", "readOnly": true },
					"etag":       { "type": "string", "writeOnly": true },
					"locationId": { "type": "string" }
				}
			},
			"itemsList": {
				"type": "array",
				"items": { "type": "string", "readOnly": true, "writeOnly": false }
			},
			"unionField": {
				"anyOf": [
					{ "type": "string", "readOnly": true },
					{ "type": "integer", "writeOnly": true }
				]
			},
			"readOnly": {
				"type": "boolean",
				"readOnly": true,
				"description": "Legitimate property named readOnly"
			},
			"writeOnly": {
				"type": "boolean",
				"writeOnly": true,
				"description": "Legitimate property named writeOnly"
			}
		},
		"required": ["parent"]
	}`

	for name, clean := range geminiRefCleaners {
		t.Run(name, func(t *testing.T) {
			got := clean(input)
			parsed := gjson.Parse(got)

			for _, forbiddenPath := range []string{
				"readOnly",
				"writeOnly",
				"properties.database.properties.name.readOnly",
				"properties.database.properties.uid.readOnly",
				"properties.database.properties.createTime.readOnly",
				"properties.database.properties.etag.writeOnly",
				"properties.itemsList.items.readOnly",
				"properties.itemsList.items.writeOnly",
				"properties.unionField.readOnly",
				"properties.unionField.writeOnly",
				"properties.unionField.anyOf.0.readOnly",
				"properties.unionField.anyOf.1.writeOnly",
				"properties.readOnly.readOnly",
				"properties.writeOnly.writeOnly",
			} {
				if parsed.Get(forbiddenPath).Exists() {
					t.Errorf("[%s] %s was not removed: %s", name, forbiddenPath, got)
				}
			}

			for _, keptField := range []string{"name", "uid", "createTime", "etag", "locationId"} {
				if gotType := parsed.Get("properties.database.properties." + keptField + ".type").String(); gotType != "string" {
					t.Errorf("[%s] properties.database.properties.%s.type = %q, want \"string\": %s", name, keptField, gotType, got)
				}
			}
			if gotDesc := parsed.Get("properties.database.properties.name.description").String(); gotDesc != "Database resource name" {
				t.Errorf("[%s] properties.database.properties.name.description = %q, want %q: %s", name, gotDesc, "Database resource name", got)
			}
			if gotType := parsed.Get("properties.readOnly.type").String(); gotType != "boolean" {
				t.Errorf("[%s] legitimate property \"readOnly\" lost its type (%q): %s", name, gotType, got)
			}
			if gotDesc := parsed.Get("properties.readOnly.description").String(); gotDesc != "Legitimate property named readOnly" {
				t.Errorf("[%s] legitimate property \"readOnly\" lost its description (%q): %s", name, gotDesc, got)
			}
			if gotType := parsed.Get("properties.writeOnly.type").String(); gotType != "boolean" {
				t.Errorf("[%s] legitimate property \"writeOnly\" lost its type (%q): %s", name, gotType, got)
			}
			if gotDesc := parsed.Get("properties.writeOnly.description").String(); gotDesc != "Legitimate property named writeOnly" {
				t.Errorf("[%s] legitimate property \"writeOnly\" lost its description (%q): %s", name, gotDesc, got)
			}
		})
	}
}

// TestIssue6950_ArrayItemsFromAllOfPreserved ports CLIProxyAPI v8.0.43 b2fda98ea831 (issue #6950):
// an array whose element schema arrives through an allOf branch (directly or through a $ref) must
// keep that schema. The {"type":"string"} placeholder is only for arrays that end up with no items
// at all after refs and allOf are resolved, so a node that already carries a real items schema must
// never be filled with the placeholder instead.
func TestIssue6950_ArrayItemsFromAllOfPreserved(t *testing.T) {
	cleaners := map[string]struct {
		clean             func(string) string
		preservesNullable bool
	}{
		"Antigravity":           {clean: CleanJSONSchemaForAntigravity, preservesNullable: true},
		"AntigravityTool":       {clean: func(s string) string { return CleanJSONSchemaForAntigravityTool(s, false) }, preservesNullable: true},
		"AntigravityClaudeTool": {clean: func(s string) string { return CleanJSONSchemaForAntigravityClaudeTool(s, false) }, preservesNullable: true},
		"Gemini":                {clean: CleanJSONSchemaForGemini},
	}

	cases := []struct {
		name         string
		input        string
		path         string
		wantItemType string
		wantDesc     string
	}{
		{
			name:         "direct_allof_items",
			input:        `{"type":"object","properties":{"ids":{"type":"array","allOf":[{"items":{"type":"integer"}}]}}}`,
			path:         "properties.ids",
			wantItemType: "integer",
		},
		{
			name:         "ref_in_allof_items",
			input:        `{"type":"object","properties":{"ids":{"type":"array","allOf":[{"$ref":"#/$defs/Ids"}]}},"$defs":{"Ids":{"type":"array","items":{"type":"integer"}}}}`,
			path:         "properties.ids",
			wantItemType: "integer",
		},
		{
			name:         "nullable_type_array_with_ref_in_allof",
			input:        `{"type":"object","properties":{"ids":{"type":["array","null"],"allOf":[{"$ref":"#/$defs/Ids"}]}},"$defs":{"Ids":{"type":"array","items":{"type":"integer"}}}}`,
			path:         "properties.ids",
			wantItemType: "integer",
			wantDesc:     "(nullable)",
		},
		{
			name:         "split_allof_branches",
			input:        `{"type":"object","properties":{"ids":{"allOf":[{"type":"array","description":"ID list"},{"items":{"type":"integer"}}]}}}`,
			path:         "properties.ids",
			wantItemType: "integer",
			wantDesc:     "ID list",
		},
		{
			name:         "ref_base_array_with_allof_items",
			input:        `{"type":"object","properties":{"ids":{"$ref":"#/$defs/BaseArray","allOf":[{"items":{"type":"integer"}}]}},"$defs":{"BaseArray":{"type":"array","description":"ID list"}}}`,
			path:         "properties.ids",
			wantItemType: "integer",
			wantDesc:     "ID list",
		},
		{
			name:         "root_array_with_allof_items",
			input:        `{"type":"array","allOf":[{"items":{"type":"integer"}}]}`,
			path:         "",
			wantItemType: "integer",
		},
		{
			name:         "allof_without_items_gets_placeholder",
			input:        `{"type":"object","properties":{"ids":{"type":"array","allOf":[{"description":"x"}]}}}`,
			path:         "properties.ids",
			wantItemType: "string",
			wantDesc:     "x",
		},
	}

	for cleanerName, cleaner := range cleaners {
		for _, tc := range cases {
			t.Run(cleanerName+"/"+tc.name, func(t *testing.T) {
				got := cleaner.clean(tc.input)
				node := gjson.Parse(got)
				if tc.path != "" {
					node = node.Get(tc.path)
				}
				if gotType := node.Get("type").String(); gotType != "array" {
					t.Errorf("type = %q, want \"array\": %s", gotType, got)
				}
				if gotItemType := node.Get("items.type").String(); gotItemType != tc.wantItemType {
					t.Errorf("items.type = %q, want %q: %s", gotItemType, tc.wantItemType, got)
				}
				if tc.wantDesc != "" {
					if gotDesc := node.Get("description").String(); !strings.Contains(gotDesc, tc.wantDesc) {
						t.Errorf("description = %q, want it to contain %q: %s", gotDesc, tc.wantDesc, got)
					}
				}
				compareJSON(t, got, cleaner.clean(got))
			})
		}
	}
}
