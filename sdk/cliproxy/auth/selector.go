package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/logging"
	"github.com/therealtinhtute/llmhub/internal/thinking"
	cliproxyexecutor "github.com/therealtinhtute/llmhub/sdk/cliproxy/executor"
	cliproxysession "github.com/therealtinhtute/llmhub/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
//
// Rotation continues from the identity of the previous pick rather than from a numeric
// index. Candidate slices shrink whenever a retry excludes already tried credentials or a
// credential enters cooldown, and indexing a monotonic counter into a shrinking slice
// silently re-seats the rotation, which starves some credentials and hammers others.
type RoundRobinSelector struct {
	mu         sync.Mutex
	lastPicked map[string]string
	maxKeys    int
}

// WeightedRoundRobinSelector provides smooth weighted round-robin selection.
type WeightedRoundRobinSelector struct {
	mu      sync.Mutex
	states  map[string]*smoothWeightedState
	maxKeys int
}

type smoothWeightedState struct {
	current map[string]int64
	weights map[string]int64
}

type weightedSelectorStateModelKey struct{}

func withWeightedSelectorStateModel(ctx context.Context, selector Selector, routeModel string) context.Context {
	if _, ok := selector.(*WeightedRoundRobinSelector); !ok || strings.TrimSpace(routeModel) == "" {
		return ctx
	}
	return context.WithValue(ctx, weightedSelectorStateModelKey{}, routeModel)
}

func weightedSelectorStateModel(ctx context.Context, availabilityModel string) string {
	if ctx != nil {
		if routeModel, ok := ctx.Value(weightedSelectorStateModelKey{}).(string); ok && strings.TrimSpace(routeModel) != "" {
			return routeModel
		}
	}
	return availabilityModel
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
	cause    error
}

// NewModelCooldownError creates an error representing model-level cooldown.
func NewModelCooldownError(model, provider string, resetIn time.Duration) error {
	return newModelCooldownErrorWithCause(model, provider, resetIn, nil)
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	return newModelCooldownErrorWithCause(model, provider, resetIn, nil)
}

func newModelCooldownErrorWithCause(model, provider string, resetIn time.Duration, cause error) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
		cause:    cause,
	}
}

func (e *modelCooldownError) IsModelCooldown() bool {
	return true
}

func (e *modelCooldownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	if e.cause != nil {
		if causeText := ExtractUpstreamErrorSummary(e.cause.Error()); causeText != "" {
			errorBody["last_upstream_error"] = causeText
			message += fmt.Sprintf(" (last error: %s)", causeText)
			errorBody["message"] = message
		}
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

var (
	sanitizerSchemeAuthPattern             = regexp.MustCompile(`(?i)((?:[A-Za-z0-9.+_\-]+:)?//)(?:[^:\s/@]+:[^@\s]+|[^@\s/]+)@`)
	sanitizerQueryParamPattern             = regexp.MustCompile(`(?i)([?&][A-Za-z0-9_.-]*(?:key|token|secret|password|auth|sig|signature)=)[^&\s,\r\n;]+`)
	sanitizerCookiePattern                 = regexp.MustCompile(`(?i)\b(?:set-)?cookie\s*:[^\r\n]+`)
	sanitizerAuthHeaderPattern             = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*[^\r\n]+`)
	sanitizerNaturalSecretPattern          = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:api[ _-]?key|access[ _-]?token|client[ _-]?secret|private[ _-]?key|secret[ _-]?key|password|secret|token|credentials?|sessionid))\s*(?:(?:is|was|provided|used)?\s*[:= ]\s*|\s+is\s+|\s+was\s+|\s+provided\s+|\s+)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|(?:[^\r\n;,|]+?(?:\s+(?:and|with|for|via)\s+|[,;|]|\r|\n|$)|[^\r\n;,|]+))`)
	sanitizerKVPattern                     = regexp.MustCompile(`(?i)((?:'|")?(?:[A-Za-z0-9_.-]*(?:key|token|secret|password|credential|credentials|bearer|sessionid|auth|signature|sig))(?:'|")?\s*[=:]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|(?:[^\r\n;,|]+?(?:\s+(?:and|with|for|via)\s+|[,;|]|\r|\n|$)|[^\r\n;,|]+))`)
	sanitizerInvalidTokenPattern           = regexp.MustCompile(`(?i)\b(invalid|bad|expired|unknown)\s+(?:api\s+key|access\s+token|refresh\s+token|token|key|secret|password|credentials?|bearer)\s*(?:[:= ]\s*)?(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s,\r\n;]+)`)
	sanitizerSKKeyPattern                  = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9._~+/=-]{6,}|ghp_[A-Za-z0-9._~+/=-]{6,})\b`)
	sanitizerBearerPattern                 = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	sanitizerDoubleQuotedPathPattern       = regexp.MustCompile(`"/[^"\r\n]+"`)
	sanitizerSingleQuotedPathPattern       = regexp.MustCompile(`'/[^'\r\n]+'`)
	sanitizerBacktickQuotedPathPattern     = regexp.MustCompile("`/[^`\r\n]+`")
	sanitizerPathConnectorPattern          = regexp.MustCompile(`(?i)\s+(to|from|into|onto|for|via|with|and)\s+/`)
	sanitizerUnixPathBeforeColonPattern    = regexp.MustCompile(`(^|[\s\(\[\{<"';,=])(/(?:[^/\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/\s\r\n"',;?#()<>{}\[\]]+)*/)*[^/:\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/:\s\r\n"',;?#()<>{}\[\]]+)*):\s+[A-Za-z0-9]`)
	sanitizerUnixPathBeforeNextPathPattern = regexp.MustCompile(`(^|[\s\(\[\{<"';,=])(/(?:[^/\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/\s\r\n"',;?#()<>{}\[\]]+)*/)*[^/:\s\r\n"',;?#()<>{}\[\]]+(?::[^/\s\r\n"',;?#()<>{}\[\]]+|\s+[^/:\s\r\n"',;?#()<>{}\[\]]+)*)(\s+/)`)
	sanitizerUnixPathStandardPattern       = regexp.MustCompile(`(^|[\s\(\[\{<"';,=])(/(?:[^/\s\r\n"',;?#()<>{}\[\]]+(?:\s+[^/\s\r\n"',;?#()<>{}\[\]]+)*/)*[^/:\s\r\n"',;?#()<>{}\[\]]+(?::[^/:\s\r\n"',;?#()<>{}\[\]]+)?)`)
	sanitizerFileExtPathPattern            = regexp.MustCompile(`(^|[\s"'` + "`" + `(\[,;=])(/[^\s:\r\n"'` + "`" + `,;\])>]+(?:\s+[^\s:\r\n"'` + "`" + `,;\])>]+)*\.(?:json|yaml|yml|key|pem|txt|log|toml|conf|env|crt|cer))`)
	sanitizerWindowsPathPattern            = regexp.MustCompile(`(?i)\b[A-Za-z]:\\[^\r\n:,;'"<>]+`)
	sanitizerWindowsUNCPathPattern         = regexp.MustCompile(`\\\\[^\r\n:,;'"<>]+\\[^\r\n:,;'"<>]+`)
)

// ExtractUpstreamErrorSummary extracts and sanitizes a concise error summary from upstream error strings.
func ExtractUpstreamErrorSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	jsonPart := raw
	if idx := strings.Index(raw, ": {"); idx != -1 && idx < 50 {
		jsonPart = strings.TrimSpace(raw[idx+2:])
	}
	if gjson.Valid(jsonPart) {
		parsed := gjson.Parse(jsonPart)
		var code, message string
		if errNode := parsed.Get("error"); errNode.Exists() {
			if errNode.IsObject() {
				code = strings.TrimSpace(errNode.Get("code").String())
				if code == "" {
					code = strings.TrimSpace(errNode.Get("type").String())
				}
				message = strings.TrimSpace(errNode.Get("message").String())
			} else if errNode.Type == gjson.String {
				message = strings.TrimSpace(errNode.String())
			}
		}
		if code == "" && message == "" {
			code = strings.TrimSpace(parsed.Get("code").String())
			if code == "" {
				code = strings.TrimSpace(parsed.Get("type").String())
			}
			message = strings.TrimSpace(parsed.Get("message").String())
		}
		var summary string
		if code != "" && message != "" {
			if strings.EqualFold(code, message) || strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
				summary = message
			} else {
				summary = code + ": " + message
			}
		} else if message != "" {
			summary = message
		} else if code != "" {
			summary = code
		}
		if summary != "" {
			return SanitizeUpstreamErrorSummary(summary)
		}
	}
	return SanitizeUpstreamErrorSummary(raw)
}

func sanitizeUpstreamErrorSummaryNoTruncate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = sanitizerSchemeAuthPattern.ReplaceAllString(s, "${1}[REDACTED_AUTH]@")
	s = sanitizerQueryParamPattern.ReplaceAllString(s, "${1}[REDACTED]")
	s = sanitizerDoubleQuotedPathPattern.ReplaceAllString(s, `"[REDACTED_PATH]"`)
	s = sanitizerSingleQuotedPathPattern.ReplaceAllString(s, `'[REDACTED_PATH]'`)
	s = sanitizerBacktickQuotedPathPattern.ReplaceAllString(s, "`[REDACTED_PATH]`")
	s = sanitizerWindowsPathPattern.ReplaceAllString(s, "[REDACTED_PATH]")
	s = sanitizerWindowsUNCPathPattern.ReplaceAllString(s, "[REDACTED_PATH]")

	// Handle connector-separated paths like "copy /tmp/a TO /tmp/b: denied"
	if loc := sanitizerPathConnectorPattern.FindStringSubmatchIndex(s); loc != nil {
		firstPart := s[:loc[0]]
		connRaw := s[loc[0] : loc[1]-1]
		secondPart := "/" + s[loc[1]:]
		return sanitizeUpstreamErrorSummaryNoTruncate(firstPart) + connRaw + sanitizeUpstreamErrorSummaryNoTruncate(secondPart)
	}

	// Handle path before colon error delimiter, finding the first known error or first colon
	var colonIdx = -1
	knownErrorPrefixes := []string{
		"permission denied", "no such file", "file not found", "access denied",
		"operation not permitted", "denied", "read-only", "is a directory",
		"not a directory", "cannot find", "no space", "connection refused",
		"timeout", "failed", "error", "not supported", "invalid argument",
	}
	for _, errWord := range knownErrorPrefixes {
		target := ": " + errWord
		if idx := strings.Index(strings.ToLower(s), target); idx != -1 {
			if colonIdx == -1 || idx < colonIdx {
				colonIdx = idx
			}
		}
	}
	if colonIdx == -1 {
		colonIdx = strings.Index(s, ": ")
	}

	if colonIdx != -1 {
		prefix := s[:colonIdx]
		suffix := s[colonIdx:]
		slashIdx := -1
		for i := 0; i < len(prefix); i++ {
			if prefix[i] == '/' {
				if i > 0 && prefix[i-1] == '/' {
					continue
				}
				if i >= 6 && (strings.HasSuffix(prefix[:i], "http:/") || strings.HasSuffix(prefix[:i], "https:/") || strings.HasSuffix(prefix[:i], "://")) {
					continue
				}
				if i == 0 || prefix[i-1] == ' ' || prefix[i-1] == '\t' || prefix[i-1] == '(' || prefix[i-1] == '[' || prefix[i-1] == '{' || prefix[i-1] == '<' || prefix[i-1] == '"' || prefix[i-1] == '\'' || prefix[i-1] == '`' || prefix[i-1] == '=' {
					slashIdx = i
					break
				}
			}
		}
		if slashIdx != -1 {
			lead := prefix[:slashIdx]
			pathPart := prefix[slashIdx:]
			trailPunct := ""
			for len(pathPart) > 0 && (pathPart[len(pathPart)-1] == ')' || pathPart[len(pathPart)-1] == ']' || pathPart[len(pathPart)-1] == '}' || pathPart[len(pathPart)-1] == '>') {
				trailPunct = string(pathPart[len(pathPart)-1]) + trailPunct
				pathPart = pathPart[:len(pathPart)-1]
			}
			if strings.Contains(pathPart, " /") {
				segments := strings.Split(pathPart, " /")
				for j := range segments {
					segments[j] = "[REDACTED_PATH]"
				}
				pathPart = strings.Join(segments, " ")
			} else {
				pathPart = "[REDACTED_PATH]"
			}
			s = lead + pathPart + trailPunct + suffix
		}
	}

	for i := 0; i < 3; i++ {
		prev := s
		s = sanitizerUnixPathStandardPattern.ReplaceAllString(s, "${1}[REDACTED_PATH]")
		if s == prev {
			break
		}
	}
	s = sanitizerFileExtPathPattern.ReplaceAllString(s, "${1}[REDACTED_PATH]")
	s = sanitizerCookiePattern.ReplaceAllString(s, "Cookie: [REDACTED]")
	s = sanitizerAuthHeaderPattern.ReplaceAllString(s, "Authorization: [REDACTED]")
	s = sanitizerSKKeyPattern.ReplaceAllString(s, "sk-[REDACTED]")
	s = sanitizerBearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = sanitizerInvalidTokenPattern.ReplaceAllString(s, `${1} token [REDACTED]`)
	s = sanitizerNaturalSecretPattern.ReplaceAllString(s, `${1}: [REDACTED]`)
	s = sanitizerKVPattern.ReplaceAllString(s, `${1}[REDACTED]`)
	return s
}

// SanitizeUpstreamErrorSummary removes sensitive credentials, tokens, and paths, and bounds length.
func SanitizeUpstreamErrorSummary(s string) string {
	s = sanitizeUpstreamErrorSummaryNoTruncate(s)
	runes := []rune(s)
	if len(runes) > 256 {
		if len(runes) > 253 {
			return string(runes[:253]) + "..."
		}
		return string(runes) + "..."
	}
	return s
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

func collectAvailableByPriority(auths []*Auth, model string, now time.Time) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			available[priority] = append(available[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
		}
		// Ported from upstream CLIProxyAPI commit 6724a95851f9: track the earliest
		// recovery deadline for every non-disabled block (cooldown or transient),
		// so unavailable errors can advertise a locally computed Retry-After hint.
		if reason != blockReasonDisabled && next.After(now) && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	return getAvailableAuthsWithPriorityMode(auths, provider, model, now, false)
}

func getAvailableAuthsAcrossPriorities(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	return getAvailableAuthsWithPriorityMode(auths, provider, model, now, true)
}

func getAvailableAuthsWithPriorityMode(auths []*Auth, provider, model string, now time.Time, allPriorities bool) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, newAuthUnavailableError(earliest, now)
	}

	return availableAuthsFromPriorityBuckets(availableByPriority, allPriorities), nil
}

// availableAuthsFromPriorityBuckets flattens availability buckets into a stable, ID-sorted slice.
// When allPriorities is false only the highest available priority tier is returned.
// When allPriorities is true every tier is merged, so the result carries no priority ordering:
// use it for membership checks or feed it to highestPriorityAuths, never as a priority-ordered
// selection order.
func availableAuthsFromPriorityBuckets(availableByPriority map[int][]*Auth, allPriorities bool) []*Auth {
	var candidates []*Auth
	if allPriorities {
		total := 0
		for _, bucket := range availableByPriority {
			total += len(bucket)
		}
		candidates = make([]*Auth, 0, total)
		for _, bucket := range availableByPriority {
			candidates = append(candidates, bucket...)
		}
	} else {
		bestPriority := 0
		found := false
		for priority := range availableByPriority {
			if !found || priority > bestPriority {
				bestPriority = priority
				found = true
			}
		}
		bucket := availableByPriority[bestPriority]
		candidates = make([]*Auth, 0, len(bucket))
		candidates = append(candidates, bucket...)
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	}
	return candidates
}

// highestPriorityAuths narrows an availability slice to its highest priority tier while
// preserving the input order. The input slice is returned unchanged when every candidate
// already shares the highest priority, so the common single-tier case allocates nothing.
func highestPriorityAuths(auths []*Auth) []*Auth {
	if len(auths) <= 1 {
		return auths
	}
	bestPriority := 0
	bestCount := 0
	for _, auth := range auths {
		priority := authPriority(auth)
		switch {
		case bestCount == 0 || priority > bestPriority:
			bestPriority = priority
			bestCount = 1
		case priority == bestPriority:
			bestCount++
		}
	}
	if bestCount == len(auths) {
		return auths
	}
	highest := make([]*Auth, 0, bestCount)
	for _, auth := range auths {
		if authPriority(auth) == bestPriority {
			highest = append(highest, auth)
		}
	}
	return highest
}

// Pick selects the next available auth for the provider in a round-robin manner.
// For gemini-cli virtual auths (identified by the gemini_virtual_parent attribute),
// a two-level round-robin is used: first cycling across credential groups (parent
// accounts), then cycling within each group's project auths.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]string)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	s.ensureRotationKey(key, limit)
	picked := available[successorIndex(available, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// successorIndex returns the index of the first candidate ordered after lastID, wrapping to
// the start of the ring. Candidates arrive sorted by ID, so this resumes the rotation at the
// credential that follows the previous pick even when candidates were filtered out in
// between. An empty lastID starts at the head.
func successorIndex(available []*Auth, lastID string) int {
	if lastID == "" {
		return 0
	}
	index := sort.Search(len(available), func(i int) bool { return available[i].ID > lastID })
	if index >= len(available) {
		return 0
	}
	return index
}

// ensureRotationKey ensures the rotation map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureRotationKey(key string, limit int) {
	if _, ok := s.lastPicked[key]; !ok && len(s.lastPicked) >= limit {
		s.lastPicked = make(map[string]string)
	}
}

func positiveWeightAuths(auths []*Auth) []*Auth {
	weightedCandidates := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if authWeight(auth) > 0 {
			weightedCandidates = append(weightedCandidates, auth)
		}
	}
	return weightedCandidates
}

// Pick selects the next available auth using smooth weighted round-robin.
func (s *WeightedRoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	available, errAvailable := getAvailableAuths(positiveWeightAuths(auths), provider, model, time.Now())
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	stateModel := weightedSelectorStateModel(ctx, model)
	key := provider + ":" + canonicalModelKey(stateModel)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*smoothWeightedState)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.states[key]; !ok && len(s.states) >= limit {
		s.states = make(map[string]*smoothWeightedState)
	}
	state := s.states[key]
	if state == nil {
		state = &smoothWeightedState{}
		s.states[key] = state
	}
	weights := authWeightVector(available)
	state.prepare(weights)
	picked := pickSmoothWeightedAuth(available, state.current)
	if picked == nil {
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with positive weight"}
	}
	return picked, nil
}

// maxSmoothWeightedStateEntries bounds a single accumulator map so credentials that are
// removed permanently cannot leak entries. Real pools stay far below this bound, so the
// transient subsets produced by retry exclusions and cooldowns are never pruned.
const maxSmoothWeightedStateEntries = 1024

// prepare syncs the configured weights into the state without discarding accumulated
// credits. Credits are reset only when a credential's configured weight actually changes,
// never when the candidate set shrinks temporarily (retry exclusions, cooldowns, session
// affinity), because discarding credits there would collapse selection onto the first
// candidate in slice order.
func (s *smoothWeightedState) prepare(weights map[string]int64) {
	if s.current == nil || weightsConfigChanged(s.weights, weights) {
		s.current = make(map[string]int64, len(weights))
	}
	if s.weights == nil {
		s.weights = make(map[string]int64, len(weights))
	}
	for authID, weight := range weights {
		s.weights[authID] = weight
	}
	s.pruneStale(weights)
}

// pruneStale drops entries for credentials outside the current candidate set, but only
// once a map exceeds the safety bound, so ordinary transient exclusions keep their credits.
func (s *smoothWeightedState) pruneStale(weights map[string]int64) {
	if len(s.current) <= maxSmoothWeightedStateEntries && len(s.weights) <= maxSmoothWeightedStateEntries {
		return
	}
	for authID := range s.current {
		if _, ok := weights[authID]; !ok {
			delete(s.current, authID)
		}
	}
	for authID := range s.weights {
		if _, ok := weights[authID]; !ok {
			delete(s.weights, authID)
		}
	}
}

// weightsConfigChanged reports whether any credential present in both vectors has a
// different configured weight. Credentials that are merely missing from one side are
// ignored, since a candidate subset is not a configuration change.
func weightsConfigChanged(left, right map[string]int64) bool {
	if len(left) == 0 {
		return false
	}
	for authID, weight := range right {
		if previous, ok := left[authID]; ok && previous != weight {
			return true
		}
	}
	return false
}

func authWeight(auth *Auth) int64 {
	return CredentialWeight(auth)
}

func authWeightVector(auths []*Auth) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight := authWeight(auth); weight > 0 {
			weights[auth.ID] = weight
		}
	}
	return weights
}

func pickSmoothWeightedAuth(auths []*Auth, current map[string]int64) *Auth {
	var picked *Auth
	var pickedCurrent int64
	var totalWeight int64
	for _, auth := range auths {
		weight := authWeight(auth)
		if auth == nil || weight <= 0 {
			continue
		}
		current[auth.ID] = saturatingAddInt64(current[auth.ID], weight)
		totalWeight = saturatingAddInt64(totalWeight, weight)
		if picked == nil || current[auth.ID] > pickedCurrent {
			picked = auth
			pickedCurrent = current[auth.ID]
		}
	}
	if picked == nil {
		return nil
	}
	current[picked.ID] = saturatingAddInt64(current[picked.ID], -totalWeight)
	return picked
}

func saturatingAddInt64(value, delta int64) int64 {
	if delta > 0 && value > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && value < math.MinInt64-delta {
		return math.MinInt64
	}
	return value + delta
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			// Non-virtual auth present; fall back to flat round-robin.
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	// Collect parent IDs in sorted order for stable cursor indexing.
	parentOrder := make([]string, 0, len(groups))
	for p := range groups {
		parentOrder = append(parentOrder, p)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return available[0], nil
}

// IsAuthAvailableForModel reports whether auth is eligible for selection on model at now.
func IsAuthAvailableForModel(auth *Auth, model string, now time.Time) bool {
	blocked, _, _ := isAuthBlockedForModel(auth, model, now)
	return !blocked
}

// modelStateBlock reports whether a single model state blocks selection, mirroring
// the per-state logic previously inlined in isAuthBlockedForModel.
func modelStateBlock(state *ModelState, now time.Time) (bool, blockReason, time.Time) {
	if state == nil {
		return false, blockReasonNone, time.Time{}
	}
	if state.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if !state.Unavailable {
		return false, blockReasonNone, time.Time{}
	}
	if state.NextRetryAfter.IsZero() {
		return false, blockReasonNone, time.Time{}
	}
	if state.NextRetryAfter.After(now) {
		next := state.NextRetryAfter
		if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
			next = state.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if state.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if hasUnauthorizedAuthFailure(auth) {
		return true, blockReasonOther, time.Time{}
	}
	// Credentials whose access token already expired are demoted out of
	// scheduling until a refresh renews them (upstream 9812b1e76872).
	if exp, ok := auth.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(now) {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return true, blockReasonCooldown, auth.Quota.NextRecoverAt
	}
	if blocked, next := kiroProviderQuotaBlocked(auth, now); blocked {
		if !next.IsZero() {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, time.Time{}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			// All thinking-suffix variant states map to the same canonical key, so a
			// cooldown on any variant blocks every other variant of the same model.
			modelKey := canonicalModelKey(model)
			matched := false
			blocked := false
			blockedReason := blockReasonNone
			nextRetry := time.Time{}
			for stateModel, state := range auth.ModelStates {
				if state == nil || canonicalModelKey(stateModel) != modelKey {
					continue
				}
				matched = true
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				stateBlocked, reason, next := modelStateBlock(state, now)
				if !stateBlocked {
					continue
				}
				if next.IsZero() {
					return true, reason, time.Time{}
				}
				if !blocked || next.After(nextRetry) || (next.Equal(nextRetry) && reason == blockReasonCooldown) {
					blocked = true
					blockedReason = reason
					nextRetry = next
				}
			}
			if matched {
				return blocked, blockedReason, nextRetry
			}
			// Auth-level availability can aggregate failures from other models.
			return false, blockReasonNone, time.Time{}
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

func kiroProviderQuotaBlocked(auth *Auth, now time.Time) (bool, time.Time) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") || auth.Metadata == nil {
		return false, time.Time{}
	}
	raw := auth.Metadata["kiro_quota"]
	if raw == nil {
		return false, time.Time{}
	}
	source, ok := raw.(map[string]any)
	if !ok {
		if marshaled, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(marshaled, &source)
		}
	}
	if len(source) == 0 {
		return false, time.Time{}
	}
	if kiroQuotaOverageEnabled(source) {
		return false, time.Time{}
	}
	current, limit, next, ok := kiroQuotaUsage(source)
	if !ok || limit <= 0 || current < limit {
		return false, time.Time{}
	}
	if !next.IsZero() && !next.After(now) {
		return false, time.Time{}
	}
	return true, next
}

func kiroQuotaOverageEnabled(source map[string]any) bool {
	if strings.EqualFold(strings.TrimSpace(authQuotaString(firstAuthQuotaValue(source, "overage_status", "overageStatus"))), "ENABLED") {
		return true
	}
	for _, row := range kiroQuotaRows(source["quotas"]) {
		if strings.EqualFold(strings.TrimSpace(authQuotaString(firstAuthQuotaValue(row, "overage_status", "overageStatus"))), "ENABLED") {
			return true
		}
	}
	return false
}

func kiroQuotaUsage(source map[string]any) (float64, float64, time.Time, bool) {
	current, okCurrent := authQuotaNumber(firstAuthQuotaValue(source, "current"))
	limit, okLimit := authQuotaNumber(firstAuthQuotaValue(source, "limit"))
	if okCurrent && okLimit && limit > 0 {
		next := authQuotaTime(firstAuthQuotaValue(source, "next_reset_at", "nextResetAt"))
		return current, limit, next, true
	}
	if row, ok := preferredKiroQuotaRow(kiroQuotaRows(source["quotas"])); ok {
		rowCurrent, rowCurrentOK := authQuotaNumber(firstAuthQuotaValue(row, "current", "used"))
		rowLimit, rowLimitOK := authQuotaNumber(firstAuthQuotaValue(row, "limit", "total"))
		if rowCurrentOK && rowLimitOK {
			return rowCurrent, rowLimit, authQuotaTime(firstAuthQuotaValue(row, "reset_at", "resetAt", "next_reset_at", "nextResetAt")), true
		}
	}
	return 0, 0, time.Time{}, false
}

func preferredKiroQuotaRow(rows []map[string]any) (map[string]any, bool) {
	var fallback map[string]any
	for _, row := range rows {
		if authQuotaBool(firstAuthQuotaValue(row, "free_trial", "freeTrial")) {
			continue
		}
		if fallback == nil {
			fallback = row
		}
		id := strings.ToLower(strings.TrimSpace(authQuotaString(firstAuthQuotaValue(row, "id", "resource_type", "resourceType", "name"))))
		if id == "agentic_request" {
			return row, true
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

func kiroQuotaRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := typed[key]
			if row, ok := value.(map[string]any); ok {
				if row["id"] == nil {
					row["id"] = key
				}
				values = append(values, row)
			} else {
				values = append(values, map[string]any{"id": key, "value": value})
			}
		}
	default:
		if marshaled, err := json.Marshal(raw); err == nil {
			var decoded []any
			if err := json.Unmarshal(marshaled, &decoded); err == nil {
				values = decoded
			} else {
				var decodedMap map[string]any
				if err := json.Unmarshal(marshaled, &decodedMap); err == nil {
					return kiroQuotaRows(decodedMap)
				}
			}
		}
	}
	rows := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if row, ok := value.(map[string]any); ok {
			rows = append(rows, row)
			continue
		}
		if marshaled, err := json.Marshal(value); err == nil {
			var row map[string]any
			if err := json.Unmarshal(marshaled, &row); err == nil {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func firstAuthQuotaValue(source map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := source[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func authQuotaString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func authQuotaBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && parsed
	default:
		return false
	}
}

func authQuotaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func authQuotaTime(value any) time.Time {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC()
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || strings.HasPrefix(trimmed, "0001-01-01") {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed.UTC()
			}
		}
		if unix, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return authQuotaUnixTime(unix)
		}
	case float64:
		return authQuotaUnixTime(int64(typed))
	case int64:
		return authQuotaUnixTime(typed)
	case int:
		return authQuotaUnixTime(int64(typed))
	case json.Number:
		if unix, err := typed.Int64(); err == nil {
			return authQuotaUnixTime(unix)
		}
	}
	return time.Time{}
}

func authQuotaUnixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value > 1e12 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

// SessionAffinitySelector wraps another selector with session-sticky behavior.
// It extracts session ID from multiple sources and maintains session-to-auth
// mappings with automatic failover when the bound auth becomes unavailable.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
	matcher  *cliproxysession.MerklePrefixMatcher
}

// SessionAffinityConfig configures the session affinity selector.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelector creates a new session-aware selector.
func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
}

// NewSessionAffinitySelectorWithConfig creates a selector with custom configuration.
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{
		fallback: cfg.Fallback,
		cache:    NewSessionCache(cfg.TTL),
		matcher:  cliproxysession.NewMerklePrefixMatcher(cfg.TTL),
	}
}

// Trees returns a backward-compatible in-memory session tree store.
// Deprecated: Session tree management has moved to Home.
func (s *SessionAffinitySelector) Trees() *cliproxysession.InMemorySessionTreeStore {
	return cliproxysession.NewInMemorySessionTreeStore(0, time.Hour)
}

// Pick selects an auth with session affinity when possible.
// Explicit Claude Code, Codex, OpenCode, pi, and request-body session signals
// precede execution metadata, stable derived identity, and the legacy hash fallback.
//
// An established binding outranks credential priority: a bound credential that is still
// available is reused even when a higher-priority credential recovers. Credential priority
// applies to cold bindings, requests without a session, and genuine bound-credential
// failover, so the fallback selector only ever receives the highest available priority tier.
//
// Note: The cache key includes provider, session ID, and model to handle cases where
// a session uses multiple models (e.g., gemini-2.5-pro and gemini-3-flash-preview)
// that may be supported by different auth credentials, and to avoid cross-provider conflicts.
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	entry := selectorLogEntry(ctx)
	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model
	explicitID, explicitFallbackID := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	primaryID, fallbackID := explicitID, explicitFallbackID
	if primaryID == "" {
		if auth, handled, errLCP := s.pickLCP(ctx, provider, model, opts, auths, entry); handled || errLCP != nil {
			return auth, errLCP
		}
	} else if opts.Metadata != nil {
		// Explicit harness identities are absolute authority: clear any stale LCP
		// affinity and propagate the explicit parent lineage.
		delete(opts.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
		delete(opts.Metadata, cliproxyexecutor.LCPAccessGenerationMetadataKey)
		delete(opts.Metadata, cliproxyexecutor.IsCompactionMetadataKey)
		delete(opts.Metadata, cliproxyexecutor.NodeKindMetadataKey)
		if explicitFallbackID != "" {
			opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = cliproxysession.BoundSessionIdentity(explicitFallbackID)
		} else {
			delete(opts.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
		}
		if isFork, ok := opts.Metadata[cliproxyexecutor.IsForkMetadataKey].(bool); !ok || !isFork {
			delete(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
		}
	}
	if primaryID == "" {
		primaryID, fallbackID = extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	}
	if primaryID != "" {
		primaryID = cliproxysession.BoundSessionIdentity(primaryID)
		if fallbackID != "" {
			fallbackID = cliproxysession.BoundSessionIdentity(fallbackID)
		}
		if opts.Metadata != nil {
			opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = primaryID
		}
	}
	now := time.Now()
	if primaryID == "" {
		fallbackAuths, errAvailable := getAvailableAuths(auths, provider, model, now)
		if errAvailable != nil {
			return nil, errAvailable
		}
		entry.Debugf("session-affinity: no session ID extracted, falling back to default selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, fallbackAuths)
	}

	// A single availability pass serves both lookups: the bound credential is validated against
	// every priority tier, while the fallback selector keeps seeing only the highest tier.
	available, err := getAvailableAuthsAcrossPriorities(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	fallbackAuths := highestPriorityAuths(available)

	modelKey := canonicalModelKey(model)
	cacheKey := provider + "::" + primaryID + "::" + modelKey
	isFork := false
	if opts.Metadata != nil {
		if forkFlag, ok := opts.Metadata[cliproxyexecutor.IsForkMetadataKey].(bool); ok && forkFlag {
			isFork = true
		}
	}
	isSubagent := !isFork && isSubagentSession(primaryID, fallbackID)
	fallbackKey := ""
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey = provider + "::" + fallbackID + "::" + modelKey
	}
	bind := func(authID string) {
		if fallbackKey != "" && !isSubagent && !isFork {
			s.cache.Set(fallbackKey, authID)
		}
		s.cache.Set(cacheKey, authID)
	}

	if cachedAuthID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for _, auth := range available {
			if auth.ID == cachedAuthID {
				bind(auth.ID)
				entry.Infof("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
				return auth, nil
			}
		}
		// Cached auth not available, reselect via fallback selector for even distribution
		auth, err := s.fallback.Pick(ctx, provider, model, opts, fallbackAuths)
		if err != nil {
			return nil, err
		}
		bind(auth.ID)
		entry.Infof("session-affinity: cache hit but auth unavailable, reselected | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
		return auth, nil
	}

	if fallbackKey != "" {
		if cachedAuthID, ok := s.cache.Get(fallbackKey); ok {
			for _, auth := range available {
				if auth.ID == cachedAuthID {
					if !isSubagent || allowsSubagentAuthInheritance(auth, model) {
						bind(auth.ID)
						if isFork {
							entry.Infof("session-affinity: fork cache hit | session=%s parent=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
						} else {
							entry.Infof("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
						}
						return auth, nil
					}
				}
			}
		}
	}

	auth, err := s.fallback.Pick(ctx, provider, model, opts, fallbackAuths)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, nil
	}
	bind(auth.ID)
	if isFork && fallbackID != "" {
		entry.Infof("session-affinity: fork bound to new auth | session=%s parent=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
	} else {
		entry.Infof("session-affinity: cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
	}
	return auth, nil
}

func (s *SessionAffinitySelector) pickLCP(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth, entry *log.Entry) (*Auth, bool, error) {
	if s == nil || s.matcher == nil {
		return nil, false, nil
	}
	namespace := lcpAffinityNamespace(provider, model, opts.Metadata)
	if namespace == "" {
		return nil, false, nil
	}
	turns := cliproxysession.ExtractCanonicalTurns(opts.SourceFormat, opts.OriginalRequest)
	if len(turns) == 0 {
		return nil, false, nil
	}
	fingerprints, minPrefixLength, tailFingerprints, envDigest := s.matcher.PrepareExt(turns)
	if len(fingerprints) == 0 || minPrefixLength <= 0 || minPrefixLength > len(fingerprints) {
		return nil, false, nil
	}
	if opts.Metadata != nil {
		opts.Metadata[cliproxyexecutor.LCPFingerprintMetadataKey] = fingerprints
		opts.Metadata[cliproxyexecutor.LCPMinPrefixLengthMetadataKey] = minPrefixLength
		opts.Metadata[cliproxyexecutor.LCPTailFingerprintsMetadataKey] = tailFingerprints
		opts.Metadata[cliproxyexecutor.LCPEnvironmentDigestMetadataKey] = envDigest
	}

	available, errAvailable := getAvailableAuthsAcrossPriorities(auths, provider, model, time.Now())
	if errAvailable != nil {
		return nil, true, errAvailable
	}

	if match, ok := s.matcher.MatchFingerprintsWithContext(namespace, fingerprints, tailFingerprints, envDigest, minPrefixLength); ok {
		for _, auth := range available {
			if auth == nil || auth.ID != match.AuthID {
				continue
			}
			if match.SessionID != "" {
				opts.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] = match.SessionID
				opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = match.SessionID
			}
			if match.ParentSessionID != "" {
				opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = match.ParentSessionID
			} else if opts.Metadata != nil {
				delete(opts.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
			}
			if match.AccessNumber > 0 && opts.Metadata != nil {
				opts.Metadata[cliproxyexecutor.LCPAccessGenerationMetadataKey] = match.AccessNumber
			}
			if match.IsFork {
				if opts.Metadata != nil {
					opts.Metadata[cliproxyexecutor.IsForkMetadataKey] = true
					delete(opts.Metadata, cliproxyexecutor.IsCompactionMetadataKey)
					opts.Metadata[cliproxyexecutor.NodeKindMetadataKey] = "fork"
				}
				entry.Infof("session-affinity: LCP fork hit | session=%s parent=%s prefix=%d auth=%s provider=%s model=%s", truncateSessionID(match.SessionID), truncateSessionID(match.ParentSessionID), match.PrefixLength, auth.ID, provider, model)
			} else if match.IsCompaction {
				if opts.Metadata != nil {
					opts.Metadata[cliproxyexecutor.IsCompactionMetadataKey] = true
					delete(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
					opts.Metadata[cliproxyexecutor.NodeKindMetadataKey] = "compaction"
				}
				entry.Infof("session-affinity: LCP compaction hit | session=%s parent=%s prefix=%d auth=%s provider=%s model=%s", truncateSessionID(match.SessionID), truncateSessionID(match.ParentSessionID), match.PrefixLength, auth.ID, provider, model)
			} else {
				if opts.Metadata != nil {
					delete(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
					delete(opts.Metadata, cliproxyexecutor.IsCompactionMetadataKey)
					delete(opts.Metadata, cliproxyexecutor.NodeKindMetadataKey)
				}
				entry.Infof("session-affinity: LCP cache hit | session=%s prefix=%d auth=%s provider=%s model=%s", truncateSessionID(match.SessionID), match.PrefixLength, auth.ID, provider, model)
			}
			return auth, true, nil
		}
	}

	fallbackAuths := highestPriorityAuths(available)
	auth, errPick := s.fallback.Pick(ctx, provider, model, opts, fallbackAuths)
	if errPick != nil {
		return nil, true, errPick
	}
	if auth == nil {
		return nil, true, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	if bindRes := s.matcher.BindFingerprintsWithContext(namespace, fingerprints, tailFingerprints, envDigest, minPrefixLength, auth.ID); bindRes.SessionID != "" {
		opts.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] = bindRes.SessionID
		opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = bindRes.SessionID
		if bindRes.ParentSessionID != "" {
			opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = bindRes.ParentSessionID
		} else if opts.Metadata != nil {
			delete(opts.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
		}
		if bindRes.AccessNumber > 0 && opts.Metadata != nil {
			opts.Metadata[cliproxyexecutor.LCPAccessGenerationMetadataKey] = bindRes.AccessNumber
		}
		if bindRes.IsFork {
			if opts.Metadata != nil {
				opts.Metadata[cliproxyexecutor.IsForkMetadataKey] = true
				delete(opts.Metadata, cliproxyexecutor.IsCompactionMetadataKey)
				opts.Metadata[cliproxyexecutor.NodeKindMetadataKey] = "fork"
			}
			entry.Infof("session-affinity: LCP fork bound to new auth | session=%s parent=%s auth=%s provider=%s model=%s", truncateSessionID(bindRes.SessionID), truncateSessionID(bindRes.ParentSessionID), auth.ID, provider, model)
		} else if bindRes.IsCompaction {
			if opts.Metadata != nil {
				opts.Metadata[cliproxyexecutor.IsCompactionMetadataKey] = true
				delete(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
				opts.Metadata[cliproxyexecutor.NodeKindMetadataKey] = "compaction"
			}
			entry.Infof("session-affinity: LCP compaction bound to new auth | session=%s parent=%s auth=%s provider=%s model=%s", truncateSessionID(bindRes.SessionID), truncateSessionID(bindRes.ParentSessionID), auth.ID, provider, model)
		} else {
			if opts.Metadata != nil {
				delete(opts.Metadata, cliproxyexecutor.IsForkMetadataKey)
				delete(opts.Metadata, cliproxyexecutor.IsCompactionMetadataKey)
				delete(opts.Metadata, cliproxyexecutor.NodeKindMetadataKey)
			}
			entry.Infof("session-affinity: LCP cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(bindRes.SessionID), auth.ID, provider, model)
		}
	}
	return auth, true, nil
}

func canonicalLCPProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "google", "gemini", "vertex", "aistudio":
		return "google"
	case "codex", "openai":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	case "devin":
		return "devin"
	default:
		return provider
	}
}

func lcpAffinityNamespace(provider, model string, metadata map[string]any) string {
	provider = canonicalLCPProvider(provider)
	model = canonicalModelKey(model)
	callerScope := sessionMetadataString(metadata, cliproxyexecutor.CallerScopeMetadataKey)
	if provider == "" || callerScope == "" {
		return ""
	}
	return strings.Join([]string{"lcp:v1", provider, model, callerScope}, "::")
}

// CanonicalSessionID resolves the single authoritative session identity from request options and metadata.
func CanonicalSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	if explicitID, _ := extractExplicitSessionIDs(headers, payload, metadata); explicitID != "" {
		return cliproxysession.BoundSessionIdentity(explicitID)
	}
	if metadata != nil {
		if canonicalID, ok := metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok && strings.TrimSpace(canonicalID) != "" {
			return cliproxysession.BoundSessionIdentity(strings.TrimSpace(canonicalID))
		}
		if lcpID, ok := metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string); ok && strings.TrimSpace(lcpID) != "" {
			return cliproxysession.BoundSessionIdentity(strings.TrimSpace(lcpID))
		}
	}
	return cliproxysession.BoundSessionIdentity(ExtractSessionID(headers, payload, metadata))
}

func lcpFingerprintsFromMetadata(metadata map[string]any) ([]string, int) {
	if metadata == nil {
		return nil, 0
	}
	rawFingerprints, ok := metadata[cliproxyexecutor.LCPFingerprintMetadataKey]
	if !ok || rawFingerprints == nil {
		return nil, 0
	}
	var fingerprints []string
	switch v := rawFingerprints.(type) {
	case []string:
		fingerprints = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				fingerprints = append(fingerprints, s)
			}
		}
	}
	minPrefixLength, _ := metadata[cliproxyexecutor.LCPMinPrefixLengthMetadataKey].(int)
	return fingerprints, minPrefixLength
}

func sessionMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *SessionAffinitySelector) OnResult(res Result) {
	if s == nil || res.AuthID == "" {
		return
	}

	explicitID, explicitFallbackID := extractExplicitSessionIDs(res.Options.Headers, res.Options.OriginalRequest, res.Options.Metadata)
	if explicitID != "" {
		explicitID = cliproxysession.BoundSessionIdentity(explicitID)
		if explicitFallbackID != "" {
			explicitFallbackID = cliproxysession.BoundSessionIdentity(explicitFallbackID)
		}
	}
	ns := res.Provider
	if raw, ok := res.Options.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey].(string); ok && raw != "" {
		ns = raw
	}
	nsModel := canonicalModelKey(res.Model)
	if raw, ok := res.Options.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey].(string); ok && raw != "" {
		nsModel = canonicalModelKey(raw)
	}

	if res.Error != nil && shouldSkipCredentialCooldown(res.Error) {
		return
	}

	if explicitID == "" && s.matcher != nil {
		if namespace := lcpAffinityNamespace(ns, nsModel, res.Options.Metadata); namespace != "" {
			fingerprints, minPrefixLength := lcpFingerprintsFromMetadata(res.Options.Metadata)
			tailFingerprints, _ := res.Options.Metadata[cliproxyexecutor.LCPTailFingerprintsMetadataKey].([]string)
			envDigest, _ := res.Options.Metadata[cliproxyexecutor.LCPEnvironmentDigestMetadataKey].(string)
			if len(fingerprints) == 0 {
				turns := cliproxysession.ExtractCanonicalTurns(res.Options.SourceFormat, res.Options.OriginalRequest)
				fingerprints, minPrefixLength, tailFingerprints, envDigest = s.matcher.PrepareExt(turns)
			}
			if len(fingerprints) > 0 && minPrefixLength > 0 && minPrefixLength <= len(fingerprints) {
				if res.Success {
					s.matcher.TouchFingerprintsWithContext(namespace, fingerprints, tailFingerprints, envDigest, minPrefixLength, res.AuthID)
				} else {
					var generation uint64
					if res.Options.Metadata != nil {
						if gen, ok := res.Options.Metadata[cliproxyexecutor.LCPAccessGenerationMetadataKey].(uint64); ok {
							generation = gen
						}
					}
					s.matcher.RemoveFingerprintsBefore(namespace, fingerprints, res.AuthID, generation)
				}
			}
		}
	}

	if s.cache == nil {
		return
	}
	if explicitID == "" && s.matcher != nil && res.Options.Metadata != nil {
		if _, isLCP := res.Options.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey]; isLCP {
			return
		}
	}
	primaryID, fallbackID := explicitID, explicitFallbackID
	if primaryID == "" {
		primaryID, fallbackID = extractSessionIDs(res.Options.Headers, res.Options.OriginalRequest, res.Options.Metadata)
	}
	if primaryID == "" && fallbackID == "" {
		return
	}
	if primaryID != "" {
		primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	}
	if fallbackID != "" {
		fallbackID = cliproxysession.BoundSessionIdentity(fallbackID)
	}

	cacheKey := ns + "::" + primaryID + "::" + nsModel
	var fallbackKey string
	if fallbackID != "" && fallbackID != primaryID && !isSubagentSession(primaryID, fallbackID) {
		fallbackKey = ns + "::" + fallbackID + "::" + nsModel
	}
	if res.Success {
		s.cache.Touch(cacheKey, res.AuthID)
		if fallbackKey != "" {
			s.cache.Touch(fallbackKey, res.AuthID)
		}
		return
	}

	s.cache.CompareAndDelete(cacheKey, res.AuthID)
	if fallbackKey != "" {
		s.cache.CompareAndDelete(fallbackKey, res.AuthID)
	}
}

func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

// truncateSessionID shortens session ID for logging (first 8 chars + "...")
func truncateSessionID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "..."
}

// Stop releases resources held by the selector.
func (s *SessionAffinitySelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// InvalidateAuth removes all session bindings for a specific auth.
// Called when an auth becomes rate-limited or unavailable.
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// normalizedSessionCandidate validates an explicit client-provided session signal.
// It keeps opaque printable IDs intact while rejecting values that are unsafe or
// implausibly large for routing keys and logs.
func normalizedSessionCandidate(raw string) string {
	return cliproxysession.NormalizeExplicitID(raw)
}

func isSubagentSession(primaryID, fallbackID string) bool {
	if strings.Contains(primaryID, ":agent:") {
		return true
	}
	if fallbackID == "" || primaryID == "" || primaryID == fallbackID {
		return false
	}
	return isHierarchyParent(primaryID, fallbackID)
}

func isNonInheritingProvider(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "antigravity" || p == "gemini" || p == "vertex" || p == "gemini-vertex" || p == "aistudio" || p == "gemini-interactions" || strings.Contains(p, "antigravity") || strings.Contains(p, "gemini")
}

func isNonInheritingModel(model string) bool {
	baseModel := strings.ToLower(strings.TrimSpace(model))
	if parsed := thinking.ParseSuffix(baseModel); parsed.ModelName != "" {
		baseModel = strings.ToLower(strings.TrimSpace(parsed.ModelName))
	}
	baseModel = strings.TrimPrefix(baseModel, "models/")
	baseModel = strings.TrimPrefix(baseModel, "google/")
	return strings.HasPrefix(baseModel, "gemini-") || strings.HasPrefix(baseModel, "antigravity-") || strings.Contains(baseModel, "gemini") || strings.Contains(baseModel, "antigravity")
}

func allowsSubagentAuthInheritance(auth *Auth, model string) bool {
	if auth != nil && isNonInheritingProvider(auth.Provider) {
		return false
	}
	if isNonInheritingModel(model) {
		return false
	}
	return true
}

// ExtractSessionID extracts a session identifier from explicit client signals,
// then falls back to execution metadata, derived identity, and message history.
// Priority order:
//  1. X-Claude-Code-Session-Id
//  2. Claude Code metadata.user_id session
//  3. Session-Id / Session_id (Codex and compatible clients)
//  4. X-Session-ID
//  5. X-Session-Affinity (OpenCode)
//  6. X-Client-Request-Id (pi Responses)
//  7. session_id / sessionId
//  8. prompt_cache_key, with conversation / conversation.id as an alias
//  9. metadata.user_id and conversation_id legacy body fields
//  10. explicit execution session metadata
//  11. stable context-derived session identity
//  12. stable hash from initial message content
func ExtractSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	primary, _ := extractSessionIDs(headers, payload, metadata)
	return primary
}

func extractConversationAlias(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root := gjson.ParseBytes(payload)
	conversation := root.Get("conversation")
	if !conversation.Exists() {
		req := root.Get("request")
		if req.Exists() && !root.Get("contents").Exists() {
			conversation = req.Get("conversation")
		}
	}
	if sid := normalizedSessionCandidate(conversation.Get("id").String()); sid != "" {
		return "conv:" + sid
	} else if conversation.Type == gjson.String {
		if sid := normalizedSessionCandidate(conversation.String()); sid != "" {
			return "conv:" + sid
		}
	}
	return ""
}

// extractExplicitSessionIDs returns only client- or execution-provided identities.
// LCP fallback must run after this function so explicit harness sessions remain authoritative.
func extractExplicitSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	info, ok := cliproxysession.ExtractSessionInfo(headers, payload, metadata)
	if !ok || info.ClientType == "lcp" {
		return "", ""
	}
	if metadata != nil {
		if info.IsFork {
			metadata[cliproxyexecutor.IsForkMetadataKey] = true
		}
		if info.ParentSessionID != "" {
			metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = info.ParentSessionID
		}
	}
	fallback := info.ParentSessionID
	if fallback == "" && strings.HasPrefix(info.SessionID, "pck:") && len(payload) > 0 {
		fallback = extractConversationAlias(payload)
	}
	return info.SessionID, fallback
}

// extractSessionIDs returns (primaryID, fallbackID) for session affinity.
// fallbackID preserves an earlier binding when a stronger body identifier appears
// later, and lets callers bind both identifiers when both are present.
func extractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	if primaryID, fallbackID := extractExplicitSessionIDs(headers, payload, metadata); primaryID != "" {
		return primaryID, fallbackID
	}
	if derivedID := normalizedSessionCandidate(cliproxysession.DerivedID(metadata)); derivedID != "" {
		return "derived:" + derivedID, ""
	}
	if len(payload) == 0 {
		return "", ""
	}
	return extractMessageHashIDs(payload)
}

func extractMessageHashIDs(payload []byte) (primaryID, fallbackID string) {
	var systemPrompt, firstUserMsg, firstAssistantMsg string

	// OpenAI/Claude messages format
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := extractMessageContent(msg.Get("content"))
			if content == "" {
				return true
			}

			switch role {
			case "system":
				if systemPrompt == "" {
					systemPrompt = truncateString(content, 100)
				}
			case "user":
				if firstUserMsg == "" {
					firstUserMsg = truncateString(content, 100)
				}
			case "assistant":
				if firstAssistantMsg == "" {
					firstAssistantMsg = truncateString(content, 100)
				}
			}

			if systemPrompt != "" && firstUserMsg != "" && firstAssistantMsg != "" {
				return false
			}
			return true
		})
	}

	// Claude API: top-level "system" field (array or string)
	if systemPrompt == "" {
		topSystem := gjson.GetBytes(payload, "system")
		if topSystem.Exists() {
			if topSystem.IsArray() {
				topSystem.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" && systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
						return false
					}
					return true
				})
			} else if topSystem.Type == gjson.String {
				systemPrompt = truncateString(topSystem.String(), 100)
			}
		}
	}

	// Gemini format
	if systemPrompt == "" && firstUserMsg == "" {
		sysInstr := gjson.GetBytes(payload, "systemInstruction.parts")
		if sysInstr.Exists() && sysInstr.IsArray() {
			sysInstr.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" && systemPrompt == "" {
					systemPrompt = truncateString(text, 100)
					return false
				}
				return true
			})
		}

		contents := gjson.GetBytes(payload, "contents")
		if contents.Exists() && contents.IsArray() {
			contents.ForEach(func(_, msg gjson.Result) bool {
				role := msg.Get("role").String()
				msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
					text := part.Get("text").String()
					if text == "" {
						return true
					}
					switch role {
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateString(text, 100)
						}
					case "model":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateString(text, 100)
						}
					}
					return false
				})
				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	// OpenAI Responses API format (v1/responses)
	if systemPrompt == "" && firstUserMsg == "" {
		if instr := gjson.GetBytes(payload, "instructions").String(); instr != "" {
			systemPrompt = truncateString(instr, 100)
		}

		input := gjson.GetBytes(payload, "input")
		if input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				itemType := item.Get("type").String()
				if itemType == "reasoning" {
					return true
				}
				// Skip non-message typed items (function_call, function_call_output, etc.)
				// but allow items with no type that have a role (inline message format).
				if itemType != "" && itemType != "message" {
					return true
				}

				role := item.Get("role").String()
				if itemType == "" && role == "" {
					return true
				}

				// Handle both string content and array content (multimodal).
				content := item.Get("content")
				var text string
				if content.Type == gjson.String {
					text = content.String()
				} else {
					text = extractResponsesAPIContent(content)
				}
				if text == "" {
					return true
				}

				switch role {
				case "developer", "system":
					if systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateString(text, 100)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateString(text, 100)
					}
				}

				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}

	shortHash := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return shortHash, ""
	}

	fullHash := computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg)
	return fullHash, shortHash
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// extractMessageContent extracts text content from a message content field.
// Handles both string content and array content (multimodal messages).
// For array content, extracts text from all text-type elements.
func extractMessageContent(content gjson.Result) string {
	// String content: "Hello world"
	if content.Type == gjson.String {
		return content.String()
	}

	// Array content: [{"type":"text","text":"Hello"},{"type":"image",...}]
	if content.IsArray() {
		var texts []string
		content.ForEach(func(_, part gjson.Result) bool {
			// Handle Claude format: {"type":"text","text":"content"}
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			// Handle OpenAI format: {"type":"text","text":"content"}
			// Same structure as Claude, already handled above
			return true
		})
		if len(texts) > 0 {
			return strings.Join(texts, " ")
		}
	}

	return ""
}

func extractResponsesAPIContent(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "input_text" || partType == "output_text" || partType == "text" {
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	if len(texts) > 0 {
		return strings.Join(texts, " ")
	}
	return ""
}

// extractSessionID is kept for backward compatibility.
// Deprecated: Use ExtractSessionID instead.
func extractSessionID(payload []byte) string {
	return ExtractSessionID(nil, payload, nil)
}
