package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	core "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
	"github.com/tidwall/gjson"
)

// The upstream deliberately cannot finish the initial response until it reads
// steering. A serial request/response proxy deadlocks this test.
func TestCodexDuplexSteeringLifecycle(t *testing.T) {
	for _, boundary := range []string{"response.incomplete", "response.completed"} {
		t.Run(boundary, func(t *testing.T) {
			var connections atomic.Int32
			serverErrors := make(chan error, 1)
			serverDone := make(chan struct{})
			steer := []byte(`{"type":"response.steer","previous_response_id":"r1","input":[{"role":"user","content":[{"type":"input_text","text":"STEER_OK"}]}]}`)
			accepted := []byte(`{"type":"response.steer.accepted","sequence_number":2,"steer":{"id":"s1","previous_response_id":"r1"}}`)
			pending := []byte(`{"type":"response.steer.pending","sequence_number":8,"steer":{"id":"s2","previous_response_id":"r2"},"reason":"waiting_for_required_input","required_input":[{"type":"function_call_output","call_id":"c1","name":"lookup"}]}`)
			failed := []byte(`{"type":"response.steer.failed","sequence_number":14,"steer":{"id":"s3","previous_response_id":"missing","input":"recover me"},"error":{"code":"response_not_found","message":"missing response"}}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				connections.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					serverErrors <- err
					return
				}
				defer func() { _ = conn.Close() }()
				// Timeouts are test failure bounds only; no production network deadline is added.
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				read := func() []byte {
					_, b, err := conn.ReadMessage()
					if err != nil {
						t.Errorf("upstream read: %v", err)
					}
					return b
				}
				write := func(b []byte) {
					if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
						t.Errorf("upstream write: %v", err)
					}
				}
				first := read()
				if gjson.GetBytes(first, "type").String() != "response.create" {
					t.Errorf("initial create: %s", first)
				}
				write([]byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
				if got := read(); !bytes.Equal(got, steer) {
					t.Errorf("steer transformed: %s", got)
				}
				write(accepted)
				write([]byte(fmt.Sprintf(`{"type":%q,"response":{"id":"r1","output":[],"incomplete_details":{"reason":"steered"}}}`, boundary)))
				write([]byte(`{"type":"response.created","response":{"id":"r2","output":[]}}`))
				got := read()
				if gjson.GetBytes(got, "type").String() != "response.steer" {
					t.Errorf("second control: %s", got)
				}
				write([]byte(`{"type":"response.steer.accepted","steer":{"id":"s2","previous_response_id":"r2"}}`))
				write([]byte(`{"type":"response.completed","response":{"id":"r2","output":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}]}}`))
				write(pending)
				got = read()
				if gjson.GetBytes(got, "type").String() != "response.create" || gjson.GetBytes(got, "previous_response_id").String() != "r2" || gjson.GetBytes(got, "input.0.call_id").String() != "c1" {
					t.Errorf("tool continuation: %s", got)
				}
				if gjson.GetBytes(got, "instructions").String() != "New settings" {
					t.Errorf("explicit settings not applied: %s", got)
				}
				write([]byte(`{"type":"response.created","response":{"id":"r3","output":[]}}`))
				write([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"STEER_OK"}]}}`))
				write([]byte(`{"type":"response.completed","response":{"id":"r3","output":[]}}`))
				// Idle steering must also reach the same live upstream connection.
				got = read()
				if gjson.GetBytes(got, "previous_response_id").String() != "missing" {
					t.Errorf("idle control missing: %s", got)
				}
				write(failed)
				// Keep the socket alive until the client closes it; no automatic replay.
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			input := make(chan core.WebsocketInput, 8)
			ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)
			exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			auth := &cliproxyauth.Auth{ID: "A", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}}
			req := core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[],"instructions":"Initial settings"}`)}
			opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "duplex-test"}}
			result, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for !seen["failed"] {
				select {
				case chunk, ok := <-result.Chunks:
					if !ok {
						t.Fatal("stream ended before all responses")
					}
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					event := gjson.GetBytes(chunk.Payload, "type").String()
					id := gjson.GetBytes(chunk.Payload, "response.id").String()
					switch {
					case event == "response.created" && id == "r1":
						input <- core.WebsocketInput{Payload: steer}
					case event == "response.steer.accepted" && gjson.GetBytes(chunk.Payload, "steer.id").String() == "s1":
						if !bytes.Equal(chunk.Payload, accepted) {
							t.Fatal("accepted event changed")
						}
						seen["accepted"] = true
					case event == boundary && id == "r1":
						seen["boundary"] = true
					case event == "response.created" && id == "r2":
						input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"r2","input":"Use tool result"}`)}
					case event == "response.steer.pending":
						if !bytes.Equal(chunk.Payload, pending) {
							t.Fatal("pending event changed")
						}
						seen["pending"] = true
						input <- core.WebsocketInput{Payload: []byte(`{"type":"response.create","previous_response_id":"r2","instructions":"New settings","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)}
					case event == "response.completed" && id == "r3":
						if gjson.GetBytes(chunk.Payload, "response.output.0.content.0.text").String() != "STEER_OK" {
							t.Fatal("successor output missing or contaminated by preceding response")
						}
						seen["successor"] = true
						input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"missing","input":"recover me"}`)}
					case event == "response.steer.failed":
						if !bytes.Equal(chunk.Payload, failed) {
							t.Fatal("failed input changed")
						}
						seen["failed"] = true
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			cancel()
			select {
			case <-serverDone:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not close")
			}
			for range result.Chunks {
			}
			if connections.Load() != 1 {
				t.Fatalf("connections=%d", connections.Load())
			}
			for _, key := range []string{"accepted", "boundary", "pending", "successor", "failed"} {
				if !seen[key] {
					t.Fatalf("missing %s", key)
				}
			}
			select {
			case err := <-serverErrors:
				t.Fatal(err)
			default:
			}
		})
	}
}

func TestCodexDuplexAppendInheritsContextAndInstructions(t *testing.T) {
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		// 1. Read initial create
		_, first, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read first create: %v", err)
			return
		}
		if gjson.GetBytes(first, "instructions").String() != "Initial system instructions" {
			t.Errorf("first instructions = %q", gjson.GetBytes(first, "instructions").String())
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-1","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`))

		// 2. Read subsequent append (which client sent omitting previous_response_id, model, and instructions)
		_, second, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read second create (from append): %v", err)
			return
		}
		if got := gjson.GetBytes(second, "type").String(); got != "response.create" {
			t.Errorf("second type = %q, want response.create", got)
		}
		if got := gjson.GetBytes(second, "previous_response_id").String(); got != "resp-1" {
			t.Errorf("second previous_response_id = %q, want resp-1", got)
		}
		if got := gjson.GetBytes(second, "model").String(); got != "gpt-6-astra" {
			t.Errorf("second model = %q, want gpt-6-astra", got)
		}
		if got := gjson.GetBytes(second, "instructions").String(); got != "Initial system instructions" {
			t.Errorf("second instructions = %q, want 'Initial system instructions'", got)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-2","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := make(chan core.WebsocketInput, 4)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "append-test-auth", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}}
	req := core.Request{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"turn 1"}],"instructions":"Initial system instructions"}`),
	}
	opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "append-test"}}

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		eventType := gjson.GetBytes(chunk.Payload, "type").String()
		eventID := gjson.GetBytes(chunk.Payload, "response.id").String()
		if eventType == "response.completed" && eventID == "resp-1" {
			// Client sends response.append omitting previous_response_id, model, instructions
			appendPayload := []byte(`{"type":"response.append","input":[{"role":"user","content":"turn 2"}]}`)
			input <- core.WebsocketInput{Payload: appendPayload}
		}
		if eventType == "response.completed" && eventID == "resp-2" {
			cancel()
			break
		}
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not finish")
	}
}

func TestCodexDuplexStandaloneCreateDoesNotInheritParentID(t *testing.T) {
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		// 1. Read first create
		_, _, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read first create: %v", err)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-1","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`))

		// 2. Read second create (standalone / full history replacement without previous_response_id)
		_, second, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read second create: %v", err)
			return
		}
		if got := gjson.GetBytes(second, "type").String(); got != "response.create" {
			t.Errorf("second type = %q, want response.create", got)
		}
		if gjson.GetBytes(second, "previous_response_id").Exists() {
			t.Errorf("standalone create must not gain previous_response_id: %s", second)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-2","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := make(chan core.WebsocketInput, 4)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "standalone-create-test", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}}
	req := core.Request{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"turn 1"}]}`),
	}
	opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "standalone-test"}}

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		eventType := gjson.GetBytes(chunk.Payload, "type").String()
		eventID := gjson.GetBytes(chunk.Payload, "response.id").String()
		if eventType == "response.completed" && eventID == "resp-1" {
			// Client sends standalone response.create without previous_response_id
			standalonePayload := []byte(`{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":"turn 2 standalone"}]}`)
			input <- core.WebsocketInput{Payload: standalonePayload}
		}
		if eventType == "response.completed" && eventID == "resp-2" {
			cancel()
			break
		}
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not finish")
	}
}

func TestCodexDuplexQueuedCreateDoesNotBlockSubsequentSteer(t *testing.T) {
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		// 1. Read first create
		_, first, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read first create: %v", err)
			return
		}
		if gjson.GetBytes(first, "type").String() != "response.create" {
			t.Errorf("expected response.create: %s", first)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))

		// Read first steer for r1
		_, steer1, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read steer1: %v", err)
			return
		}
		if gjson.GetBytes(steer1, "type").String() != "response.steer" {
			t.Errorf("expected response.steer: %s", steer1)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"s1","previous_response_id":"r1"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.incomplete","response":{"id":"r1","output":[],"incomplete_details":{"reason":"steered"}}}`))

		// Emit automatic successor (sets automaticActive = true)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"auto-1","previous_response_id":"r1","output":[]}}`))

		// Client will send next create (blocked by automaticActive) followed by steer for auto-1.
		// Upstream expects steering to arrive NEXT, proving create did not block the steer!
		_, steerFrame, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read steer: %v", err)
			return
		}
		if got := gjson.GetBytes(steerFrame, "type").String(); got != "response.steer" {
			t.Errorf("expected response.steer to arrive before blocked create, got: %s", steerFrame)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"s2","previous_response_id":"auto-1"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"auto-1","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"auto-2","previous_response_id":"auto-1","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"auto-2","output":[]}}`))

		// Now that automatic successor is completed, the queued create is delivered to upstream
		_, nextCreate, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read queued create: %v", err)
			return
		}
		if got := gjson.GetBytes(nextCreate, "type").String(); got != "response.create" {
			t.Errorf("queued create type = %q, want response.create", got)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r2","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r2","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := make(chan core.WebsocketInput, 8)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "steer-blocking-test", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}}
	req := core.Request{
		Model:   "gpt-6-astra",
		Payload: []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"start"}]}`),
	}
	opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "steer-block-test"}}

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		eventType := gjson.GetBytes(chunk.Payload, "type").String()
		eventID := gjson.GetBytes(chunk.Payload, "response.id").String()
		if eventType == "response.created" && eventID == "r1" {
			input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"r1","input":"steer 1"}`)}
		}
		if eventType == "response.created" && eventID == "auto-1" {
			// During automatic successor generation, send a create first, followed by steer:
			input <- core.WebsocketInput{Payload: []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"auto-1","input":[{"role":"user","content":"queued next"}]}`)}
			input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"auto-1","input":"steer 2 in flight"}`)}
		}
		if eventType == "response.completed" && eventID == "r2" {
			cancel()
			break
		}
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not finish")
	}
}

func TestCodexAuthUsesSIWC(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{name: "nil auth", auth: nil, want: false},
		{name: "no flag", auth: &cliproxyauth.Auth{}, want: false},
		{name: "attribute true", auth: &cliproxyauth.Auth{Attributes: map[string]string{"use_siwc": "true"}}, want: true},
		{name: "attribute false", auth: &cliproxyauth.Auth{Attributes: map[string]string{"use_siwc": "false"}}, want: false},
		{name: "attribute invalid", auth: &cliproxyauth.Auth{Attributes: map[string]string{"use_siwc": "yes"}}, want: false},
		{name: "metadata bool", auth: &cliproxyauth.Auth{Metadata: map[string]any{"use_siwc": true}}, want: true},
		{name: "metadata string", auth: &cliproxyauth.Auth{Metadata: map[string]any{"use_siwc": " TRUE "}}, want: true},
		{name: "metadata invalid", auth: &cliproxyauth.Auth{Metadata: map[string]any{"use_siwc": 1}}, want: false},
		{
			name: "attribute false falls through to metadata true",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"use_siwc": "false"},
				Metadata:   map[string]any{"use_siwc": true},
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexAuthUsesSIWC(tc.auth); got != tc.want {
				t.Fatalf("codexAuthUsesSIWC() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCodexDuplexSIWCSteerStripsConfigurationUpdates(t *testing.T) {
	steerPayload := []byte(`{"type":"response.steer","previous_response_id":"r1","input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":[{"type":"input_text","text":"STEER_OK"}]},{"type":"configuration_update","tools":[]}]}`)
	for _, tc := range []struct {
		name      string
		useSIWC   bool
		wantInput string
	}{
		{
			name:      "siwc drops every configuration update",
			useSIWC:   true,
			wantInput: `[{"role":"user","content":[{"type":"input_text","text":"STEER_OK"}]}]`,
		},
		{
			name:      "non siwc keeps the native control frame",
			useSIWC:   false,
			wantInput: `[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":[{"type":"input_text","text":"STEER_OK"}]},{"type":"configuration_update","tools":[]}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if errUpgrade != nil {
					t.Errorf("upgrade websocket: %v", errUpgrade)
					return
				}
				defer func() { _ = conn.Close() }()
				// This deadline bounds test failures only.
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, first, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Errorf("read initial create: %v", errRead)
					return
				}
				if gjson.GetBytes(first, "type").String() != "response.create" {
					t.Errorf("initial create: %s", first)
				}
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`)); errWrite != nil {
					t.Errorf("write response.created: %v", errWrite)
					return
				}
				_, steer, errSteer := conn.ReadMessage()
				if errSteer != nil {
					t.Errorf("read steer: %v", errSteer)
					return
				}
				captured <- append([]byte(nil), steer...)
				for _, event := range [][]byte{
					[]byte(`{"type":"response.steer.accepted","sequence_number":2,"steer":{"id":"s1","previous_response_id":"r1"}}`),
					[]byte(`{"type":"response.completed","response":{"id":"r1","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`),
				} {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						t.Errorf("write event: %v", errWrite)
						return
					}
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			input := make(chan core.WebsocketInput, 1)
			ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)
			exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			attributes := map[string]string{"api_key": "test-key", "base_url": server.URL, "websockets": "true"}
			if tc.useSIWC {
				attributes["use_siwc"] = "true"
			}
			auth := &cliproxyauth.Auth{ID: "duplex-siwc-steer", Provider: "codex", Attributes: attributes}
			req := core.Request{
				Model:   "gpt-6-astra",
				Payload: []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"instructions":"Initial settings"}`),
			}
			opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "duplex-siwc-steer"}}
			result, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}

			var order []string
			completed := false
			for !completed {
				select {
				case chunk, ok := <-result.Chunks:
					if !ok {
						t.Fatal("stream ended before response.completed")
					}
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					event := gjson.GetBytes(chunk.Payload, "type").String()
					id := gjson.GetBytes(chunk.Payload, "response.id").String()
					order = append(order, event)
					if event == "response.created" && id == "r1" {
						input <- core.WebsocketInput{Payload: append([]byte(nil), steerPayload...)}
					}
					if event == "response.completed" && id == "r1" {
						// Usage telemetry must survive the control-frame strip.
						if got := gjson.GetBytes(chunk.Payload, "response.usage.total_tokens").Int(); got != 10 {
							t.Fatalf("usage total_tokens = %d, want 10; payload=%s", got, chunk.Payload)
						}
						completed = true
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			cancel()
			for range result.Chunks {
			}
			select {
			case <-serverDone:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not close")
			}

			if len(order) < 3 || order[len(order)-3] != "response.created" || order[len(order)-2] != "response.steer.accepted" || order[len(order)-1] != "response.completed" {
				t.Fatalf("stream ordering changed: %v", order)
			}

			select {
			case got := <-captured:
				if gjson.GetBytes(got, "type").String() != "response.steer" || gjson.GetBytes(got, "previous_response_id").String() != "r1" {
					t.Fatalf("steer routing changed: %s", got)
				}
				if input := gjson.GetBytes(got, "input").Raw; input != tc.wantInput {
					t.Fatalf("steer input = %s, want %s; body=%s", input, tc.wantInput, got)
				}
				if gjson.GetBytes(got, "reasoning").Exists() {
					t.Fatalf("steer must not promote reasoning: %s", got)
				}
				if gjson.GetBytes(got, "store").Exists() || gjson.GetBytes(got, "stream").Exists() {
					t.Fatalf("steer must not inherit response.create defaults: %s", got)
				}
			default:
				t.Fatal("upstream steer was not captured")
			}
		})
	}
}

// TestCodexDuplexBindsTurnToRequestedModel drives one duplex socket whose
// payload model was rewritten before the executor saw it. The client asked for
// client-model, an interceptor left upstream-model in the request body, and only
// the request metadata still names the client model. Every authorization must
// therefore see client-model, so a model-scoped denial on the client alias
// cannot be bypassed and a continuation naming the alias is not a model change
// (upstream CLIProxyAPI commit 6340fe4312a9).
func TestCodexDuplexBindsTurnToRequestedModel(t *testing.T) {
	const (
		clientModel   = "client-model"
		upstreamModel = "upstream-model"
	)
	serverErrors := make(chan error, 4)
	serverDone := make(chan struct{})
	var upstreamSteers atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		initial := readDuplexMessage(t, conn, serverErrors)
		if got := gjson.GetBytes(initial, "model").String(); got != upstreamModel {
			serverErrors <- fmt.Errorf("initial payload model = %q, want %q (the rewritten body)", got, upstreamModel)
			return
		}
		writeDuplexMessage(t, conn, serverErrors, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
		writeDuplexMessage(t, conn, serverErrors, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
		steer := readDuplexMessage(t, conn, serverErrors)
		if got := gjson.GetBytes(steer, "type").String(); got != "response.steer" {
			serverErrors <- fmt.Errorf("frame after the first response = %q, want the client steer", got)
			return
		}
		if got := gjson.GetBytes(steer, "model").String(); got != clientModel {
			serverErrors <- fmt.Errorf("forwarded steer model = %q, want the client's own %q", got, clientModel)
			return
		}
		upstreamSteers.Add(1)
		// No second frame may arrive: the following steer is refused locally.
		if _, _, errRead := conn.ReadMessage(); errRead == nil {
			serverErrors <- fmt.Errorf("refused steer reached upstream")
			return
		}
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	input := make(chan core.WebsocketInput, 4)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)

	var mu sync.Mutex
	var seenModels []string
	denyTurns := false
	ctx = core.WithWebsocketTurnAuthorizer(ctx, func(turnPayload []byte) ([]byte, error) {
		model := gjson.GetBytes(turnPayload, "model").String()
		mu.Lock()
		seenModels = append(seenModels, model)
		deny := denyTurns
		mu.Unlock()
		if deny {
			return []byte(`{"type":"error","status":429,"error":{"message":"quota exhausted for model"}}`), nil
		}
		return nil, nil
	})

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{CodexResponseSteering: true}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "duplex-model-bind", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}}
	req := core.Request{Model: upstreamModel, Payload: []byte(`{"model":"` + upstreamModel + `","input":[],"instructions":"Initial settings"}`)}
	opts := core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{
		core.ExecutionSessionMetadataKey: "duplex-model-bind",
		core.RequestedModelMetadataKey:   clientModel,
	}}
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}

	// First response completes, then the client steers with an explicit model that
	// names the alias rather than the rewritten body.
	awaitChunkEvent := func(want string, what string) []byte {
		t.Helper()
		for {
			select {
			case chunk, ok := <-result.Chunks:
				if !ok {
					t.Fatalf("stream ended while waiting for %s", what)
				}
				if chunk.Err != nil {
					t.Fatalf("stream error while waiting for %s: %v", what, chunk.Err)
				}
				if got := gjson.GetBytes(chunk.Payload, "type").String(); got == want {
					return chunk.Payload
				}
			case <-ctx.Done():
				t.Fatalf("timed out while waiting for %s", what)
			}
		}
	}

	if got := gjson.GetBytes(awaitChunkEvent("response.completed", "the first response"), "response.id").String(); got != "r1" {
		t.Fatalf("first completed response id = %q, want r1", got)
	}
	input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"r1","model":"` + clientModel + `","input":"continue in place"}`)}

	// The client alias must not trip the model-change rule: the steer reaches
	// upstream on the same socket instead of a replay-required close.
	deadline := time.Now().Add(5 * time.Second)
	for upstreamSteers.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("steer naming the client model never reached upstream")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The same turn model is what the access check sees, so a model-scoped denial
	// for the client alias refuses the next steer locally.
	mu.Lock()
	denyTurns = true
	mu.Unlock()
	input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"r1","input":"must not bypass the client model"}`)}
	deniedPayload := awaitChunkEvent("error", "the denied steer")
	if got := int(gjson.GetBytes(deniedPayload, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("denied steer status = %d, want 429 (%s)", got, deniedPayload)
	}
	if got := upstreamSteers.Load(); got != 1 {
		t.Fatalf("upstream steers after the denied steer = %d, want 1", got)
	}
	cancel()
	for range result.Chunks {
	}

	mu.Lock()
	gotModels := append([]string(nil), seenModels...)
	mu.Unlock()
	if len(gotModels) != 2 {
		t.Fatalf("authorized turns = %d, want 2 (allowed steer, denied steer)", len(gotModels))
	}
	for i, model := range gotModels {
		if model != clientModel {
			t.Fatalf("authorized turn %d model = %q, want the requested client model %q", i, model, clientModel)
		}
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not close")
	}
	select {
	case errServer := <-serverErrors:
		t.Fatal(errServer)
	default:
	}
}

func readDuplexMessage(t *testing.T, conn *websocket.Conn, errs chan error) []byte {
	t.Helper()
	_, payload, err := conn.ReadMessage()
	if err != nil {
		errs <- err
		t.Fatalf("upstream read: %v", err)
	}
	return payload
}

func writeDuplexMessage(t *testing.T, conn *websocket.Conn, errs chan error, payload []byte) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		errs <- err
		t.Fatalf("upstream write: %v", err)
	}
}
