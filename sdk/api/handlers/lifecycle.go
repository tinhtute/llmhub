package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/textproto"
	"sync"

	"github.com/therealtinhtute/llmhub/internal/interfaces"
)

type requestLifecyclePluginContextKey struct{}

type RequestLifecyclePlugin interface {
	InterceptBefore(context.Context, *RequestLifecycleRequest) (*RequestLifecycleDecision, error)
	InterceptAfter(context.Context, *RequestLifecycleRequest, *RequestLifecycleResponse) (*RequestLifecycleDecision, error)
}

type RequestLifecycleRequest struct {
	HandlerType     string
	Model           string
	NormalizedModel string
	Alt             string
	Stream          bool
	// Payload contains the current request payload. Treat it as read-only; mutations must be returned via RequestLifecycleDecision.Payload.
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any
}

type RequestLifecycleResponse struct {
	Stream  bool
	Payload []byte
	Headers http.Header
	Error   *interfaces.ErrorMessage
}

type RequestLifecycleDecision struct {
	ReplacePayload bool
	Payload        []byte
	Headers        http.Header
	Termination    *RequestLifecycleTermination
}

type RequestLifecycleTermination struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
	Error      error
}

func WithRequestLifecyclePlugin(ctx context.Context, plugin RequestLifecyclePlugin) context.Context {
	if plugin == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestLifecyclePluginContextKey{}, plugin)
}

func requestLifecyclePlugin(ctx context.Context) RequestLifecyclePlugin {
	if ctx == nil {
		return nil
	}
	plugin, _ := ctx.Value(requestLifecyclePluginContextKey{}).(RequestLifecyclePlugin)
	return plugin
}

type StreamEmitResult struct {
	Accepted bool
	Err      error
}

type SafeStreamEmitter struct {
	mu     sync.Mutex
	closed bool
	send   func([]byte) bool
}

func NewSafeStreamEmitter(send func([]byte) bool) *SafeStreamEmitter {
	return &SafeStreamEmitter{send: send}
}

func (e *SafeStreamEmitter) Emit(chunk []byte) (result StreamEmitResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = StreamEmitResult{Err: fmt.Errorf("stream emit panic: %v", recovered)}
		}
	}()
	if e == nil || e.send == nil {
		return StreamEmitResult{Err: fmt.Errorf("stream emitter is not configured")}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return StreamEmitResult{Err: fmt.Errorf("stream emitter is closed")}
	}
	send := e.send
	e.mu.Unlock()
	if !send(cloneBytes(chunk)) {
		return StreamEmitResult{Err: fmt.Errorf("stream emit was not accepted")}
	}
	return StreamEmitResult{Accepted: true}
}

func (e *SafeStreamEmitter) Close() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("stream close panic: %v", recovered)
		}
	}()
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

func cloneLifecycleMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func lifecycleErrorMessage(err error) *interfaces.ErrorMessage {
	if err == nil {
		return nil
	}
	return &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
}

func applyLifecycleDecision(payload []byte, headers http.Header, decision *RequestLifecycleDecision) ([]byte, http.Header) {
	if decision == nil {
		return payload, headers
	}
	if decision.ReplacePayload {
		payload = cloneBytes(decision.Payload)
	}
	if len(decision.Headers) > 0 {
		if headers == nil {
			headers = make(http.Header, len(decision.Headers))
		} else {
			headers = cloneHeader(headers)
		}
		for key, values := range decision.Headers {
			// A plugin may spell a header any way it likes. Every consumer
			// reads the map with Header.Get, which canonicalizes its lookup
			// key, so a non-canonical key would be invisible: store the
			// canonical spelling here, at the single place plugin headers
			// enter the request path.
			canonical := textproto.CanonicalMIMEHeaderKey(key)
			if canonical == "" {
				continue
			}
			headers[canonical] = append([]string(nil), values...)
		}
	}
	return payload, headers
}

func lifecycleTerminationError(term *RequestLifecycleTermination) *interfaces.ErrorMessage {
	if term == nil {
		return nil
	}
	status := term.StatusCode
	if status <= 0 {
		status = http.StatusOK
	}
	if status < http.StatusBadRequest {
		return nil
	}
	err := term.Error
	if err == nil {
		text := string(term.Body)
		if text == "" {
			text = http.StatusText(status)
		}
		err = fmt.Errorf("%s", text)
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: cloneHeader(term.Headers)}
}

func lifecycleTerminationPayload(term *RequestLifecycleTermination) ([]byte, http.Header) {
	if term == nil {
		return nil, nil
	}
	return cloneBytes(term.Body), cloneHeader(term.Headers)
}
