package util

// Live-path coverage for R4/R5: the Gemini-bound tool declaration builder that the Responses route
// calls (internal/translator/gemini/openai/responses/gemini_openai-responses_request.go:34) must
// forward a schema whose local references are inlined and whose unsupported access annotations are
// stripped, without losing the referenced fields.

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiFunctionDeclarationsInlineLocalRefsAndStripAccessAnnotations(t *testing.T) {
	root := gjson.Parse(`{
		"model": "gemini-2.5-pro",
		"tools": [{
			"type": "function",
			"name": "lookup",
			"description": "Look up a person",
			"parameters": {
				"type": "object",
				"$defs": {
					"Address": {
						"type": "object",
						"properties": {
							"city": { "type": "string", "readOnly": true },
							"note": { "type": "string", "writeOnly": true }
						},
						"required": ["city"]
					}
				},
				"properties": {
					"person": { "$ref": "#/$defs/Address" },
					"readOnly": { "type": "boolean", "readOnly": true, "description": "a property named readOnly" }
				},
				"readOnly": false,
				"writeOnly": false,
				"required": ["person"]
			}
		}]
	}`)

	declarations, _, reverseMap := BuildGeminiFunctionDeclarations(root)
	if len(declarations) != 1 {
		t.Fatalf("expected 1 function declaration, got %d", len(declarations))
	}
	emitted := string(declarations[0])
	params := gjson.Get(emitted, "parametersJsonSchema")
	if !params.Exists() {
		t.Fatalf("parametersJsonSchema missing: %s", emitted)
	}

	// The referenced definition reaches Gemini with its own fields.
	if got := params.Get("properties.person.properties.city.type").String(); got != "string" {
		t.Errorf("referenced property city.type = %q, want string: %s", got, emitted)
	}
	if got := params.Get("properties.person.required.0").String(); got != "city" {
		t.Errorf("referenced required entry = %q, want city: %s", got, emitted)
	}

	// Unsupported access annotations are stripped, but a property carrying that name survives.
	for _, path := range []string{
		"properties.person.properties.city.readOnly",
		"properties.person.properties.note.writeOnly",
		"properties.readOnly.readOnly",
	} {
		if params.Get(path).Exists() {
			t.Errorf("%s was not stripped from the emitted declaration: %s", path, emitted)
		}
	}
	if got := params.Get("properties.readOnly.type").String(); got != "boolean" {
		t.Errorf("property named readOnly lost its schema, type = %q: %s", got, emitted)
	}

	// No reference or definition container survives anywhere in the emitted schema.
	for _, forbidden := range []string{`"$ref"`, `"$defs"`} {
		if strings.Contains(emitted, forbidden) {
			t.Errorf("emitted declaration still contains %s: %s", forbidden, emitted)
		}
	}

	// The reverse identity map still resolves the emitted function name to the caller's tool.
	identity, ok := reverseMap["lookup"]
	if !ok || identity.Name != "lookup" || identity.Custom {
		t.Errorf("reverse identity map lost the tool: %+v (present=%v)", identity, ok)
	}
}

func TestGeminiFunctionDeclarationsInlineNestedDefinitionOnChatRoute(t *testing.T) {
	root := gjson.Parse(`{
		"tools": [{
			"type": "function",
			"name": "ship",
			"parameters": {
				"type": "object",
				"properties": {
					"billing": { "$ref": "#/properties/shipping" },
					"shipping": { "type": "object", "properties": { "zip": { "type": "string" } } }
				}
			}
		}]
	}`)

	declarations, _, _ := BuildGeminiFunctionDeclarations(root)
	if len(declarations) != 1 {
		t.Fatalf("expected 1 function declaration, got %d", len(declarations))
	}
	params := gjson.Get(string(declarations[0]), "parametersJsonSchema")
	if got := params.Get("properties.billing.properties.zip.type").String(); got != "string" {
		t.Errorf("property reference was not inlined, billing.zip.type = %q: %s", got, declarations[0])
	}
}

// TestSanitizeAntigravityRequestSchemasForModelInlinesLocalRefs drives the Antigravity executor's
// real schema seam (internal/runtime/executor/antigravity_executor.go:2901 delegates here). Both
// live routes are covered in one request: tool parameter schemas, cleaned per declaration inside the
// {"schema": ...} carrier that cleanNestedSchema builds, and the generationConfig response schema.
func TestSanitizeAntigravityRequestSchemasForModelInlinesLocalRefs(t *testing.T) {
	payload := `{
		"request": {
			"tools": [{
				"functionDeclarations": [{
					"name": "lookup",
					"description": "Look up a person",
					"parameters": {
						"type": "object",
						"$defs": {
							"Address": {
								"type": "object",
								"properties": { "city": { "type": "string", "readOnly": true } },
								"required": ["city"]
							}
						},
						"properties": { "person": { "$ref": "#/$defs/Address" } },
						"required": ["person"]
					}
				}]
			}],
			"generationConfig": {
				"responseSchema": {
					"type": "object",
					"$defs": {
						"Address": {
							"type": "object",
							"properties": { "city": { "type": "string", "writeOnly": true } }
						}
					},
					"properties": { "addr": { "$ref": "#/$defs/Address" } }
				}
			}
		}
	}`

	got := SanitizeAntigravityRequestSchemasForModel(payload, true, false)
	parsed := gjson.Parse(got)

	if gotType := parsed.Get("request.tools.0.functionDeclarations.0.parameters.properties.person.properties.city.type").String(); gotType != "string" {
		t.Errorf("tool schema reference was not inlined, person.city.type = %q: %s", gotType, got)
	}
	if gotType := parsed.Get("request.generationConfig.responseSchema.properties.addr.properties.city.type").String(); gotType != "string" {
		t.Errorf("response schema reference was not inlined, addr.city.type = %q: %s", gotType, got)
	}
	for _, forbidden := range []string{`"$ref"`, `"$defs"`, `"readOnly"`, `"writeOnly"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized payload still contains %s: %s", forbidden, got)
		}
	}
}
