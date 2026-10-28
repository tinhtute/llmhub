package executor

import (
	"net/http"
	"net/url"

	sdktranslator "github.com/therealtinhtute/llmhub/sdk/translator"
)

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
const RequestedModelMetadataKey = "requested_model"

// AuthSelectionModelMetadataKey overrides the model used only for auth selection.
// Restored execution requests carry the upstream execution model in Request.Model
// while this key keeps the original selection model for auth picking
// (upstream parity).
const AuthSelectionModelMetadataKey = "auth_selection_model"

// RequestPathMetadataKey stores the inbound HTTP request path (e.g. "/v1/images/generations") in Options.Metadata.
// It is optional and may be absent for non-HTTP executions.
const RequestPathMetadataKey = "request_path"

// DisallowFreeAuthMetadataKey instructs auth selection to skip known free-tier credentials.
const DisallowFreeAuthMetadataKey = "disallow_free_auth"

// ReasoningEffortMetadataKey stores the client-requested reasoning effort for usage logs.
const ReasoningEffortMetadataKey = "reasoning_effort"

const (
	// PinnedAuthMetadataKey locks execution to a specific auth ID.
	PinnedAuthMetadataKey = "pinned_auth_id"
	// SelectedAuthMetadataKey stores the auth ID selected by the scheduler.
	SelectedAuthMetadataKey = "selected_auth_id"
	// SelectedAuthCallbackMetadataKey carries an optional callback invoked with the selected auth ID.
	SelectedAuthCallbackMetadataKey = "selected_auth_callback"
	// ExecutionSessionMetadataKey identifies a long-lived downstream execution session.
	ExecutionSessionMetadataKey = "execution_session_id"
	// DerivedSessionIDMetadataKey stores a stable session identity inferred from request context.
	DerivedSessionIDMetadataKey = "derived_session_id"
	// SessionAffinityProviderMetadataKey carries the affinity selection namespace
	// (provider string, e.g. the literal "mixed" pool key) used by SessionAffinitySelector.Pick,
	// so OnResult keys the session cache identically to how selection read it.
	SessionAffinityProviderMetadataKey = "session_affinity_provider"
	// SessionAffinityModelMetadataKey carries the model used during session affinity selection.
	SessionAffinityModelMetadataKey = "session_affinity_model"
	// LCPAffinitySessionIDMetadataKey stores an LCP-only routing identity.
	LCPAffinitySessionIDMetadataKey = "lcp_affinity_session_id"
	// CanonicalSessionIDMetadataKey stores the single unified session identity reconciled.
	CanonicalSessionIDMetadataKey = "canonical_session_id"
	// ParentSessionIDMetadataKey stores the parent session identity for hierarchical sessions and forks.
	// For top-level Merkle LCP forks, it represents the deterministic Merkle prefix hash at the divergence point.
	ParentSessionIDMetadataKey = "parent_session_id"
	// IsForkMetadataKey indicates whether the request represents a conversational branch or fork.
	IsForkMetadataKey = "is_fork"
	// IsCompactionMetadataKey indicates whether the request represents a context compaction continuation.
	IsCompactionMetadataKey = "is_compaction"
	// NodeKindMetadataKey indicates the session DAG topology kind ("compaction", "fork", or "trunk").
	NodeKindMetadataKey = "node_kind"
	// LCPTailFingerprintsMetadataKey stores the actual trailing turn fingerprints for context compaction matching.
	LCPTailFingerprintsMetadataKey = "lcp_tail_fingerprints"
	// LCPEnvironmentDigestMetadataKey stores the environment digest across all system and developer instructions.
	LCPEnvironmentDigestMetadataKey = "lcp_environment_digest"
	// LCPAccessGenerationMetadataKey stores the monotonic access generation when an LCP entry was touched or bound.
	LCPAccessGenerationMetadataKey = "lcp_access_generation"
	// LCPFingerprintMetadataKey stores bounded request-scoped turn fingerprints.
	LCPFingerprintMetadataKey = "lcp_fingerprints"
	// LCPMinPrefixLengthMetadataKey stores the minimum eligible prefix boundary.
	LCPMinPrefixLengthMetadataKey = "lcp_min_prefix_length"
	// CallerScopeMetadataKey isolates inferred session identities between downstream callers.
	CallerScopeMetadataKey = "caller_scope"
)

// Request encapsulates the translated payload that will be sent to a provider executor.
type Request struct {
	// Model is the upstream model identifier after translation.
	Model string
	// Payload is the provider specific JSON payload.
	Payload []byte
	// Format represents the provider payload schema.
	Format sdktranslator.Format
	// Metadata carries optional provider specific execution hints.
	Metadata map[string]any
}

// Options controls execution behavior for both streaming and non-streaming calls.
type Options struct {
	// Stream toggles streaming mode.
	Stream bool
	// Alt carries optional alternate format hint (e.g. SSE JSON key).
	Alt string
	// Headers are forwarded to the provider request builder.
	Headers http.Header
	// Query contains optional query string parameters.
	Query url.Values
	// OriginalRequest preserves the inbound request bytes prior to translation.
	OriginalRequest []byte
	// SourceFormat identifies the inbound schema.
	SourceFormat sdktranslator.Format
	// ResponseFormat identifies the downstream response schema.
	// When empty, SourceFormat is used as the response target.
	ResponseFormat sdktranslator.Format
	// Metadata carries extra execution hints shared across selection and executors.
	Metadata map[string]any
	// ExecutionLifecycle owns Home-dispatched execution resources. Executors must not add it to request metadata.
	ExecutionLifecycle ExecutionLifecycle
	// KeepTopLevelEffort tells SIWC request-body rules to keep the resolved
	// top-level reasoning.effort when configuration_update items are collapsed.
	KeepTopLevelEffort bool
}

// ResponseFormatOrSource returns the response target format for an execution.
// Ported from upstream CLIProxyAPI sdk/cliproxy/executor/types.go.
func ResponseFormatOrSource(opts Options) sdktranslator.Format {
	if opts.ResponseFormat != "" {
		return opts.ResponseFormat
	}
	return opts.SourceFormat
}

// EnsureMetadata initializes and returns Metadata, ensuring it is non-nil.
func (o *Options) EnsureMetadata() map[string]any {
	if o.Metadata == nil {
		o.Metadata = make(map[string]any)
	}
	return o.Metadata
}

// Response wraps either a full provider response or metadata for streaming flows.
type Response struct {
	// Payload is the provider response in the executor format.
	Payload []byte
	// Metadata exposes optional structured data for translators.
	Metadata map[string]any
	// Headers carries upstream HTTP response headers for passthrough to clients.
	Headers http.Header
}

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	Payload []byte
	// Metadata exposes optional structured data for runtime accounting.
	Metadata map[string]any
	// Err reports any terminal error encountered while producing chunks.
	Err error
}

// StreamResult wraps the streaming response, providing both the chunk channel
// and the upstream HTTP response headers captured before streaming begins.
type StreamResult struct {
	// Headers carries upstream HTTP response headers from the initial connection.
	Headers http.Header
	// Chunks is the channel of streaming payload units.
	Chunks <-chan StreamChunk
}

// StatusError represents an error that carries an HTTP-like status code.
// Provider executors should implement this when possible to enable
// better auth state updates on failures (e.g., 401/402/429).
type StatusError interface {
	error
	StatusCode() int
}

// StreamTruncatedError identifies a locally synthesized failure for an
// accepted upstream stream that ended without its terminal frame. The upstream
// generation was already submitted, so auth managers must not cool the
// credential or replay the request on another credential because of it.
type StreamTruncatedError interface {
	error
	IsStreamTruncated() bool
}
