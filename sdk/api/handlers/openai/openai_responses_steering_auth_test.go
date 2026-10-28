package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/therealtinhtute/llmhub/internal/config"
	"github.com/therealtinhtute/llmhub/internal/registry"
	runtimeexecutor "github.com/therealtinhtute/llmhub/internal/runtime/executor"
	sdkaccess "github.com/therealtinhtute/llmhub/sdk/access"
	"github.com/therealtinhtute/llmhub/sdk/api/handlers"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestResponsesSteeringDisabledAccountCannotSendAnotherFrame(t *testing.T) {
	var frames atomic.Int32
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		if _, _, err := c.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		frames.Add(1)
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1"}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
		if _, _, err := c.ReadMessage(); err == nil {
			frames.Add(1)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	id := "steering-disable-test"
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "steering-disable-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"steering-disable-model","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	disabled := auth.Clone()
	disabled.Disabled = true
	disabled.Status = coreauth.StatusDisabled
	if _, err := manager.Update(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer","previous_response_id":"r1","input":"must not be sent"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("disabled account connection remained usable")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream close stalled")
	}
	if frames.Load() != 1 {
		t.Fatalf("disabled credential sent %d frames", frames.Load())
	}
}

// reauthUsageCapture records the usage published for one credential so a test
// can prove the reauthorization gate never charges a turn upstream rejected.
type reauthUsageCapture struct {
	authID string
	mu     sync.Mutex
	seen   []coreusage.Record
}

func (c *reauthUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.AuthID != c.authID {
		return
	}
	c.mu.Lock()
	c.seen = append(c.seen, record)
	c.mu.Unlock()
}

func (c *reauthUsageCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *reauthUsageCapture) snapshot() []coreusage.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]coreusage.Record(nil), c.seen...)
}

type reauthNoopUsagePlugin struct{}

func (reauthNoopUsagePlugin) HandleUsage(context.Context, coreusage.Record) {}

// TestResponsesSteeringReauthChargesOneUsageRecordPerUpstreamTurn runs a duplex
// responses socket through an authorized turn, a revoked-key steer, a revoked
// create and a turn after reauthorization. The socket must stay open, the
// rejected turns must never reach upstream, and usage must stay one record per
// upstream turn (upstream CLIProxyAPI commit b9bb031dcbb0).
func TestResponsesSteeringReauthChargesOneUsageRecordPerUpstreamTurn(t *testing.T) {
	const authID = "steering-reauth-test"
	const modelName = "steering-reauth-model"

	var upstreamCreates atomic.Int32
	var upstreamSteers atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, raw, errRead := c.ReadMessage()
			if errRead != nil {
				return
			}
			if gjson.GetBytes(raw, "type").String() == "response.steer" {
				upstreamSteers.Add(1)
				continue
			}
			n := upstreamCreates.Add(1)
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r`+strings.Repeat("1", int(n))+`"}}`))
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r`+strings.Repeat("1", int(n))+`","output":[]}}`))
		}
	}))
	defer upstream.Close()

	usageCapture := &reauthUsageCapture{authID: authID}
	coreusage.RegisterNamedPlugin(t.Name(), usageCapture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), reauthNoopUsagePlugin{})
	})

	cfg := &config.Config{}
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL, "websockets": "true"},
		Metadata:   map[string]any{"access_token": "test-token"},
	}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})

	provider := &revocableWebsocketAccessProvider{}
	access := sdkaccess.NewManager()
	access.SetProviders([]sdkaccess.Provider{provider})

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	h.AccessManager = access
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer key-1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	writeFrame := func(raw string) {
		t.Helper()
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(raw)); errWrite != nil {
			t.Fatalf("write %s: %v", raw, errWrite)
		}
	}
	readFrame := func() []byte {
		t.Helper()
		_, msg, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read: %v", errRead)
		}
		return msg
	}
	awaitUsage := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for usageCapture.count() < want {
			if time.Now().After(deadline) {
				t.Fatalf("usage records = %d, want %d", usageCapture.count(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	readCompletedTurn := func(turn string) {
		t.Helper()
		if got := gjson.GetBytes(readFrame(), "type").String(); got != "response.created" {
			t.Fatalf("%s first event = %q, want response.created", turn, got)
		}
		if got := gjson.GetBytes(readFrame(), "type").String(); got != wsEventTypeCompleted {
			t.Fatalf("%s second event = %q, want %s", turn, got, wsEventTypeCompleted)
		}
	}

	// Turn 1: authorized, one upstream turn and exactly one usage record.
	writeFrame(`{"type":"response.create","model":"` + modelName + `","input":[]}`)
	readCompletedTurn("turn 1")
	if got := upstreamCreates.Load(); got != 1 {
		t.Fatalf("upstream creates after turn 1 = %d, want 1", got)
	}
	awaitUsage(1)

	// Turn 2: the key is revoked mid-socket. Both a steer and a create are
	// rejected with the provider's status and never reach upstream.
	provider.revoked.Store(true)
	writeFrame(`{"type":"response.steer","previous_response_id":"r1","input":"must not be forwarded"}`)
	steerErr := readFrame()
	if got := gjson.GetBytes(steerErr, "type").String(); got != wsEventTypeError {
		t.Fatalf("steer after revoke event = %q, want %s (%s)", got, wsEventTypeError, steerErr)
	}
	if got := int(gjson.GetBytes(steerErr, "status").Int()); got != http.StatusForbidden {
		t.Fatalf("steer after revoke status = %d, want %d (%s)", got, http.StatusForbidden, steerErr)
	}
	if got := upstreamSteers.Load(); got != 0 {
		t.Fatalf("upstream steers after revoke = %d, want 0", got)
	}

	writeFrame(`{"type":"response.create","model":"` + modelName + `","input":[]}`)
	createErr := readFrame()
	if got := gjson.GetBytes(createErr, "type").String(); got != wsEventTypeError {
		t.Fatalf("create after revoke event = %q, want %s (%s)", got, wsEventTypeError, createErr)
	}
	if got := int(gjson.GetBytes(createErr, "status").Int()); got != http.StatusForbidden {
		t.Fatalf("create after revoke status = %d, want %d (%s)", got, http.StatusForbidden, createErr)
	}
	if got := gjson.GetBytes(createErr, "error.message").String(); got != "key revoked" {
		t.Fatalf("create after revoke error.message = %q, want %q (%s)", got, "key revoked", createErr)
	}
	if got := upstreamCreates.Load(); got != 1 {
		t.Fatalf("upstream creates after rejected turn = %d, want 1", got)
	}
	if got := usageCapture.count(); got != 1 {
		t.Fatalf("usage records after rejected turn = %d, want 1", got)
	}

	// Turn 3: reauthorized on the same socket, so the turn succeeds instead of
	// the socket having been closed by turn 2.
	provider.revoked.Store(false)
	writeFrame(`{"type":"response.create","model":"` + modelName + `","input":[]}`)
	readCompletedTurn("turn 3 after reauthorization")
	if got := upstreamCreates.Load(); got != 2 {
		t.Fatalf("upstream creates after reauthorized turn = %d, want 2", got)
	}
	awaitUsage(2)
	time.Sleep(200 * time.Millisecond)
	records := usageCapture.snapshot()
	if len(records) != 2 {
		t.Fatalf("usage records = %d, want one per upstream turn (2)", len(records))
	}
	// The per-turn principal must reach usage reporting: the middleware in this
	// test never sets one, so only a reauthorized turn can attribute the key.
	for i, record := range records {
		if record.APIKey != "key-1" {
			t.Fatalf("usage record %d APIKey = %q, want %q", i, record.APIKey, "key-1")
		}
	}

	// A steer on the reauthorized socket still carries the bound model to the
	// provider: spoofing another model must not bypass a model-scoped denial.
	provider.denyModel(modelName)
	writeFrame(`{"type":"response.steer","previous_response_id":"r11","model":"allowed-other-model","input":"must not bypass model quota"}`)
	spoofErr := readFrame()
	if got := gjson.GetBytes(spoofErr, "type").String(); got != wsEventTypeError {
		t.Fatalf("steer with spoofed model event = %q, want %s (%s)", got, wsEventTypeError, spoofErr)
	}
	if got := int(gjson.GetBytes(spoofErr, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("steer with spoofed model status = %d, want %d (%s)", got, http.StatusTooManyRequests, spoofErr)
	}
	if got := upstreamSteers.Load(); got != 0 {
		t.Fatalf("upstream steers after model denial = %d, want 0", got)
	}
}

// TestResponsesSteeringPrincipalChangeIsRejectedMidStream drives one duplex
// socket whose credential starts resolving to principal key-1 and then resolves
// to key-2. The socket belongs to key-1, so a steer and a create that would run
// as key-2 must be refused with 401, must not reach upstream, must not charge
// usage and must not re-point the caller identity published for later turns.
// Restoring key-1 makes the socket usable again, which proves the check reads
// the principal rather than latching on the first refusal (upstream CLIProxyAPI
// commit 3e7a21e2f3cc).
func TestResponsesSteeringPrincipalChangeIsRejectedMidStream(t *testing.T) {
	const authID = "steering-principal-test"
	const modelName = "steering-principal-model"

	var upstreamCreates atomic.Int32
	var upstreamSteers atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, raw, errRead := c.ReadMessage()
			if errRead != nil {
				return
			}
			if gjson.GetBytes(raw, "type").String() == "response.steer" {
				// Only a socket still running as the bound principal can get here,
				// so acknowledge it: an executed refused turn then shows up as a
				// visible event instead of a bare read timeout.
				upstreamSteers.Add(1)
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"s1"}}`))
				continue
			}
			n := upstreamCreates.Add(1)
			responseID := "r" + strings.Repeat("1", int(n))
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+responseID+`"}}`))
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+responseID+`","output":[]}}`))
		}
	}))
	defer upstream.Close()

	usageCapture := &reauthUsageCapture{authID: authID}
	coreusage.RegisterNamedPlugin(t.Name(), usageCapture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), reauthNoopUsagePlugin{})
	})

	cfg := &config.Config{}
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL, "websockets": "true"},
		Metadata:   map[string]any{"access_token": "test-token"},
	}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})

	provider := &revocableWebsocketAccessProvider{}
	access := sdkaccess.NewManager()
	access.SetProviders([]sdkaccess.Provider{provider})

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	h.AccessManager = access
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()

	dial := func(token string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses",
			http.Header{"Authorization": []string{"Bearer " + token}},
		)
		if err != nil {
			t.Fatalf("dial with %s: %v", token, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		return conn
	}
	writeFrame := func(conn *websocket.Conn, raw string) {
		t.Helper()
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(raw)); errWrite != nil {
			t.Fatalf("write %s: %v", raw, errWrite)
		}
	}
	readFrame := func(conn *websocket.Conn) []byte {
		t.Helper()
		_, msg, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read: %v", errRead)
		}
		return msg
	}
	readCompletedTurn := func(conn *websocket.Conn, turn string) {
		t.Helper()
		if got := gjson.GetBytes(readFrame(conn), "type").String(); got != "response.created" {
			t.Fatalf("%s first event = %q, want response.created", turn, got)
		}
		if got := gjson.GetBytes(readFrame(conn), "type").String(); got != wsEventTypeCompleted {
			t.Fatalf("%s second event = %q, want %s", turn, got, wsEventTypeCompleted)
		}
	}
	awaitUsage := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for usageCapture.count() < want {
			if time.Now().After(deadline) {
				t.Fatalf("usage records = %d, want %d", usageCapture.count(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	requirePrincipalChange := func(msg []byte, what string) {
		t.Helper()
		if got := gjson.GetBytes(msg, "type").String(); got != wsEventTypeError {
			t.Fatalf("%s event = %q, want %s (%s)", what, got, wsEventTypeError, msg)
		}
		if got := int(gjson.GetBytes(msg, "status").Int()); got != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want %d (%s)", what, got, http.StatusUnauthorized, msg)
		}
		if got := gjson.GetBytes(msg, "error.message").String(); !strings.Contains(got, "credential changed") {
			t.Fatalf("%s message = %q, want a credential-changed rejection (%s)", what, got, msg)
		}
	}

	conn := dial("key-1")
	defer func() { _ = conn.Close() }()

	// Turn 1: the socket binds to key-1 and charges exactly one usage record.
	writeFrame(conn, `{"type":"response.create","model":"`+modelName+`","input":[]}`)
	readCompletedTurn(conn, "turn 1")
	if got := upstreamCreates.Load(); got != 1 {
		t.Fatalf("upstream creates after turn 1 = %d, want 1", got)
	}
	awaitUsage(1)
	if got := usageCapture.snapshot()[0].APIKey; got != "key-1" {
		t.Fatalf("turn 1 usage APIKey = %q, want key-1", got)
	}

	// The same presented credential now resolves to key-2. Neither a steer (whose
	// authorization runs inside the executor) nor a create may execute.
	provider.rotated.Store(true)
	writeFrame(conn, `{"type":"response.steer","previous_response_id":"r1","input":"must not run as key-2"}`)
	requirePrincipalChange(readFrame(conn), "steer after principal change")
	if got := upstreamSteers.Load(); got != 0 {
		t.Fatalf("upstream steers after principal change = %d, want 0", got)
	}
	writeFrame(conn, `{"type":"response.create","model":"`+modelName+`","input":[]}`)
	requirePrincipalChange(readFrame(conn), "create after principal change")
	if got := upstreamCreates.Load(); got != 1 {
		t.Fatalf("upstream creates after principal change = %d, want 1", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := usageCapture.count(); got != 1 {
		t.Fatalf("usage records after refused turns = %d, want 1", got)
	}

	// The principal is restored on the same socket, so nothing is latched and the
	// published identity is still key-1.
	provider.rotated.Store(false)
	writeFrame(conn, `{"type":"response.create","model":"`+modelName+`","input":[]}`)
	readCompletedTurn(conn, "turn 2 after principal restored")
	if got := upstreamCreates.Load(); got != 2 {
		t.Fatalf("upstream creates after principal restored = %d, want 2", got)
	}
	awaitUsage(2)
	time.Sleep(200 * time.Millisecond)
	records := usageCapture.snapshot()
	if len(records) != 2 {
		t.Fatalf("usage records = %d, want 2 (one per upstream turn)", len(records))
	}
	for i, record := range records {
		if record.APIKey != "key-1" {
			t.Fatalf("usage record %d APIKey = %q, want key-1", i, record.APIKey)
		}
	}

	// A token rotation that keeps the principal authorizes normally: the binding
	// is the principal, not the presented credential bytes.
	rotatedConn := dial("key-1b")
	defer func() { _ = rotatedConn.Close() }()
	writeFrame(rotatedConn, `{"type":"response.create","model":"`+modelName+`","input":[]}`)
	readCompletedTurn(rotatedConn, "turn after token rotation")
	if got := upstreamCreates.Load(); got != 3 {
		t.Fatalf("upstream creates after token rotation = %d, want 3", got)
	}
}
