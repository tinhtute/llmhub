package registry

import (
	"strings"
	"testing"
)

// formerClaudeFamilyVariantIDs is the definition-side helper as it stood before
// the two Claude-family normalizers were collapsed into claudeFamilyIDs
// (model_definitions.go). It is the differential's reference implementation: no
// production code may call it.
func formerClaudeFamilyVariantIDs(modelID string) []string {
	if !strings.HasPrefix(modelID, "claude-") {
		return nil
	}
	var ids []string
	base := modelID
	if i := strings.IndexByte(base, '['); i > 0 && strings.HasSuffix(base, "]") {
		base = base[:i]
		ids = append(ids, base)
	}
	if n := len(base); n > 9 && base[n-9] == '-' && isEightDigits(base[n-8:]) {
		ids = append(ids, base[:n-9])
	}
	return ids
}

// formerClaudeFamilyModelIDs is the registry-side helper as it stood before the
// collapse (model_registry.go). Reference implementation only.
func formerClaudeFamilyModelIDs(modelID string) []string {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	if clean == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{clean: true}
	out = append(out, clean)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if bracket := strings.IndexByte(clean, '['); bracket != -1 {
		add(clean[:bracket])
	}
	if lastDash := strings.LastIndexByte(clean, '-'); lastDash != -1 && lastDash+1 < len(clean) {
		suffix := clean[lastDash+1:]
		if len(suffix) == 8 {
			isDate := true
			for i := range suffix {
				if suffix[i] < '0' || suffix[i] > '9' {
					isDate = false
					break
				}
			}
			if isDate {
				add(clean[:lastDash])
			}
		}
	}
	return out
}

// sequentialStripFamily returns the family the new order reaches after the
// bracket strip: only a bracketed ID with a dated base has one.
func sequentialStripFamily(modelID string) string {
	base := strings.ToLower(strings.TrimSpace(modelID))
	bracket := strings.IndexByte(base, '[')
	if bracket <= 0 || !strings.HasSuffix(base, "]") {
		return ""
	}
	base = base[:bracket]
	if n := len(base); n > 9 && base[n-9] == '-' && isEightDigits(base[n-8:]) {
		return base[:n-9]
	}
	return ""
}

// isSubsequence reports whether every element of sub appears in seq in order.
func isSubsequence(sub, seq []string) bool {
	if len(sub) == 0 {
		return true
	}
	i := 0
	for _, s := range seq {
		if s == sub[i] {
			i++
			if i == len(sub) {
				return true
			}
		}
	}
	return false
}

func modelInfoID(info *ModelInfo) string {
	if info == nil {
		return ""
	}
	return info.ID
}

func catalogContains(models []*ModelInfo, id string) bool {
	for _, m := range models {
		if m != nil && m.ID == id {
			return true
		}
	}
	return false
}

// staticScanUniverse mirrors LookupStaticModelInfo's exact-scan order.
func staticScanUniverse(data *staticModelsJSON) [][]*ModelInfo {
	return [][]*ModelInfo{
		data.Claude, data.Gemini, data.Vertex, data.GeminiCLI, data.AIStudio,
		data.CodexPro, data.Kimi, data.Antigravity, data.XAI, data.Kiro,
		data.Devin, staticDevinModels, data.Meta,
	}
}

// TestClaudeFamilyNormalizerEquivalence pins claudeFamilyIDs against both former
// helpers over the embedded catalog: every canonical call-site input resolves to
// the same catalog entry at every call site, the former candidate order survives,
// and the only outcomes that moved are the two documented classes asserted below.
func TestClaudeFamilyNormalizerEquivalence(t *testing.T) {
	data := getModels()
	if data == nil || len(data.Claude) == 0 {
		t.Fatal("embedded catalog has no Claude section")
	}
	universe := staticScanUniverse(data)
	codexSection := GetStaticModelDefinitionsByChannel("codex")

	claudeIDs := make([]string, 0, len(data.Claude))
	for _, m := range data.Claude {
		if m != nil && m.ID != "" {
			claudeIDs = append(claudeIDs, m.ID)
		}
	}

	// Old call-site behavior, reproduced from the two former helpers.
	exactScan := func(modelID string) bool {
		for _, models := range universe {
			if catalogContains(models, modelID) {
				return true
			}
		}
		return false
	}
	oldStaticSite := func(modelID string) string {
		if exactScan(modelID) {
			return modelID
		}
		for _, familyID := range formerClaudeFamilyVariantIDs(modelID) {
			if catalogContains(data.Claude, familyID) {
				return familyID
			}
		}
		return ""
	}
	oldChannelSite := func(modelID string) string {
		trimmed := strings.TrimSpace(modelID)
		if catalogContains(data.Claude, trimmed) {
			return trimmed
		}
		for _, familyID := range formerClaudeFamilyVariantIDs(trimmed) {
			if catalogContains(data.Claude, familyID) {
				return familyID
			}
		}
		return ""
	}
	oldRegistrySite := func(modelID string) string {
		if id := oldStaticSite(modelID); id != "" {
			return id
		}
		for _, familyID := range formerClaudeFamilyModelIDs(modelID) {
			if familyID == modelID {
				continue
			}
			if id := oldStaticSite(familyID); id != "" {
				return id
			}
		}
		return ""
	}

	type inputClass int
	const (
		classCanonical inputClass = iota
		classLowerVariant
		classMixedCase
		classNonClaude
	)
	type probe struct {
		input string
		class inputClass
	}
	var corpus []probe
	for _, id := range claudeIDs {
		corpus = append(corpus,
			probe{id, classCanonical},
			probe{id + "[1m]", classLowerVariant},
			probe{id + "-20261001", classLowerVariant},
			probe{id + "-20261001[1m]", classLowerVariant},
			probe{strings.ToUpper(id), classMixedCase},
			probe{strings.ToUpper(id) + "[1M]", classMixedCase},
			probe{"  " + id + "  ", classMixedCase},
		)
	}
	corpus = append(corpus,
		probe{"gpt-5.2[1m]", classNonClaude},
		probe{"gpt-5.2-20261005", classNonClaude},
		probe{"gpt-5.2-20261005[1m]", classNonClaude},
		probe{"gemini-2.5-pro[1m]", classNonClaude},
		probe{"gpt-5.2", classNonClaude},
		probe{"", classNonClaude},
	)

	var widenedDefinitionSite, widenedDoubleVariant int
	for _, p := range corpus {
		claudeInput := strings.HasPrefix(p.input, "claude-")
		normalized := strings.ToLower(strings.TrimSpace(p.input))

		if claudeInput {
			// No former lookup target may be dropped, and the former order must survive.
			if oldVariants := formerClaudeFamilyVariantIDs(p.input); !isSubsequence(oldVariants, claudeFamilyIDs(p.input)) {
				t.Errorf("%q: former definition candidates %v are not an ordered subsequence of %v", p.input, oldVariants, claudeFamilyIDs(p.input))
			}
			var filteredOld []string
			for _, c := range formerClaudeFamilyModelIDs(p.input) {
				if c != p.input {
					filteredOld = append(filteredOld, c)
				}
			}
			var filteredNew []string
			for _, c := range claudeFamilyIDs(p.input) {
				if c != p.input {
					filteredNew = append(filteredNew, c)
				}
			}
			if !isSubsequence(filteredOld, filteredNew) {
				t.Errorf("%q: former registry candidates %v are not an ordered subsequence of %v", p.input, filteredOld, filteredNew)
			}
			// Guard the reference: a candidate that resolves only outside the Claude
			// section would make oldStaticSite's family loop unfaithful.
			for _, candidate := range append(append([]string{}, formerClaudeFamilyVariantIDs(p.input)...), claudeFamilyIDs(p.input)...) {
				if !catalogContains(data.Claude, candidate) && exactScan(candidate) {
					t.Fatalf("%q: candidate %q resolves only outside the Claude section; the reference lookup is unfaithful here", p.input, candidate)
				}
			}
		}

		// Definition-side site: canonical and variant inputs keep their answer;
		// normalization is the only widening.
		wantStatic := oldStaticSite(p.input)
		if p.class == classMixedCase {
			wantStatic = oldStaticSite(normalized)
		}
		if got := modelInfoID(LookupStaticModelInfo(p.input)); got != wantStatic {
			t.Errorf("LookupStaticModelInfo(%q)=%q, want %q (former definition helper)", p.input, got, wantStatic)
		} else if p.class == classMixedCase && oldStaticSite(p.input) == "" && got != "" {
			widenedDefinitionSite++
			t.Logf("widening (definition/channel sites, mixed case or padding): %q now resolves %q; the former helper required a canonical ID", p.input, got)
		}

		// Claude channel site.
		wantChannel := oldChannelSite(p.input)
		if p.class == classMixedCase {
			wantChannel = oldChannelSite(normalized)
		}
		if got := modelInfoID(LookupStaticModelInfoByChannel(p.input, "claude")); got != wantChannel {
			t.Errorf("LookupStaticModelInfoByChannel(%q, claude)=%q, want %q", p.input, got, wantChannel)
		}

		// A non-Claude channel never takes the Claude family fallback.
		wantCodex := ""
		if trimmed := strings.TrimSpace(p.input); catalogContains(codexSection, trimmed) {
			wantCodex = trimmed
		}
		if got := modelInfoID(LookupStaticModelInfoByChannel(p.input, "codex")); got != wantCodex {
			t.Errorf("LookupStaticModelInfoByChannel(%q, codex)=%q, want %q", p.input, got, wantCodex)
		}

		// Registry-side site: the dynamic registry is consulted first, so compare
		// only when it holds nothing for this ID.
		if dynamic := GetGlobalRegistry().GetModelInfo(p.input, ""); dynamic != nil {
			t.Logf("skipping registry comparison for %q: the dynamic registry holds it", p.input)
			continue
		}
		wantRegistry := oldRegistrySite(p.input)
		if p.class == classMixedCase {
			// The former registry helper lowercased too, so its answer stands.
			wantRegistry = oldRegistrySite(normalized)
		}
		newRegistry := modelInfoID(LookupModelInfo(p.input))
		if newRegistry != wantRegistry {
			// The one allowed divergence: a bracketed dated variant now also reaches
			// the date-stripped family, matching the definition lookups.
			if family := sequentialStripFamily(p.input); family != "" && wantRegistry == "" && newRegistry == oldStaticSite(family) {
				widenedDoubleVariant++
				t.Logf("widening (registry site, bracketed dated variant): %q now resolves %q; the former helper stopped at the bracketed date", p.input, newRegistry)
			} else {
				t.Errorf("LookupModelInfo(%q)=%q, want %q (former registry helper)", p.input, newRegistry, wantRegistry)
			}
		}
	}

	// The corpus must exercise both documented divergence classes, and no other.
	if widenedDefinitionSite == 0 {
		t.Error("corpus exercised no mixed-case widening; the differential is vacuous")
	}
	if widenedDoubleVariant == 0 {
		t.Error("corpus exercised no bracketed dated variant at the registry site; the differential is vacuous")
	}
}
