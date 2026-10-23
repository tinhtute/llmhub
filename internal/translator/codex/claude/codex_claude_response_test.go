package claude

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToClaude_StreamThinkingIncludesSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_123\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	startFound := false
	signatureDeltaFound := false
	stopFound := false

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() == "thinking" {
					startFound = true
					if data.Get("content_block.signature").Exists() {
						t.Fatalf("thinking start block should NOT have signature field when signature is unknown: %s", line)
					}
				}
			case "content_block_delta":
				if data.Get("delta.type").String() == "signature_delta" {
					signatureDeltaFound = true
					if got := data.Get("delta.signature").String(); got != "enc_sig_123" {
						t.Fatalf("unexpected signature delta: %q", got)
					}
				}
			case "content_block_stop":
				stopFound = true
			}
		}
	}

	if !startFound {
		t.Fatal("expected thinking content_block_start event")
	}
	if !signatureDeltaFound {
		t.Fatal("expected signature_delta event for thinking block")
	}
	if !stopFound {
		t.Fatal("expected content_block_stop event for thinking block")
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingWithoutReasoningItemStillIncludesSignatureField(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	thinkingStartFound := false
	thinkingStopFound := false
	signatureDeltaFound := false

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				thinkingStartFound = true
				if data.Get("content_block.signature").Exists() {
					t.Fatalf("thinking start block should NOT have signature field without encrypted_content: %s", line)
				}
			}
			if data.Get("type").String() == "content_block_stop" && data.Get("index").Int() == 0 {
				thinkingStopFound = true
			}
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaFound = true
			}
		}
	}

	if !thinkingStartFound {
		t.Fatal("expected thinking content_block_start event")
	}
	if !thinkingStopFound {
		t.Fatal("expected thinking content_block_stop event")
	}
	if signatureDeltaFound {
		t.Fatal("did not expect signature_delta without encrypted_content")
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingFinalizesPendingBlockBeforeNextSummaryPart(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	startCount := 0
	stopCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				startCount++
			}
			if data.Get("type").String() == "content_block_stop" {
				stopCount++
			}
		}
	}

	if startCount != 2 {
		t.Fatalf("expected 2 thinking block starts, got %d", startCount)
	}
	if stopCount != 1 {
		t.Fatalf("expected pending thinking block to be finalized before second start, got %d stops", stopCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingRetainsSignatureAcrossMultipartReasoning(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_multipart\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Second part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaCount++
				if got := data.Get("delta.signature").String(); got != "enc_sig_multipart" {
					t.Fatalf("unexpected signature delta: %q", got)
				}
			}
		}
	}

	if signatureDeltaCount != 2 {
		t.Fatalf("expected signature_delta for both multipart thinking blocks, got %d", signatureDeltaCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingUsesEarlyCapturedSignatureWhenDoneOmitsIt(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_early\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "signature_delta" {
				signatureDeltaCount++
				if got := data.Get("delta.signature").String(); got != "enc_sig_early" {
					t.Fatalf("unexpected signature delta: %q", got)
				}
			}
		}
	}

	if signatureDeltaCount != 1 {
		t.Fatalf("expected signature_delta from early-captured signature, got %d", signatureDeltaCount)
	}
}

func TestConvertCodexResponseToClaude_StreamThinkingUsesFinalDoneSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_initial\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_final\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	signatureDeltaCount := 0
	events := []string{}
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "thinking" {
				events = append(events, "thinking_start")
			}
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "thinking_delta" {
				events = append(events, "thinking_delta")
			}
			if data.Get("type").String() == "content_block_stop" && data.Get("index").Int() == 0 {
				events = append(events, "thinking_stop")
			}
			if data.Get("type").String() != "content_block_delta" || data.Get("delta.type").String() != "signature_delta" {
				continue
			}
			events = append(events, "signature_delta")
			signatureDeltaCount++
			if got := data.Get("delta.signature").String(); got != "enc_sig_final" {
				t.Fatalf("signature delta = %q, want final done signature", got)
			}
		}
	}

	if signatureDeltaCount != 1 {
		t.Fatalf("expected one signature_delta, got %d", signatureDeltaCount)
	}
	if got, want := strings.Join(events, ","), "thinking_start,thinking_delta,signature_delta,thinking_stop"; got != want {
		t.Fatalf("thinking event order = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaude_StreamSignatureOnlyReasoningEmitsThinkingSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_initial\"}}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_only\"}}"),
		[]byte("data: {\"type\":\"response.content_part.added\"}"),
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	thinkingStartFound := false
	thinkingDeltaFound := false
	signatureDeltaFound := false
	thinkingStopFound := false
	textStartIndex := int64(-1)
	events := []string{}

	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_start":
				if data.Get("content_block.type").String() == "thinking" {
					events = append(events, "thinking_start")
					thinkingStartFound = true
					if got := data.Get("index").Int(); got != 0 {
						t.Fatalf("thinking block index = %d, want 0", got)
					}
				}
				if data.Get("content_block.type").String() == "text" {
					events = append(events, "text_start")
					textStartIndex = data.Get("index").Int()
				}
			case "content_block_delta":
				switch data.Get("delta.type").String() {
				case "thinking_delta":
					thinkingDeltaFound = true
				case "signature_delta":
					events = append(events, "signature_delta")
					signatureDeltaFound = true
					if got := data.Get("index").Int(); got != 0 {
						t.Fatalf("signature delta index = %d, want 0", got)
					}
					if got := data.Get("delta.signature").String(); got != "enc_sig_only" {
						t.Fatalf("unexpected signature delta: %q", got)
					}
				}
			case "content_block_stop":
				if data.Get("index").Int() == 0 {
					events = append(events, "thinking_stop")
					thinkingStopFound = true
				}
			}
		}
	}

	if !thinkingStartFound {
		t.Fatal("expected signature-only reasoning to start a thinking block")
	}
	if thinkingDeltaFound {
		t.Fatal("did not expect thinking_delta when upstream omitted summary text")
	}
	if !signatureDeltaFound {
		t.Fatal("expected signature_delta from encrypted_content-only reasoning")
	}
	if !thinkingStopFound {
		t.Fatal("expected signature-only thinking block to stop")
	}
	if textStartIndex != 1 {
		t.Fatalf("text block index = %d, want 1 after signature-only thinking block", textStartIndex)
	}
	if got, want := strings.Join(events, ","), "thinking_start,signature_delta,thinking_stop,text_start"; got != want {
		t.Fatalf("signature-only event order = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaudeNonStream_ThinkingIncludesSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"model":"gpt-5",
			"usage":{"input_tokens":10,"output_tokens":20},
			"output":[
				{
					"type":"reasoning",
					"encrypted_content":"enc_sig_nonstream",
					"summary":[{"type":"summary_text","text":"internal reasoning"}]
				},
				{
					"type":"message",
					"content":[{"type":"output_text","text":"final answer"}]
				}
			]
		}
	}`)

	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)

	thinking := parsed.Get("content.0")
	if thinking.Get("type").String() != "thinking" {
		t.Fatalf("expected first content block to be thinking, got %s", thinking.Raw)
	}
	if got := thinking.Get("signature").String(); got != "enc_sig_nonstream" {
		t.Fatalf("expected signature to be preserved, got %q", got)
	}
	if got := thinking.Get("thinking").String(); got != "internal reasoning" {
		t.Fatalf("unexpected thinking text: %q", got)
	}
}

func TestConvertCodexResponseToClaude_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	foundText := false
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "content_block_delta" && data.Get("delta.type").String() == "text_delta" && data.Get("delta.text").String() == "ok" {
				foundText = true
				break
			}
		}
		if foundText {
			break
		}
	}
	if !foundText {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}

func TestConvertCodexResponseToClaude_StreamTextUsesExplicitZeroOutputIndex(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_zero","output_index":0,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_zero","output_index":0,"content_index":0,"delta":"zero"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_zero","output_index":0,"content_index":0}`),
	}
	assertClaudeTextLifecycle(t, chunks, "start:0,delta:0:zero,stop:0")
}

func TestConvertCodexResponseToClaude_StreamTextUsesExplicitNonZeroOutputIndex(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_seven","output_index":7,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_seven","output_index":7,"content_index":0,"delta":"seven"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_seven","output_index":7,"content_index":0}`),
	}
	assertClaudeTextLifecycle(t, chunks, "start:7,delta:7:seven,stop:7")
}

func TestConvertCodexResponseToClaude_StreamTextRetainsProviderIndexWhenLaterEventsOmitIt(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_7","output_index":7,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_7","content_index":0,"delta":"stable"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_7","content_index":0}`),
	}
	assertClaudeTextLifecycle(t, chunks, "start:7,delta:7:stable,stop:7")
}

func TestConvertCodexResponseToClaude_StreamTextMissingOutputIndexUsesSequentialFallback(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_first","content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_first","content_index":0,"delta":"first"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_first","content_index":0}`),
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_second","content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_second","content_index":0,"delta":"second"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_second","content_index":0}`),
	}
	assertClaudeTextLifecycle(t, chunks, "start:0,delta:0:first,stop:0,start:1,delta:1:second,stop:1")
}

func TestConvertCodexResponseToClaude_StreamInterleavedTextItemsKeepProviderIndexes(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"first"}`),
		[]byte(`data: {"type":"response.content_part.added","item_id":"msg_2","output_index":7,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"msg_2","output_index":7,"content_index":0,"delta":"second"}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"msg_2","output_index":7,"content_index":0}`),
	}
	assertClaudeTextLifecycle(t, chunks, "start:0,delta:0:first,stop:0,start:7,delta:7:second,stop:7")
}

func assertClaudeTextLifecycle(t *testing.T, chunks [][]byte, want string) {
	t.Helper()
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any
	var events []string

	for _, chunk := range chunks {
		outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)
		for _, out := range outputs {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := gjson.Parse(strings.TrimPrefix(line, "data: "))
				switch data.Get("type").String() {
				case "content_block_start":
					if data.Get("content_block.type").String() == "text" {
						events = append(events, "start:"+data.Get("index").String())
					}
				case "content_block_delta":
					if data.Get("delta.type").String() == "text_delta" {
						events = append(events, "delta:"+data.Get("index").String()+":"+data.Get("delta.text").String())
					}
				case "content_block_stop":
					events = append(events, "stop:"+data.Get("index").String())
				}
			}
		}
	}

	if got := strings.Join(events, ","); got != want {
		t.Fatalf("text block lifecycle = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaude_StreamSignatureOnlyReasoningClosesActiveText(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.content_part.added","item_id":"message_5","output_index":5,"content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","item_id":"message_5","output_index":5,"content_index":0,"delta":"answer"}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"reasoning_2","type":"reasoning","encrypted_content":"sig_2"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"reasoning_2","type":"reasoning","encrypted_content":"sig_2"}}`),
		[]byte(`data: {"type":"response.content_part.done","item_id":"message_5","content_index":0}`),
	}
	want := `start:text:5,delta:text_delta:5:answer,stop:5,start:thinking:2,delta:signature_delta:2:sig_2,stop:2`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamReasoningOutputIndexSelection(t *testing.T) {
	tests := []struct {
		name         string
		outputIndex  string
		initialIndex int
		wantIndex    string
	}{
		{name: "explicit zero", outputIndex: "0", initialIndex: 4, wantIndex: "0"},
		{name: "explicit non-zero", outputIndex: "7", initialIndex: 1, wantIndex: "7"},
		{name: "missing uses fallback", initialIndex: 3, wantIndex: "3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := ""
			if tt.outputIndex != "" {
				field = `,"output_index":` + tt.outputIndex
			}
			chunks := [][]byte{
				[]byte(`data: {"type":"response.output_item.added"` + field + `,"item":{"id":"reasoning_1","type":"reasoning"}}`),
				[]byte(`data: {"type":"response.reasoning_summary_part.added"` + field + `,"item_id":"reasoning_1"}`),
				[]byte(`data: {"type":"response.reasoning_summary_text.delta"` + field + `,"item_id":"reasoning_1","delta":"think"}`),
				[]byte(`data: {"type":"response.reasoning_summary_part.done"` + field + `,"item_id":"reasoning_1"}`),
				[]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`),
			}
			want := "start:thinking:" + tt.wantIndex + ",delta:thinking_delta:" + tt.wantIndex + ":think,stop:" + tt.wantIndex
			assertClaudeContentLifecycle(t, chunks, tt.initialIndex, want)
		})
	}
}

func TestConvertCodexResponseToClaude_StreamFunctionCallOutputIndexSelection(t *testing.T) {
	tests := []struct {
		name         string
		outputIndex  string
		initialIndex int
		wantIndex    string
	}{
		{name: "explicit zero", outputIndex: "0", initialIndex: 4, wantIndex: "0"},
		{name: "explicit non-zero", outputIndex: "7", initialIndex: 1, wantIndex: "7"},
		{name: "missing uses fallback", initialIndex: 3, wantIndex: "3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := ""
			if tt.outputIndex != "" {
				field = `,"output_index":` + tt.outputIndex
			}
			chunks := [][]byte{
				[]byte(`data: {"type":"response.output_item.added"` + field + `,"item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"lookup"}}`),
				[]byte(`data: {"type":"response.function_call_arguments.delta"` + field + `,"item_id":"function_1","delta":"{\"q\":\"x\"}"}`),
				[]byte(`data: {"type":"response.output_item.done"` + field + `,"item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"lookup"}}`),
			}
			want := "start:tool_use:" + tt.wantIndex + ",delta:input_json_delta:" + tt.wantIndex + ":,delta:input_json_delta:" + tt.wantIndex + `:{"q":"x"},stop:` + tt.wantIndex
			assertClaudeContentLifecycle(t, chunks, tt.initialIndex, want)
		})
	}
}

func TestConvertCodexResponseToClaude_StreamInterleavedReasoningTextAndFunctionCallKeepProviderIndexes(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"reasoning_1","type":"reasoning","encrypted_content":"sig_2"}}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.added","output_index":2,"item_id":"reasoning_1"}`),
		[]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":2,"item_id":"reasoning_1","delta":"think"}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.done","output_index":2,"item_id":"reasoning_1"}`),
		[]byte(`data: {"type":"response.content_part.added","output_index":5,"item_id":"message_1","content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":5,"item_id":"message_1","content_index":0,"delta":"answer"}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":9,"item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"lookup"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":9,"item_id":"function_1","delta":"{\"q\":\"x\"}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":9,"item":{"id":"function_1","type":"function_call","call_id":"call_1","name":"lookup"}}`),
	}
	want := `start:thinking:2,delta:thinking_delta:2:think,delta:signature_delta:2:sig_2,stop:2,start:text:5,delta:text_delta:5:answer,stop:5,start:tool_use:9,delta:input_json_delta:9:,delta:input_json_delta:9:{"q":"x"},stop:9`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamNewReasoningItemFinalizesPendingLifecycle(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"reasoning_2","type":"reasoning","encrypted_content":"sig_2"}}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.added","output_index":2,"item_id":"reasoning_2"}`),
		[]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":2,"item_id":"reasoning_2","delta":"first"}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.done","output_index":2,"item_id":"reasoning_2"}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":7,"item":{"id":"reasoning_7","type":"reasoning","encrypted_content":"sig_7"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"reasoning_2","type":"reasoning","encrypted_content":"sig_2_final"}}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.added","output_index":7,"item_id":"reasoning_7"}`),
		[]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":7,"item_id":"reasoning_7","delta":"second"}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.done","output_index":7,"item_id":"reasoning_7"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":7,"item":{"id":"reasoning_7","type":"reasoning","encrypted_content":"sig_7"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"reasoning_2","type":"reasoning","encrypted_content":"sig_2_late"}}`),
	}
	want := `start:thinking:2,delta:thinking_delta:2:first,delta:signature_delta:2:sig_2,stop:2,start:thinking:7,delta:thinking_delta:7:second,delta:signature_delta:7:sig_7,stop:7`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamTextClosesActiveFunctionCall(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"lookup"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":9,"item_id":"function_9","delta":"{\"q\":\"x\"}"}`),
		[]byte(`data: {"type":"response.content_part.added","output_index":5,"item_id":"message_5","content_index":0}`),
		[]byte(`data: {"type":"response.output_text.delta","output_index":5,"item_id":"message_5","content_index":0,"delta":"answer"}`),
		[]byte(`data: {"type":"response.content_part.done","output_index":5,"item_id":"message_5","content_index":0}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"lookup"}}`),
	}
	want := `start:tool_use:9,delta:input_json_delta:9:,delta:input_json_delta:9:{"q":"x"},stop:9,start:text:5,delta:text_delta:5:answer,stop:5`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamNewFunctionCallClosesPreviousFunctionCall(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":3,"item_id":"function_3","delta":"{}"}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"second"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"second"}}`),
	}
	want := `start:tool_use:3,delta:input_json_delta:3:,delta:input_json_delta:3:{},stop:3,start:tool_use:9,delta:input_json_delta:9:,stop:9`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamInterleavedFunctionCallArgumentsAreSerialized(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":3,"item_id":"function_3","delta":"{\"a\":"}`),
		[]byte(`data: {"type":"response.output_item.added","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"second"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":9,"item_id":"function_9","delta":"{\"b\":2}"}`),
		[]byte(`data: {"type":"response.function_call_arguments.delta","output_index":3,"item_id":"function_3","delta":"1}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first"}}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":9,"item":{"id":"function_9","type":"function_call","call_id":"call_9","name":"second"}}`),
	}
	want := `start:tool_use:3,delta:input_json_delta:3:,delta:input_json_delta:3:{"a":,delta:input_json_delta:3:1},stop:3,start:tool_use:9,delta:input_json_delta:9:,delta:input_json_delta:9:{"b":2},stop:9`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func TestConvertCodexResponseToClaude_StreamTerminalFunctionCallArgumentsAreNotDuplicated(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.output_item.added","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first"}}`),
		[]byte(`data: {"type":"response.function_call_arguments.done","output_index":3,"item_id":"function_3","arguments":"{\"a\":1}"}`),
		[]byte(`data: {"type":"response.output_item.done","output_index":3,"item":{"id":"function_3","type":"function_call","call_id":"call_3","name":"first","arguments":"{\"a\":1}"}}`),
	}
	want := `start:tool_use:3,delta:input_json_delta:3:,delta:input_json_delta:3:{"a":1},stop:3`
	assertClaudeContentLifecycle(t, chunks, 0, want)
}

func assertClaudeContentLifecycle(t *testing.T, chunks [][]byte, initialIndex int, want string) {
	t.Helper()
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any = &ConvertCodexResponseToClaudeParams{BlockIndex: initialIndex}
	var events []string

	for _, chunk := range chunks {
		outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)
		for _, out := range outputs {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := gjson.Parse(strings.TrimPrefix(line, "data: "))
				switch data.Get("type").String() {
				case "content_block_start":
					events = append(events, "start:"+data.Get("content_block.type").String()+":"+data.Get("index").String())
				case "content_block_delta":
					deltaType := data.Get("delta.type").String()
					value := data.Get("delta.thinking").String()
					if deltaType == "text_delta" {
						value = data.Get("delta.text").String()
					} else if deltaType == "input_json_delta" {
						value = data.Get("delta.partial_json").String()
					} else if deltaType == "signature_delta" {
						value = data.Get("delta.signature").String()
					}
					events = append(events, "delta:"+deltaType+":"+data.Get("index").String()+":"+value)
				case "content_block_stop":
					events = append(events, "stop:"+data.Get("index").String())
				}
			}
		}
	}

	if got := strings.Join(events, ","); got != want {
		t.Fatalf("content block lifecycle = %s, want %s", got, want)
	}
}

func TestConvertCodexResponseToClaude_ShortensLongToolUseIDs(t *testing.T) {
	longCallID := "call_" + strings.Repeat("a", 62)
	if len(longCallID) <= 64 {
		t.Fatalf("test setup error: longCallID length = %d, want > 64", len(longCallID))
	}

	t.Run("stream", func(t *testing.T) {
		ctx := context.Background()
		originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
		var param any

		outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"`+longCallID+`","name":"lookup"}}`), &param)

		toolID := ""
		for _, out := range outputs {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := gjson.Parse(strings.TrimPrefix(line, "data: "))
				if data.Get("type").String() == "content_block_start" && data.Get("content_block.type").String() == "tool_use" {
					toolID = data.Get("content_block.id").String()
				}
			}
		}

		if toolID == "" {
			t.Fatalf("missing stream tool_use block. Outputs=%q", outputs)
		}
		if len(toolID) > 64 {
			t.Fatalf("stream tool_use id length = %d, want <= 64: %q", len(toolID), toolID)
		}
		if toolID == longCallID {
			t.Fatalf("stream tool_use id was not shortened: %q", toolID)
		}
	})

	t.Run("nonstream", func(t *testing.T) {
		ctx := context.Background()
		originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
		response := []byte(`{
			"type":"response.completed",
			"response":{
				"id":"resp_1",
				"model":"gpt-5",
				"usage":{"input_tokens":1,"output_tokens":1},
				"output":[{"type":"function_call","call_id":"` + longCallID + `","name":"lookup","arguments":"{}"}]
			}
		}`)

		out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
		toolID := gjson.GetBytes(out, "content.0.id").String()
		if toolID == "" {
			t.Fatalf("missing nonstream tool_use id. Output: %s", string(out))
		}
		if len(toolID) > 64 {
			t.Fatalf("nonstream tool_use id length = %d, want <= 64: %q", len(toolID), toolID)
		}
		if toolID == longCallID {
			t.Fatalf("nonstream tool_use id was not shortened: %q", toolID)
		}
	})
}

func TestConvertCodexResponseToClaude_StreamStopReasonMapping(t *testing.T) {
	tests := []struct {
		name       string
		chunks     [][]byte
		wantReason string
	}{
		{
			name: "Stop maps to end_turn",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "end_turn",
		},
		{
			name: "Incomplete max output maps to max_tokens",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "max_tokens",
		},
		{
			name: "Tool call wins over stop",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}"),
				[]byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "tool_use",
		},
		{
			name: "Content filter maps to Claude refusal",
			chunks: [][]byte{
				[]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"content_filter\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
			},
			wantReason: "refusal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
			var param any
			var outputs [][]byte

			for _, chunk := range tt.chunks {
				outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
			}

			got, ok := findClaudeStreamStopReason(outputs)
			if !ok {
				t.Fatalf("did not find message_delta stop_reason; outputs=%q", outputs)
			}
			if got != tt.wantReason {
				t.Fatalf("stop_reason = %q, want %q. Outputs=%q", got, tt.wantReason, outputs)
			}
		})
	}
}

func TestConvertCodexResponseToClaude_StreamStopSequenceMapping(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	outputs := ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, []byte("data: {\"type\":\"response.completed\",\"response\":{\"stop_reason\":\"stop\",\"stop_sequence\":\"\\nEND\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"), &param)
	messageDelta, ok := findClaudeStreamMessageDelta(outputs)
	if !ok {
		t.Fatalf("did not find message_delta; outputs=%q", outputs)
	}
	if got := messageDelta.Get("delta.stop_reason").String(); got != "stop_sequence" {
		t.Fatalf("stop_reason = %q, want stop_sequence. Outputs=%q", got, outputs)
	}
	if got := messageDelta.Get("delta.stop_sequence").String(); got != "\nEND" {
		t.Fatalf("stop_sequence = %q, want newline END. Outputs=%q", got, outputs)
	}
}

func TestConvertCodexResponseToClaudeNonStream_StopReasonMapping(t *testing.T) {
	tests := []struct {
		name       string
		response   []byte
		wantReason string
	}{
		{
			name: "Stop maps to end_turn",
			response: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "end_turn",
		},
		{
			name: "Incomplete max output maps to max_tokens",
			response: []byte(`{
				"type":"response.incomplete",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"incomplete_details":{"reason":"max_output_tokens"},
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "max_tokens",
		},
		{
			name: "Tool call wins over stop",
			response: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]
				}
			}`),
			wantReason: "tool_use",
		},
		{
			name: "Content filter maps to Claude refusal",
			response: []byte(`{
				"type":"response.incomplete",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"incomplete_details":{"reason":"content_filter"},
					"usage":{"input_tokens":1,"output_tokens":1},
					"output":[]
				}
			}`),
			wantReason: "refusal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`)
			out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, tt.response, nil)
			parsed := gjson.ParseBytes(out)

			if got := parsed.Get("stop_reason").String(); got != tt.wantReason {
				t.Fatalf("stop_reason = %q, want %q. Output: %s", got, tt.wantReason, string(out))
			}
		})
	}
}

func TestConvertCodexResponseToClaudeNonStream_StopSequenceMapping(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5",
			"stop_reason":"stop",
			"stop_sequence":"\nEND",
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[]
		}
	}`)

	out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, response, nil)
	parsed := gjson.ParseBytes(out)

	if got := parsed.Get("stop_reason").String(); got != "stop_sequence" {
		t.Fatalf("stop_reason = %q, want stop_sequence. Output: %s", got, string(out))
	}
	if got := parsed.Get("stop_sequence").String(); got != "\nEND" {
		t.Fatalf("stop_sequence = %q, want newline END. Output: %s", got, string(out))
	}
}

func findClaudeStreamStopReason(outputs [][]byte) (string, bool) {
	messageDelta, ok := findClaudeStreamMessageDelta(outputs)
	if !ok {
		return "", false
	}
	return messageDelta.Get("delta.stop_reason").String(), true
}

func findClaudeStreamMessageDelta(outputs [][]byte) (gjson.Result, bool) {
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() == "message_delta" {
				return data, true
			}
		}
	}
	return gjson.Result{}, false
}

// Ported from upstream CLIProxyAPI commit 893abbabc2a5 (cache write tokens in
// Claude responses); covers local symbol extractResponsesUsage via
// ConvertCodexResponseToClaude streaming usage mapping.
func TestConvertCodexResponseToClaude_StreamPreservesCacheWriteUsage(t *testing.T) {
	tests := []struct {
		name                 string
		terminalUsageJSON    string
		wantInputTokens      int64
		wantOutputTokens     int64
		wantCacheReadTokens  int64
		wantCacheWriteTokens int64
	}{
		{
			name:                 "cache_write_tokens field",
			terminalUsageJSON:    `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}}`,
			wantInputTokens:      50,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 150,
		},
		{
			name:                 "cache_creation_tokens field alias",
			terminalUsageJSON:    `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_creation_tokens":150}}`,
			wantInputTokens:      50,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 150,
		},
		{
			name:                 "cached_tokens greater than input_tokens clamps input_tokens to zero",
			terminalUsageJSON:    `{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":50}}`,
			wantInputTokens:      0,
			wantOutputTokens:     100,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 50,
		},
		{
			name:                 "zero cache_write_tokens does not emit cache_creation_input_tokens",
			terminalUsageJSON:    `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":0}}`,
			wantInputTokens:      200,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "cache_write_tokens only deducts from input_tokens",
			terminalUsageJSON:    `{"input_tokens":4022,"output_tokens":462,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":4019}}`,
			wantInputTokens:      3,
			wantOutputTokens:     462,
			wantCacheReadTokens:  0,
			wantCacheWriteTokens: 4019,
		},
		{
			name:                 "combined cached and cache_write greater than input_tokens clamps to zero",
			terminalUsageJSON:    `{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":300}}`,
			wantInputTokens:      0,
			wantOutputTokens:     100,
			wantCacheReadTokens:  300,
			wantCacheWriteTokens: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"messages":[]}`)
			var param any

			chunks := [][]byte{
				[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5"}}`),
				[]byte(`data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`),
				[]byte(fmt.Sprintf(`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":%s}}`, tt.terminalUsageJSON)),
			}

			var outputs [][]byte
			for _, chunk := range chunks {
				outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
			}

			delta, ok := findClaudeStreamMessageDelta(outputs)
			if !ok {
				t.Fatalf("missing message_delta event; outputs=%q", outputs)
			}

			usage := delta.Get("usage")
			if got := usage.Get("input_tokens").Int(); got != tt.wantInputTokens {
				t.Fatalf("input_tokens = %d, want %d", got, tt.wantInputTokens)
			}
			if got := usage.Get("output_tokens").Int(); got != tt.wantOutputTokens {
				t.Fatalf("output_tokens = %d, want %d", got, tt.wantOutputTokens)
			}
			if got := usage.Get("cache_read_input_tokens").Int(); got != tt.wantCacheReadTokens {
				t.Fatalf("cache_read_input_tokens = %d, want %d", got, tt.wantCacheReadTokens)
			}
			if tt.wantCacheWriteTokens == 0 {
				if usage.Get("cache_creation_input_tokens").Exists() {
					t.Fatalf("cache_creation_input_tokens should not be emitted when zero; got %v", usage.Get("cache_creation_input_tokens").Raw)
				}
			} else if got := usage.Get("cache_creation_input_tokens").Int(); got != tt.wantCacheWriteTokens {
				t.Fatalf("cache_creation_input_tokens = %d, want %d", got, tt.wantCacheWriteTokens)
			}
		})
	}
}

// Ported from upstream CLIProxyAPI commit 893abbabc2a5 (cache write tokens in
// Claude responses); covers local symbol extractResponsesUsage via
// ConvertCodexResponseToClaudeNonStream usage mapping.
func TestConvertCodexResponseToClaudeNonStream_PreservesCacheWriteUsage(t *testing.T) {
	tests := []struct {
		name                 string
		responseJSON         string
		wantInputTokens      int64
		wantOutputTokens     int64
		wantCacheReadTokens  int64
		wantCacheWriteTokens int64
	}{
		{
			name: "cache_write_tokens field",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      50,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 150,
		},
		{
			name: "cache_creation_tokens alias",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_creation_tokens":150}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      50,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 150,
		},
		{
			name: "cached_tokens greater than input_tokens clamps input_tokens to zero",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":50}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      0,
			wantOutputTokens:     100,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 50,
		},
		{
			name: "zero cache_write_tokens does not emit cache_creation_input_tokens",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":0}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      200,
			wantOutputTokens:     200,
			wantCacheReadTokens:  800,
			wantCacheWriteTokens: 0,
		},
		{
			name: "cache_write_tokens only deducts from input_tokens",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":4022,"output_tokens":462,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":4019}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      3,
			wantOutputTokens:     462,
			wantCacheReadTokens:  0,
			wantCacheWriteTokens: 4019,
		},
		{
			name: "combined cached and cache_write greater than input_tokens clamps to zero",
			responseJSON: `{
				"type":"response.completed",
				"response":{
					"id":"resp_1",
					"model":"gpt-5",
					"stop_reason":"stop",
					"usage":{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":300}},
					"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
				}
			}`,
			wantInputTokens:      0,
			wantOutputTokens:     100,
			wantCacheReadTokens:  300,
			wantCacheWriteTokens: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			originalRequest := []byte(`{"messages":[]}`)
			out := ConvertCodexResponseToClaudeNonStream(ctx, "", originalRequest, nil, []byte(tt.responseJSON), nil)
			parsed := gjson.ParseBytes(out)

			usage := parsed.Get("usage")
			if got := usage.Get("input_tokens").Int(); got != tt.wantInputTokens {
				t.Fatalf("input_tokens = %d, want %d", got, tt.wantInputTokens)
			}
			if got := usage.Get("output_tokens").Int(); got != tt.wantOutputTokens {
				t.Fatalf("output_tokens = %d, want %d", got, tt.wantOutputTokens)
			}
			if got := usage.Get("cache_read_input_tokens").Int(); got != tt.wantCacheReadTokens {
				t.Fatalf("cache_read_input_tokens = %d, want %d", got, tt.wantCacheReadTokens)
			}
			if tt.wantCacheWriteTokens == 0 {
				if usage.Get("cache_creation_input_tokens").Exists() {
					t.Fatalf("cache_creation_input_tokens should not be emitted when zero; got %v", usage.Get("cache_creation_input_tokens").Raw)
				}
			} else if got := usage.Get("cache_creation_input_tokens").Int(); got != tt.wantCacheWriteTokens {
				t.Fatalf("cache_creation_input_tokens = %d, want %d", got, tt.wantCacheWriteTokens)
			}
		})
	}
}

// Ported from upstream CLIProxyAPI commit 883660fb (deduct cache-write tokens from input_tokens).

func TestExtractResponsesUsage(t *testing.T) {
	tests := []struct {
		name                 string
		rawUsage             string
		wantInputTokens      int64
		wantOutputTokens     int64
		wantCachedTokens     int64
		wantCacheWriteTokens int64
	}{
		{
			name:                 "nil / absent usage",
			rawUsage:             "",
			wantInputTokens:      0,
			wantOutputTokens:     0,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "null usage",
			rawUsage:             "null",
			wantInputTokens:      0,
			wantOutputTokens:     0,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "only input and output tokens without cache details",
			rawUsage:             `{"input_tokens":100,"output_tokens":50}`,
			wantInputTokens:      100,
			wantOutputTokens:     50,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "deducts cache_read_tokens only",
			rawUsage:             `{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"cached_tokens":30}}`,
			wantInputTokens:      70,
			wantOutputTokens:     50,
			wantCachedTokens:     30,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "deducts cache_write_tokens only (issue 5956)",
			rawUsage:             `{"input_tokens":4022,"output_tokens":462,"input_tokens_details":{"cache_write_tokens":4019}}`,
			wantInputTokens:      3,
			wantOutputTokens:     462,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 4019,
		},
		{
			name:                 "deducts cache_creation_tokens alias only",
			rawUsage:             `{"input_tokens":4022,"output_tokens":462,"input_tokens_details":{"cache_creation_tokens":4019}}`,
			wantInputTokens:      3,
			wantOutputTokens:     462,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 4019,
		},
		{
			name:                 "deducts both cached_tokens and cache_write_tokens",
			rawUsage:             `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}}`,
			wantInputTokens:      50,
			wantOutputTokens:     200,
			wantCachedTokens:     800,
			wantCacheWriteTokens: 150,
		},
		{
			name:                 "clamps input_tokens to zero when cache exceeds input",
			rawUsage:             `{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":300}}`,
			wantInputTokens:      0,
			wantOutputTokens:     100,
			wantCachedTokens:     300,
			wantCacheWriteTokens: 300,
		},
		{
			name:                 "handles negative cache numbers safely without corrupting input",
			rawUsage:             `{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"cached_tokens":-10,"cache_write_tokens":-5}}`,
			wantInputTokens:      100,
			wantOutputTokens:     50,
			wantCachedTokens:     -10,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "clamps raw negative input_tokens to zero",
			rawUsage:             `{"input_tokens":-10,"output_tokens":50}`,
			wantInputTokens:      0,
			wantOutputTokens:     50,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 0,
		},
		{
			name:                 "negative cache_write_tokens falls back to cache_creation_tokens alias",
			rawUsage:             `{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"cache_write_tokens":-1,"cache_creation_tokens":40}}`,
			wantInputTokens:      60,
			wantOutputTokens:     50,
			wantCachedTokens:     0,
			wantCacheWriteTokens: 40,
		},
		{
			name:                 "prevents int64 overflow when cached_tokens and cache_write_tokens are huge",
			rawUsage:             `{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"cached_tokens":9223372036854775800,"cache_write_tokens":100}}`,
			wantInputTokens:      0,
			wantOutputTokens:     50,
			wantCachedTokens:     9223372036854775800,
			wantCacheWriteTokens: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := gjson.Parse(tt.rawUsage)
			input, output, cached, cacheWrite := extractResponsesUsage(usage)
			if input != tt.wantInputTokens {
				t.Fatalf("input_tokens = %d, want %d", input, tt.wantInputTokens)
			}
			if output != tt.wantOutputTokens {
				t.Fatalf("output_tokens = %d, want %d", output, tt.wantOutputTokens)
			}
			if cached != tt.wantCachedTokens {
				t.Fatalf("cached_tokens = %d, want %d", cached, tt.wantCachedTokens)
			}
			if cacheWrite != tt.wantCacheWriteTokens {
				t.Fatalf("cache_write_tokens = %d, want %d", cacheWrite, tt.wantCacheWriteTokens)
			}
		})
	}
}

// TestConvertCodexResponseToClaude_ReasoningTextDeltaKeepsSignature covers the
// Muse/routed shape: reasoning arrives as reasoning_text (not
// reasoning_summary_text), and the signature must still leave on a thinking
// block that carries that text.
func TestConvertCodexResponseToClaude_ReasoningTextDeltaKeepsSignature(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"muse-spark-1.3\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"Let me think\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"muse_sig_123\"}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	var thinkingText strings.Builder
	signature := ""
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			switch data.Get("type").String() {
			case "content_block_delta":
				if data.Get("delta.type").String() == "thinking_delta" {
					thinkingText.WriteString(data.Get("delta.thinking").String())
				}
				if data.Get("delta.type").String() == "signature_delta" {
					signature = data.Get("delta.signature").String()
				}
			}
		}
	}
	if got := thinkingText.String(); got != "Let me think" {
		t.Fatalf("thinking text = %q, want the reasoning_text delta; outputs=%q", got, outputs)
	}
	if signature != "muse_sig_123" {
		t.Fatalf("signature delta = %q, want muse_sig_123; outputs=%q", signature, outputs)
	}
}

// TestConvertCodexResponseToClaude_StreamDoneOnlyReasoningText covers a provider
// that streams no reasoning delta at all: the item's text and signature must
// still leave together, never as an empty signature-only block.
func TestConvertCodexResponseToClaude_StreamDoneOnlyReasoningText(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"messages":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"muse-spark-1.3\"}}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"muse_sig_456\",\"content\":[{\"type\":\"reasoning_text\",\"text\":\"done-only thinking\"}]}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToClaude(ctx, "", originalRequest, nil, chunk, &param)...)
	}

	var thinkingText strings.Builder
	signature := ""
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if data.Get("type").String() != "content_block_delta" {
				continue
			}
			switch data.Get("delta.type").String() {
			case "thinking_delta":
				thinkingText.WriteString(data.Get("delta.thinking").String())
			case "signature_delta":
				signature = data.Get("delta.signature").String()
			}
		}
	}
	if got := thinkingText.String(); got != "done-only thinking" {
		t.Fatalf("thinking text = %q, want the done-only reasoning text; outputs=%q", got, outputs)
	}
	if signature != "muse_sig_456" {
		t.Fatalf("signature delta = %q, want muse_sig_456; outputs=%q", signature, outputs)
	}
}
