// Package util provides utility functions for the CLI Proxy API server.
package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var gjsonPathKeyReplacer = strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?")

const placeholderReasonDescription = "Brief explanation of why you are calling this tool"

// jsonSchemaCleanOptions controls which schema features cleanJSONSchema keeps
// or rewrites. Semantic port of upstream CLIProxyAPI jsonSchemaCleanOptions
// (internal/util/gemini_schema.go), reduced to the flags local callers need.
type jsonSchemaCleanOptions struct {
	// addPlaceholder adds a placeholder property to empty object schemas
	// (Claude VALIDATED mode requirement) and keeps nullable/placeholder fields.
	addPlaceholder bool
	// keepNullable preserves nullable:true fields (Antigravity semantics).
	keepNullable bool
	// flattenMultiBranchUnions forces multi-branch anyOf/oneOf unions down to a single
	// strongest branch instead of preserving them. It is used on routes whose upstream
	// rejects nested multi-branch unions even though they are valid JSON Schema.
	flattenMultiBranchUnions bool
	// preserveAllAdditionalProperties keeps additionalProperties (boolean and
	// schema-valued) instead of deleting it or degrading it to a hint
	// (upstream b532db9c).
	preserveAllAdditionalProperties bool
	// preserveStandardConstraints keeps standard JSON Schema constraint
	// keywords (pattern, minLength, maxLength, ...) instead of moving them to
	// description hints and stripping them (upstream b532db9c).
	preserveStandardConstraints bool
}

// CleanJSONSchemaForAntigravity transforms a JSON schema to be compatible with Antigravity API.
// It handles unsupported keywords, type flattening, and schema simplification while preserving
// semantic information as description hints.
func CleanJSONSchemaForAntigravity(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{addPlaceholder: true, keepNullable: true})
}

// CleanJSONSchemaForAntigravityTool transforms an Antigravity function schema.
func CleanJSONSchemaForAntigravityTool(jsonStr string, requirePlaceholder bool) string {
	return cleanJSONSchemaForAntigravityTool(jsonStr, requirePlaceholder, false)
}

// CleanJSONSchemaForAntigravityClaudeTool is the Antigravity tool cleaner for Claude
// models. It applies the same Antigravity schema cleanup, but flattens multi-branch
// anyOf/oneOf unions to a single strongest branch: the Antigravity Claude backend
// converts Gemini function declarations into Claude custom.input_schema and rejects
// nested multi-branch unions with "JSON schema is invalid", while the Gemini
// backends accept preserved unions (see Issue 3662 and its regression follow-up).
func CleanJSONSchemaForAntigravityClaudeTool(jsonStr string, requirePlaceholder bool) string {
	return cleanJSONSchemaForAntigravityTool(jsonStr, requirePlaceholder, true)
}

func cleanJSONSchemaForAntigravityTool(jsonStr string, requirePlaceholder, flattenMultiBranchUnions bool) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		addPlaceholder:           requirePlaceholder,
		keepNullable:             true,
		flattenMultiBranchUnions: flattenMultiBranchUnions,
	})
}

// CleanJSONSchemaForAntigravityResponse transforms a response schema without applying
// tool-only compatibility rewrites that would alter the client's structured output contract.
func CleanJSONSchemaForAntigravityResponse(jsonStr string) string {
	// Phase 0: normalize malformed schemas (bare property maps, boolean required,
	// boolean true subschemas). Upstream runs this phase for response schemas
	// too (c93978c4); addMissingArrayItems stays tool-only.
	jsonStr = normalizeMalformedSchemaObjects(jsonStr, false)
	// Definition containers are stripped below like on every other route, so local references
	// must be expanded first or the referenced fields are lost.
	jsonStr = inlineLocalRefsForClean(jsonStr)
	jsonStr = convertRefsToHints(jsonStr)
	jsonStr = convertConstToEnum(jsonStr)
	jsonStr = convertEnumValuesToStringsPreservingType(jsonStr)
	jsonStr = addEnumHints(jsonStr)
	jsonStr = addAdditionalPropertiesHints(jsonStr)
	jsonStr = moveConstraintsToDescription(jsonStr, jsonSchemaCleanOptions{})
	jsonStr = mergeAllOf(jsonStr)
	jsonStr = flattenTypeArrays(jsonStr)
	jsonStr = removeUnsupportedKeywords(jsonStr, jsonSchemaCleanOptions{})
	jsonStr = cleanupRequiredFields(jsonStr)
	return jsonStr
}

// CleanJSONSchemaForGemini transforms a JSON schema to be compatible with Gemini tool calling.
// It removes unsupported keywords and simplifies schemas, without adding empty-schema placeholders.
func CleanJSONSchemaForGemini(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{})
}

// CleanJSONSchemaForGeminiJSONSchema transforms a JSON schema for Gemini tool calling
// when using the parametersJsonSchema carrier. It preserves standard JSON Schema constraints
// (such as pattern, minLength, maxLength) and additionalProperties (both boolean and
// schema-valued), while removing Gemini-incompatible metadata fields and cleaning required
// properties. Ported from upstream CLIProxyAPI commit b532db9c (issue #5959).
func CleanJSONSchemaForGeminiJSONSchema(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		preserveAllAdditionalProperties: true,
		preserveStandardConstraints:     true,
	})
}

// cleanJSONSchema performs the core cleaning operations on the JSON schema.
func cleanJSONSchema(jsonStr string, options jsonSchemaCleanOptions) string {
	// Phase 0: Normalize malformed schemas (e.g. bare property maps and boolean required from MCP tools)
	jsonStr = normalizeMalformedSchemaObjects(jsonStr, true)

	// Phase 1: Convert and add hints. Every cleaner removes $defs/definitions later (see
	// removeUnsupportedKeywords), so local references must be expanded here for every route;
	// otherwise the fields of the referenced definition are lost.
	jsonStr = inlineLocalRefsForClean(jsonStr)
	jsonStr = convertRefsToHints(jsonStr)
	jsonStr = convertConstToEnum(jsonStr)
	jsonStr = convertEnumValuesToStrings(jsonStr)
	jsonStr = addEnumHints(jsonStr)
	if !options.preserveAllAdditionalProperties {
		jsonStr = addAdditionalPropertiesHints(jsonStr)
	}
	jsonStr = moveConstraintsToDescription(jsonStr, options)

	// Phase 2: Flatten complex structures
	jsonStr = mergeAllOf(jsonStr)
	jsonStr = flattenAnyOfOneOf(jsonStr, options.preserveStandardConstraints, options.flattenMultiBranchUnions)
	jsonStr = flattenTypeArrays(jsonStr)

	// Phase 3: Cleanup
	jsonStr = removeUnsupportedKeywords(jsonStr, options)
	// Remove schema metadata title values while preserving properties named "title".
	jsonStr = removeKeywords(jsonStr, []string{"title"})
	if !options.addPlaceholder && !options.keepNullable {
		// Gemini schema cleanup: remove nullable and placeholder-only fields.
		jsonStr = removeKeywords(jsonStr, []string{"nullable"})
		jsonStr = removePlaceholderFields(jsonStr)
	}
	jsonStr = cleanupRequiredFields(jsonStr)
	// Reference inlining runs after Phase 0, so array-typed nodes that only became arrays (or that
	// carried a $ref in place of items) are reconciled here: items survive only on arrays, and
	// array-typed nodes missing items get the same placeholder Phase 0 would have added.
	jsonStr = sanitizeArrayItems(jsonStr, true)
	// Phase 4: Add placeholder for empty object schemas (Claude VALIDATED mode requirement)
	if options.addPlaceholder {
		jsonStr = addEmptySchemaPlaceholder(jsonStr)
	}

	return jsonStr
}

// removeKeywords removes all occurrences of specified keywords from the JSON schema.
func removeKeywords(jsonStr string, keywords []string) string {
	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	return jsonStr
}

// removePlaceholderFields removes placeholder-only properties ("_" and "reason") and their required entries.
func removePlaceholderFields(jsonStr string) string {
	// Remove "_" placeholder properties.
	paths := findPaths(jsonStr, "_")
	sortByDepth(paths)
	for _, p := range paths {
		if !strings.HasSuffix(p, ".properties._") {
			continue
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
		parentPath := trimSuffix(p, ".properties._")
		reqPath := joinPath(parentPath, "required")
		req := gjson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "_" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	// Remove placeholder-only "reason" objects.
	reasonPaths := findPaths(jsonStr, "reason")
	sortByDepth(reasonPaths)
	for _, p := range reasonPaths {
		if !strings.HasSuffix(p, ".properties.reason") {
			continue
		}
		parentPath := trimSuffix(p, ".properties.reason")
		props := gjson.Get(jsonStr, joinPath(parentPath, "properties"))
		if !props.IsObject() || len(props.Map()) != 1 {
			continue
		}
		desc := gjson.Get(jsonStr, p+".description").String()
		if desc != placeholderReasonDescription {
			continue
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
		reqPath := joinPath(parentPath, "required")
		req := gjson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "reason" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	return jsonStr
}

// normalizeMalformedSchemaObjects normalizes malformed JSON schema nodes commonly produced by
// certain MCP tool definitions (e.g. Asana MCP server):
// 1. Bare property maps missing the "type": "object" and "properties": {...} wrappers are wrapped.
// 2. Boolean "required": true on property definitions are stripped and promoted to the parent's "required" array.
// 3. Tool array schemas missing "items" receive a string item schema required by Gemini and Antigravity.
func normalizeMalformedSchemaObjects(jsonStr string, addMissingArrayItems bool) string {
	if jsonStr == "" {
		return jsonStr
	}

	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}

	// Boolean true subschema normalizes to the empty object schema {}
	// (upstream c93978c4, issue #3551).
	if rootBool, ok := root.(bool); ok && rootBool {
		return "{}"
	}

	rootMap, ok := root.(map[string]any)
	if !ok || isAPIRequestDocument(rootMap) {
		return jsonStr
	}

	// If wrapped in single-key {"schema": ...} by cleanNestedSchema, unwrap, repair, and re-wrap.
	if len(rootMap) == 1 {
		if innerSchema, ok := rootMap["schema"].(map[string]any); ok {
			repairedInner, modified := repairSchemaNode(innerSchema, addMissingArrayItems)
			if !modified {
				return jsonStr
			}
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": repairedInner})
			if err != nil {
				return jsonStr
			}
			return string(out)
		} else if innerBool, ok := rootMap["schema"].(bool); ok && innerBool {
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": map[string]any{}})
			if err != nil {
				return jsonStr
			}
			return string(out)
		}
	}

	repaired, modified := repairSchemaNode(rootMap, addMissingArrayItems)
	if !modified {
		return jsonStr
	}

	out, err := marshalJSONNoHTMLEscape(repaired)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func marshalJSONNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func isKnownSchemaKeywordOrExtension(key string) bool {
	if strings.HasPrefix(key, "x-") {
		return true
	}
	switch key {
	case "properties", "patternProperties", "additionalProperties", "items", "prefixItems",
		"$defs", "definitions", "dependentSchemas", "dependentRequired", "dependencies",
		"if", "then", "else", "not", "contains", "propertyNames",
		"unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems",
		"default", "const", "example", "examples", "discriminator", "xml", "externalDocs",
		"enumDescriptions", "enumTitles":
		return true
	}
	return false
}

func isNonObjectDeclaredType(t any) bool {
	if s, ok := t.(string); ok {
		return s != "" && !strings.EqualFold(s, "object")
	}
	if arr, ok := t.([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok && strings.EqualFold(s, "object") {
				return false
			}
		}
		return len(arr) > 0
	}
	return false
}

func isArrayDeclaredType(t any) bool {
	switch typeValue := t.(type) {
	case string:
		return strings.EqualFold(typeValue, "array")
	case []any:
		for _, item := range typeValue {
			if itemType, ok := item.(string); ok && strings.EqualFold(itemType, "array") {
				return true
			}
		}
	}
	return false
}

func isAPIRequestDocument(m map[string]any) bool {
	if _, ok := m["tools"].([]any); ok {
		return true
	}
	if _, ok := m["contents"].([]any); ok {
		return true
	}
	if _, ok := m["messages"].([]any); ok {
		return true
	}
	if _, ok := m["functionDeclarations"].([]any); ok {
		return true
	}
	if _, ok := m["function_declarations"].([]any); ok {
		return true
	}
	if reqMap, ok := m["request"].(map[string]any); ok {
		if isAPIRequestDocument(reqMap) {
			return true
		}
	}
	return false
}

func repairSchemaNode(node map[string]any, addMissingArrayItems bool) (map[string]any, bool) {
	if node == nil {
		return nil, false
	}

	modified := false
	clone := make(map[string]any, len(node))
	for k, v := range node {
		clone[k] = v
	}

	// 1. If not declared as a primitive/array type, collect bare property definition maps
	if !isNonObjectDeclaredType(clone["type"]) {
		var bareProps map[string]any
		for k, v := range clone {
			if childMap, isMap := v.(map[string]any); isMap {
				if !isKnownSchemaKeywordOrExtension(k) {
					if bareProps == nil {
						bareProps = make(map[string]any)
					}
					bareProps[k] = childMap
				}
			}
		}

		if len(bareProps) > 0 {
			repairedProps, promotedReqs, _ := repairPropertyMap(bareProps, addMissingArrayItems)
			for k := range bareProps {
				delete(clone, k)
			}

			if existingProps, ok := clone["properties"].(map[string]any); ok {
				newProps := make(map[string]any, len(existingProps)+len(repairedProps))
				for k, v := range existingProps {
					newProps[k] = v
				}
				for k, v := range repairedProps {
					newProps[k] = v
				}
				clone["properties"] = newProps
			} else {
				clone["properties"] = repairedProps
				if _, hasType := clone["type"]; !hasType {
					clone["type"] = "object"
				}
			}

			if len(promotedReqs) > 0 {
				existingReqs := extractStringArray(clone["required"])
				merged := mergeStringSlices(existingReqs, promotedReqs)
				clone["required"] = merged
			}
			modified = true
		}
	}

	// 2. If node has a "properties" map, recursively repair all properties inside it
	if propsVal, ok := clone["properties"].(map[string]any); ok {
		repairedProps, promotedReqs, propsMod := repairPropertyMap(propsVal, addMissingArrayItems)
		if propsMod {
			clone["properties"] = repairedProps
			modified = true
		}
		if len(promotedReqs) > 0 {
			existingReqs := extractStringArray(clone["required"])
			merged := mergeStringSlices(existingReqs, promotedReqs)
			clone["required"] = merged
			modified = true
		}
	}

	// Gemini and Antigravity reject tool array schemas without an items definition. A node still
	// holding an unresolved $ref gets none: inlining runs after this phase, where the placeholder
	// would become a sibling keyword of $ref and clobber the referenced definition's real items.
	if addMissingArrayItems && clone["$ref"] == nil && isArrayDeclaredType(clone["type"]) {
		if _, hasItems := clone["items"]; !hasItems {
			clone["items"] = map[string]any{"type": "string"}
			modified = true
		}
	}

	// 3. Recurse into all other standard schema containers
	if itemsVal, ok := clone["items"].(map[string]any); ok {
		repairedItems, itemsMod := repairSchemaNode(itemsVal, addMissingArrayItems)
		if itemsMod {
			clone["items"] = repairedItems
			modified = true
		}
	} else if itemsList, ok := clone["items"].([]any); ok {
		repairedList, listMod := repairSchemaList(itemsList, addMissingArrayItems)
		if listMod {
			clone["items"] = repairedList
			modified = true
		}
	} else if itemsBool, ok := clone["items"].(bool); ok && itemsBool {
		clone["items"] = map[string]any{}
		modified = true
	}

	if addProps, ok := clone["additionalProperties"].(map[string]any); ok {
		repairedAddProps, addPropsMod := repairSchemaNode(addProps, addMissingArrayItems)
		if addPropsMod {
			clone["additionalProperties"] = repairedAddProps
			modified = true
		}
	}

	if patProps, ok := clone["patternProperties"].(map[string]any); ok {
		repairedPatProps, _, patMod := repairPropertyMap(patProps, addMissingArrayItems)
		if patMod {
			clone["patternProperties"] = repairedPatProps
			modified = true
		}
	}

	for _, key := range []string{"if", "then", "else", "not", "contains", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems"} {
		if subVal, ok := clone[key].(map[string]any); ok {
			repairedSub, subMod := repairSchemaNode(subVal, addMissingArrayItems)
			if subMod {
				clone[key] = repairedSub
				modified = true
			}
		} else if subBool, ok := clone[key].(bool); ok && subBool {
			clone[key] = map[string]any{}
			modified = true
		}
	}

	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if listVal, ok := clone[key].([]any); ok {
			repairedList, listMod := repairSchemaList(listVal, addMissingArrayItems)
			if listMod {
				clone[key] = repairedList
				modified = true
			}
		}
	}

	for _, key := range []string{"$defs", "definitions", "dependentSchemas", "dependencies"} {
		if defsVal, ok := clone[key].(map[string]any); ok {
			repairedDefs := make(map[string]any, len(defsVal))
			defsModified := false
			for dk, dv := range defsVal {
				if defMap, ok := dv.(map[string]any); ok {
					repairedDef, defMod := repairSchemaNode(defMap, addMissingArrayItems)
					repairedDefs[dk] = repairedDef
					if defMod {
						defsModified = true
						modified = true
					}
				} else if defBool, ok := dv.(bool); ok && defBool {
					repairedDefs[dk] = map[string]any{}
					defsModified = true
					modified = true
				} else {
					repairedDefs[dk] = dv
				}
			}
			if defsModified {
				clone[key] = repairedDefs
			}
		}
	}

	return clone, modified
}

func repairSchemaList(list []any, addMissingArrayItems bool) ([]any, bool) {
	var repairedList []any
	listModified := false
	for _, item := range list {
		if itemMap, ok := item.(map[string]any); ok {
			repairedItem, itemMod := repairSchemaNode(itemMap, addMissingArrayItems)
			repairedList = append(repairedList, repairedItem)
			if itemMod {
				listModified = true
			}
		} else if itemBool, ok := item.(bool); ok && itemBool {
			repairedList = append(repairedList, map[string]any{})
			listModified = true
		} else {
			repairedList = append(repairedList, item)
		}
	}
	return repairedList, listModified
}

func repairPropertyMap(props map[string]any, addMissingArrayItems bool) (map[string]any, []string, bool) {
	out := make(map[string]any, len(props))
	var promotedReqs []string
	modified := false

	for k, v := range props {
		if b, ok := v.(bool); ok && b {
			out[k] = map[string]any{}
			modified = true
			continue
		}

		childMap, isMap := v.(map[string]any)
		if !isMap {
			out[k] = v
			continue
		}

		childClone := make(map[string]any, len(childMap))
		for ck, cv := range childMap {
			childClone[ck] = cv
		}

		if reqBool, isBool := childClone["required"].(bool); isBool {
			delete(childClone, "required")
			modified = true
			if reqBool {
				promotedReqs = append(promotedReqs, k)
			}
		}

		repairedChild, childMod := repairSchemaNode(childClone, addMissingArrayItems)
		if childMod {
			modified = true
		}
		out[k] = repairedChild
	}

	sort.Strings(promotedReqs)
	return out, promotedReqs, modified
}

func extractStringArray(val any) []string {
	if val == nil {
		return nil
	}
	arr, ok := val.([]any)
	if !ok {
		if strArr, ok := val.([]string); ok {
			return strArr
		}
		return nil
	}
	var res []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			res = append(res, s)
		}
	}
	return res
}

func mergeStringSlices(existing, promoted []string) []string {
	seen := make(map[string]bool)
	var res []string
	for _, s := range existing {
		if !seen[s] && s != "" {
			seen[s] = true
			res = append(res, s)
		}
	}
	for _, s := range promoted {
		if !seen[s] && s != "" {
			seen[s] = true
			res = append(res, s)
		}
	}
	return res
}

// convertRefsToHints converts $ref to description hints (Lazy Hint strategy). Local references
// were already expanded by the inlining pre-pass; what reaches here is an unresolvable or
// external reference, which keeps the hint shape callers already rely on.
func convertRefsToHints(jsonStr string) string {
	paths := findPaths(jsonStr, "$ref")
	sortByDepth(paths)

	for _, p := range paths {
		parentPath := trimSuffix(p, ".$ref")
		if isPropertyDefinition(parentPath) {
			continue // a property literally named "$ref", not a reference keyword
		}
		refVal := gjson.Get(jsonStr, p).String()
		defName := refVal
		if idx := strings.LastIndex(refVal, "/"); idx >= 0 {
			defName = refVal[idx+1:]
		}

		hint := fmt.Sprintf("See: %s", defName)
		if existing := gjson.Get(jsonStr, descriptionPath(parentPath)).String(); existing != "" {
			hint = fmt.Sprintf("%s (%s)", existing, hint)
		}

		replacement := `{"type":"object","description":""}`
		replacementBytes, _ := sjson.SetBytes([]byte(replacement), "description", hint)
		replacement = string(replacementBytes)
		jsonStr = setRawAt(jsonStr, parentPath, replacement)
	}
	return jsonStr
}

func convertConstToEnum(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "const") {
		if isPropertyDefinition(trimSuffix(p, ".const")) {
			continue // a property literally named "const", not a const keyword
		}
		val := gjson.Get(jsonStr, p)
		if !val.Exists() {
			continue
		}
		enumPath := trimSuffix(p, ".const") + ".enum"
		if !gjson.Get(jsonStr, enumPath).Exists() {
			updated, _ := sjson.SetBytes([]byte(jsonStr), enumPath, []interface{}{val.Value()})
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

// convertEnumValuesToStrings ensures all enum values are strings and the schema type is set to string.
// Gemini API requires enum values to be of type string, not numbers or booleans.
func convertEnumValuesToStrings(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := gjson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}

		var stringVals []string
		for _, item := range arr.Array() {
			stringVals = append(stringVals, item.String())
		}

		// Always update enum values to strings and set type to "string"
		// This ensures compatibility with Antigravity Gemini which only allows enum for STRING type
		updated, _ := sjson.SetBytes([]byte(jsonStr), p, stringVals)
		jsonStr = string(updated)
		parentPath := trimSuffix(p, ".enum")
		updated, _ = sjson.SetBytes([]byte(jsonStr), joinPath(parentPath, "type"), "string")
		jsonStr = string(updated)
	}
	return jsonStr
}

func convertEnumValuesToStringsPreservingType(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := gjson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}

		var stringVals []string
		for _, item := range arr.Array() {
			stringVals = append(stringVals, item.String())
		}

		updated, _ := sjson.SetBytes([]byte(jsonStr), p, stringVals)
		jsonStr = string(updated)
	}
	return jsonStr
}

func addEnumHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := gjson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}
		items := arr.Array()
		if len(items) <= 1 || len(items) > 10 {
			continue
		}

		var vals []string
		for _, item := range items {
			vals = append(vals, item.String())
		}
		jsonStr = appendHint(jsonStr, trimSuffix(p, ".enum"), "Allowed: "+strings.Join(vals, ", "))
	}
	return jsonStr
}

func addAdditionalPropertiesHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "additionalProperties") {
		if gjson.Get(jsonStr, p).Type == gjson.False {
			jsonStr = appendHint(jsonStr, trimSuffix(p, ".additionalProperties"), "No extra properties allowed")
		}
	}
	return jsonStr
}

var unsupportedConstraints = []string{
	"minLength", "maxLength", "exclusiveMinimum", "exclusiveMaximum",
	"pattern", "minItems", "maxItems", "uniqueItems", "contains", "format",
	"default", "examples", // Claude rejects these in VALIDATED mode
}

// constraintKeywords returns the constraint keywords subject to description
// hints and stripping. When preserveStandardConstraints is set the standard
// JSON Schema constraints are kept untouched (upstream b532db9c).
func constraintKeywords(options jsonSchemaCleanOptions) []string {
	if options.preserveStandardConstraints {
		return nil
	}
	return append([]string(nil), unsupportedConstraints...)
}

func moveConstraintsToDescription(jsonStr string, options jsonSchemaCleanOptions) string {
	constraints := constraintKeywords(options)
	if len(constraints) == 0 {
		return jsonStr
	}
	pathsByField := findPathsByFields(jsonStr, constraints)
	for _, key := range constraints {
		for _, p := range pathsByField[key] {
			val := gjson.Get(jsonStr, p)
			if !val.Exists() {
				continue
			}
			parentPath := trimSuffix(p, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			if val.IsObject() || val.IsArray() {
				jsonStr = appendHint(jsonStr, parentPath, fmt.Sprintf("%s: %s", key, val.Raw))
				continue
			}
			jsonStr = appendHint(jsonStr, parentPath, fmt.Sprintf("%s: %s", key, val.String()))
		}
	}
	return jsonStr
}

func mergeAllOf(jsonStr string) string {
	paths := findPaths(jsonStr, "allOf")
	sortByDepth(paths)

	for _, p := range paths {
		allOf := gjson.Get(jsonStr, p)
		if !allOf.IsArray() {
			continue
		}
		parentPath := trimSuffix(p, ".allOf")

		for _, item := range allOf.Array() {
			if props := item.Get("properties"); props.IsObject() {
				props.ForEach(func(key, value gjson.Result) bool {
					destPath := joinPath(parentPath, "properties."+escapeGJSONPathKey(key.String()))
					updated, _ := sjson.SetRawBytes([]byte(jsonStr), destPath, []byte(value.Raw))
					jsonStr = string(updated)
					return true
				})
			}
			if req := item.Get("required"); req.IsArray() {
				reqPath := joinPath(parentPath, "required")
				current := getStrings(jsonStr, reqPath)
				for _, r := range req.Array() {
					if s := r.String(); !contains(current, s) {
						current = append(current, s)
					}
				}
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, current)
				jsonStr = string(updated)
			}
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	return jsonStr
}

func mergeMissingSchemaAtPath(jsonStr, destination string, incoming gjson.Result) string {
	existing := gjson.Get(jsonStr, destination)
	if !existing.Exists() {
		updated, _ := sjson.SetRawBytes([]byte(jsonStr), destination, []byte(incoming.Raw))
		return string(updated)
	}
	if !existing.IsObject() || !incoming.IsObject() {
		return jsonStr
	}
	incoming.ForEach(func(key, value gjson.Result) bool {
		child := joinPath(destination, escapeGJSONPathKey(key.String()))
		jsonStr = mergeMissingSchemaAtPath(jsonStr, child, value)
		return true
	})
	return jsonStr
}

func isRequiredOnlyUnionBranch(item gjson.Result) bool {
	if !item.IsObject() || !item.Get("required").Exists() {
		return false
	}
	for _, key := range []string{"type", "properties", "items", "enum", "anyOf", "oneOf", "allOf"} {
		if item.Get(key).Exists() {
			return false
		}
	}
	return true
}

func materializeRequiredOnlyBranch(item, parentProps gjson.Result) (gjson.Result, bool) {
	if !parentProps.IsObject() {
		return gjson.Result{}, false
	}
	reqs := getStrings(item.Raw, "required")
	if len(reqs) == 0 {
		return gjson.Result{}, false
	}
	raw := item.Raw
	for _, reqName := range reqs {
		escaped := escapeGJSONPathKey(reqName)
		propVal := parentProps.Get(escaped)
		if !propVal.Exists() {
			return gjson.Result{}, false
		}
		updated, errSet := sjson.SetRawBytes([]byte(raw), "properties."+escaped, []byte(propVal.Raw))
		if errSet == nil {
			raw = string(updated)
		}
	}
	if updated, errSet := sjson.SetBytes([]byte(raw), "type", "object"); errSet == nil {
		raw = string(updated)
	}
	return gjson.Parse(raw), true
}

func isNullOnlyBranch(item gjson.Result) bool {
	if item.Get("properties").Exists() || item.Get("items").Exists() {
		return false
	}
	t := item.Get("type")
	if t.Type == gjson.String {
		return t.String() == "null"
	}
	if t.IsArray() && len(t.Array()) > 0 {
		for _, elem := range t.Array() {
			if elem.String() != "null" {
				return false
			}
		}
		return true
	}
	return false
}

// partitionUnionBranches splits union branches into standalone non-null branches,
// reporting whether null-only branches and other filtered branches were present.
// Required-only branches are materialized into typed object branches using the
// parent object's properties when possible.
func partitionUnionBranches(items []gjson.Result, parentProps gjson.Result) (nonNullItems []gjson.Result, hasNull, hasFilteredBranch bool) {
	for _, item := range items {
		if !item.IsObject() {
			hasFilteredBranch = true
			continue
		}
		if isNullOnlyBranch(item) {
			hasNull = true
			continue
		}
		if isRequiredOnlyUnionBranch(item) {
			if materialized, ok := materializeRequiredOnlyBranch(item, parentProps); ok {
				nonNullItems = append(nonNullItems, materialized)
				hasFilteredBranch = true
				continue
			}
			hasFilteredBranch = true
			continue
		}
		nonNullItems = append(nonNullItems, item)
	}
	return
}

// preserveUnionBranches retains multi-branch unions instead of flattening them
// into a single branch, so non-first branches are not lost. Null-only branches
// are projected to nullable:true with a description hint. On Gemini/Antigravity
// schema carriers oneOf is normalized to anyOf. It reports whether the union
// was preserved (caller must skip legacy flattening) and the updated document.
func preserveUnionBranches(jsonStr, parentPath, key, unionPath string, items []gjson.Result, parentProps gjson.Result) (string, bool) {
	nonNullItems, hasNull, hasFilteredBranch := partitionUnionBranches(items, parentProps)
	if len(nonNullItems) < 2 {
		return jsonStr, false
	}
	targetKey := key
	if key == "oneOf" {
		targetKey = "anyOf"
	}
	targetPath := joinPath(parentPath, targetKey)

	if targetKey != key || hasNull || hasFilteredBranch {
		rawBranches := make([]json.RawMessage, 0, len(nonNullItems))
		for _, item := range nonNullItems {
			rawBranches = append(rawBranches, json.RawMessage(item.Raw))
		}
		updated, errSet := sjson.SetBytes([]byte(jsonStr), targetPath, rawBranches)
		if errSet == nil {
			jsonStr = string(updated)
		}
		if targetKey != key {
			jsonStr, _ = sjson.Delete(jsonStr, unionPath)
		}
	}
	if hasNull {
		updated, errSet := sjson.SetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
		if errSet == nil {
			jsonStr = string(updated)
		}
		jsonStr = appendHint(jsonStr, parentPath, "(nullable)")
	}
	return jsonStr, true
}

func flattenAnyOfOneOf(jsonStr string, preserveStandardConstraints, flattenMultiBranch bool) string {
	for _, key := range []string{"anyOf", "oneOf"} {
		paths := findPaths(jsonStr, key)
		sortByDepth(paths)

		for _, p := range paths {
			arr := gjson.Get(jsonStr, p)
			if !arr.IsArray() || len(arr.Array()) == 0 {
				continue
			}

			parentPath := trimSuffix(p, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			parent := gjson.Get(jsonStr, parentPath)
			if parentPath == "" {
				parent = gjson.Parse(jsonStr)
			}

			if key == "oneOf" && !flattenMultiBranch && !preserveStandardConstraints && parent.Get("anyOf").IsArray() {
				jsonStr = appendHint(jsonStr, parentPath, "oneOf: "+arr.Raw)
				jsonStr, _ = sjson.Delete(jsonStr, p)
				continue
			}

			items := arr.Array()
			parentProps := parent.Get("properties")
			nonNullItems, hasNull, hasFilteredBranch := partitionUnionBranches(items, parentProps)
			nullable := hasNull || parent.Get("nullable").Bool()

			// Preserve multi-branch unions (2+ standalone non-null branches).
			if len(nonNullItems) >= 2 && !flattenMultiBranch {
				if next, preserved := preserveUnionBranches(jsonStr, parentPath, key, p, items, parentProps); preserved {
					jsonStr = next
					continue
				}
			}

			if parentProps.IsObject() {
				for _, item := range items {
					if branchProps := item.Get("properties"); branchProps.IsObject() {
						branchProps.ForEach(func(propKey, propVal gjson.Result) bool {
							destPath := joinPath(parentPath, "properties."+escapeGJSONPathKey(propKey.String()))
							jsonStr = mergeMissingSchemaAtPath(jsonStr, destPath, propVal)
							return true
						})
					}
				}
				if nullable {
					updated, _ := sjson.SetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
					jsonStr = string(updated)
				}
				jsonStr, _ = sjson.Delete(jsonStr, p)
				continue
			}

			candidateItems := items
			if hasFilteredBranch || len(nonNullItems) >= 2 {
				candidateItems = nonNullItems
				if hasNull {
					candidateItems = append(append([]gjson.Result(nil), nonNullItems...), gjson.Parse(`{"type":"null"}`))
				}
			}

			parentDesc := gjson.Get(jsonStr, descriptionPath(parentPath)).String()
			_, allTypes := selectBest(candidateItems)
			selected := items[0].Raw
			if len(nonNullItems) > 0 {
				selected = nonNullItems[0].Raw
				if bestIdx, _ := selectBest(nonNullItems); bestIdx >= 0 && bestIdx < len(nonNullItems) {
					selected = nonNullItems[bestIdx].Raw
				}
			} else if bestIdx, _ := selectBest(items); bestIdx >= 0 && bestIdx < len(items) {
				selected = items[bestIdx].Raw
			}

			if nullable {
				updated, _ := sjson.SetBytes([]byte(selected), "nullable", true)
				selected = string(updated)
			}

			if parentDesc != "" {
				selected = mergeDescriptionRaw(selected, parentDesc)
			}

			if len(allTypes) > 1 {
				hint := "Accepts: " + strings.Join(allTypes, " | ")
				selected = appendHintRaw(selected, hint)
			} else if nullable {
				selected = appendHintRaw(selected, "(nullable)")
			}

			jsonStr = setRawAt(jsonStr, parentPath, selected)
		}
	}
	return jsonStr
}

func selectBest(items []gjson.Result) (bestIdx int, types []string) {
	bestScore := -1
	for i, item := range items {
		t := item.Get("type").String()
		score := 0

		switch {
		case t == "object" || item.Get("properties").Exists():
			score, t = 3, orDefault(t, "object")
		case t == "array" || item.Get("items").Exists():
			score, t = 2, orDefault(t, "array")
		case t != "" && t != "null":
			score = 1
		case t == "null":
			score, t = 0, "null"
		default:
			score, t = 0, ""
		}

		if t != "" {
			types = append(types, t)
		}
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	return
}

func flattenTypeArrays(jsonStr string) string {
	paths := findPaths(jsonStr, "type")
	sortByDepth(paths)

	nullableFields := make(map[string][]string)

	for _, p := range paths {
		res := gjson.Get(jsonStr, p)
		if !res.IsArray() || len(res.Array()) == 0 {
			continue
		}

		hasNull := false
		var nonNullTypes []string
		for _, item := range res.Array() {
			s := item.String()
			if s == "null" {
				hasNull = true
			} else if s != "" {
				nonNullTypes = append(nonNullTypes, s)
			}
		}

		firstType := "string"
		if len(nonNullTypes) > 0 {
			firstType = nonNullTypes[0]
		}

		updated, _ := sjson.SetBytes([]byte(jsonStr), p, firstType)
		jsonStr = string(updated)

		parentPath := trimSuffix(p, ".type")
		if len(nonNullTypes) > 1 {
			hint := "Accepts: " + strings.Join(nonNullTypes, " | ")
			jsonStr = appendHint(jsonStr, parentPath, hint)
		}

		if hasNull {
			parts := splitGJSONPath(p)
			if len(parts) >= 3 && parts[len(parts)-3] == "properties" {
				fieldNameEscaped := parts[len(parts)-2]
				fieldName := unescapeGJSONPathKey(fieldNameEscaped)
				objectPath := strings.Join(parts[:len(parts)-3], ".")
				nullableFields[objectPath] = append(nullableFields[objectPath], fieldName)

				propPath := joinPath(objectPath, "properties."+fieldNameEscaped)
				jsonStr = appendHint(jsonStr, propPath, "(nullable)")
			}
		}
	}

	for objectPath, fields := range nullableFields {
		reqPath := joinPath(objectPath, "required")
		req := gjson.Get(jsonStr, reqPath)
		if !req.IsArray() {
			continue
		}

		var filtered []string
		for _, r := range req.Array() {
			if !contains(fields, r.String()) {
				filtered = append(filtered, r.String())
			}
		}

		if len(filtered) == 0 {
			jsonStr, _ = sjson.Delete(jsonStr, reqPath)
		} else {
			updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, filtered)
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

func removeUnsupportedKeywords(jsonStr string, options jsonSchemaCleanOptions) string {
	keywords := append(constraintKeywords(options),
		"$schema", "$defs", "definitions", "const", "$ref", "$id", "id", "additionalProperties",
		"$anchor", "$vocabulary", "$dynamicRef", "$dynamicAnchor",
		"$comment",
		"propertyNames", "patternProperties", // Gemini doesn't support these schema keywords
		"enumTitles", "prefill", "deprecated", "encrypted", "discriminator", "readOnly", "writeOnly", // Schema metadata fields unsupported by Gemini
		"additionalItems", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "contentMediaType", "contentEncoding", // upstream c93978c4, e9ba5c507f46, a43026d75196
	)

	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			if key == "additionalProperties" && options.preserveAllAdditionalProperties {
				continue
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	// Remove x-* extension fields (e.g., x-google-enum-descriptions) that are not supported by Gemini API
	jsonStr = removeExtensionFields(jsonStr)
	return jsonStr
}

// removeExtensionFields removes all x-* extension fields from the JSON schema.
// These are OpenAPI/JSON Schema extension fields that Google APIs don't recognize.
func removeExtensionFields(jsonStr string) string {
	var paths []string
	walkForExtensions(gjson.Parse(jsonStr), "", &paths)
	// walkForExtensions returns paths in a way that deeper paths are added before their ancestors
	// when they are not deleted wholesale, but since we skip children of deleted x-* nodes,
	// any collected path is safe to delete. We still use DeleteBytes for efficiency.

	b := []byte(jsonStr)
	for _, p := range paths {
		b, _ = sjson.DeleteBytes(b, p)
	}
	return string(b)
}

func walkForExtensions(value gjson.Result, path string, paths *[]string) {
	if value.IsArray() {
		arr := value.Array()
		for i := len(arr) - 1; i >= 0; i-- {
			walkForExtensions(arr[i], joinPath(path, strconv.Itoa(i)), paths)
		}
		return
	}

	if value.IsObject() {
		value.ForEach(func(key, val gjson.Result) bool {
			keyStr := key.String()
			safeKey := escapeGJSONPathKey(keyStr)
			childPath := joinPath(path, safeKey)

			// If it's an extension field, we delete it and don't need to look at its children.
			if strings.HasPrefix(keyStr, "x-") && !isPropertyDefinition(path) {
				*paths = append(*paths, childPath)
				return true
			}

			// Literal values are client data; x-* keys inside them are not schema extensions.
			if isLiteralValueKeyword(keyStr) && !pathEndsWithSchemaNameMapKeyword(path) {
				return true
			}

			walkForExtensions(val, childPath, paths)
			return true
		})
	}
}

// sanitizeArrayItems keeps "items" consistent with the node's declared type: Gemini and
// Antigravity reject items on a non-array field, and phantom an array when the type is absent.
// With addMissing set, array-typed nodes still lacking items after reference inlining (a $ref
// node cannot receive the Phase 0 placeholder, see repairSchemaNode) get that placeholder here.
func sanitizeArrayItems(jsonStr string, addMissing bool) string {
	paths := findPaths(jsonStr, "items")
	sortByDepth(paths)
	for _, p := range paths {
		parentPath := trimSuffix(p, ".items")
		if isPropertyDefinition(parentPath) {
			continue
		}
		typePath := joinPath(parentPath, "type")
		if t := gjson.Get(jsonStr, typePath).String(); t == "" {
			updated, _ := sjson.SetBytes([]byte(jsonStr), typePath, "array")
			jsonStr = string(updated)
		} else if !strings.EqualFold(t, "array") {
			jsonStr, _ = sjson.Delete(jsonStr, p)
		}
	}

	if !addMissing {
		return jsonStr
	}
	typePaths := findPaths(jsonStr, "type")
	sortByDepth(typePaths)
	for _, p := range typePaths {
		parentPath := trimSuffix(p, ".type")
		if isPropertyDefinition(parentPath) || !isArrayDeclaredType(gjson.Get(jsonStr, p).Value()) {
			continue
		}
		if gjson.Get(jsonStr, joinPath(parentPath, "items")).Exists() || gjson.Get(jsonStr, joinPath(parentPath, "$ref")).Exists() {
			continue
		}
		updated, _ := sjson.SetRawBytes([]byte(jsonStr), joinPath(parentPath, "items"), []byte(`{"type":"string"}`))
		jsonStr = string(updated)
	}
	return jsonStr
}

func cleanupRequiredFields(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "required") {
		parentPath := trimSuffix(p, ".required")
		propsPath := joinPath(parentPath, "properties")

		req := gjson.Get(jsonStr, p)
		props := gjson.Get(jsonStr, propsPath)
		if !req.IsArray() {
			continue
		}
		if !props.IsObject() {
			jsonStr, _ = sjson.Delete(jsonStr, p)
			continue
		}

		var valid []string
		for _, r := range req.Array() {
			key := r.String()
			if props.Get(escapeGJSONPathKey(key)).Exists() {
				valid = append(valid, key)
			}
		}

		if len(valid) != len(req.Array()) {
			if len(valid) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, p)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), p, valid)
				jsonStr = string(updated)
			}
		}
	}
	return jsonStr
}

// addEmptySchemaPlaceholder adds a placeholder "reason" property to empty object schemas.
// Claude VALIDATED mode requires at least one required property in tool schemas.
func addEmptySchemaPlaceholder(jsonStr string) string {
	// Find all "type" fields
	paths := findPaths(jsonStr, "type")

	// Process from deepest to shallowest (to handle nested objects properly)
	sortByDepth(paths)

	for _, p := range paths {
		typeVal := gjson.Get(jsonStr, p)
		if typeVal.String() != "object" {
			continue
		}

		// Get the parent path (the object containing "type")
		parentPath := trimSuffix(p, ".type")

		// Check if properties exists and is empty or missing
		propsPath := joinPath(parentPath, "properties")
		propsVal := gjson.Get(jsonStr, propsPath)
		reqPath := joinPath(parentPath, "required")
		reqVal := gjson.Get(jsonStr, reqPath)
		hasRequiredProperties := reqVal.IsArray() && len(reqVal.Array()) > 0

		needsPlaceholder := false
		if !propsVal.Exists() {
			// No properties field at all
			needsPlaceholder = true
		} else if propsVal.IsObject() && len(propsVal.Map()) == 0 {
			// Empty properties object
			needsPlaceholder = true
		}

		if needsPlaceholder {
			// Add placeholder "reason" property
			reasonPath := joinPath(propsPath, "reason")
			updated, _ := sjson.SetBytes([]byte(jsonStr), reasonPath+".type", "string")
			jsonStr = string(updated)
			updated, _ = sjson.SetBytes([]byte(jsonStr), reasonPath+".description", placeholderReasonDescription)
			jsonStr = string(updated)

			// Add to required array
			updated, _ = sjson.SetBytes([]byte(jsonStr), reqPath, []string{"reason"})
			jsonStr = string(updated)
			continue
		}

		// If schema has properties but none are required, add a minimal placeholder.
		if propsVal.IsObject() && !hasRequiredProperties {
			// DO NOT add placeholder if it's a top-level schema (parentPath is empty)
			// or if we've already added a placeholder reason above.
			if parentPath == "" {
				continue
			}
			placeholderPath := joinPath(propsPath, "_")
			if !gjson.Get(jsonStr, placeholderPath).Exists() {
				updated, _ := sjson.SetBytes([]byte(jsonStr), placeholderPath+".type", "boolean")
				jsonStr = string(updated)
			}
			updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, []string{"_"})
			jsonStr = string(updated)
		}
	}

	return jsonStr
}

// --- Helpers ---

func findPaths(jsonStr, field string) []string {
	var paths []string
	Walk(gjson.Parse(jsonStr), "", field, &paths)
	// Literal-value keywords carry client data, not subschemas: a key inside them that is spelled
	// like a schema keyword is data and must survive cleaning untouched. Walk cannot know that, so
	// the literal context is filtered here (see pathInsideLiteralValue).
	kept := paths[:0]
	for _, path := range paths {
		if !pathInsideLiteralValue(path) {
			kept = append(kept, path)
		}
	}
	return kept
}

func findPathsByFields(jsonStr string, fields []string) map[string][]string {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	paths := make(map[string][]string, len(set))
	walkForFields(gjson.Parse(jsonStr), "", set, paths)
	return paths
}

func walkForFields(value gjson.Result, path string, fields map[string]struct{}, paths map[string][]string) {
	switch value.Type {
	case gjson.JSON:
		value.ForEach(func(key, val gjson.Result) bool {
			keyStr := key.String()
			safeKey := escapeGJSONPathKey(keyStr)

			var childPath string
			if path == "" {
				childPath = safeKey
			} else {
				childPath = path + "." + safeKey
			}

			if _, ok := fields[keyStr]; ok {
				paths[keyStr] = append(paths[keyStr], childPath)
			}

			// Literal values are client data; schema-looking keys inside them are not keywords.
			// Under a name map the key is a property or definition name instead, so descend there.
			if isLiteralValueKeyword(keyStr) && !pathEndsWithSchemaNameMapKeyword(path) {
				return true
			}

			walkForFields(val, childPath, fields, paths)
			return true
		})
	case gjson.String, gjson.Number, gjson.True, gjson.False, gjson.Null:
		// Terminal types - no further traversal needed
	}
}

func sortByDepth(paths []string) {
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
}

func trimSuffix(path, suffix string) string {
	if path == strings.TrimPrefix(suffix, ".") {
		return ""
	}
	return strings.TrimSuffix(path, suffix)
}

func joinPath(base, suffix string) string {
	if base == "" {
		return suffix
	}
	return base + "." + suffix
}

func setRawAt(jsonStr, path, value string) string {
	if path == "" {
		return value
	}
	result, _ := sjson.SetRawBytes([]byte(jsonStr), path, []byte(value))
	return string(result)
}

func isPropertyDefinition(path string) bool {
	return path == "properties" || strings.HasSuffix(path, ".properties")
}

// pathEndsWithSchemaNameMapKeyword reports whether path points at a map whose keys are author-chosen
// names, so a key spelled like a literal-value keyword there is a name and its value a subschema.
func pathEndsWithSchemaNameMapKeyword(path string) bool {
	if path == "" {
		return false
	}
	segments := splitGJSONPath(path)
	_, ok := schemaNameMapKeywords[unescapeGJSONPathKey(segments[len(segments)-1])]
	return ok
}

// pathInsideLiteralValue reports whether path descends through a literal-value keyword (default,
// example, examples, const, enum). Everything below those keywords is client data: a schema-shaped
// key there must be copied verbatim, never rewritten or stripped. The keyword's own path is not
// "inside" itself, so collecting the keyword still works. A segment whose parent is a name map is
// a property or definition name (`properties.default`), not the literal keyword.
func pathInsideLiteralValue(path string) bool {
	segments := splitGJSONPath(path)
	for i := range len(segments) - 1 {
		if !isLiteralValueKeyword(unescapeGJSONPathKey(segments[i])) {
			continue
		}
		if i == 0 {
			return true
		}
		if _, isNameMap := schemaNameMapKeywords[unescapeGJSONPathKey(segments[i-1])]; !isNameMap {
			return true
		}
	}
	return false
}

func descriptionPath(parentPath string) string {
	if parentPath == "" || parentPath == "@this" {
		return "description"
	}
	return parentPath + ".description"
}

func appendHint(jsonStr, parentPath, hint string) string {
	descPath := parentPath + ".description"
	if parentPath == "" || parentPath == "@this" {
		descPath = "description"
	}
	existing := gjson.Get(jsonStr, descPath).String()
	if existing != "" {
		hint = fmt.Sprintf("%s (%s)", existing, hint)
	}
	updated, _ := sjson.SetBytes([]byte(jsonStr), descPath, hint)
	jsonStr = string(updated)
	return jsonStr
}

func appendHintRaw(jsonRaw, hint string) string {
	existing := gjson.Get(jsonRaw, "description").String()
	if existing != "" {
		hint = fmt.Sprintf("%s (%s)", existing, hint)
	}
	updated, _ := sjson.SetBytes([]byte(jsonRaw), "description", hint)
	jsonRaw = string(updated)
	return jsonRaw
}

func getStrings(jsonStr, path string) []string {
	var result []string
	if arr := gjson.Get(jsonStr, path); arr.IsArray() {
		for _, r := range arr.Array() {
			result = append(result, r.String())
		}
	}
	return result
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func orDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func escapeGJSONPathKey(key string) string {
	if strings.IndexAny(key, ".*?") == -1 {
		return key
	}
	return gjsonPathKeyReplacer.Replace(key)
}

func unescapeGJSONPathKey(key string) string {
	if !strings.Contains(key, "\\") {
		return key
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' && i+1 < len(key) {
			i++
			b.WriteByte(key[i])
			continue
		}
		b.WriteByte(key[i])
	}
	return b.String()
}

func splitGJSONPath(path string) []string {
	if path == "" {
		return nil
	}

	parts := make([]string, 0, strings.Count(path, ".")+1)
	var b strings.Builder
	b.Grow(len(path))

	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' && i+1 < len(path) {
			b.WriteByte('\\')
			i++
			b.WriteByte(path[i])
			continue
		}
		if c == '.' {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	parts = append(parts, b.String())
	return parts
}

func mergeDescriptionRaw(schemaRaw, parentDesc string) string {
	childDesc := gjson.Get(schemaRaw, "description").String()
	switch {
	case childDesc == "":
		updated, _ := sjson.SetBytes([]byte(schemaRaw), "description", parentDesc)
		return string(updated)
	case childDesc == parentDesc:
		return schemaRaw
	default:
		combined := fmt.Sprintf("%s (%s)", parentDesc, childDesc)
		updated, _ := sjson.SetBytes([]byte(schemaRaw), "description", combined)
		return string(updated)
	}
}

// InlineLocalRefs resolves JSON Pointer references against the original schema. Every reference
// receives its own copy as long as the expansion budget lasts, sibling keywords override the
// referenced definition, and cycles terminate as a typed hint instead of recursing forever.
func InlineLocalRefs(jsonStr string) string {
	return inlineLocalRefs(jsonStr)
}

// maxInlinedSchemaBytes bounds the total size of schema material copied into the output by local
// reference expansion. Shared definitions fan out once per reference, so a small input can expand
// exponentially; expansions past the budget degrade to a typed hint like cycles do.
const maxInlinedSchemaBytes = 256 * 1024

// maxFallbackDescriptionBytes bounds the target description copied into a degraded reference, so a
// large shared description cannot be repeated per reference and defeat the expansion budget.
const maxFallbackDescriptionBytes = 512

// inlineLocalRefs resolves JSON Pointer references against the original schema. Every reference
// receives its own copy as long as the expansion budget lasts, sibling keywords override the
// referenced definition, and cycles terminate as a typed hint instead of recursing forever.
func inlineLocalRefs(jsonStr string) string {
	return inlineLocalRefsRooted(jsonStr, nil, false)
}

// inlineLocalRefsForClean inlines local references for schema cleaning. Cleaning sometimes
// receives the single-key {"schema": ...} carrier used to nest a schema one level down (see Phase
// 0); references inside it must then resolve against the inner schema, not the carrier, or every
// lookup misses. The carrier shape and root keys are otherwise preserved byte-for-byte.
func inlineLocalRefsForClean(jsonStr string) string {
	if !strings.Contains(jsonStr, `"$ref"`) {
		return jsonStr
	}

	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}

	resolveRoot := root
	wrapped := false
	if rootMap, ok := root.(map[string]any); ok && len(rootMap) == 1 {
		if inner, okInner := rootMap["schema"].(map[string]any); okInner {
			resolveRoot = inner
			wrapped = true
		}
	}
	if !wrapped {
		return inlineLocalRefsRooted(jsonStr, root, true)
	}

	state := newRefResolveState()
	resolved := resolveSchemaRoot(resolveRoot, state, true)
	out, err := json.Marshal(map[string]any{"schema": resolved})
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func inlineLocalRefsRooted(jsonStr string, parsedRoot any, forClean bool) string {
	if !strings.Contains(jsonStr, `"$ref"`) {
		return jsonStr
	}

	root := parsedRoot
	if root == nil {
		decoder := json.NewDecoder(strings.NewReader(jsonStr))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return jsonStr
		}
	}

	state := newRefResolveState()
	resolved := resolveSchemaRoot(root, state, forClean)
	out, err := json.Marshal(resolved)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

// refResolveState carries the remaining expansion budget of one inlining pass. Every reference
// charges the size of the target it copies (nested references charge their own targets), so
// shared-definition fan-out stops once the budget is spent while ordinary reuse still expands.
type refResolveState struct {
	budget int
}

func newRefResolveState() *refResolveState {
	return &refResolveState{budget: maxInlinedSchemaBytes}
}

// sortedKeys returns map keys in a deterministic order, so budget allocation never depends on
// Go map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// resolveSchemaRoot resolves references in the schema body before resolving definition
// containers. Definitions only matter through references, so walking them first would spend the
// expansion budget on copies the cleaners delete next (and whose map order is arbitrary), while
// the fields that are actually kept degraded to hints. For the public InlineLocalRefs the
// containers are kept, so they are still resolved afterwards with whatever budget remains; when
// cleaning they are dropped here altogether (references still resolve against the original
// root), so their bytes never reach the intermediate JSON at all.
func resolveSchemaRoot(root any, state *refResolveState, forClean bool) any {
	rootMap, ok := root.(map[string]any)
	if !ok {
		return resolveLocalRefs(root, root, make(map[string]bool), state, forClean)
	}

	out := make(map[string]any, len(rootMap))
	for _, key := range sortedKeys(rootMap) {
		item := rootMap[key]
		switch key {
		case "$defs", "definitions":
			if !forClean {
				out[key] = nil // placeholder, resolved after the schema body below
			}
		case "$ref":
			out[key] = item
		default:
			out[key] = resolveSchemaEntry(rootMap, key, item, make(map[string]bool), state, forClean)
		}
	}

	// A root-level reference expands into the whole referenced schema; its siblings were resolved
	// above and must override the target, mirroring resolveLocalRefs.
	if ref, hasRef := rootMap["$ref"].(string); hasRef && strings.HasPrefix(ref, "#/") {
		if target, okTarget := resolveJSONPointer(rootMap, ref); okTarget {
			if size := targetExpansionSize(target, forClean); size <= state.budget {
				state.budget -= size
				active := map[string]bool{ref: true}
				if targetMap, okMap := resolveLocalRefs(rootMap, target, active, state, forClean).(map[string]any); okMap {
					merged := make(map[string]any, len(targetMap)+len(out))
					for _, key := range sortedKeys(targetMap) {
						merged[key] = targetMap[key]
					}
					for _, key := range sortedKeys(out) {
						if key != "$ref" {
							merged[key] = out[key]
						}
					}
					out = merged
				} else {
					out = resolveRefFallback(rootMap, out, target, ref, make(map[string]bool), state, forClean)
				}
			} else {
				out = resolveRefFallback(rootMap, out, target, ref, make(map[string]bool), state, forClean)
			}
			delete(out, "$ref")
		}
	}

	if !forClean {
		for _, key := range []string{"$defs", "definitions"} {
			if item, exists := rootMap[key]; exists {
				out[key] = resolveSchemaEntry(rootMap, key, item, make(map[string]bool), state, false)
			}
		}
	}
	return out
}

// targetExpansionSize estimates what copying a reference target into the output costs. When
// cleaning, definitions nested inside the target are deleted later and never copied, so they are
// excluded; otherwise a small target carrying a large unused definitions block would be degraded
// purely because of material that is about to be thrown away. Name-map and literal context is
// threaded so properties named like a container keyword, and data inside literal values, still
// count in full.
func targetExpansionSize(value any, forClean bool) int {
	if !forClean {
		return jsonValueSize(value)
	}
	return cleanExpansionSize(value, false)
}

func cleanExpansionSize(value any, inNameMap bool) int {
	switch node := value.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case json.Number:
		return len(node.String())
	case string:
		return jsonStringEncodedSize(node)
	case []any:
		size := 2
		for _, item := range node {
			size += cleanExpansionSize(item, false) + 1
			if size > maxInlinedSchemaBytes+1 {
				return maxInlinedSchemaBytes + 1
			}
		}
		return size
	case map[string]any:
		size := 2
		for key, item := range node {
			if !inNameMap && (key == "$defs" || key == "definitions") {
				continue
			}
			child := item
			childInNameMap := false
			if !inNameMap {
				if isLiteralValueKeyword(key) {
					size += jsonStringEncodedSize(key) + 1 + jsonValueSize(child) + 1
					if size > maxInlinedSchemaBytes+1 {
						return maxInlinedSchemaBytes + 1
					}
					continue
				}
				if _, isNameMap := schemaNameMapKeywords[key]; isNameMap {
					childInNameMap = true
				}
			}
			size += jsonStringEncodedSize(key) + 1 + cleanExpansionSize(child, childInNameMap) + 1
			if size > maxInlinedSchemaBytes+1 {
				return maxInlinedSchemaBytes + 1
			}
		}
		return size
	default:
		return 16
	}
}

// jsonValueSize estimates the encoded size of a decoded JSON value as encoding/json will emit
// it, including string escaping (notably the six-byte \u00XX forms for <, >, & and control
// characters). It is used to bound ref expansion, so it must not under-count what serialization
// will actually produce.
//
// The estimate stops counting once it passes the expansion budget: a caller only compares it
// against that budget, and an uncapped walk would scan one large shared target again per
// reference, turning r references to an n-byte target into O(r*n) work for an answer that is
// already past the cap.
func jsonValueSize(value any) int {
	switch node := value.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case json.Number:
		return len(node.String())
	case string:
		return jsonStringEncodedSize(node)
	case []any:
		size := 2
		for _, item := range node {
			size += jsonValueSize(item) + 1
			if size > maxInlinedSchemaBytes+1 {
				return maxInlinedSchemaBytes + 1
			}
		}
		return size
	case map[string]any:
		size := 2
		for key, item := range node {
			size += jsonStringEncodedSize(key) + 1 + jsonValueSize(item) + 1
			if size > maxInlinedSchemaBytes+1 {
				return maxInlinedSchemaBytes + 1
			}
		}
		return size
	default:
		return 16
	}
}

// jsonStringEncodedSize returns the encoded length of one JSON string including quotes,
// mirroring encoding/json's default HTML escaping.
func jsonStringEncodedSize(s string) int {
	size := 2
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c < 0x20:
			if c == '\n' || c == '\r' || c == '\t' {
				size += 2
			} else {
				size += 6
			}
		case c == '"' || c == '\\':
			size += 2
		case c == '<' || c == '>' || c == '&':
			size += 6
		case c < 0x80:
			size++
		case c == 0xe2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xa8 || s[i+2] == 0xa9):
			size += 6
			i += 2
		default:
			size++
		}
	}
	return size
}

// schemaNameMapKeywords are the schema keywords whose value maps author-chosen names to
// subschemas. A key directly under one of them is a name, never a schema keyword. The Draft-07
// "dependencies" container qualifies for object (schema) values; array values are property-name
// lists and are never walked as maps by the checks that use this set.
var schemaNameMapKeywords = map[string]struct{}{
	"properties":        {},
	"patternProperties": {},
	"dependentSchemas":  {},
	"dependencies":      {},
	"$defs":             {},
	"definitions":       {},
}

// literalValueKeywords carry client data rather than subschemas. Cleaning and ref inlining must
// copy their values verbatim: keys inside them that happen to look like schema keywords are data.
var literalValueKeywords = map[string]struct{}{
	"default":  {},
	"example":  {},
	"examples": {},
	"const":    {},
	"enum":     {},
}

func isLiteralValueKeyword(key string) bool {
	_, ok := literalValueKeywords[key]
	return ok
}

func resolveLocalRefs(root, value any, active map[string]bool, state *refResolveState, forClean bool) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = resolveLocalRefs(root, item, active, state, forClean)
		}
		return out
	case map[string]any:
		ref, hasRef := node["$ref"].(string)
		if hasRef && strings.HasPrefix(ref, "#/") {
			if target, ok := resolveJSONPointer(root, ref); ok {
				if active[ref] {
					return resolveRefFallback(root, node, target, ref, active, state, forClean)
				}
				// Charge the stored target size and let nested references charge their own
				// costs during recursion. The total copied material stays bounded, while an
				// over-budget chain still expands its affordable outer levels and only the
				// deepest references degrade to hints.
				if size := targetExpansionSize(target, forClean); size > state.budget {
					return resolveRefFallback(root, node, target, ref, active, state, forClean)
				} else {
					state.budget -= size
				}
				active[ref] = true
				resolvedTarget := resolveLocalRefs(root, target, active, state, forClean)
				delete(active, ref)
				if targetMap, okTarget := resolvedTarget.(map[string]any); okTarget {
					out := make(map[string]any, len(targetMap)+len(node))
					for _, key := range sortedKeys(targetMap) {
						if forClean && (key == "$defs" || key == "definitions") {
							continue // dropped with every other definitions container when cleaning
						}
						out[key] = targetMap[key]
					}
					for _, key := range sortedKeys(node) {
						if key == "$ref" {
							continue
						}
						if forClean && (key == "$defs" || key == "definitions") {
							continue
						}
						out[key] = resolveSchemaEntry(root, key, node[key], active, state, forClean)
					}
					return out
				}
			}
		}

		out := make(map[string]any, len(node))
		for _, key := range sortedKeys(node) {
			if forClean && (key == "$defs" || key == "definitions") {
				continue // dropped with every other definitions container when cleaning
			}
			out[key] = resolveSchemaEntry(root, key, node[key], active, state, forClean)
		}
		return out
	default:
		return value
	}
}

// resolveRefFallback degrades a reference that cannot be expanded (cycle or exhausted budget) to
// a typed hint, but still resolves the sibling subschemas on the same node so their own references
// are not left dangling behind the degraded one. Literal values stay verbatim through
// resolveSchemaEntry. The description assembled by cyclicRefFallback is kept afterwards so
// resolved siblings cannot clobber the hint.
func resolveRefFallback(root any, node map[string]any, target any, ref string, active map[string]bool, state *refResolveState, forClean bool) map[string]any {
	out := cyclicRefFallback(node, target, ref)
	// The target description copied into the fallback is expansion output like any other; charge
	// it to the budget. Once the budget is spent, fallbacks carry only the short hint, so a large
	// number of degraded references cannot multiply copied descriptions without bound. Cyclic
	// fallbacks omit over-long descriptions entirely, and omitted content is not charged.
	if targetMap, okMap := target.(map[string]any); okMap {
		if description, okDescription := targetMap["description"].(string); okDescription && description != "" && len(description) <= maxFallbackDescriptionBytes {
			if _, hasOwn := node["description"]; !hasOwn {
				cost := jsonStringEncodedSize(description)
				if cost > state.budget {
					out["description"] = "See: " + refName(ref)
				} else {
					state.budget -= cost
				}
			}
		}
	}
	for _, key := range sortedKeys(node) {
		if key == "$ref" || key == "description" {
			continue
		}
		if forClean && (key == "$defs" || key == "definitions") {
			delete(out, key)
			continue
		}
		out[key] = resolveSchemaEntry(root, key, node[key], active, state, forClean)
	}
	return out
}

// resolveSchemaEntry resolves one map entry during ref inlining. A literal-value keyword's value
// is client data and is deep-copied without resolving refs inside it. Under a name map
// ("properties" and friends), however, a key spelled like a literal keyword is an author-chosen
// property name whose value is a subschema; name-map entries are therefore resolved directly
// here so such property names keep their schema semantics. Nested definition containers outside
// the root still resolve through this path; when cleaning, callers skip them entirely instead.
func resolveSchemaEntry(root any, key string, item any, active map[string]bool, state *refResolveState, forClean bool) any {
	if isLiteralValueKeyword(key) {
		return deepCopyJSONValue(item)
	}
	if _, isNameMap := schemaNameMapKeywords[key]; isNameMap {
		if nameMap, ok := item.(map[string]any); ok {
			out := make(map[string]any, len(nameMap))
			for _, name := range sortedKeys(nameMap) {
				out[name] = resolveLocalRefs(root, nameMap[name], active, state, forClean)
			}
			return out
		}
	}
	return resolveLocalRefs(root, item, active, state, forClean)
}

func deepCopyJSONValue(value any) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = deepCopyJSONValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, item := range node {
			out[key] = deepCopyJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func resolveJSONPointer(root any, ref string) (any, bool) {
	current := root
	for _, rawPart := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(rawPart, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func cyclicRefFallback(node map[string]any, target any, ref string) map[string]any {
	out := make(map[string]any, len(node)+2)
	if targetMap, ok := target.(map[string]any); ok {
		for _, key := range []string{"type", "nullable", "description"} {
			if value, exists := targetMap[key]; exists {
				if key == "description" {
					if description, okDescription := value.(string); okDescription && len(description) > maxFallbackDescriptionBytes {
						continue
					}
				}
				out[key] = value
			}
		}
	}
	for key, value := range node {
		if key != "$ref" {
			out[key] = value
		}
	}
	name := refName(ref)
	hint := "See: " + name
	if description, _ := out["description"].(string); description != "" {
		out["description"] = mergeHint(description, hint)
	} else {
		out["description"] = hint
	}
	return out
}

func refName(ref string) string {
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		return strings.ReplaceAll(strings.ReplaceAll(ref[index+1:], "~1", "/"), "~0", "~")
	}
	return ref
}

func mergeHint(existing, hint string) string {
	if existing == "" {
		return hint
	}
	if existing == hint ||
		strings.HasPrefix(existing, hint+" (") ||
		strings.Contains(existing, fmt.Sprintf("(%s)", hint)) {
		return existing
	}
	return fmt.Sprintf("%s (%s)", existing, hint)
}

var antigravityDeclarationContainers = []string{"functionDeclarations", "function_declarations"}

var antigravityDeclarationSchemaKeys = []string{
	"parameters",
	"parametersJsonSchema",
	"parameters_json_schema",
	"response",
	"responseJsonSchema",
	"response_json_schema",
	"responseSchema",
	"response_schema",
}

// AntigravityElementEdit replaces one array element with replacement.
type AntigravityElementEdit struct {
	Offset      int
	Raw         string
	Replacement string
}

func isParameterSchemaPath(key string) bool {
	return key == "parameters" || key == "parametersJsonSchema" || key == "parameters_json_schema"
}

// SanitizeAntigravityToolSchemaDocumentForModel renames and cleans every
// declaration schema in the tools-only document. Each declaration is edited
// on its own raw bytes and the results are spliced back into the tools array
// in a single pass, so the cost stays linear in the document size instead of
// scaling with declarations x document size (Issue 6714).
func SanitizeAntigravityToolSchemaDocumentForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	toolSchemaCleaner := func(schema string, isParameterSchema bool) string {
		if isClaudeModel && isParameterSchema {
			return CleanJSONSchemaForAntigravityClaudeTool(schema, useAntigravitySchema)
		}
		if useAntigravitySchema {
			return CleanJSONSchemaForAntigravityTool(schema, useAntigravitySchema)
		}
		return CleanJSONSchemaForGemini(schema)
	}
	cleanNestedToolSchema := func(schemaRaw string, isParameterSchema bool) string {
		return cleanNestedSchema(func(schema string) string {
			return toolSchemaCleaner(schema, isParameterSchema)
		}, schemaRaw)
	}

	tools := gjson.Parse(payloadStr).Get("request.tools")
	if !tools.IsArray() {
		return payloadStr
	}
	elements := make([]gjson.Result, 0)
	edits := make([]AntigravityElementEdit, 0)
	tools.ForEach(func(_, tool gjson.Result) bool {
		elements = append(elements, tool)
		if !tool.IsObject() {
			return true
		}
		toolRaw := tool.Raw
		for _, declKey := range antigravityDeclarationContainers {
			decls := tool.Get(declKey)
			if !decls.IsArray() {
				continue
			}
			cleanedDecls, declsChanged := SanitizeAntigravityDeclarations(decls, cleanNestedToolSchema)
			if !declsChanged {
				continue
			}
			updated, errSet := sjson.SetRawBytes([]byte(toolRaw), declKey, []byte(cleanedDecls))
			if errSet != nil {
				continue
			}
			toolRaw = string(updated)
		}
		if toolRaw != tool.Raw {
			edits = append(edits, AntigravityElementEdit{Offset: tool.Index - tools.Index, Raw: tool.Raw, Replacement: toolRaw})
		}
		return true
	})
	if len(edits) == 0 {
		return payloadStr
	}
	updated, errSet := sjson.SetRawBytes([]byte(payloadStr), "request.tools", []byte(ApplyAntigravityElementEdits(tools.Raw, tools.Index, elements, edits)))
	if errSet != nil {
		return payloadStr
	}
	return string(updated)
}

func ApplyAntigravityElementEdits(arrayRaw string, arrayIndex int, elements []gjson.Result, edits []AntigravityElementEdit) string {
	var rebuilt strings.Builder
	rebuilt.Grow(len(arrayRaw))
	cursor := 0
	for _, edit := range edits {
		if edit.Offset < cursor || edit.Offset+len(edit.Raw) > len(arrayRaw) || arrayRaw[edit.Offset:edit.Offset+len(edit.Raw)] != edit.Raw {
			return joinAntigravityElements(arrayIndex, elements, edits)
		}
		rebuilt.WriteString(arrayRaw[cursor:edit.Offset])
		rebuilt.WriteString(edit.Replacement)
		cursor = edit.Offset + len(edit.Raw)
	}
	rebuilt.WriteString(arrayRaw[cursor:])
	return rebuilt.String()
}

func joinAntigravityElements(arrayIndex int, elements []gjson.Result, edits []AntigravityElementEdit) string {
	replacements := make(map[int]string, len(edits))
	for _, edit := range edits {
		replacements[edit.Offset] = edit.Replacement
	}
	var rebuilt strings.Builder
	rebuilt.WriteByte('[')
	for i, element := range elements {
		if i > 0 {
			rebuilt.WriteByte(',')
		}
		if replacement, ok := replacements[element.Index-arrayIndex]; ok {
			rebuilt.WriteString(replacement)
		} else {
			rebuilt.WriteString(element.Raw)
		}
	}
	rebuilt.WriteByte(']')
	return rebuilt.String()
}

func SanitizeAntigravityDeclarations(decls gjson.Result, clean func(schemaRaw string, isParameterSchema bool) string) (string, bool) {
	elements := make([]gjson.Result, 0)
	edits := make([]AntigravityElementEdit, 0)
	decls.ForEach(func(_, decl gjson.Result) bool {
		elements = append(elements, decl)
		if !decl.IsObject() {
			return true
		}
		cleanedDecl := SanitizeAntigravityDeclaration(decl, clean)
		if cleanedDecl != decl.Raw {
			edits = append(edits, AntigravityElementEdit{Offset: decl.Index - decls.Index, Raw: decl.Raw, Replacement: cleanedDecl})
		}
		return true
	})
	if len(edits) == 0 {
		return "", false
	}
	return ApplyAntigravityElementEdits(decls.Raw, decls.Index, elements, edits), true
}

func SanitizeAntigravityDeclaration(decl gjson.Result, clean func(schemaRaw string, isParameterSchema bool) string) string {
	declRaw := decl.Raw
	if decl.Get("parametersJsonSchema").Exists() {
		renamed, errRename := RenameKey(declRaw, "parametersJsonSchema", "parameters")
		if errRename == nil {
			declRaw = renamed
			decl = gjson.Parse(declRaw)
		}
	}
	if decl.Get("parameters_json_schema").Exists() {
		renamed, errRename := RenameKey(declRaw, "parameters_json_schema", "parameters")
		if errRename == nil {
			declRaw = renamed
			decl = gjson.Parse(declRaw)
		}
	}
	for _, key := range antigravityDeclarationSchemaKeys {
		schema := decl.Get(key)
		if !schema.IsObject() {
			continue
		}
		cleanedSchema := clean(schema.Raw, isParameterSchemaPath(key))
		if cleanedSchema == schema.Raw {
			continue
		}
		updated, errSet := sjson.SetRawBytes([]byte(declRaw), key, []byte(cleanedSchema))
		if errSet != nil {
			continue
		}
		declRaw = string(updated)
		decl = gjson.Parse(declRaw)
	}
	return declRaw
}

func cleanNestedSchema(clean func(string) string, schemaRaw string) string {
	const antigravitySchemaWrapperKey = "schema"
	wrapped, errWrap := sjson.SetRaw("{}", antigravitySchemaWrapperKey, schemaRaw)
	if errWrap != nil {
		return clean(schemaRaw)
	}
	if unwrapped := gjson.Get(clean(wrapped), antigravitySchemaWrapperKey); unwrapped.Exists() {
		return unwrapped.Raw
	}
	return clean(schemaRaw)
}

var antigravityGenerationSchemaPaths = []string{
	"request.generationConfig.responseSchema",
	"request.generationConfig.response_schema",
	"request.generationConfig.responseJsonSchema",
	"request.generationConfig.response_json_schema",
	"request.generation_config.responseSchema",
	"request.generation_config.response_schema",
	"request.generation_config.responseJsonSchema",
	"request.generation_config.response_json_schema",
}

func SanitizeAntigravityGenerationSchemas(payloadStr string) string {
	for _, path := range antigravityGenerationSchemaPaths {
		schema := gjson.Get(payloadStr, path)
		if !schema.Exists() || !schema.IsObject() {
			continue
		}
		cleaned := CleanJSONSchemaForAntigravityResponse(schema.Raw)
		if cleaned == schema.Raw {
			continue
		}
		updated, errSet := sjson.SetRawBytes([]byte(payloadStr), path, []byte(cleaned))
		if errSet != nil {
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

// SanitizeAntigravityRequestSchemasForModel cleans with model-aware tool semantics.
func SanitizeAntigravityRequestSchemasForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	payloadStr = SanitizeAntigravityToolSchemaDocumentForModel(payloadStr, useAntigravitySchema, isClaudeModel)
	payloadStr = SanitizeAntigravityGenerationSchemas(payloadStr)
	return payloadStr
}

func sanitizeAntigravityRequestSchemasForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	return SanitizeAntigravityRequestSchemasForModel(payloadStr, useAntigravitySchema, isClaudeModel)
}
