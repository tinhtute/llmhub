package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/clienterror"
	"github.com/therealtinhtute/llmhub/internal/runtime/executor/helps"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// ExecuteStream performs a streaming call against Meta's Responses endpoint.
// Ported from upstream CLIProxyAPI commit 65348b959425
// (meta_executor_stream.go).
func (e *MetaExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	enriched, errAuth := e.ensureAuth(ctx, auth)
	if errAuth != nil {
		return nil, errAuth
	}

	prepared, errPrepare := e.prepareResponsesRequest(ctx, enriched, req, opts, true)
	if errPrepare != nil {
		return nil, errPrepare
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, enriched)
	defer reporter.TrackFailure(ctx, &err)
	// reporter.SetTranslatedReasoningEffort has no local equivalent (recorded
	// remainder — see meta_executor_execute.go).

	baseURL, token := metaCreds(enriched)
	if strings.TrimSpace(baseURL) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "meta executor: missing provider baseURL"}
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if errRequest != nil {
		return nil, errRequest
	}
	applyMetaAPIHeaders(httpReq, enriched, token, true, opts.Headers)
	e.recordMetaRequest(ctx, enriched, url, httpReq.Header.Clone(), prepared.body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, enriched, 0)
	httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("meta executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return nil, wrapMetaUpstreamError(httpResp.StatusCode, data)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("meta executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		claudeInputTokens := helps.NewClaudeInputTokenState(prepared.from, prepared.to, prepared.responseFormat, prepared.originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		var terminalDelivered bool
		emitTranslatedLine := func(translatedLine []byte) bool {
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, translatedLine, &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		var pendingEventLine []byte
		var filteringSubscriptionUsage bool
		var skipFilteredBlankLine bool
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			trimmedLine := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmedLine, metaEventTag) {
				eventName := bytes.TrimSpace(trimmedLine[len(metaEventTag):])
				if bytes.Equal(eventName, []byte(metaSubscriptionUsageEventType)) {
					if pendingEventLine != nil {
						if !emitTranslatedLine(pendingEventLine) {
							return
						}
						pendingEventLine = nil
					}
					filteringSubscriptionUsage = true
					skipFilteredBlankLine = true
					continue
				}
				filteringSubscriptionUsage = false
				skipFilteredBlankLine = false
				if pendingEventLine != nil {
					if !emitTranslatedLine(pendingEventLine) {
						return
					}
				}
				pendingEventLine = bytes.Clone(line)
				continue
			}
			if !bytes.HasPrefix(trimmedLine, dataTag) {
				if len(trimmedLine) == 0 {
					filteringSubscriptionUsage = false
					if skipFilteredBlankLine {
						skipFilteredBlankLine = false
						continue
					}
				}
				if filteringSubscriptionUsage {
					continue
				}
				skipFilteredBlankLine = false
				if pendingEventLine != nil {
					if !emitTranslatedLine(pendingEventLine) {
						return
					}
					pendingEventLine = nil
				}
				if !emitTranslatedLine(bytes.Clone(line)) {
					return
				}
				continue
			}
			eventData := bytes.TrimSpace(trimmedLine[len(dataTag):])
			eventType := gjson.GetBytes(eventData, "type").String()
			if isMetaSubscriptionUsageEvent(eventType, eventData) {
				pendingEventLine = nil
				filteringSubscriptionUsage = false
				skipFilteredBlankLine = true
				continue
			}
			if filteringSubscriptionUsage {
				if eventType == "" {
					pendingEventLine = nil
					if gjson.ValidBytes(eventData) {
						filteringSubscriptionUsage = false
					}
					skipFilteredBlankLine = true
					continue
				}
				filteringSubscriptionUsage = false
			}
			skipFilteredBlankLine = false
			// Observe upstream content before translation or failure publication.
			// ObserveResponsesTokenEvent records the effective token TTFT and,
			// through ObserveResponseModel, the served model.
			helps.ObserveResponsesTokenEvent(reporter, eventData)
			if errEvent := metaStreamEventError(eventData); errEvent != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errEvent)
				reporter.PublishFailure(ctx, errEvent)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errEvent}:
				case <-ctx.Done():
				}
				return
			}
			switch eventType {
			case "response.output_item.done":
				xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.incomplete":
				if detail, ok := helps.ParseCodexUsage(eventData); ok {
					reporter.Publish(ctx, detail)
				}
				eventData = patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			}
			if pendingEventLine != nil {
				if !emitTranslatedLine(pendingEventLine) {
					return
				}
				pendingEventLine = nil
			}
			if !emitTranslatedLine(append([]byte("data: "), eventData...)) {
				return
			}
			if eventType == "response.completed" || eventType == "response.incomplete" {
				terminalDelivered = true
			}
		}
		if pendingEventLine != nil {
			if !emitTranslatedLine(pendingEventLine) {
				return
			}
			pendingEventLine = nil
		}
		if errScan := scanner.Err(); errScan != nil {
			if terminalDelivered && errors.Is(errScan, context.Canceled) {
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			scanErr := errScan
			if code := clienterror.HTTPStatusFromError(errScan); code > 0 {
				scanErr = statusErr{code: code, msg: errScan.Error()}
			}
			reporter.PublishFailure(ctx, scanErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: scanErr}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}
