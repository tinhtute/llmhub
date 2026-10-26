package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	internallogging "github.com/therealtinhtute/llmhub/internal/logging"
	coresession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	coreusage "github.com/therealtinhtute/llmhub/sdk/cliproxy/usage"
)

func init() {
	coreusage.RegisterPlugin(&usageQueuePlugin{})
}

type usageQueuePlugin struct{}

func (p *usageQueuePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	if !Enabled() || !UsageStatisticsEnabled() {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	modelName := strings.TrimSpace(record.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	aliasName := strings.TrimSpace(record.Alias)
	if aliasName == "" {
		aliasName = modelName
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	executorType := strings.TrimSpace(record.ExecutorType)
	if executorType == "" {
		executorType = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}
	apiKey := strings.TrimSpace(record.APIKey)
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	traceID := strings.TrimSpace(record.TraceID)
	if traceID == "" {
		traceID = requestID
	}
	executionID := strings.TrimSpace(record.RequestID)
	if executionID == "" {
		executionID = uuid.NewString()
	}
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	responseModel := strings.TrimSpace(record.ResponseModel)
	clientRequestMetadata := internallogging.GetClientRequestMetadata(ctx)
	sessionID := strings.TrimSpace(record.SessionID)
	parentSessionID := strings.TrimSpace(record.ParentSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(clientRequestMetadata.SessionID)
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	} else if parentSessionID == "" && sessionID == strings.TrimSpace(clientRequestMetadata.SessionID) {
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)
	parentSessionID = coresession.NormalizeToCanonicalUUID(parentSessionID)
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}

	tokens := tokenStats{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}

	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	fail := resolveFail(ctx, record, failed)

	detail := requestDetail{
		Timestamp:        timestamp,
		LatencyMs:        record.Latency.Milliseconds(),
		Source:           record.Source,
		AuthIndex:        record.AuthIndex,
		ResolvedClientIP: clientRequestMetadata.ResolvedClientIP,
		Tokens:           tokens,
		Failed:           failed,
		Fail:             fail,
		ResponseHeaders:  record.ResponseHeaders,
	}

	payload, err := json.Marshal(queuedUsageDetail{
		requestDetail:        detail,
		Provider:             provider,
		ExecutorType:         executorType,
		Model:                modelName,
		Alias:                aliasName,
		Endpoint:             resolveEndpoint(ctx),
		AuthType:             authType,
		APIKey:               apiKey,
		RequestID:            requestID,
		ExecutionID:          executionID,
		TraceID:              traceID,
		SessionID:            sessionID,
		ParentSessionID:      parentSessionID,
		NodeKind:             strings.TrimSpace(clientRequestMetadata.NodeKind),
		IsFork:               clientRequestMetadata.IsFork,
		IsCompaction:         clientRequestMetadata.IsCompaction,
		ReasoningEffort:      reasoningEffort,
		EffectiveServiceTier: strings.TrimSpace(record.EffectiveServiceTier),
		ResponseServiceTier:  strings.TrimSpace(record.ResponseServiceTier),
		ResponseModel:        responseModel,
	})
	if err != nil {
		return
	}
	EnqueueUsage(payload, timestamp)
	PrunePersistentUsage()
}

type queuedUsageDetail struct {
	requestDetail
	Provider        string `json:"provider"`
	ExecutorType    string `json:"executor_type"`
	Model           string `json:"model"`
	Alias           string `json:"alias"`
	Endpoint        string `json:"endpoint"`
	AuthType        string `json:"auth_type"`
	APIKey          string `json:"api_key"`
	RequestID       string `json:"request_id"`
	ExecutionID     string `json:"execution_id,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	NodeKind        string `json:"node_kind,omitempty"`
	IsFork          bool   `json:"is_fork,omitempty"`
	IsCompaction    bool   `json:"is_compaction,omitempty"`
	ReasoningEffort string `json:"reasoning_effort"`
	// EffectiveServiceTier is the tier the final outbound request carried after
	// translation and payload rules; omitted when the request specified none.
	EffectiveServiceTier string `json:"effective_service_tier,omitempty"`
	// ResponseServiceTier is the tier the upstream reported serving at; omitted
	// when the provider reports none.
	ResponseServiceTier string `json:"response_service_tier,omitempty"`
	ResponseModel       string `json:"response_model,omitempty"`
}

type requestDetail struct {
	Timestamp        time.Time   `json:"timestamp"`
	LatencyMs        int64       `json:"latency_ms"`
	Source           string      `json:"source"`
	AuthIndex        string      `json:"auth_index"`
	ResolvedClientIP string      `json:"resolved_client_ip,omitempty"`
	Tokens           tokenStats  `json:"tokens"`
	Failed           bool        `json:"failed"`
	Fail             failDetail  `json:"fail"`
	ResponseHeaders  http.Header `json:"response_headers,omitempty"`
}

type tokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type failDetail struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func resolveFail(ctx context.Context, record coreusage.Record, failed bool) failDetail {
	fail := failDetail{
		StatusCode: record.Fail.StatusCode,
		Body:       strings.TrimSpace(record.Fail.Body),
	}
	if !failed {
		return failDetail{StatusCode: 200}
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = internallogging.GetResponseStatus(ctx)
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = 500
	}
	return fail
}

func resolveSuccess(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

func resolveEndpoint(ctx context.Context) string {
	return strings.TrimSpace(internallogging.GetEndpoint(ctx))
}

const httpStatusBadRequest = 400
