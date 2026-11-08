package helps

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// geminiFunctionCallCounter counts logical Gemini/Vertex function calls in a
// stream, deduplicating streamed partialArgs continuations, repeated call IDs
// and empty separator objects.
//
// Upstream (bd5ec5310aba) delegates argument assembly to
// translatorcommon.VertexStreamingToolCall. That type has no host in this fork
// and the translator packages are outside this slice, so the state machine below
// is the local equivalent: it tracks the partialArgs leaf paths of each
// in-progress call and applies the same continuation rules (a string leaf stays
// open only while the fragment that wrote it carries willContinue, and writing
// to a closed leaf starts a new logical call).
type geminiFunctionCallCounter struct {
	activeByCandidate map[int64]*geminiStreamingFunctionCall
	seenCallIDs       map[string]struct{}
}

const (
	geminiArgNodeUnset uint8 = iota
	geminiArgNodeString
	geminiArgNodeScalar
)

type geminiArgNode struct {
	kind   uint8
	open   bool
	closed bool
}

type geminiStreamingFunctionCall struct {
	name  string
	id    string
	nodes map[string]*geminiArgNode
	last  *geminiArgNode
	err   bool
}

// observePart returns 1 for the first observation of a logical function call and
// 0 for continuations, duplicates and empty separators.
func (c *geminiFunctionCallCounter) observePart(candidateIdx int64, call gjson.Result) int64 {
	if c == nil || !call.Exists() || !call.IsObject() {
		return 0
	}
	rawName := strings.TrimSpace(call.Get("name").String())
	upstreamID := strings.TrimSpace(call.Get("id").String())

	if active := c.activeByCandidate[candidateIdx]; active != nil {
		if active.canContinueWith(rawName, upstreamID, call) {
			done := active.push(call)
			if active.err || done {
				delete(c.activeByCandidate, candidateIdx)
			}
			return 0
		}
	}
	delete(c.activeByCandidate, candidateIdx)

	if isVertexStreamingFunctionCall(call) {
		if rawName == "" {
			return 0
		}
		active := &geminiStreamingFunctionCall{name: rawName, id: upstreamID}
		done := active.push(call)
		if active.err {
			return 0
		}
		if !done {
			if c.activeByCandidate == nil {
				c.activeByCandidate = make(map[int64]*geminiStreamingFunctionCall)
			}
			c.activeByCandidate[candidateIdx] = active
		}
		return c.countLogicalCall(candidateIdx, upstreamID)
	}

	if rawName == "" && upstreamID == "" && !call.Get("args").Exists() {
		return 0
	}
	return c.countLogicalCall(candidateIdx, upstreamID)
}

func (c *geminiFunctionCallCounter) countLogicalCall(candidateIdx int64, upstreamID string) int64 {
	if upstreamID != "" {
		key := fmt.Sprintf("%d:%s", candidateIdx, upstreamID)
		if c.seenCallIDs == nil {
			c.seenCallIDs = make(map[string]struct{})
		}
		if _, exists := c.seenCallIDs[key]; exists {
			return 0
		}
		c.seenCallIDs[key] = struct{}{}
	}
	return 1
}

// canContinueWith reports whether call belongs to this in-progress streaming call.
func (a *geminiStreamingFunctionCall) canContinueWith(name, id string, call gjson.Result) bool {
	if a == nil {
		return false
	}
	if call.Get("args").Exists() && !isVertexStreamingFunctionCall(call) {
		return false
	}
	if id != "" && a.id != "" && id != a.id {
		return false
	}
	if name == "" {
		return true
	}
	if name != a.name {
		return false
	}
	if id != "" && a.id != "" && id == a.id {
		return true
	}
	return a.canAcceptContinuation(call)
}

// canAcceptContinuation reports whether call's partialArgs extend the current
// argument tree without overwriting an already-completed leaf value.
func (a *geminiStreamingFunctionCall) canAcceptContinuation(call gjson.Result) bool {
	if a == nil || a.err {
		return false
	}
	fragments, ok := geminiPartialArgs(call)
	if !ok {
		return true
	}
	if !fragments.IsArray() {
		return false
	}
	for _, item := range fragments.Array() {
		if !item.IsObject() {
			return false
		}
		path, hasPath := geminiArgPath(item)
		if !hasPath {
			continue
		}
		key, okPath := geminiArgPathKey(path)
		if !okPath || key == "" {
			continue
		}
		node := a.nodes[key]
		if node == nil || node.kind == geminiArgNodeUnset {
			continue
		}
		innerWillContinue := geminiFragmentWillContinue(item)
		hasString, hasScalar := geminiFragmentValueKind(item)
		if !hasString && !hasScalar && !innerWillContinue {
			continue
		}
		if hasString || innerWillContinue {
			if node.kind != geminiArgNodeString || node.closed || !node.open {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// push applies one functionCall chunk and reports whether the call completed.
func (a *geminiStreamingFunctionCall) push(call gjson.Result) bool {
	if a == nil || a.err {
		return false
	}
	fragments, _ := geminiPartialArgs(call)
	if fragments.IsArray() {
		for _, item := range fragments.Array() {
			a.pushFragment(item)
			if a.err {
				return false
			}
		}
	}
	for _, node := range a.nodes {
		if node.kind == geminiArgNodeString && !node.open {
			node.closed = true
		}
	}
	return !geminiFragmentWillContinue(call)
}

func (a *geminiStreamingFunctionCall) pushFragment(item gjson.Result) {
	if !item.IsObject() {
		a.err = true
		return
	}
	path, hasPath := geminiArgPath(item)
	var key string
	if hasPath {
		var okPath bool
		key, okPath = geminiArgPathKey(path)
		if !okPath {
			a.err = true
			return
		}
	}
	innerWillContinue := geminiFragmentWillContinue(item)
	hasString, hasScalar := geminiFragmentValueKind(item)

	// Valueless end marker (e.g. {"jsonPath":"$.city"} or {"jsonPath":"$.city","willContinue":false}).
	if !hasString && !hasScalar && !innerWillContinue {
		if key == "" {
			if a.last != nil && a.last.open {
				a.last.open = false
			}
			return
		}
		if node := a.nodes[key]; node != nil && node.kind == geminiArgNodeString && node.open {
			node.open = false
		}
		return
	}
	if key == "" {
		a.err = true
		return
	}
	if a.nodes == nil {
		a.nodes = make(map[string]*geminiArgNode)
	}
	node := a.nodes[key]
	if hasString || innerWillContinue {
		if node == nil {
			node = &geminiArgNode{kind: geminiArgNodeString}
			a.nodes[key] = node
		} else if node.kind != geminiArgNodeString || node.closed || !node.open {
			a.err = true
			return
		}
		node.open = innerWillContinue
		a.last = node
		return
	}
	if node != nil && node.kind != geminiArgNodeUnset {
		a.err = true
		return
	}
	a.nodes[key] = &geminiArgNode{kind: geminiArgNodeScalar}
}

// isVertexStreamingFunctionCall reports whether the call carries incremental
// argument fragments rather than a complete args object.
func isVertexStreamingFunctionCall(call gjson.Result) bool {
	partialArgs := call.Get("partialArgs")
	if !partialArgs.Exists() {
		partialArgs = call.Get("partial_args")
	}
	if partialArgs.Exists() {
		if partialArgs.IsArray() && len(partialArgs.Array()) == 0 && call.Get("args").Exists() && !geminiFragmentWillContinue(call) {
			return false
		}
		return true
	}
	if call.Get("willContinue").Exists() || call.Get("will_continue").Exists() {
		if call.Get("args").Exists() && !geminiFragmentWillContinue(call) {
			return false
		}
		return true
	}
	return false
}

func geminiPartialArgs(node gjson.Result) (gjson.Result, bool) {
	if partialArgs := node.Get("partialArgs"); partialArgs.Exists() {
		return partialArgs, true
	}
	partialArgs := node.Get("partial_args")
	return partialArgs, partialArgs.Exists()
}

func geminiFragmentWillContinue(node gjson.Result) bool {
	return node.Get("willContinue").Bool() || node.Get("will_continue").Bool()
}

// geminiFragmentValueKind reports whether the fragment carries a string value or
// any other scalar (number/bool/null) value.
func geminiFragmentValueKind(item gjson.Result) (hasString, hasScalar bool) {
	strRes := item.Get("stringValue")
	if !strRes.Exists() {
		strRes = item.Get("string_value")
	}
	numRes := item.Get("numberValue")
	if !numRes.Exists() {
		numRes = item.Get("number_value")
	}
	boolRes := item.Get("boolValue")
	if !boolRes.Exists() {
		boolRes = item.Get("bool_value")
	}
	nullRes := item.Get("nullValue")
	if !nullRes.Exists() {
		nullRes = item.Get("null_value")
	}
	hasString = strRes.Exists() && strRes.Type != gjson.Null
	hasNumber := numRes.Exists() && numRes.Type != gjson.Null
	hasBool := boolRes.Exists() && boolRes.Type != gjson.Null
	hasNull := nullRes.Exists() && !hasString && !hasNumber && !hasBool
	return hasString, hasNumber || hasBool || hasNull
}

func geminiArgPath(item gjson.Result) (string, bool) {
	if path := item.Get("jsonPath"); path.Exists() {
		return path.String(), true
	}
	path := item.Get("json_path")
	return path.String(), path.Exists()
}

// geminiArgPathKey canonicalizes a Vertex partialArgs jsonPath into a map key.
// It returns an empty key for the root path and false for unparsable paths.
func geminiArgPathKey(rawPath string) (string, bool) {
	s := strings.TrimSpace(rawPath)
	if s == "" || s == "$" {
		return "", true
	}
	if !strings.HasPrefix(s, "$") {
		return "", false
	}
	s = s[1:]
	var key strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '.':
			i++
			start := i
			for i < len(s) && s[i] != '.' && s[i] != '[' {
				if s[i] < 0x20 {
					return "", false
				}
				i++
			}
			if i == start {
				return "", false
			}
			key.WriteString("\x00")
			key.WriteString(s[start:i])
		case '[':
			i++
			if i >= len(s) {
				return "", false
			}
			if s[i] == '\'' || s[i] == '"' {
				quote := s[i]
				i++
				start := i
				for i < len(s) && s[i] != quote {
					i++
				}
				if i >= len(s) {
					return "", false
				}
				key.WriteString("\x00")
				key.WriteString(s[start:i])
				i++
				if i >= len(s) || s[i] != ']' {
					return "", false
				}
				i++
				continue
			}
			start := i
			for i < len(s) && s[i] != ']' {
				i++
			}
			if i >= len(s) {
				return "", false
			}
			idx, errIdx := strconv.Atoi(strings.TrimSpace(s[start:i]))
			if errIdx != nil || idx < 0 {
				return "", false
			}
			key.WriteString("#")
			key.WriteString(strconv.Itoa(idx))
			i++
		default:
			return "", false
		}
	}
	return key.String(), true
}
