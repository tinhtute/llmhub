// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	multiagentv2 "github.com/therealtinhtute/llmhub/internal/client/codex/optimize-multi-agent-v2"
	. "github.com/therealtinhtute/llmhub/internal/constant"
	"github.com/therealtinhtute/llmhub/internal/interfaces"
	"github.com/therealtinhtute/llmhub/internal/registry"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func writeResponsesSSEChunk(w io.Writer, chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}
	if _, err := w.Write(chunk); err != nil {
		return
	}
	if bytes.HasSuffix(chunk, []byte("\n\n")) || bytes.HasSuffix(chunk, []byte("\r\n\r\n")) {
		return
	}
	suffix := []byte("\n\n")
	if bytes.HasSuffix(chunk, []byte("\r\n")) {
		suffix = []byte("\r\n")
	} else if bytes.HasSuffix(chunk, []byte("\n")) {
		suffix = []byte("\n")
	}
	if _, err := w.Write(suffix); err != nil {
		return
	}
}

type responsesSSEFramer struct {
	pending              []byte
	nextFramePrefix      []byte
	nextFieldSuffix      []byte
	syntheticBreaks      []int
	isCodexClient        bool
	outputItems          map[int][]byte
	outputOrder          []int
	unindexedOutputItems [][]byte
	lastEvent            string
	terminalEvent        string
	terminalError        *interfaces.ErrorMessage
	failureEvent         string
	dataFrames           int
}

func (f *responsesSSEFramer) WriteChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 || f.terminalEvent != "" {
		return
	}
	if len(f.nextFieldSuffix) > 0 {
		trimmed := bytes.TrimLeft(chunk, " \t\r\n")
		_, complete, _ := responsesSSEChunkField(trimmed)
		lastLine := bytes.TrimSpace(f.nextFramePrefix[bytes.LastIndexByte(f.nextFramePrefix, '\n')+1:])
		if complete && len(lastLine) > 0 && !bytes.HasPrefix(lastLine, []byte("data:")) {
			f.nextFramePrefix = append(f.nextFramePrefix, f.nextFieldSuffix...)
			f.nextFieldSuffix = nil
		} else {
			f.nextFieldSuffix = append(f.nextFieldSuffix, chunk...)
			trimmed = bytes.TrimLeft(f.nextFieldSuffix, " \t\r\n")
			_, _, partial := responsesSSEChunkField(trimmed)
			if partial {
				return
			}
			chunk = f.nextFieldSuffix
			f.nextFieldSuffix = nil
		}
	}
	if len(f.nextFramePrefix) > 0 {
		payload, found := responsesSSEDataPayload(f.nextFramePrefix)
		if found && json.Valid(payload) && gjson.GetBytes(payload, "type").String() == "" &&
			!responsesSSEHasField(f.nextFramePrefix, []byte("event:")) && responsesSSEFrameLen(f.nextFramePrefix) == 0 {
			trimmed := bytes.TrimLeft(chunk, " \t\r\n")
			field, complete, partial := responsesSSEChunkField(trimmed)
			lastLine := bytes.TrimSpace(f.nextFramePrefix[bytes.LastIndexByte(f.nextFramePrefix, '\n')+1:])
			_, _, lastPartial := responsesSSEChunkField(lastLine)
			if complete && lastPartial {
				joined := append(bytes.Clone(lastLine), trimmed...)
				joinedField, joinedComplete, _ := responsesSSEChunkField(joined)
				if joinedComplete && (joinedField == "event" || joinedField == "data" || joinedField == "id" || joinedField == "retry" ||
					(field != "" && field != "event" && field != "data" && field != "id" && field != "retry")) {
					complete = false
				}
			}
			newFieldPrefix := bytes.HasPrefix(lastLine, []byte("data:"))
			for _, prefix := range [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:")} {
				newFieldPrefix = newFieldPrefix || (len(trimmed) < len(prefix) && bytes.HasPrefix(prefix, trimmed))
			}
			if partial && newFieldPrefix {
				f.nextFieldSuffix = append(f.nextFieldSuffix, chunk...)
				return
			}
			if complete && !bytes.HasSuffix(f.nextFramePrefix, []byte("\n")) && !bytes.HasSuffix(f.nextFramePrefix, []byte("\r")) &&
				chunk[0] != '\n' && chunk[0] != '\r' {
				f.nextFramePrefix = append(f.nextFramePrefix, '\n')
			}
		}
		f.nextFramePrefix = append(f.nextFramePrefix, chunk...)
		chunk = f.nextFramePrefix
		f.nextFramePrefix = nil
	}
	if responsesSSEHasEmptyEventData(f.pending) {
		trimmed := bytes.TrimLeft(chunk, " \t\r\n")
		field, complete, partial := responsesSSEChunkField(trimmed)
		if partial {
			f.nextFramePrefix = append(f.nextFramePrefix, chunk...)
			return
		}
		newFrame := complete && field != "data"
		if complete && field == "data" {
			payload, _ := responsesSSEDataPayload(trimmed)
			valid := json.Valid(payload)
			if !valid && !bytes.Equal(payload, []byte("[DONE]")) && responsesSSEFrameLen(trimmed) == 0 {
				f.nextFramePrefix = append(f.nextFramePrefix, chunk...)
				return
			}
			hasEvent := responsesSSEHasField(trimmed, []byte("event:"))
			if valid && hasEvent && responsesSSEEventName(trimmed) == "" && responsesSSEFrameLen(trimmed) == 0 {
				f.nextFramePrefix = append(f.nextFramePrefix, chunk...)
				return
			}
			newFrame = bytes.Equal(payload, []byte("[DONE]")) ||
				(valid && (gjson.GetBytes(payload, "type").String() != "" || hasEvent))
			if valid && !newFrame && responsesSSEFrameLen(trimmed) == 0 {
				f.nextFramePrefix = append(f.nextFramePrefix, chunk...)
				return
			}
		}
		if newFrame {
			f.writeFrame(w, f.pending)
			f.pending = f.pending[:0]
			f.syntheticBreaks = f.syntheticBreaks[:0]
			if f.terminalEvent != "" {
				return
			}
		}
	}
	if responsesSSEStartsNewDataFrame(f.pending, chunk) {
		f.writeFrame(w, f.pending)
		f.pending = f.pending[:0]
		f.syntheticBreaks = f.syntheticBreaks[:0]
		if f.terminalEvent != "" {
			return
		}
	}
	f.pending = responsesSSEAppendChunk(f.pending, &f.syntheticBreaks, chunk)
	for {
		frameLen := responsesSSEFrameLen(f.pending)
		if frameLen == 0 {
			break
		}
		f.writeFrame(w, f.pending[:frameLen])
		copy(f.pending, f.pending[frameLen:])
		f.pending = f.pending[:len(f.pending)-frameLen]
		f.syntheticBreaks = responsesSSEConsumeBreaks(f.syntheticBreaks, frameLen)
		if f.terminalEvent != "" {
			f.pending = f.pending[:0]
			return
		}
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		f.syntheticBreaks = f.syntheticBreaks[:0]
		return
	}
	if len(f.pending) == 0 || responsesSSEHasEmptyEventData(f.pending) || !responsesSSECanEmitWithoutDelimiter(f.pending) {
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
	f.syntheticBreaks = f.syntheticBreaks[:0]
}

func (f *responsesSSEFramer) Flush(w io.Writer) {
	if len(f.nextFieldSuffix) > 0 {
		if responsesSSENeedsLineBreak(f.nextFramePrefix, f.nextFieldSuffix) {
			f.nextFramePrefix = append(f.nextFramePrefix, '\n')
		}
		f.nextFramePrefix = append(f.nextFramePrefix, f.nextFieldSuffix...)
		f.nextFieldSuffix = nil
	}
	if len(f.nextFramePrefix) > 0 {
		trimmed := bytes.TrimLeft(f.nextFramePrefix, " \t\r\n")
		_, _, partial := responsesSSEChunkField(trimmed)
		if partial && !bytes.HasPrefix([]byte("data"), trimmed) {
			f.writeFrame(w, f.pending)
			f.pending = f.pending[:0]
			f.syntheticBreaks = f.syntheticBreaks[:0]
		}
		f.pending = responsesSSEAppendChunk(f.pending, &f.syntheticBreaks, f.nextFramePrefix)
		f.nextFramePrefix = nil
	}
	if len(f.pending) == 0 || f.terminalEvent != "" {
		return
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	frame := responsesSSERecoverJSONFrame(f.pending, f.syntheticBreaks)
	if !responsesSSECanFlushWithoutDelimiter(frame) {
		f.pending = f.pending[:0]
		return
	}
	f.writeFrame(w, frame)
	f.pending = f.pending[:0]
	f.syntheticBreaks = f.syntheticBreaks[:0]
}

func (f *responsesSSEFramer) writeFrame(w io.Writer, frame []byte) {
	writeResponsesSSEChunk(w, f.repairFrame(responsesSSERecoverJSONFrame(frame, f.syntheticBreaks)))
}
func (f *responsesSSEFramer) shouldFilterPrivateEvent(streamEvent, payloadType string) bool {
	check := func(name string) bool {
		name = strings.TrimSpace(name)
		if name == "" {
			return false
		}
		if responsesSSEErrorEvent(name) {
			return false
		}

		// Always filter internal WebSocket timing telemetry and Meta subscription usage from SSE streams.
		if strings.HasPrefix(name, "responsesapi.") || name == "response.subscription_usage" {
			return true
		}

		// The Codex backend's moderation metadata is not a public Responses event:
		// clients that parse a closed event list fail the whole turn on it.
		if name == "response.metadata" && !f.isCodexClient {
			return true
		}

		return false
	}

	return check(streamEvent) || check(payloadType)
}

func (f *responsesSSEFramer) repairFrame(frame []byte) []byte {
	payload, ok := responsesSSEDataPayload(frame)
	streamEvent := responsesSSEEventName(frame)
	if streamEvent != "" && f.shouldFilterPrivateEvent(streamEvent, "") {
		return nil
	}
	if !ok || len(payload) == 0 {
		return frame
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		f.dataFrames++
		return frame
	}
	payloadType := gjson.GetBytes(payload, "type").String()
	if f.shouldFilterPrivateEvent(streamEvent, payloadType) {
		return nil
	}
	if !json.Valid(payload) {
		return frame
	}
	if payloadType == "" && gjson.GetBytes(payload, "subscription").IsObject() {
		return nil
	}

	f.dataFrames++
	if responsesSSEErrorEvent(payloadType) || responsesSSEPayloadHasError(payload) {
		if payloadType != "" {
			f.lastEvent = sanitizeResponsesStreamEventName(payloadType)
		}
		return f.repairErrorPayload(payload)
	}
	eventType := payloadType
	if responsesSSETerminalEvent(streamEvent) {
		eventType = streamEvent
	} else if eventType == "" {
		eventType = streamEvent
	}
	if eventType != "" {
		f.lastEvent = sanitizeResponsesStreamEventName(eventType)
	}
	if responsesSSEErrorEvent(eventType) {
		return f.repairErrorPayload(payload)
	}
	if responsesSSETerminalEvent(eventType) {
		f.terminalEvent = eventType
	}

	switch eventType {
	case "response.output_item.done":
		f.recordOutputItem(payload)
	case "response.completed":
		repaired := f.repairCompletedPayload(payload)
		if !bytes.Equal(repaired, payload) {
			return responsesSSEFrameWithData(frame, repaired)
		}
	}
	return frame
}

func responsesSSEPayloadErrorMessage(payload []byte) *interfaces.ErrorMessage {
	status := http.StatusBadGateway
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		candidate := int(gjson.GetBytes(payload, path).Int())
		if candidate >= http.StatusBadRequest && candidate <= 599 {
			status = candidate
			break
		}
	}
	return sanitizeResponsesStreamErrorMessage(&interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", payload)})
}

func (f *responsesSSEFramer) repairErrorPayload(payload []byte) []byte {
	errMsg := responsesSSEPayloadErrorMessage(payload)
	status := errMsg.StatusCode
	f.terminalError = errMsg
	failureEvent := f.failureEvent
	if failureEvent != "response.failed" {
		failureEvent = "error"
	}
	f.terminalEvent = failureEvent
	errText := responsesStreamErrorText(errMsg, status)
	if failureEvent == "response.failed" {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, 0)
		return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\n", chunk))
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
	return []byte(fmt.Sprintf("event: error\ndata: %s\n\n", chunk))
}

func responsesSSEErrorEvent(eventType string) bool {
	switch eventType {
	case "response.failed", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSETerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.done", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSEPayloadHasError(payload []byte) bool {
	for _, path := range []string{"error", "response.error"} {
		result := gjson.GetBytes(payload, path)
		if result.Exists() && result.Type != gjson.Null {
			return true
		}
	}
	return gjson.GetBytes(payload, "code").Exists() && gjson.GetBytes(payload, "message").Exists()
}

func responsesSSEDataPayload(frame []byte) ([]byte, bool) {
	var payload []byte
	found := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if found {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
		found = true
	}
	return payload, found
}

func responsesSSERecoverJSONFrame(frame []byte, syntheticBreaks []int) []byte {
	if len(syntheticBreaks) == 0 {
		return frame
	}
	payload, found := responsesSSEDataPayload(frame)
	if found && json.Valid(payload) {
		return frame
	}

	// Consider only newlines inserted at chunk boundaries when recovering invalid JSON.
	positions := make([]int, 0, len(syntheticBreaks))
	for _, pos := range syntheticBreaks {
		if pos >= 0 && pos < len(frame) && frame[pos] == '\n' {
			positions = append(positions, pos)
		}
	}
	try := func(candidate []byte) []byte {
		recovered, ok := responsesSSEDataPayload(candidate)
		if !ok || !json.Valid(recovered) {
			return nil
		}
		return responsesSSEFrameWithData(candidate, recovered)
	}
	for i, pos := range positions {
		if i == 8 {
			break
		}
		candidate := make([]byte, 0, len(frame)-1)
		candidate = append(candidate, frame[:pos]...)
		candidate = append(candidate, frame[pos+1:]...)
		if recovered := try(candidate); recovered != nil {
			return recovered
		}
	}
	if len(positions) > 1 {
		candidate := make([]byte, 0, len(frame)-len(positions))
		start := 0
		for _, pos := range positions {
			candidate = append(candidate, frame[start:pos]...)
			start = pos + 1
		}
		candidate = append(candidate, frame[start:]...)
		if recovered := try(candidate); recovered != nil {
			return recovered
		}
	}
	return frame
}

func responsesSSEAppendChunk(pending []byte, syntheticBreaks *[]int, chunk []byte) []byte {
	if responsesSSENeedsLineBreak(pending, chunk) {
		*syntheticBreaks = append(*syntheticBreaks, len(pending))
		pending = append(pending, '\n')
	}
	return append(pending, chunk...)
}

func responsesSSEConsumeBreaks(syntheticBreaks []int, consumed int) []int {
	remaining := syntheticBreaks[:0]
	for _, pos := range syntheticBreaks {
		if pos >= consumed {
			remaining = append(remaining, pos-consumed)
		}
	}
	return remaining
}

func responsesSSEFrameWithData(frame, payload []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		out.WriteString("data: ")
		out.Write(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func (f *responsesSSEFramer) recordOutputItem(payload []byte) {
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() == "" {
		return
	}

	if outputIndex := gjson.GetBytes(payload, "output_index"); outputIndex.Exists() {
		index := int(outputIndex.Int())
		if f.outputItems == nil {
			f.outputItems = make(map[int][]byte)
		}
		if _, exists := f.outputItems[index]; !exists {
			f.outputOrder = append(f.outputOrder, index)
		}
		f.outputItems[index] = append([]byte(nil), item.Raw...)
		return
	}

	f.unindexedOutputItems = append(f.unindexedOutputItems, append([]byte(nil), item.Raw...))
}

func (f *responsesSSEFramer) repairCompletedPayload(payload []byte) []byte {
	if len(f.outputOrder) == 0 && len(f.unindexedOutputItems) == 0 {
		return payload
	}
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && (!output.IsArray() || len(output.Array()) > 0) {
		return payload
	}

	var outputJSON bytes.Buffer
	outputJSON.WriteByte('[')
	indexes := append([]int(nil), f.outputOrder...)
	sort.Ints(indexes)
	written := 0
	for _, index := range indexes {
		item, ok := f.outputItems[index]
		if !ok {
			continue
		}
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	for _, item := range f.unindexedOutputItems {
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	outputJSON.WriteByte(']')

	repaired, err := sjson.SetRawBytes(payload, "response.output", outputJSON.Bytes())
	if err != nil {
		return payload
	}
	return repaired
}

func responsesSSEFrameLen(chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}
	lf := bytes.Index(chunk, []byte("\n\n"))
	crlf := bytes.Index(chunk, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return 0
		}
		return crlf + 4
	case crlf < 0:
		return lf + 2
	case lf < crlf:
		return lf + 2
	default:
		return crlf + 4
	}
}

func responsesSSENeedsMoreData(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	return responsesSSEHasField(trimmed, []byte("event:")) && !responsesSSEHasField(trimmed, []byte("data:"))
}

func responsesSSEHasField(chunk []byte, prefix []byte) bool {
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func responsesSSECanEmitWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || responsesSSENeedsMoreData(trimmed) ||
		!responsesSSEHasField(trimmed, []byte("event:")) || !responsesSSEHasField(trimmed, []byte("data:")) {
		return false
	}
	return responsesSSEDataLinesValid(trimmed)
}

func responsesSSEChunkField(chunk []byte) (name string, complete, partial bool) {
	if len(chunk) == 0 {
		return "", false, false
	}
	for i, b := range chunk {
		if b == ':' || b == '\n' || b == '\r' {
			return string(chunk[:i]), true, false
		}
		if !(b >= 'a' && b <= 'z') && !(b >= 'A' && b <= 'Z') && !(b >= '0' && b <= '9') && b != '-' && b != '_' && b != '.' {
			return "", false, false
		}
	}
	return "", false, true
}

func responsesSSEHasEmptyEventData(chunk []byte) bool {
	if !responsesSSEHasField(chunk, []byte("event:")) {
		return false
	}
	payload, found := responsesSSEDataPayload(chunk)
	return found && len(bytes.TrimSpace(payload)) == 0
}

func responsesSSECanFlushWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	return len(trimmed) > 0 && responsesSSEHasField(trimmed, []byte("data:")) && responsesSSEDataLinesValid(trimmed)
}

func responsesSSEStartsNewDataFrame(pending, chunk []byte) bool {
	trimmedPending := bytes.TrimSpace(pending)
	if len(trimmedPending) == 0 || responsesSSEHasField(trimmedPending, []byte("event:")) ||
		!responsesSSEHasField(trimmedPending, []byte("data:")) || !responsesSSEDataLinesValid(trimmedPending) {
		return false
	}
	trimmedChunk := bytes.TrimLeft(chunk, " \t\r\n")
	return bytes.HasPrefix(trimmedChunk, []byte("data:"))
}

func responsesSSEEventName(frame []byte) string {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			return strings.TrimSpace(string(trimmed[len("event:"):]))
		}
	}
	return ""
}

func responsesSSEDataLinesValid(chunk []byte) bool {
	payload, found := responsesSSEDataPayload(chunk)
	if !found {
		return true
	}
	payload = bytes.TrimSpace(payload)
	return len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload)
}

func responsesSSENeedsLineBreak(pending, chunk []byte) bool {
	if len(pending) == 0 || len(chunk) == 0 {
		return false
	}
	if bytes.HasSuffix(pending, []byte("\n")) || bytes.HasSuffix(pending, []byte("\r")) {
		return false
	}
	if chunk[0] == '\n' || chunk[0] == '\r' {
		return false
	}
	trimmed := bytes.TrimLeft(chunk, " \t")
	if len(trimmed) == 0 {
		return false
	}
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(trimmed, prefix) {
			return !responsesSSEDataEndsInsideJSONString(pending)
		}
	}
	return false
}

func responsesSSEDataEndsInsideJSONString(chunk []byte) bool {
	payload, found := responsesSSEDataPayload(chunk)
	payload = bytes.TrimSpace(payload)
	if !found || len(payload) == 0 || (payload[0] != '{' && payload[0] != '[' && payload[0] != '"') {
		return false
	}
	inString := false
	escaped := false
	for _, b := range payload {
		if escaped {
			escaped = false
			continue
		}
		if inString && b == '\\' {
			escaped = true
			continue
		}
		if b == '"' {
			inString = !inString
		}
	}
	return inString
}

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
	// AccessManager re-authorizes each websocket turn. A zero value leaves the
	// upgrade-time authentication as the only check (upstream CLIProxyAPI
	// commit b9bb031dcbb0).
	AccessManager *sdkaccess.Manager
}

const codexAlphaSearchHandlerType = "codex-alpha-search"

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   h.Models(),
	})
}

// prepareCodexOrphanDelegation rewrites orphan Codex delegation outputs into
// standard user messages when codex-orphan-delegation-compatibility is enabled
// and the request carries X-Openai-Subagent: collab_spawn
// (upstream 291cfb87efac).
func (h *OpenAIResponsesAPIHandler) prepareCodexOrphanDelegation(c *gin.Context, payload []byte) []byte {
	if h == nil || h.Cfg == nil || !h.Cfg.CodexOrphanDelegationCompatibility {
		return payload
	}
	requestCtx := context.Background()
	var requestHeaders http.Header
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
		requestHeaders = c.Request.Header
	}
	requestCtx = context.WithValue(requestCtx, "gin", c)
	return multiagentv2.RewriteCodexOrphanDelegationInput(requestCtx, requestHeaders, payload, true)
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	rawJSON = h.prepareCodexOrphanDelegation(c, rawJSON)

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

func (h *OpenAIResponsesAPIHandler) AlphaSearch(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	if gjson.GetBytes(rawJSON, "stream").Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for alpha search",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, codexAlphaSearchHandlerType, modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	rawJSON = h.prepareCodexOrphanDelegation(c, rawJSON)

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "responses/compact")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	isCodexClient := isCodexResponsesClientRequest(c)
	failureEvent := "error"
	if isCodexClient {
		failureEvent = "response.failed"
	}
	framer := &responsesSSEFramer{failureEvent: failureEvent, isCodexClient: isCodexClient}
	var initialOutput bytes.Buffer

	// Peek at the first complete SSE data frame.
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			framer.Flush(&initialOutput)
			safeErrMsg := sanitizeResponsesStreamErrorMessage(errMsg)
			if framer.dataFrames == 0 {
				safeErrMsg = sanitizeResponsesInitialErrorMessage(errMsg)
			}
			if safeErrMsg != nil && framer.dataFrames > 0 {
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write(initialOutput.Bytes())
				flusher.Flush()
				pendingErrors := make(chan *interfaces.ErrorMessage, 1)
				pendingErrors <- safeErrMsg
				close(pendingErrors)
				h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, make(chan []byte), pendingErrors, framer)
				return
			}
			// Upstream failed before a complete SSE data frame. Return JSON.
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), safeErrMsg)
			h.WriteErrorResponse(c, safeErrMsg)
			if safeErrMsg != nil {
				cliCancel(safeErrMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				framer.Flush(&initialOutput)
				errMsg, hasPendingError := handlers.PendingStreamError(errChan)
				if !hasPendingError && framer.terminalEvent == "" {
					message := "upstream stream closed before first payload"
					if framer.dataFrames > 0 {
						message = "upstream stream closed before a terminal event"
					}
					errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("%s", message)}
				}
				if framer.dataFrames > 0 {
					errMsg = sanitizeResponsesStreamErrorMessage(errMsg)
				} else {
					errMsg = sanitizeResponsesInitialErrorMessage(errMsg)
				}

				if framer.dataFrames > 0 {
					setSSEHeaders()
					handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
					_, _ = c.Writer.Write(initialOutput.Bytes())
					flusher.Flush()
					if framer.terminalError != nil {
						h.logResponsesStreamError(c, framer, framer.terminalError)
						cliCancel(framer.terminalError.Error)
						return
					}
					if errMsg == nil {
						cliCancel(nil)
						return
					}
					pendingErrors := make(chan *interfaces.ErrorMessage, 1)
					pendingErrors <- errMsg
					close(pendingErrors)
					h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, make(chan []byte), pendingErrors, framer)
					return
				}

				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				h.WriteErrorResponse(c, errMsg)
				if errMsg != nil {
					cliCancel(errMsg.Error)
				} else {
					cliCancel(nil)
				}
				return
			}

			framer.WriteChunk(&initialOutput, chunk)
			if framer.dataFrames == 0 {
				continue
			}

			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			_, _ = c.Writer.Write(initialOutput.Bytes())
			flusher.Flush()
			if framer.terminalError != nil {
				h.logResponsesStreamError(c, framer, framer.terminalError)
				cliCancel(framer.terminalError.Error)
				return
			}

			h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, framer)
			return
		}
	}
}

// isCodexResponsesClientRequest limits the alternate terminal event to official Codex clients.
func isCodexResponsesClientRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if multiagentv2.IsCodexClientUserAgent(c.GetHeader("User-Agent")) {
		return true
	}

	switch originator := strings.ToLower(strings.TrimSpace(c.GetHeader("Originator"))); originator {
	case "codex desktop", "codex-tui", "codex_cli_rs":
		return true
	default:
		return strings.HasPrefix(originator, "codex desktop/") || strings.HasPrefix(originator, "codex-tui/") || strings.HasPrefix(originator, "codex_cli_rs/")
	}
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, framer *responsesSSEFramer) {
	if framer == nil {
		framer = &responsesSSEFramer{}
	}
	framer.isCodexClient = isCodexResponsesClientRequest(c)
	if framer.isCodexClient {
		framer.failureEvent = "response.failed"
	} else {
		framer.failureEvent = "error"
	}
	writeTerminalError := func(errMsg *interfaces.ErrorMessage) {
		framer.Flush(c.Writer)
		if errMsg == nil {
			return
		}
		status := http.StatusInternalServerError
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
		}
		errText := responsesStreamErrorText(errMsg, status)
		h.logResponsesStreamError(c, framer, errMsg)
		if framer.terminalEvent != "" {
			return
		}
		if isCodexResponsesClientRequest(c) {
			chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, 0)
			_, _ = fmt.Fprintf(c.Writer, "\nevent: response.failed\ndata: %s\n\n", string(chunk))
			return
		}
		chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
		_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(chunk))
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		NormalizeTerminalError: sanitizeResponsesStreamErrorMessage,
		WriteChunk: func(chunk []byte) {
			framer.WriteChunk(c.Writer, chunk)
		},
		ChunkError: func() *interfaces.ErrorMessage {
			if framer.terminalError != nil {
				h.logResponsesStreamError(c, framer, framer.terminalError)
			}
			return framer.terminalError
		},
		WriteTerminalError: writeTerminalError,
		CloseError: func() *interfaces.ErrorMessage {
			framer.Flush(c.Writer)
			if framer.terminalError != nil {
				return framer.terminalError
			}
			if framer.terminalEvent != "" {
				return nil
			}
			lastEvent := framer.lastEvent
			if lastEvent == "" {
				lastEvent = "none"
			}
			return &interfaces.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      fmt.Errorf("upstream stream closed before a terminal event (last event: %s)", lastEvent),
			}
		},
		WriteDone: func() {
			framer.Flush(c.Writer)
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}

const (
	responsesStreamErrorMessageLimit = 2048
	responsesStreamErrorFieldLimit   = 256
)

var (
	responsesStreamSensitiveValuePattern = regexp.MustCompile(`(?i)((?:"?(?:api[_-]?key|access[_-]?token|token|authorization|secret)"?)\s*[=:]\s*"?)([^\s"&,;}]+)`)
	responsesStreamBearerPattern         = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

func truncateResponsesStreamErrorText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func redactResponsesStreamErrorText(text string) string {
	text = responsesStreamSensitiveValuePattern.ReplaceAllString(text, `${1}[REDACTED]`)
	return responsesStreamBearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
}

func sanitizeResponsesStreamEventName(eventName string) string {
	return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(strings.TrimSpace(eventName)), responsesStreamErrorFieldLimit)
}

func responsesStreamErrorText(errMsg *interfaces.ErrorMessage, status int) string {
	text := http.StatusText(status)
	if errMsg != nil && errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		text = strings.TrimSpace(errMsg.Error.Error())
	}
	if !json.Valid([]byte(text)) {
		return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(text), responsesStreamErrorMessageLimit)
	}

	root := gjson.Parse(text)
	errorNode := root.Get("error")
	if !errorNode.Exists() || !errorNode.IsObject() {
		errorNode = root.Get("response.error")
	}
	if errorNode.Exists() && errorNode.IsObject() {
		safe := []byte(`{"error":{}}`)
		copied := false
		// Ported from upstream CLIProxyAPI commit aedc9e6a3987: forward the
		// boolean retryable classification alongside the scalar error fields.
		for _, field := range []string{"type", "code", "message", "param", "retryable"} {
			value := errorNode.Get(field)
			if !value.Exists() || value.Type == gjson.Null {
				continue
			}
			if field == "retryable" {
				safe, _ = sjson.SetBytes(safe, "error.retryable", value.Bool())
				copied = true
				continue
			}
			limit := responsesStreamErrorFieldLimit
			if field == "message" {
				limit = responsesStreamErrorMessageLimit
			}
			safe, _ = sjson.SetBytes(safe, "error."+field, truncateResponsesStreamErrorText(redactResponsesStreamErrorText(value.String()), limit))
			copied = true
		}
		if copied {
			return string(safe)
		}
	}

	safe := []byte(`{"type":"error"}`)
	copied := false
	for _, field := range []string{"code", "message", "param", "retryable"} {
		value := root.Get(field)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		if field == "retryable" {
			safe, _ = sjson.SetBytes(safe, "retryable", value.Bool())
			copied = true
			continue
		}
		limit := responsesStreamErrorFieldLimit
		if field == "message" {
			limit = responsesStreamErrorMessageLimit
		}
		safe, _ = sjson.SetBytes(safe, field, truncateResponsesStreamErrorText(redactResponsesStreamErrorText(value.String()), limit))
		copied = true
	}
	if copied {
		return string(safe)
	}
	return http.StatusText(status)
}

type responsesStreamSanitizedError struct {
	message string
	cause   error
}

func (e *responsesStreamSanitizedError) Error() string { return e.message }
func (e *responsesStreamSanitizedError) Unwrap() error { return e.cause }

func sanitizeResponsesInitialErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	// Marked direct responses carry a trusted preformatted body; forward them
	// untouched instead of collapsing them into a sanitized envelope (upstream
	// 9a2201c36a0a).
	if errMsg != nil && errMsg.DirectResponse {
		return errMsg
	}
	return sanitizeResponsesStreamErrorMessage(errMsg)
}

func sanitizeResponsesStreamErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg == nil {
		return nil
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	safe := *errMsg
	safe.StatusCode = status
	safe.Error = &responsesStreamSanitizedError{message: responsesStreamErrorText(errMsg, status), cause: errMsg.Error}
	safe.DirectResponse = false
	safe.Body = nil
	return &safe
}

func (h *OpenAIResponsesAPIHandler) logResponsesStreamError(c *gin.Context, framer *responsesSSEFramer, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	lastEvent := "none"
	if framer != nil && framer.lastEvent != "" {
		lastEvent = framer.lastEvent
	}
	errText := responsesStreamErrorText(errMsg, status)
	h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      fmt.Errorf("responses stream terminated after %s: %s", lastEvent, errText),
	})
}
