package util

// Tests ported from CLIProxyAPI v8.0.34 c558d3203e89 (issue #6732): local $ref inlining and
// literal-value preservation in the Gemini/Antigravity schema cleaners.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// geminiRefCleaners lists every cleaner that goes through the shared cleanJSONSchema pass, so a
// contract asserted here holds on all of those routes.
var geminiRefCleaners = map[string]func(string) string{
	"gemini":                CleanJSONSchemaForGemini,
	"geminiJSONSchema":      CleanJSONSchemaForGeminiJSONSchema,
	"antigravity":           CleanJSONSchemaForAntigravity,
	"antigravityResponse":   CleanJSONSchemaForAntigravityResponse,
	"antigravityTool":       func(s string) string { return CleanJSONSchemaForAntigravityTool(s, false) },
	"antigravityClaudeTool": func(s string) string { return CleanJSONSchemaForAntigravityClaudeTool(s, false) },
}

func TestIssue6732_GeminiCleanersInlineLocalDefs(t *testing.T) {
	input := `{"type":"object","properties":{"person":{"$ref":"#/$defs/Person"}},"required":["person"],"$defs":{"Person":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}`

	for name, clean := range geminiRefCleaners {
		got := clean(input)
		parsed := gjson.Parse(got)
		if gotType := parsed.Get("properties.person.properties.name.type").String(); gotType != "string" {
			t.Errorf("%s: inlined properties.person.properties.name.type = %q, want %q: %s", name, gotType, "string", got)
		}
		if parsed.Get("$defs").Exists() {
			t.Errorf("%s: $defs should be removed after inlining: %s", name, got)
		}
		if strings.Contains(got, "See: Person") {
			t.Errorf("%s: resolvable local ref degraded to a hint: %s", name, got)
		}
	}
}

// TestIssue6732_GeminiCleanersInlinePropertyReuse verifies that a Zod-style #/properties/...
// reference is expanded by the cleaners instead of becoming a description hint.
func TestIssue6732_GeminiCleanersInlinePropertyReuse(t *testing.T) {
	input := `{"type":"object","properties":{"billing":{"type":"object","properties":{"zip":{"type":"string"}}},"shipping":{"$ref":"#/properties/billing"}}}`

	for name, clean := range geminiRefCleaners {
		got := clean(input)
		if gotType := gjson.Parse(got).Get("properties.shipping.properties.zip.type").String(); gotType != "string" {
			t.Errorf("%s: properties.shipping.properties.zip.type = %q, want %q: %s", name, gotType, "string", got)
		}
	}
}

// TestIssue6732_GeminiCleanersReuseSameDefinitionTwice verifies that two properties referencing
// the same definition both keep the referenced fields: bounding the expansion must not degrade
// ordinary reuse of a shared definition to a hint.
func TestIssue6732_GeminiCleanersReuseSameDefinitionTwice(t *testing.T) {
	input := `{"type":"object","properties":{"billing":{"$ref":"#/$defs/Address"},"shipping":{"$ref":"#/$defs/Address"}},"$defs":{"Address":{"type":"object","properties":{"zip":{"type":"string"}}}}}`

	for name, clean := range geminiRefCleaners {
		got := clean(input)
		parsed := gjson.Parse(got)
		for _, field := range []string{"billing", "shipping"} {
			if gotType := parsed.Get("properties." + field + ".properties.zip.type").String(); gotType != "string" {
				t.Errorf("%s: properties.%s.properties.zip.type = %q, want %q: %s", name, field, gotType, "string", got)
			}
		}
	}
}

// TestIssue6732_WrappedSchemaResolvesAgainstInnerRoot verifies that a schema wrapped in the
// single-key {"schema": ...} carrier resolves local refs against the inner schema rather than the
// carrier. internal/util/translator.go relies on this when it simulates the final sent schema.
func TestIssue6732_WrappedSchemaResolvesAgainstInnerRoot(t *testing.T) {
	inner := `{"type":"object","properties":{"person":{"$ref":"#/$defs/Person"}},"required":["person"],"$defs":{"Person":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}`
	wrapped := `{"schema":` + inner + `}`

	got := CleanJSONSchemaForAntigravityTool(wrapped, false)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get("schema.properties.person.properties.name.type").String(); gotType != "string" {
		t.Errorf("wrapped schema.properties.person.properties.name.type = %q, want %q: %s", gotType, "string", got)
	}
	if parsed.Get("schema.$defs").Exists() {
		t.Errorf("wrapped $defs should be removed after inlining: %s", got)
	}
	if strings.Contains(got, "See: Person") {
		t.Errorf("wrapped resolvable local ref degraded to a hint: %s", got)
	}
}

// TestIssue6732_DefaultLiteralPreservedOnJSONSchemaRoute verifies that the parametersJsonSchema
// route forwards default values verbatim even when they contain schema-looking keys.
func TestIssue6732_DefaultLiteralPreservedOnJSONSchemaRoute(t *testing.T) {
	input := `{"type":"object","properties":{"report":{"type":"object","default":{"title":"Report","format":"pdf","id":7,"x-owner":"ops"}}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	def := gjson.Parse(got).Get("properties.report.default")
	if !def.Exists() {
		t.Fatalf("default literal missing: %s", got)
	}
	if def.Get("title").String() != "Report" || def.Get("format").String() != "pdf" || def.Get("id").Int() != 7 || def.Get("x-owner").String() != "ops" {
		t.Errorf("default literal was rewritten, got %s: %s", def.Raw, got)
	}
}

// TestIssue6732_ExampleLiteralsPreserved verifies that example/examples values keep
// schema-looking keys on the parametersJsonSchema route.
func TestIssue6732_ExampleLiteralsPreserved(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"string","example":{"title":"Hi","x-owner":"ops","id":3},"examples":[{"title":"Hi2","x-owner":"ops","id":4}]}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)
	example := parsed.Get("properties.a.example")
	if example.Get("title").String() != "Hi" || example.Get("x-owner").String() != "ops" || example.Get("id").Int() != 3 {
		t.Errorf("example literal was rewritten, got %s: %s", example.Raw, got)
	}
	exampleItem := parsed.Get("properties.a.examples.0")
	if exampleItem.Get("title").String() != "Hi2" || exampleItem.Get("x-owner").String() != "ops" || exampleItem.Get("id").Int() != 4 {
		t.Errorf("examples literal was rewritten, got %s: %s", exampleItem.Raw, got)
	}
}

// TestIssue6732_InlineLocalRefsLeavesLiteralRefs verifies that the inliner does not resolve
// {"$ref": ...} objects that appear as client data inside a default value.
func TestIssue6732_InlineLocalRefsLeavesLiteralRefs(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"type":"object","default":{"$ref":"#/$defs/Person"}}},"$defs":{"Person":{"type":"object","properties":{"name":{"type":"string"}}}}}`

	got := InlineLocalRefs(input)
	parsed := gjson.Parse(got)
	if ref := parsed.Get("properties.a.default.$ref").String(); ref != "#/$defs/Person" {
		t.Errorf("literal $ref inside default = %q, want it preserved verbatim: %s", ref, got)
	}
	if parsed.Get("properties.a.default.properties").Exists() {
		t.Errorf("literal $ref inside default was expanded: %s", got)
	}
	if !parsed.Get("$defs.Person.properties.name").Exists() {
		t.Errorf("InlineLocalRefs must not strip definitions: %s", got)
	}
}

// TestIssue6732_PropertiesNamedDefaultAndTypeRemainSchemas verifies that properties literally
// named "default" and "type" keep their schema semantics: their own keywords are still cleaned
// and their own $ref is still inlined.
func TestIssue6732_PropertiesNamedDefaultAndTypeRemainSchemas(t *testing.T) {
	input := `{"type":"object","properties":{"default":{"$ref":"#/$defs/Person"},"type":{"type":"string","title":"Kind"}},"$defs":{"Person":{"type":"object","properties":{"name":{"type":"string"}}}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get("properties.default.properties.name.type").String(); gotType != "string" {
		t.Errorf("property named \"default\" ref was not inlined, name type = %q: %s", gotType, got)
	}
	if parsed.Get("properties.type.title").Exists() {
		t.Errorf("property named \"type\" schema title should still be cleaned: %s", got)
	}
}

// TestIssue6732_ExpansionBudgetBoundsSharedDefs verifies that inlining a schema whose shared
// definitions fan out exponentially stays bounded instead of expanding without limit, while the
// affordable outer levels are still expanded.
func TestIssue6732_ExpansionBudgetBoundsSharedDefs(t *testing.T) {
	input := geminiSchemaDiamondInput()
	got := CleanJSONSchemaForGeminiJSONSchema(input)
	if len(got) >= 512*1024 {
		t.Errorf("diamond schema output = %d bytes, want < %d (input %d bytes)", len(got), 512*1024, len(input))
	}
	parsed := gjson.Parse(got)
	if !parsed.Get("properties.leaf").Exists() {
		t.Errorf("diamond schema lost the top-level property: %s", got)
	}
	if !parsed.Get("properties.leaf.properties.a.properties.a").Exists() {
		t.Errorf("diamond schema outer levels should still be expanded: %s", got)
	}
	if parsed.Get("$defs").Exists() {
		t.Errorf("diamond schema should not retain $defs: %s", got)
	}
}

// TestIssue6732_UnusedDefinitionsDoNotConsumeExpansionBudget verifies that definitions the
// cleaners are about to delete anyway neither consume the expansion budget nor make the kept
// fields depend on map iteration order.
func TestIssue6732_UnusedDefinitionsDoNotConsumeExpansionBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{"person":{"$ref":"#/$defs/Person"}},"required":["person"],"$defs":{"leaf0":{"type":"string","description":"leaf leaf leaf leaf leaf leaf leaf leaf leaf leaf"},"Person":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	for i := range 13 {
		target := "leaf0"
		if i > 0 {
			target = "D" + strconv.Itoa(i-1)
		}
		sb.WriteString(`,"D` + strconv.Itoa(i) + `":{"type":"object","properties":{"a":{"$ref":"#/$defs/` + target + `"},"b":{"$ref":"#/$defs/` + target + `"}}}`)
	}
	sb.WriteString(`}}`)
	input := sb.String()

	first := CleanJSONSchemaForGeminiJSONSchema(input)
	for run := range 30 {
		got := CleanJSONSchemaForGeminiJSONSchema(input)
		if got != first {
			t.Fatalf("run %d: output is not deterministic:\nfirst: %s\ngot:   %s", run, first, got)
		}
		parsed := gjson.Parse(got)
		if gotType := parsed.Get("properties.person.properties.name.type").String(); gotType != "string" {
			t.Fatalf("run %d: used definition was not inlined, name type = %q: %s", run, gotType, got)
		}
		if parsed.Get("$defs").Exists() {
			t.Fatalf("run %d: $defs should be removed: %s", run, got)
		}
	}
}

// TestIssue6732_BudgetFallbackDoesNotCopyLargeDescriptions verifies that references degraded by
// the expansion budget do not each copy a large target description, which would defeat the bound.
func TestIssue6732_BudgetFallbackDoesNotCopyLargeDescriptions(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{`)
	for i := range 200 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"p` + strconv.Itoa(i) + `":{"$ref":"#/$defs/Big"}`)
	}
	sb.WriteString(`},"$defs":{"Big":{"type":"object","description":"` + strings.Repeat("x", 32*1024) + `","properties":{"name":{"type":"string"}}}}}`)

	got := CleanJSONSchemaForGeminiJSONSchema(sb.String())
	if len(got) >= 512*1024 {
		t.Errorf("shared large-description output = %d bytes, want < %d", len(got), 512*1024)
	}
	parsed := gjson.Parse(got)
	if !parsed.Get("properties.p0").Exists() || !parsed.Get("properties.p199").Exists() {
		t.Errorf("shared large-description schema lost properties: %s", got)
	}
	if gotType := parsed.Get("properties.p0.properties.name.type").String(); gotType != "string" {
		t.Errorf("first affordable expansion should keep its fields, name type = %q: %s", gotType, got)
	}
	if parsed.Get("$defs").Exists() {
		t.Errorf("$defs should be removed: %s", got)
	}
}

// TestIssue6732_NestedUnusedDefinitionsGraphDoesNotConsumeBudget verifies that a self-contained
// reference graph inside a nested definitions container (which cleaning deletes at every level)
// neither spends the expansion budget nor starves kept fields.
func TestIssue6732_NestedUnusedDefinitionsGraphDoesNotConsumeBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{"person":{"$ref":"#/$defs/Person"},"a":{"type":"object","$defs":{"Leaf":{"type":"string","description":"` + strings.Repeat("x", 1024) + `"}`)
	for i := range 256 {
		sb.WriteString(`,"U` + strconv.Itoa(i) + `":{"$ref":"#/properties/a/$defs/Leaf"}`)
	}
	sb.WriteString(`}}},"required":["person"],"$defs":{"Person":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}`)

	got := CleanJSONSchemaForGeminiJSONSchema(sb.String())
	parsed := gjson.Parse(got)
	if gotType := parsed.Get("properties.person.properties.name.type").String(); gotType != "string" {
		t.Errorf("nested unused definitions starved the kept field, name type = %q: %s", gotType, got)
	}
	if parsed.Get("properties.a.$defs").Exists() || parsed.Get("$defs").Exists() {
		t.Errorf("nested and root $defs should be removed: %s", got)
	}
}

// TestIssue6732_RefSiblingNameMapReferencesStillExpand verifies that references inside a $ref
// node's own name-map siblings expand when the reference target itself expands.
func TestIssue6732_RefSiblingNameMapReferencesStillExpand(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"$ref":"#/$defs/Base","properties":{"x":{"$ref":"#/$defs/S"}}}},"$defs":{"Base":{"type":"object"},"S":{"type":"string"}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	if gotType := gjson.Parse(got).Get("properties.a.properties.x.type").String(); gotType != "string" {
		t.Errorf("ref sibling name-map reference lost its type, got %q: %s", gotType, got)
	}
}

// TestIssue6732_ArrayRefWithExplicitTypeKeepsTargetItems verifies that a placeholder items added
// to a typed but unresolved $ref node does not clobber the items of the referenced definition
// during inlining.
func TestIssue6732_ArrayRefWithExplicitTypeKeepsTargetItems(t *testing.T) {
	input := `{"type":"object","properties":{"rows":{"type":"array","$ref":"#/$defs/Rows"}},"$defs":{"Rows":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"}}}}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	if gotType := gjson.Parse(got).Get("properties.rows.items.properties.name.type").String(); gotType != "string" {
		t.Errorf("array ref target items were clobbered, name type = %q: %s", gotType, got)
	}
}

// TestIssue6732_BudgetFallbackStillResolvesSiblings verifies that when a reference target is too
// large to expand, sibling subschemas on the same node are still resolved instead of being copied
// verbatim with their own references left dangling.
func TestIssue6732_BudgetFallbackStillResolvesSiblings(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"$ref":"#/$defs/Big","properties":{"x":{"$ref":"#/$defs/S"}}}},"$defs":{"Big":{"type":"object","description":"` + strings.Repeat("x", 280*1024) + `"},"S":{"type":"string"}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get("properties.a.properties.x.type").String(); gotType != "string" {
		t.Errorf("sibling reference after budget fallback lost its type, got %q: %s", gotType, got)
	}
	if strings.Contains(got, `"$ref"`) {
		t.Errorf("cleaned schema left a dangling reference: %s", got)
	}
}

// TestIssue6732_InlineLocalRefsDefinitionNamesKeepSchemaSemantics verifies that definitions whose
// names look like schema keywords are treated as schemas by the public inliner: a definition named
// "properties" keeps its literal default intact, and a definition named "default" still expands.
func TestIssue6732_InlineLocalRefsDefinitionNamesKeepSchemaSemantics(t *testing.T) {
	input := `{"$defs":{"S":{"type":"string"},"properties":{"type":"object","default":{"$ref":"#/$defs/S"}},"default":{"type":"object","properties":{"x":{"$ref":"#/$defs/S"}}}}}`

	got := InlineLocalRefs(input)
	parsed := gjson.Parse(got)
	if ref := parsed.Get("$defs.properties.default.$ref").String(); ref != "#/$defs/S" {
		t.Errorf("literal default inside definition named \"properties\" was expanded, $ref = %q: %s", ref, got)
	}
	if gotType := parsed.Get("$defs.default.properties.x.type").String(); gotType != "string" {
		t.Errorf("definition named \"default\" was not resolved as a schema, x type = %q: %s", gotType, got)
	}
}

// TestIssue6732_TargetNestedDefinitionsExemptFromBudget verifies that unused definitions nested
// inside a referenced target (which cleaning deletes) do not inflate the target's budget charge.
func TestIssue6732_TargetNestedDefinitionsExemptFromBudget(t *testing.T) {
	person := `{"type":"object","properties":{"name":{"type":"string"}},"$defs":{"Unused":{"type":"string","description":"` + strings.Repeat("x", 280*1024) + `"}}}`
	cases := map[string]string{
		"propertyRef": `{"type":"object","properties":{"person":{"$ref":"#/$defs/Person"}},"$defs":{"Person":` + person + `}}`,
		"rootRef":     `{"$ref":"#/$defs/Person","$defs":{"Person":` + person + `}}`,
	}
	for caseName, input := range cases {
		got := CleanJSONSchemaForGeminiJSONSchema(input)
		parsed := gjson.Parse(got)
		namePath := "properties.person.properties.name.type"
		if caseName == "rootRef" {
			namePath = "properties.name.type"
		}
		if gotType := parsed.Get(namePath).String(); gotType != "string" {
			t.Errorf("%s: %s = %q, want %q: %s", caseName, namePath, gotType, "string", got)
		}
		if parsed.Get("$defs").Exists() {
			t.Errorf("%s: $defs should be removed: %s", caseName, got)
		}
	}
}

// TestIssue6732_ArrayRefWithoutTargetItemsGetsPlaceholder verifies that a typed array whose
// referenced definition carries no items still ends up with the placeholder items Phase 0 would
// have added, and that cleaning twice is stable.
func TestIssue6732_ArrayRefWithoutTargetItemsGetsPlaceholder(t *testing.T) {
	input := `{"type":"object","properties":{"rows":{"type":"array","$ref":"#/$defs/Common"}},"$defs":{"Common":{"description":"Rows"}}}`

	first := CleanJSONSchemaForAntigravityTool(input, false)
	if gotType := gjson.Parse(first).Get("properties.rows.items.type").String(); gotType != "string" {
		t.Errorf("array ref without target items missing placeholder items, got %q: %s", gotType, first)
	}
	if second := CleanJSONSchemaForAntigravityTool(first, false); second != first {
		t.Errorf("cleaning is not idempotent for array refs:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestIssue6732_SmallTargetFanOutStaysBounded verifies that thousands of references to a small
// target with a near-limit description cannot multiply the description without bound through a
// mix of expansions and fallback copies.
func TestIssue6732_SmallTargetFanOutStaysBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{`)
	for i := range 2000 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"p` + strconv.Itoa(i) + `":{"$ref":"#/$defs/S"}`)
	}
	sb.WriteString(`},"$defs":{"S":{"type":"string","description":"` + strings.Repeat("x", 512) + `"}}}`)

	got := CleanJSONSchemaForGeminiJSONSchema(sb.String())
	if len(got) >= 512*1024 {
		t.Errorf("small-target fan-out output = %d bytes, want < %d", len(got), 512*1024)
	}
}

// TestIssue6732_EscapedDescriptionFanOutStaysBounded verifies that budget accounting covers
// HTML-escaped string growth during JSON encoding.
func TestIssue6732_EscapedDescriptionFanOutStaysBounded(t *testing.T) {
	input := `{"type":"object","properties":{"p0":{"$ref":"#/$defs/Big"},"p1":{"$ref":"#/$defs/Big"},"p2":{"$ref":"#/$defs/Big"},"p3":{"$ref":"#/$defs/Big"},"p4":{"$ref":"#/$defs/Big"},"p5":{"$ref":"#/$defs/Big"},"p6":{"$ref":"#/$defs/Big"},"p7":{"$ref":"#/$defs/Big"}},"$defs":{"Big":{"type":"object","description":"` + strings.Repeat("<", 32*1024) + `","properties":{"name":{"type":"string"}}}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	if len(got) >= 512*1024 {
		t.Errorf("escaped-description fan-out output = %d bytes, want < %d", len(got), 512*1024)
	}
}

// TestIssue6732_DependenciesNameMapKeepsSchemaSemantics verifies that the Draft-07 dependencies
// container behaves as a name map: a dependency keyed "default" holds a schema whose references
// still expand in the public inliner.
func TestIssue6732_DependenciesNameMapKeepsSchemaSemantics(t *testing.T) {
	input := `{"type":"object","dependencies":{"default":{"properties":{"x":{"$ref":"#/definitions/S"}}}},"definitions":{"S":{"type":"string"}}}`

	got := InlineLocalRefs(input)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get("dependencies.default.properties.x.type").String(); gotType != "string" {
		t.Errorf("dependency schema named \"default\" was not resolved, x type = %q: %s", gotType, got)
	}
	if parsed.Get("dependencies.default.properties.x.$ref").Exists() {
		t.Errorf("dependency schema reference was left dangling: %s", got)
	}
}

// TestIssue6732_OmittedFallbackDescriptionNotCharged verifies that a target description the
// fallback omits (because it exceeds the copy limit) is not charged to the expansion budget, so
// a following small reference still expands.
func TestIssue6732_OmittedFallbackDescriptionNotCharged(t *testing.T) {
	input := `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"},"b":{"$ref":"#/$defs/B"}},"$defs":{"A":{"type":"string","description":"` + strings.Repeat("x", 262142) + `"},"B":{"type":"object","properties":{"name":{"type":"string"}}}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	if gotType := gjson.Parse(got).Get("properties.b.properties.name.type").String(); gotType != "string" {
		t.Errorf("small reference after omitted fallback description lost its fields, name type = %q: %s", gotType, got)
	}
}

// TestIssue6732_SynthesizedTypeDoesNotOverrideRefTarget verifies that the type Phase 0 would
// infer from a ref node's sibling items does not clobber the referenced definition's object
// type, and that cleaning the result again is stable.
func TestIssue6732_SynthesizedTypeDoesNotOverrideRefTarget(t *testing.T) {
	input := `{"$ref":"#/$defs/O","items":{"type":"integer"},"$defs":{"O":{"type":"object","properties":{"x":{"type":"string"}}}}}`

	first := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(first)
	if gotType := parsed.Get("type").String(); gotType != "object" {
		t.Errorf("root ref target type = %q, want %q: %s", gotType, "object", first)
	}
	if parsed.Get("items").Exists() {
		t.Errorf("object schema should not carry items: %s", first)
	}
	if gotType := parsed.Get("properties.x.type").String(); gotType != "string" {
		t.Errorf("target property lost its type, got %q: %s", gotType, first)
	}
	if second := CleanJSONSchemaForGeminiJSONSchema(first); second != first {
		t.Errorf("cleaning is not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestIssue6732_PropertyNamedRefRemainsProperty verifies that a property literally named "$ref"
// is treated as a property, not as a reference keyword, by the cleaner.
func TestIssue6732_PropertyNamedRefRemainsProperty(t *testing.T) {
	input := `{"type":"object","properties":{"$ref":{"type":"string"}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get(`properties.\$ref.type`).String(); gotType != "string" {
		t.Errorf("property named $ref lost its schema, type = %q: %s", gotType, got)
	}
	if strings.Contains(got, "See:") {
		t.Errorf("property named $ref was converted into a hint: %s", got)
	}
}

// TestIssue6732_PropertyNamedConstRemainsProperty verifies that a property literally named
// "const" holding a schema object is not converted into an enum constraint on its parent.
func TestIssue6732_PropertyNamedConstRemainsProperty(t *testing.T) {
	input := `{"type":"object","properties":{"const":{"type":"string"}}}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)
	if gotType := parsed.Get(`properties.const.type`).String(); gotType != "string" {
		t.Errorf("property named const lost its schema, type = %q: %s", gotType, got)
	}
	if parsed.Get("properties.enum").Exists() {
		t.Errorf("property named const fabricated an enum on its parent map: %s", got)
	}
}

// geminiSchemaDiamondInput builds a schema referencing D12, where each Di is an object with two
// properties referencing Di-1 and D0 references a leaf definition twice. Fully expanding it
// produces 2^13 leaf copies.
func geminiSchemaDiamondInput() string {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{"leaf":{"$ref":"#/$defs/D12"}},"$defs":{"leaf0":{"type":"string","description":"leaf leaf leaf leaf leaf leaf leaf leaf leaf leaf"}`)
	for i := range 13 {
		target := "leaf0"
		if i > 0 {
			target = "D" + strconv.Itoa(i-1)
		}
		sb.WriteString(`,"D` + strconv.Itoa(i) + `":{"type":"object","properties":{"a":{"$ref":"#/$defs/` + target + `"},"b":{"$ref":"#/$defs/` + target + `"}}}`)
	}
	sb.WriteString(`}}`)
	return sb.String()
}

// TestIssue6732_MutuallyReferentialDefsTerminate verifies the cycle guard for a two-definition
// cycle (A references B, B references A): expansion terminates, the outer levels are expanded, the
// re-entrant reference degrades to a hint naming the definition it re-entered, and the output stays
// bounded instead of recursing.
func TestIssue6732_MutuallyReferentialDefsTerminate(t *testing.T) {
	input := `{
		"type": "object",
		"properties": { "root": { "$ref": "#/$defs/A" } },
		"$defs": {
			"A": {
				"type": "object",
				"properties": {
					"aName": { "type": "string" },
					"b": { "$ref": "#/$defs/B" }
				}
			},
			"B": {
				"type": "object",
				"properties": {
					"bName": { "type": "integer" },
					"a": { "$ref": "#/$defs/A" }
				}
			}
		}
	}`

	got := CleanJSONSchemaForGeminiJSONSchema(input)
	parsed := gjson.Parse(got)

	if gotType := parsed.Get("properties.root.properties.aName.type").String(); gotType != "string" {
		t.Errorf("outer level A was not expanded, aName type = %q: %s", gotType, got)
	}
	if gotType := parsed.Get("properties.root.properties.b.properties.bName.type").String(); gotType != "integer" {
		t.Errorf("outer level B was not expanded, bName type = %q: %s", gotType, got)
	}
	hint := parsed.Get("properties.root.properties.b.properties.a.description").String()
	if !strings.Contains(hint, "A") {
		t.Errorf("re-entrant reference should degrade to a hint naming A, description = %q: %s", hint, got)
	}
	if parsed.Get("properties.root.properties.b.properties.a.properties").Exists() {
		t.Errorf("re-entrant reference expanded instead of stopping: %s", got)
	}
	if parsed.Get("$defs").Exists() {
		t.Errorf("$defs should be removed: %s", got)
	}
	if len(got) >= 8*1024 {
		t.Errorf("mutually-referential schema output = %d bytes, want bounded: %s", len(got), got)
	}
}
