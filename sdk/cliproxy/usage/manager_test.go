package usage

import (
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"
	"weak"
)

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

type notifyUsagePlugin struct {
	delivered chan struct{}
}

func (p *notifyUsagePlugin) HandleUsage(_ context.Context, _ Record) {
	p.delivered <- struct{}{}
}

type retainedRequestPayload struct {
	data [64 << 10]byte
}

type retainedRequestContextKey struct{}

// publishConsumableRecord enqueues one record whose request context, response
// headers and failure body are only reachable through the queue slot, then
// returns weak pointers to them. Ported from upstream f2e255f73eda.
//
//go:noinline
func publishConsumableRecord(mgr *Manager) (weak.Pointer[retainedRequestPayload], weak.Pointer[string], weak.Pointer[string]) {
	ctxPayload := &retainedRequestPayload{}
	headerSlice := []string{"req-header-value"}
	body := string([]byte("upstream-failure-body"))

	ctx := context.WithValue(context.Background(), retainedRequestContextKey{}, ctxPayload)
	mgr.Publish(ctx, Record{
		Provider:        "openai",
		Model:           "gpt-5.4",
		ResponseHeaders: http.Header{"X-Request-Id": headerSlice},
		Fail:            Failure{StatusCode: 502, Body: body},
	})

	return weak.Make(ctxPayload), weak.Make(&headerSlice[0]), weak.Make(&body)
}

// TestManagerReleasesConsumedQueueItemReferencesWhenIdle proves the dispatcher
// clears the consumed queue slot instead of leaving it in the backing array.
func TestManagerReleasesConsumedQueueItemReferencesWhenIdle(t *testing.T) {
	mgr := NewManager(10)
	t.Cleanup(mgr.Stop)

	plugin := &notifyUsagePlugin{delivered: make(chan struct{}, 1)}
	mgr.Register(plugin)

	ctxWeak, headerWeak, bodyWeak := publishConsumableRecord(mgr)

	select {
	case <-plugin.delivered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record dispatch")
	}

	for range 10 {
		runtime.Gosched()
		runtime.GC()
		if ctxWeak.Value() == nil && headerWeak.Value() == nil && bodyWeak.Value() == nil {
			break
		}
	}

	if ctxWeak.Value() != nil {
		t.Fatalf("consumed request context payload is still retained while manager is idle")
	}
	if headerWeak.Value() != nil {
		t.Fatalf("consumed record ResponseHeaders backing value is still retained while manager is idle")
	}
	if bodyWeak.Value() != nil {
		t.Fatalf("consumed record Fail.Body is still retained while manager is idle")
	}
}
