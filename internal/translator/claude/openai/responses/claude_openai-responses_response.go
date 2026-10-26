package responses

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	translatorcommon "github.com/therealtinhtute/llmhub/internal/translator/common"
	"github.com/therealtinhtute/llmhub/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeToResponsesState struct {
	Seq        int
	ResponseID string
	CreatedAt  int64
	// RequestEcho holds the request fields echoed into the completed response and
	// ToolWinners the resolved tool declarations, both extracted once at stream
	// init so the request body is neither retained nor re-scanned per chunk.
	RequestEcho util.ResponsesRequestEcho
	ToolWinners map[string]util.ResponsesToolDescriptor
	// CustomToolNames is derived from ToolWinners at init.
	CustomToolNames    map[string]struct{}
	NextOutputIndex    int
	CurrentMsgID       string
	CurrentFCID        string
	InTextBlock        bool
	InFuncBlock        bool
	MessageOpen        bool
	ContentPartOpen    bool
	MessageOutputIndex int
	FuncArgsBuf        map[int]*strings.Builder // index -> args
	FuncArgsDone       map[int]bool
	FuncItemDone       map[int]bool
	FuncItemStatus     map[int]string
	// function call bookkeeping for output aggregation
	FuncNames         map[int]string // Claude block index -> function name
	FuncCallIDs       map[int]string // Claude block index -> call id
	FuncCustom        map[int]bool   // Claude block index -> freeform custom tool
	FuncOutputIndices map[int]int    // Claude block index -> Responses output index
	// message text aggregation
	TextBuf            strings.Builder
	CurrentTextBuf     strings.Builder
	MessageAnnotations []any
	MessageItems       []claudeResponsesMessageItem
	// reasoning state
	ReasoningActive     bool
	ReasoningDeltasDone bool
	ReasoningItemID     string
	ReasoningBuf        strings.Builder
	ReasoningSignature  string
	ReasoningIndex      int
	ReasoningItems      []claudeResponsesReasoningItem
	// server-side web search state: a Claude server_tool_use block and its
	// web_search_tool_result block fold into one Responses web_search_call item.
	WebSearchByBlock  map[int]*claudeResponsesWebSearchItem
	WebSearchByToolID map[string]*claudeResponsesWebSearchItem
	WebSearchItems    []*claudeResponsesWebSearchItem
	StopReason        string
	// usage aggregation
	InputTokens  int64
	OutputTokens int64
	UsageSeen    bool
}

type claudeResponsesWebSearchItem struct {
	ToolUseID   string
	OutputIndex int
	InputBuf    strings.Builder
	Results     []byte
	Emitted     bool
	Status      string
}

func (item *claudeResponsesWebSearchItem) render() []byte {
	return buildResponsesWebSearchCallItem(item.ToolUseID, claudeWebSearchQuery(item.InputBuf.String()), item.Results)
}

type claudeResponsesMessageItem struct {
	ID          string
	OutputIndex int
	Text        string
	Annotations []any
	Status      string
}

type claudeResponsesReasoningItem struct {
	ID          string
	OutputIndex int
	Text        string
	Signature   string
	Status      string
}

var dataTag = []byte("data:")

// claudeResponsesIncompleteDetails is the only Claude→OpenAI Responses
// stop-reason table. A row's reason becomes response.incomplete_details.reason;
// a reason with no row leaves the response completed.
func claudeResponsesIncompleteDetails(stopReason string) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "max_tokens", "model_context_window_exceeded":
		// A context-window stop is a length stop: the output and usage the
		// caller received are still worth keeping, so report it truncated
		// rather than failed.
		return []byte(`{"reason":"max_output_tokens"}`), true
	case "refusal":
		// Safety classifiers stop the turn with a normal 200. Responses clients
		// treat content_filter as a retryable refusal instead of a finished answer:
		// https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons#refusal
		return []byte(`{"reason":"content_filter"}`), true
	}
	return nil, false
}

// claudeResponsesTerminal is the terminal event an OpenAI Responses client sees
// for one Claude stop reason.
type claudeResponsesTerminal struct {
	event            string // response.completed | response.incomplete
	status           string // completed | incomplete
	incompleteDetail []byte // response.incomplete_details, nil when not incomplete
	// unfinishedTurn marks Anthropic's server-tool iteration pause. The turn is
	// unfinished but valid: the client continues it by sending the paused
	// assistant content back unchanged, so Responses ends the response as
	// completed with end_turn=false instead of an error:
	// https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons#pause_turn
	unfinishedTurn bool
}

func claudeResponsesOutputStatus(stopReason string) string {
	if _, incomplete := claudeResponsesIncompleteDetails(stopReason); incomplete {
		return "incomplete"
	}
	return "completed"
}

// claudeResponsesTerminalState is the single outcome both the streaming and the
// non-streaming converters read, so no converter re-tests a stop reason.
func claudeResponsesTerminalState(stopReason string) claudeResponsesTerminal {
	if incompleteDetails, incomplete := claudeResponsesIncompleteDetails(stopReason); incomplete {
		return claudeResponsesTerminal{
			event:            "response.incomplete",
			status:           "incomplete",
			incompleteDetail: incompleteDetails,
		}
	}
	return claudeResponsesTerminal{
		event:          "response.completed",
		status:         "completed",
		unfinishedTurn: strings.EqualFold(strings.TrimSpace(stopReason), "pause_turn"),
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

func requestModelName(originalRequestRawJSON, requestRawJSON []byte) string {
	for _, rawJSON := range [][]byte{originalRequestRawJSON, requestRawJSON} {
		if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
			continue
		}
		root := gjson.ParseBytes(rawJSON)
		for _, path := range []string{"model", "request.model"} {
			model := root.Get(path)
			if model.Type == gjson.String && strings.TrimSpace(model.String()) != "" {
				return model.String()
			}
		}
	}
	return ""
}

// resolveResponsesFunctionCallIdentity reproduces
// splitResponsesQualifiedFunctionCallFromRequest from precomputed winners, so a
// response chunk does not re-parse the request and rebuild them per tool call.
func resolveResponsesFunctionCallIdentity(winners map[string]util.ResponsesToolDescriptor, qualifiedName string) (name, namespace string) {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if qualifiedName == "" {
		return "", ""
	}
	descriptor, ok := winners[qualifiedName]
	if !ok {
		return qualifiedName, ""
	}
	if !descriptor.Direct {
		return descriptor.LocalName, descriptor.Namespace
	}
	return qualifiedName, ""
}

// applyResponsesFunctionCallNamespaceFields rewrites a Responses tool-call item's
// name/namespace from the request's winning tool declaration, so a flattened
// Claude tool name maps back to its local name plus namespace.
func applyResponsesFunctionCallNamespaceFields(item []byte, winners map[string]util.ResponsesToolDescriptor, qualifiedName string, itemPath string) []byte {
	name, namespace := resolveResponsesFunctionCallIdentity(winners, qualifiedName)
	return translatorcommon.SetResponsesToolCallIdentity(item, name, namespace, itemPath)
}

// applyFunctionCallNamespaceFields applies the stream's precomputed winners.
func (st *claudeToResponsesState) applyFunctionCallNamespaceFields(item []byte, qualifiedName, itemPath string) []byte {
	return applyResponsesFunctionCallNamespaceFields(item, st.ToolWinners, qualifiedName, itemPath)
}

func emitEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func (st *claudeToResponsesState) appendMessageAnnotation(annotation any) {
	if annotation == nil {
		return
	}
	st.MessageAnnotations = append(st.MessageAnnotations, annotation)
}

func (st *claudeToResponsesState) allocateOutputIndex() int {
	index := st.NextOutputIndex
	st.NextOutputIndex++
	return index
}

func (st *claudeToResponsesState) messageOutputIndex() int {
	if st.MessageOutputIndex < 0 {
		st.MessageOutputIndex = st.allocateOutputIndex()
	}
	return st.MessageOutputIndex
}

func (st *claudeToResponsesState) functionOutputIndex(blockIndex int) int {
	if index, ok := st.FuncOutputIndices[blockIndex]; ok {
		return index
	}
	index := st.allocateOutputIndex()
	st.FuncOutputIndices[blockIndex] = index
	return index
}

func (st *claudeToResponsesState) startWebSearch(blockIndex int, toolUseID string) *claudeResponsesWebSearchItem {
	item := &claudeResponsesWebSearchItem{ToolUseID: toolUseID, OutputIndex: st.allocateOutputIndex()}
	st.WebSearchByBlock[blockIndex] = item
	st.WebSearchByToolID[toolUseID] = item
	st.WebSearchItems = append(st.WebSearchItems, item)
	return item
}

// finalizeWebSearchWithStatus emits output_item.done once the result block has been seen,
// or at message_stop when the turn ended without one.
func (st *claudeToResponsesState) finalizeWebSearchWithStatus(item *claudeResponsesWebSearchItem, status string, nextSeq func() int) [][]byte {
	if item == nil || item.Emitted {
		return nil
	}
	item.Emitted = true
	item.Status = status
	done := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{}}`)
	done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
	done, _ = sjson.SetBytes(done, "output_index", item.OutputIndex)
	rendered := item.render()
	rendered, _ = sjson.SetBytes(rendered, "status", status)
	done, _ = sjson.SetRawBytes(done, "item", rendered)
	return [][]byte{emitEvent("response.output_item.done", done)}
}

func (st *claudeToResponsesState) finalizeFuncItem(idx int, status string, nextSeq func() int) [][]byte {
	if st.FuncItemDone[idx] {
		return nil
	}
	if st.FuncItemDone == nil {
		st.FuncItemDone = make(map[int]bool)
	}
	st.FuncItemDone[idx] = true
	if st.FuncItemStatus == nil {
		st.FuncItemStatus = make(map[int]string)
	}
	st.FuncItemStatus[idx] = status

	outputIndex := st.functionOutputIndex(idx)
	args := ""
	if buf := st.FuncArgsBuf[idx]; buf != nil {
		if buf.Len() > 0 {
			args = buf.String()
		}
	}
	if !st.FuncCustom[idx] && args == "" && status == "completed" {
		args = "{}"
	}
	callID := st.FuncCallIDs[idx]
	if callID == "" {
		callID = st.CurrentFCID
	}
	name := st.FuncNames[idx]

	var out [][]byte
	if st.FuncCustom[idx] {
		input := unwrapCustomToolInput(args)
		if !st.FuncArgsDone[idx] {
			if st.FuncArgsDone == nil {
				st.FuncArgsDone = make(map[int]bool)
			}
			st.FuncArgsDone[idx] = true
			inputDone := []byte(`{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","output_index":0,"input":""}`)
			inputDone, _ = sjson.SetBytes(inputDone, "sequence_number", nextSeq())
			inputDone, _ = sjson.SetBytes(inputDone, "item_id", fmt.Sprintf("ctc_%s", callID))
			inputDone, _ = sjson.SetBytes(inputDone, "output_index", outputIndex)
			inputDone, _ = sjson.SetBytes(inputDone, "input", input)
			out = append(out, emitEvent("response.custom_tool_call_input.done", inputDone))
		}

		itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}}`)
		itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
		itemDone, _ = sjson.SetBytes(itemDone, "output_index", outputIndex)
		itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("ctc_%s", callID))
		itemDone, _ = sjson.SetBytes(itemDone, "item.status", status)
		itemDone, _ = sjson.SetBytes(itemDone, "item.input", input)
		itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", callID)
		itemDone = st.applyFunctionCallNamespaceFields(itemDone, name, "item")
		out = append(out, emitEvent("response.output_item.done", itemDone))
	} else {
		if !st.FuncArgsDone[idx] {
			if st.FuncArgsDone == nil {
				st.FuncArgsDone = make(map[int]bool)
			}
			st.FuncArgsDone[idx] = true
			fcDone := []byte(`{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`)
			fcDone, _ = sjson.SetBytes(fcDone, "sequence_number", nextSeq())
			fcDone, _ = sjson.SetBytes(fcDone, "item_id", fmt.Sprintf("fc_%s", callID))
			fcDone, _ = sjson.SetBytes(fcDone, "output_index", outputIndex)
			fcDone, _ = translatorcommon.SetStringWithoutHTMLEscape(fcDone, "arguments", args)
			out = append(out, emitEvent("response.function_call_arguments.done", fcDone))
		}

		itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`)
		itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
		itemDone, _ = sjson.SetBytes(itemDone, "output_index", outputIndex)
		itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("fc_%s", callID))
		itemDone, _ = sjson.SetBytes(itemDone, "item.status", status)
		itemDone, _ = translatorcommon.SetStringWithoutHTMLEscape(itemDone, "item.arguments", args)
		itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", callID)
		itemDone = st.applyFunctionCallNamespaceFields(itemDone, name, "item")
		out = append(out, emitEvent("response.output_item.done", itemDone))
	}
	st.InFuncBlock = false
	return out
}

func (st *claudeToResponsesState) finalizeReasoningDeltas(nextSeq func() int) [][]byte {
	if !st.ReasoningActive || st.ReasoningDeltasDone {
		return nil
	}
	st.ReasoningDeltasDone = true
	full := st.ReasoningBuf.String()
	var out [][]byte
	textDone := []byte(`{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`)
	textDone, _ = sjson.SetBytes(textDone, "sequence_number", nextSeq())
	textDone, _ = sjson.SetBytes(textDone, "item_id", st.ReasoningItemID)
	textDone, _ = sjson.SetBytes(textDone, "output_index", st.ReasoningIndex)
	textDone, _ = sjson.SetBytes(textDone, "text", full)
	out = append(out, emitEvent("response.reasoning_summary_text.done", textDone))
	partDone := []byte(`{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
	partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.SetBytes(partDone, "item_id", st.ReasoningItemID)
	partDone, _ = sjson.SetBytes(partDone, "output_index", st.ReasoningIndex)
	partDone, _ = sjson.SetBytes(partDone, "part.text", full)
	out = append(out, emitEvent("response.reasoning_summary_part.done", partDone))
	return out
}

func (st *claudeToResponsesState) finalizeReasoningItem(status string, nextSeq func() int) [][]byte {
	if !st.ReasoningActive && st.ReasoningItemID == "" {
		return nil
	}
	var out [][]byte
	out = append(out, st.finalizeReasoningDeltas(nextSeq)...)

	full := st.ReasoningBuf.String()
	itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"completed","encrypted_content":"","summary":[]}}`)
	itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
	itemDone, _ = sjson.SetBytes(itemDone, "output_index", st.ReasoningIndex)
	itemDone, _ = sjson.SetBytes(itemDone, "item.id", st.ReasoningItemID)
	itemDone, _ = sjson.SetBytes(itemDone, "item.status", status)
	itemDone, _ = sjson.SetBytes(itemDone, "item.encrypted_content", st.ReasoningSignature)
	summary := []byte(`{"type":"summary_text","text":""}`)
	summary, _ = sjson.SetBytes(summary, "text", full)
	itemDone = translatorcommon.SetRawArrayItems(itemDone, "item.summary", [][]byte{summary})
	out = append(out, emitEvent("response.output_item.done", itemDone))
	st.ReasoningItems = append(st.ReasoningItems, claudeResponsesReasoningItem{
		ID:          st.ReasoningItemID,
		OutputIndex: st.ReasoningIndex,
		Text:        full,
		Signature:   st.ReasoningSignature,
		Status:      status,
	})
	st.ReasoningActive = false
	st.ReasoningItemID = ""
	st.ReasoningBuf.Reset()
	st.ReasoningSignature = ""
	st.ReasoningIndex = -1
	return out
}

func (st *claudeToResponsesState) finalizeAssistantMessage(nextSeq func() int) [][]byte {
	if !st.MessageOpen {
		return nil
	}
	fullText := st.TextBuf.String()
	outputIndex := st.messageOutputIndex()
	status := claudeResponsesOutputStatus(st.StopReason)
	var out [][]byte
	done := []byte(`{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`)
	done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
	done, _ = sjson.SetBytes(done, "item_id", st.CurrentMsgID)
	done, _ = sjson.SetBytes(done, "output_index", outputIndex)
	done, _ = sjson.SetBytes(done, "text", fullText)
	out = append(out, emitEvent("response.output_text.done", done))

	partDone := []byte(`{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
	partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.SetBytes(partDone, "item_id", st.CurrentMsgID)
	partDone, _ = sjson.SetBytes(partDone, "output_index", outputIndex)
	partDone, _ = sjson.SetBytes(partDone, "part.text", fullText)
	if len(st.MessageAnnotations) > 0 {
		partDone, _ = sjson.SetBytes(partDone, "part.annotations", st.MessageAnnotations)
	}
	out = append(out, emitEvent("response.content_part.done", partDone))

	final := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}}`)
	final, _ = sjson.SetBytes(final, "sequence_number", nextSeq())
	final, _ = sjson.SetBytes(final, "output_index", outputIndex)
	final, _ = sjson.SetBytes(final, "item.id", st.CurrentMsgID)
	final, _ = sjson.SetBytes(final, "item.status", status)
	final, _ = sjson.SetBytes(final, "item.content.0.text", fullText)
	if len(st.MessageAnnotations) > 0 {
		final, _ = sjson.SetBytes(final, "item.content.0.annotations", st.MessageAnnotations)
	}
	out = append(out, emitEvent("response.output_item.done", final))

	st.MessageItems = append(st.MessageItems, claudeResponsesMessageItem{
		ID:          st.CurrentMsgID,
		OutputIndex: outputIndex,
		Text:        fullText,
		Annotations: append([]any(nil), st.MessageAnnotations...),
		Status:      status,
	})
	st.InTextBlock = false
	st.MessageOpen = false
	st.ContentPartOpen = false
	st.CurrentMsgID = ""
	st.MessageOutputIndex = -1
	st.TextBuf.Reset()
	st.CurrentTextBuf.Reset()
	st.MessageAnnotations = nil
	return out
}

// ConvertClaudeResponseToOpenAIResponses converts Claude SSE to OpenAI Responses SSE events.
func ConvertClaudeResponseToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
		winners := util.CollectResponsesToolWinners(gjson.ParseBytes(reqJSON))
		*param = &claudeToResponsesState{
			MessageOutputIndex: -1,
			ReasoningIndex:     -1,
			FuncArgsBuf:        make(map[int]*strings.Builder),
			FuncArgsDone:       make(map[int]bool),
			FuncItemDone:       make(map[int]bool),
			FuncItemStatus:     make(map[int]string),
			FuncNames:          make(map[int]string),
			FuncCallIDs:        make(map[int]string),
			FuncCustom:         make(map[int]bool),
			FuncOutputIndices:  make(map[int]int),
			WebSearchByBlock:   make(map[int]*claudeResponsesWebSearchItem),
			WebSearchByToolID:  make(map[string]*claudeResponsesWebSearchItem),
			RequestEcho:        util.ExtractResponsesRequestEcho(gjson.ParseBytes(reqJSON)),
			ToolWinners:        winners,
			CustomToolNames:    responsesCustomToolNamesFromWinners(winners),
		}
	}
	st := (*param).(*claudeToResponsesState)

	// Expect `data: {..}` from Claude clients
	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])
	root := gjson.ParseBytes(rawJSON)
	ev := root.Get("type").String()
	var out [][]byte

	nextSeq := func() int { st.Seq++; return st.Seq }

	switch ev {
	case "message_start":
		if msg := root.Get("message"); msg.Exists() {
			st.ResponseID = msg.Get("id").String()
			st.CreatedAt = time.Now().Unix()
			// Reset per-message aggregation state
			st.TextBuf.Reset()
			st.CurrentTextBuf.Reset()
			st.MessageAnnotations = nil
			st.MessageItems = nil
			st.ReasoningBuf.Reset()
			st.ReasoningActive = false
			st.ReasoningDeltasDone = false
			st.NextOutputIndex = 0
			st.InTextBlock = false
			st.InFuncBlock = false
			st.MessageOpen = false
			st.ContentPartOpen = false
			st.CurrentMsgID = ""
			st.CurrentFCID = ""
			st.MessageOutputIndex = -1
			st.ReasoningItemID = ""
			st.ReasoningSignature = ""
			st.ReasoningIndex = -1
			st.ReasoningItems = nil
			st.StopReason = ""
			st.FuncArgsBuf = make(map[int]*strings.Builder)
			st.FuncArgsDone = make(map[int]bool)
			st.FuncItemDone = make(map[int]bool)
			st.FuncItemStatus = make(map[int]string)
			st.FuncNames = make(map[int]string)
			st.FuncCallIDs = make(map[int]string)
			st.FuncCustom = make(map[int]bool)
			st.FuncOutputIndices = make(map[int]int)
			st.WebSearchByBlock = make(map[int]*claudeResponsesWebSearchItem)
			st.WebSearchByToolID = make(map[string]*claudeResponsesWebSearchItem)
			st.WebSearchItems = nil
			st.InputTokens = 0
			st.OutputTokens = 0
			st.UsageSeen = false
			if usage := msg.Get("usage"); usage.Exists() {
				if v := usage.Get("input_tokens"); v.Exists() {
					st.InputTokens = v.Int()
					st.UsageSeen = true
				}
				if v := usage.Get("output_tokens"); v.Exists() {
					st.OutputTokens = v.Int()
					st.UsageSeen = true
				}
			}
			// response.created
			created := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
			created, _ = sjson.SetBytes(created, "sequence_number", nextSeq())
			created, _ = sjson.SetBytes(created, "response.id", st.ResponseID)
			created, _ = sjson.SetBytes(created, "response.created_at", st.CreatedAt)
			reqModel := requestModelName(originalRequestRawJSON, requestRawJSON)
			if reqModel == "" {
				reqModel = modelName
			}
			if reqModel != "" {
				created, _ = sjson.SetBytes(created, "response.model", reqModel)
			}
			out = append(out, emitEvent("response.created", created))
			// response.in_progress
			inprog := []byte(`{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress"}}`)
			inprog, _ = sjson.SetBytes(inprog, "sequence_number", nextSeq())
			inprog, _ = sjson.SetBytes(inprog, "response.id", st.ResponseID)
			inprog, _ = sjson.SetBytes(inprog, "response.created_at", st.CreatedAt)
			if reqModel != "" {
				inprog, _ = sjson.SetBytes(inprog, "response.model", reqModel)
			}
			out = append(out, emitEvent("response.in_progress", inprog))
		}
	case "content_block_start":
		cb := root.Get("content_block")
		if !cb.Exists() {
			return out
		}
		idx := int(root.Get("index").Int())
		typ := cb.Get("type").String()

		// Keep adjacent text blocks in the same assistant message.
		if typ != "text" {
			out = append(out, st.finalizeAssistantMessage(nextSeq)...)
		}
		// Finalize previous reasoning item
		if st.ReasoningActive || st.ReasoningItemID != "" {
			out = append(out, st.finalizeReasoningItem("completed", nextSeq)...)
		}
		// Finalize any previous completed function calls
		for prevIdx := range st.FuncCallIDs {
			if !st.FuncItemDone[prevIdx] && prevIdx != idx {
				out = append(out, st.finalizeFuncItem(prevIdx, "completed", nextSeq)...)
			}
		}
		// Finalize any previous web search items
		for _, item := range st.WebSearchItems {
			if !item.Emitted && item.Results != nil {
				out = append(out, st.finalizeWebSearchWithStatus(item, "completed", nextSeq)...)
			}
		}

		if typ == "text" {
			st.InTextBlock = true
			outputIndex := st.messageOutputIndex()
			if st.CurrentMsgID == "" {
				st.CurrentMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, len(st.MessageItems))
			}
			if !st.MessageOpen {
				item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`)
				item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
				item, _ = sjson.SetBytes(item, "output_index", outputIndex)
				item, _ = sjson.SetBytes(item, "item.id", st.CurrentMsgID)
				out = append(out, emitEvent("response.output_item.added", item))
				st.MessageOpen = true
			}
			if !st.ContentPartOpen {
				part := []byte(`{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
				part, _ = sjson.SetBytes(part, "sequence_number", nextSeq())
				part, _ = sjson.SetBytes(part, "item_id", st.CurrentMsgID)
				part, _ = sjson.SetBytes(part, "output_index", outputIndex)
				out = append(out, emitEvent("response.content_part.added", part))
				st.ContentPartOpen = true
			}
		} else if typ == "tool_use" {
			st.InFuncBlock = true
			st.CurrentFCID = cb.Get("id").String()
			name := cb.Get("name").String()
			_, isCustomTool := st.CustomToolNames[name]
			if st.FuncCustom == nil {
				st.FuncCustom = make(map[int]bool)
			}
			st.FuncCustom[idx] = isCustomTool
			outputIndex := st.functionOutputIndex(idx)
			var item []byte
			if isCustomTool {
				item = []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"in_progress","input":"","call_id":"","name":""}}`)
				item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("ctc_%s", st.CurrentFCID))
				item, _ = sjson.SetBytes(item, "item.call_id", st.CurrentFCID)
				item = st.applyFunctionCallNamespaceFields(item, name, "item")
			} else {
				item = []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`)
				item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				item, _ = sjson.SetBytes(item, "item.call_id", st.CurrentFCID)
				item = st.applyFunctionCallNamespaceFields(item, name, "item")
			}
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", outputIndex)
			out = append(out, emitEvent("response.output_item.added", item))
			if st.FuncArgsBuf[idx] == nil {
				st.FuncArgsBuf[idx] = &strings.Builder{}
			}
			// record function metadata for aggregation
			st.FuncCallIDs[idx] = st.CurrentFCID
			st.FuncNames[idx] = name
		} else if typ == "server_tool_use" {
			if name := cb.Get("name").String(); name != claudeWebSearchToolName {
				log.Debugf("claude->responses: unmapped server_tool_use %q at block %d", name, idx)
			} else {
				item := st.startWebSearch(idx, cb.Get("id").String())
				added := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"web_search_call","status":"in_progress","action":{"type":"search","query":""}}}`)
				added, _ = sjson.SetBytes(added, "sequence_number", nextSeq())
				added, _ = sjson.SetBytes(added, "output_index", item.OutputIndex)
				added, _ = sjson.SetBytes(added, "item.id", responsesWebSearchCallID(item.ToolUseID))
				out = append(out, emitEvent("response.output_item.added", added))
			}
		} else if typ == "web_search_tool_result" {
			// A result block carries its full content up front and has no deltas.
			// Results are stored here and the item is closed when the next block
			// starts or at message_stop so the final stop_reason is respected.
			if item := st.WebSearchByToolID[cb.Get("tool_use_id").String()]; item != nil {
				item.Results = claudeWebSearchResultsToResponses(cb.Get("content"))
			} else {
				log.Debugf("claude->responses: web_search_tool_result without matching server_tool_use at block %d", idx)
			}
		} else if typ == "thinking" {
			// start reasoning item
			st.ReasoningActive = true
			st.ReasoningDeltasDone = false
			st.ReasoningIndex = st.allocateOutputIndex()
			st.ReasoningBuf.Reset()
			st.ReasoningSignature = ""
			if signature := cb.Get("signature"); signature.Exists() && signature.String() != "" {
				st.ReasoningSignature = signature.String()
			}
			st.ReasoningItemID = fmt.Sprintf("rs_%s_%d", st.ResponseID, idx)
			item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", st.ReasoningIndex)
			item, _ = sjson.SetBytes(item, "item.id", st.ReasoningItemID)
			item, _ = sjson.SetBytes(item, "item.encrypted_content", st.ReasoningSignature)
			out = append(out, emitEvent("response.output_item.added", item))
			// add a summary part placeholder
			part := []byte(`{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
			part, _ = sjson.SetBytes(part, "sequence_number", nextSeq())
			part, _ = sjson.SetBytes(part, "item_id", st.ReasoningItemID)
			part, _ = sjson.SetBytes(part, "output_index", st.ReasoningIndex)
			out = append(out, emitEvent("response.reasoning_summary_part.added", part))
		}
	case "content_block_delta":
		d := root.Get("delta")
		if !d.Exists() {
			return out
		}
		dt := d.Get("type").String()
		if dt == "text_delta" {
			if t := d.Get("text"); t.Exists() {
				msg := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", st.CurrentMsgID)
				msg, _ = sjson.SetBytes(msg, "output_index", st.messageOutputIndex())
				msg, _ = sjson.SetBytes(msg, "delta", t.String())
				out = append(out, emitEvent("response.output_text.delta", msg))
				// aggregate text for response.output
				st.TextBuf.WriteString(t.String())
				st.CurrentTextBuf.WriteString(t.String())
			}
		} else if dt == "input_json_delta" {
			if item := st.WebSearchByBlock[int(root.Get("index").Int())]; item != nil {
				if pj := d.Get("partial_json"); pj.Exists() {
					item.InputBuf.WriteString(pj.String())
				}
				return [][]byte{}
			}
			if !st.InFuncBlock || st.CurrentFCID == "" {
				return [][]byte{}
			}
			idx := int(root.Get("index").Int())
			if pj := d.Get("partial_json"); pj.Exists() {
				if st.FuncArgsBuf[idx] == nil {
					st.FuncArgsBuf[idx] = &strings.Builder{}
				}
				st.FuncArgsBuf[idx].WriteString(pj.String())
				if st.FuncCustom[idx] {
					return [][]byte{}
				}
				outputIndex := st.functionOutputIndex(idx)
				msg := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				msg, _ = sjson.SetBytes(msg, "output_index", outputIndex)
				msg, _ = sjson.SetBytes(msg, "delta", pj.String())
				out = append(out, emitEvent("response.function_call_arguments.delta", msg))
			}
		} else if dt == "thinking_delta" {
			if st.ReasoningActive {
				if t := d.Get("thinking"); t.Exists() {
					st.ReasoningBuf.WriteString(t.String())
					msg := []byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
					msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
					msg, _ = sjson.SetBytes(msg, "item_id", st.ReasoningItemID)
					msg, _ = sjson.SetBytes(msg, "output_index", st.ReasoningIndex)
					msg, _ = sjson.SetBytes(msg, "delta", t.String())
					out = append(out, emitEvent("response.reasoning_summary_text.delta", msg))
				}
			}
		} else if dt == "signature_delta" {
			if st.ReasoningActive {
				if signature := d.Get("signature"); signature.Exists() && signature.String() != "" {
					st.ReasoningSignature = signature.String()
				}
			}
		} else if dt == "citations_delta" {
			if citation := d.Get("citation"); citation.Exists() {
				st.appendMessageAnnotation(citation.Value())
			}
			return [][]byte{}
		}
	case "content_block_stop":
		if st.InTextBlock {
			st.InTextBlock = false
		} else if st.InFuncBlock {
			st.InFuncBlock = false
		} else if st.ReasoningActive {
			out = append(out, st.finalizeReasoningDeltas(nextSeq)...)
		}
	case "message_delta":
		if usage := root.Get("usage"); usage.Exists() {
			if v := usage.Get("output_tokens"); v.Exists() {
				st.OutputTokens = v.Int()
				st.UsageSeen = true
			}
			if v := usage.Get("input_tokens"); v.Exists() {
				st.InputTokens = v.Int()
				st.UsageSeen = true
			}
		}
		if stopReason := root.Get("delta.stop_reason"); stopReason.Exists() {
			st.StopReason = stopReason.String()
		}
	case "message_stop":
		toolStatus := claudeResponsesOutputStatus(st.StopReason)
		if st.ReasoningActive || st.ReasoningItemID != "" {
			out = append(out, st.finalizeReasoningItem(toolStatus, nextSeq)...)
		}
		out = append(out, st.finalizeAssistantMessage(nextSeq)...)
		for idx := range st.FuncCallIDs {
			if !st.FuncItemDone[idx] {
				out = append(out, st.finalizeFuncItem(idx, toolStatus, nextSeq)...)
			}
		}
		for _, item := range st.WebSearchItems {
			if !item.Emitted {
				out = append(out, st.finalizeWebSearchWithStatus(item, toolStatus, nextSeq)...)
			}
		}

		terminal := claudeResponsesTerminalState(st.StopReason)
		completed := []byte(`{"type":"","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"","background":false,"error":null}}`)
		completed, _ = sjson.SetBytes(completed, "type", terminal.event)
		completed, _ = sjson.SetBytes(completed, "sequence_number", nextSeq())
		completed, _ = sjson.SetBytes(completed, "response.id", st.ResponseID)
		completed, _ = sjson.SetBytes(completed, "response.created_at", st.CreatedAt)
		completed, _ = sjson.SetBytes(completed, "response.status", terminal.status)
		if len(terminal.incompleteDetail) > 0 {
			completed, _ = sjson.SetRawBytes(completed, "response.incomplete_details", terminal.incompleteDetail)
		}
		if terminal.unfinishedTurn {
			completed, _ = sjson.SetBytes(completed, "response.end_turn", false)
		}
		// Inject original request fields into response as per docs/response.completed.json
		completed = st.RequestEcho.Apply(completed, "response.")

		// Build response.output from aggregated state
		outputsWrapper := []byte(`{"arr":[]}`)
		// reasoning items
		for _, reasoning := range st.ReasoningItems {
			status := reasoning.Status
			if status == "" {
				status = "completed"
			}
			item := []byte(`{"id":"","type":"reasoning","status":"completed","encrypted_content":"","summary":[]}`)
			item, _ = sjson.SetBytes(item, "id", reasoning.ID)
			item, _ = sjson.SetBytes(item, "status", status)
			item, _ = sjson.SetBytes(item, "encrypted_content", reasoning.Signature)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", reasoning.Text)
			item = translatorcommon.SetRawArrayItems(item, "summary", [][]byte{summary})
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", reasoning.OutputIndex), item)
		}
		// assistant message items
		for _, message := range st.MessageItems {
			item := []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			item, _ = sjson.SetBytes(item, "id", message.ID)
			item, _ = sjson.SetBytes(item, "status", message.Status)
			item, _ = sjson.SetBytes(item, "content.0.text", message.Text)
			if len(message.Annotations) > 0 {
				item, _ = sjson.SetBytes(item, "content.0.annotations", message.Annotations)
			}
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", message.OutputIndex), item)
		}
		// web_search_call items
		for _, item := range st.WebSearchItems {
			status := item.Status
			if status == "" {
				status = "completed"
			}
			rendered := item.render()
			rendered, _ = sjson.SetBytes(rendered, "status", status)
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", item.OutputIndex), rendered)
		}
		// function_call items (in ascending index order for determinism)
		if len(st.FuncArgsBuf) > 0 {
			// collect indices
			idxs := make([]int, 0, len(st.FuncArgsBuf))
			for idx := range st.FuncArgsBuf {
				idxs = append(idxs, idx)
			}
			sort.Ints(idxs)
			for _, idx := range idxs {
				status := st.FuncItemStatus[idx]
				if status == "" {
					status = "completed"
				}
				args := ""
				if !st.FuncCustom[idx] && status == "completed" {
					args = "{}"
				}
				if b := st.FuncArgsBuf[idx]; b != nil && b.Len() > 0 {
					args = b.String()
				}
				callID := st.FuncCallIDs[idx]
				name := st.FuncNames[idx]
				if callID == "" && st.CurrentFCID != "" {
					callID = st.CurrentFCID
				}
				if st.FuncCustom[idx] {
					item := []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("ctc_%s", callID))
					item, _ = sjson.SetBytes(item, "status", status)
					item, _ = sjson.SetBytes(item, "input", unwrapCustomToolInput(args))
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = st.applyFunctionCallNamespaceFields(item, name, "")
					outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", st.functionOutputIndex(idx)), item)
				} else {
					item := []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("fc_%s", callID))
					item, _ = sjson.SetBytes(item, "status", status)
					item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, "arguments", args)
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = st.applyFunctionCallNamespaceFields(item, name, "")
					outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", st.functionOutputIndex(idx)), item)
				}
			}
		}
		if gjson.GetBytes(outputsWrapper, "arr.#").Int() > 0 {
			completed, _ = sjson.SetRawBytes(completed, "response.output", []byte(gjson.GetBytes(outputsWrapper, "arr").Raw))
		}

		reasoningTokens := int64(0)
		for _, reasoning := range st.ReasoningItems {
			reasoningTokens += int64(len(reasoning.Text) / 4)
		}
		usagePresent := st.UsageSeen || reasoningTokens > 0
		if usagePresent {
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens", st.InputTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens_details.cached_tokens", 0)
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens", st.OutputTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens_details.reasoning_tokens", reasoningTokens)
			total := st.InputTokens + st.OutputTokens
			if total > 0 || st.UsageSeen {
				completed, _ = sjson.SetBytes(completed, "response.usage.total_tokens", total)
			}
		}
		out = append(out, emitEvent(terminal.event, completed))
	}

	return out
}

// ConvertClaudeResponseToOpenAIResponsesNonStream aggregates Claude SSE into a single OpenAI Responses JSON.
func ConvertClaudeResponseToOpenAIResponsesNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	// Aggregate Claude SSE lines into a single OpenAI Responses JSON (non-stream)
	// We follow the same aggregation logic as the streaming variant but produce
	// one final object matching docs/out.json structure.

	// Collect SSE data: lines start with "data: "; ignore others
	var chunks [][]byte
	remaining := rawJSON
	for len(remaining) > 0 {
		var line []byte
		idx := bytes.IndexByte(remaining, '\n')
		if idx >= 0 {
			line = remaining[:idx]
			remaining = remaining[idx+1:]
		} else {
			line = remaining
			remaining = nil
		}
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		chunks = append(chunks, line[len(dataTag):])
	}

	// Base OpenAI Responses (non-stream) object
	out := []byte(`{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{},"total_tokens":0}}`)

	// Aggregation state
	var (
		responseID   string
		createdAt    int64
		stopReason   string
		inputTokens  int64
		outputTokens int64
	)

	type nonStreamOutputItem struct {
		outputIndex int
		itemType    string
		id          string
		callID      string
		name        string
		text        strings.Builder
		signature   string
		annotations []any
		args        strings.Builder
		results     []byte
	}

	// Preserve content block order: map each Claude block index to its output
	// item and keep the insertion order in outputItems.
	blockToItem := make(map[int]*nonStreamOutputItem)
	webSearchByToolID := make(map[string]*nonStreamOutputItem)
	outputItems := make([]*nonStreamOutputItem, 0)
	nextOutputIndex := 0
	messageCount := 0
	var activeMessageItem *nonStreamOutputItem
	var pendingAnnotations []any

	allocateOutputIndex := func() int {
		outputIndex := nextOutputIndex
		nextOutputIndex++
		return outputIndex
	}
	reqBytes := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	requestWinners := util.CollectResponsesToolWinners(gjson.ParseBytes(reqBytes))
	customToolNames := responsesCustomToolNamesFromWinners(requestWinners)
	newOutputItem := func(itemType string, blockIndex int) *nonStreamOutputItem {
		item := &nonStreamOutputItem{
			outputIndex: allocateOutputIndex(),
			itemType:    itemType,
		}
		outputItems = append(outputItems, item)
		blockToItem[blockIndex] = item
		return item
	}

	// Walk through SSE chunks to fill state
	for _, ch := range chunks {
		root := gjson.ParseBytes(ch)
		ev := root.Get("type").String()

		switch ev {
		case "message_start":
			if msg := root.Get("message"); msg.Exists() {
				responseID = msg.Get("id").String()
				createdAt = time.Now().Unix()
				if usage := msg.Get("usage"); usage.Exists() {
					if v := usage.Get("input_tokens"); v.Exists() {
						inputTokens = v.Int()
					}
				}
			}

		case "content_block_start":
			cb := root.Get("content_block")
			if !cb.Exists() {
				continue
			}
			idx := int(root.Get("index").Int())
			typ := cb.Get("type").String()
			if typ != "text" {
				activeMessageItem = nil
			}
			switch typ {
			case "text":
				item := activeMessageItem
				if item == nil {
					item = newOutputItem("message", idx)
					item.id = fmt.Sprintf("msg_%s_%d", responseID, messageCount)
					messageCount++
				} else {
					blockToItem[idx] = item
				}
				if len(pendingAnnotations) > 0 {
					item.annotations = append(item.annotations, pendingAnnotations...)
					pendingAnnotations = nil
				}
				activeMessageItem = item
			case "tool_use":
				itemType := "function_call"
				if _, isCustomTool := customToolNames[cb.Get("name").String()]; isCustomTool {
					itemType = "custom_tool_call"
				}
				item := newOutputItem(itemType, idx)
				item.callID = cb.Get("id").String()
				if itemType == "custom_tool_call" {
					item.id = fmt.Sprintf("ctc_%s", item.callID)
				} else {
					item.id = fmt.Sprintf("fc_%s", item.callID)
				}
				item.name = cb.Get("name").String()
			case "server_tool_use":
				name := cb.Get("name").String()
				if name != claudeWebSearchToolName {
					log.Debugf("claude->responses: unmapped server_tool_use %q at block %d", name, idx)
					continue
				}
				toolUseID := cb.Get("id").String()
				item := newOutputItem("web_search_call", idx)
				item.id = responsesWebSearchCallID(toolUseID)
				item.callID = toolUseID
				webSearchByToolID[toolUseID] = item
				if input := cb.Get("input"); input.IsObject() && claudeWebSearchQuery(input.Raw) != "" {
					item.args.WriteString(input.Raw)
				}
			case "web_search_tool_result":
				if item := webSearchByToolID[cb.Get("tool_use_id").String()]; item != nil {
					item.results = claudeWebSearchResultsToResponses(cb.Get("content"))
				} else {
					log.Debugf("claude->responses: web_search_tool_result without matching server_tool_use at block %d", idx)
				}
			case "thinking":
				item := newOutputItem("reasoning", idx)
				item.id = fmt.Sprintf("rs_%s_%d", responseID, idx)
				if signature := cb.Get("signature"); signature.Exists() && signature.String() != "" {
					item.signature = signature.String()
				}
			}

		case "content_block_delta":
			d := root.Get("delta")
			if !d.Exists() {
				continue
			}
			idx := int(root.Get("index").Int())
			item := blockToItem[idx]
			dt := d.Get("type").String()
			switch dt {
			case "text_delta":
				if item != nil && item.itemType == "message" {
					if t := d.Get("text"); t.Exists() {
						item.text.WriteString(t.String())
					}
				}
			case "input_json_delta":
				if item != nil && (item.itemType == "function_call" || item.itemType == "custom_tool_call" || item.itemType == "web_search_call") {
					if pj := d.Get("partial_json"); pj.Exists() {
						item.args.WriteString(pj.String())
					}
				}
			case "thinking_delta":
				if item != nil && item.itemType == "reasoning" {
					if t := d.Get("thinking"); t.Exists() {
						item.text.WriteString(t.String())
					}
				}
			case "signature_delta":
				if item != nil && item.itemType == "reasoning" {
					if signature := d.Get("signature"); signature.Exists() && signature.String() != "" {
						item.signature = signature.String()
					}
				}
			case "citations_delta":
				if citation := d.Get("citation"); citation.Exists() {
					if item != nil && item.itemType == "message" {
						item.annotations = append(item.annotations, citation.Value())
					} else if activeMessageItem != nil {
						activeMessageItem.annotations = append(activeMessageItem.annotations, citation.Value())
					} else {
						pendingAnnotations = append(pendingAnnotations, citation.Value())
					}
				}
			}

		case "content_block_stop":
			// Output items are finalized after all deltas have been aggregated.

		case "message_delta":
			if usage := root.Get("usage"); usage.Exists() {
				if v := usage.Get("output_tokens"); v.Exists() {
					outputTokens = v.Int()
				}
			}
			if value := root.Get("delta.stop_reason"); value.Exists() {
				stopReason = value.String()
			}
		}
	}

	// Populate base fields
	terminal := claudeResponsesTerminalState(stopReason)
	out, _ = sjson.SetBytes(out, "id", responseID)
	out, _ = sjson.SetBytes(out, "created_at", createdAt)
	out, _ = sjson.SetBytes(out, "status", terminal.status)
	if len(terminal.incompleteDetail) > 0 {
		out, _ = sjson.SetRawBytes(out, "incomplete_details", terminal.incompleteDetail)
	}
	if terminal.unfinishedTurn {
		out, _ = sjson.SetBytes(out, "end_turn", false)
	}

	// Inject request echo fields as top-level (similar to streaming variant)
	out = util.ExtractResponsesRequestEcho(gjson.ParseBytes(reqBytes)).Apply(out, "")

	// Build output array in the order of the original content blocks.
	outputs := make([][]byte, 0, len(outputItems))
	for i, outputItem := range outputItems {
		itemStatus := "completed"
		if terminal.status == "incomplete" && i == len(outputItems)-1 {
			itemStatus = "incomplete"
		}
		var item []byte
		switch outputItem.itemType {
		case "reasoning":
			item = []byte(`{"id":"","type":"reasoning","status":"completed","encrypted_content":"","summary":[]}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "status", itemStatus)
			item, _ = sjson.SetBytes(item, "encrypted_content", outputItem.signature)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", outputItem.text.String())
			item = translatorcommon.SetRawArrayItems(item, "summary", [][]byte{summary})
		case "message":
			item = []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "status", itemStatus)
			item, _ = sjson.SetBytes(item, "content.0.text", outputItem.text.String())
			if len(outputItem.annotations) > 0 {
				item, _ = sjson.SetBytes(item, "content.0.annotations", outputItem.annotations)
			}
		case "web_search_call":
			item = buildResponsesWebSearchCallItem(outputItem.callID, claudeWebSearchQuery(outputItem.args.String()), outputItem.results)
			item, _ = sjson.SetBytes(item, "status", itemStatus)
		case "function_call", "custom_tool_call":
			if outputItem.itemType == "custom_tool_call" {
				item = []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
				item, _ = sjson.SetBytes(item, "id", outputItem.id)
				item, _ = sjson.SetBytes(item, "status", itemStatus)
				item, _ = sjson.SetBytes(item, "input", unwrapCustomToolInput(outputItem.args.String()))
				item, _ = sjson.SetBytes(item, "call_id", outputItem.callID)
				item = applyResponsesFunctionCallNamespaceFields(item, requestWinners, outputItem.name, "")
				break
			}
			args := outputItem.args.String()
			if args == "" && itemStatus == "completed" {
				args = "{}"
			}
			item = []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "status", itemStatus)
			item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, "arguments", args)
			item, _ = sjson.SetBytes(item, "call_id", outputItem.callID)
			item = applyResponsesFunctionCallNamespaceFields(item, requestWinners, outputItem.name, "")
		}
		if len(item) > 0 {
			outputs = append(outputs, item)
		}
	}
	if len(outputs) > 0 {
		out, _ = sjson.SetRawBytes(out, "output", translatorcommon.JoinRawArray(outputs))
	}

	// Usage
	total := inputTokens + outputTokens
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	out, _ = sjson.SetBytes(out, "usage.total_tokens", total)
	reasoningLength := 0
	for _, outputItem := range outputItems {
		if outputItem.itemType == "reasoning" {
			reasoningLength += outputItem.text.Len()
		}
	}
	if reasoningLength > 0 {
		// Rough estimate similar to chat completions
		reasoningTokens := int64(reasoningLength / 4)
		if reasoningTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.output_tokens_details.reasoning_tokens", reasoningTokens)
		}
	}

	return out
}
