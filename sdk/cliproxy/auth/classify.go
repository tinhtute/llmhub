package auth

import (
	"net/http"
	"strings"
	"time"
)

// Disposition is the routing decision produced by Classify for an upstream failure.
type Disposition int

const (
	// DispositionNone means no rule matched; the caller keeps its current behavior.
	DispositionNone Disposition = iota
	// DispositionRetryBackoff means the failure is transient (quota/rate/capacity);
	// retry after an exponential backoff wait.
	DispositionRetryBackoff
	// DispositionCooldown means retry after a fixed cooldown, typically switching credential.
	DispositionCooldown
	// DispositionReturn means the failure is a client error; surface it as-is, never retry.
	DispositionReturn
)

// classifyRule maps a text or status match to a routing disposition. Text rules are
// matched before status rules; within each set, the first match wins.
type classifyRule struct {
	text     string // lowercase substring; empty for a status-only rule
	status   int    // matched only when text is empty
	cooldown time.Duration
	disp     Disposition
}

// KeywordNoCapacityAvailable and KeywordResourceHasBeenExhausted are exported so
// antigravity_executor.go's narrow, single-phrase retry gates (antigravityShouldRetryNoCapacity,
// antigravityShouldRetryTransientResourceExhausted429) can share the same literal instead of
// re-declaring it, without adopting Classify's coarser Disposition-based matching.
const (
	KeywordNoCapacityAvailable      = "no capacity available"
	KeywordResourceHasBeenExhausted = "resource has been exhausted"
)

// classifyTextRules is checked first, in order, against the lowercased error body.
// Ported from decolua/9router open-sse/config/errorConfig.js:59, reconciled with the
// keyword tables previously duplicated across antigravity_executor.go and codex_executor.go.
var classifyTextRules = []classifyRule{
	{text: "your credit balance is too low", disp: DispositionCooldown, cooldown: 30 * time.Minute},
	{text: "no credentials", disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{text: "request not allowed", disp: DispositionCooldown, cooldown: 5 * time.Second},
	{text: "improperly formed request", disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{text: "rate limit", disp: DispositionRetryBackoff},
	{text: "too many requests", disp: DispositionRetryBackoff},
	{text: "quota exceeded", disp: DispositionRetryBackoff},
	{text: KeywordResourceHasBeenExhausted, disp: DispositionRetryBackoff},
	{text: KeywordNoCapacityAvailable, disp: DispositionRetryBackoff},
	{text: "at capacity", disp: DispositionRetryBackoff},
	{text: "overloaded", disp: DispositionRetryBackoff},
}

// classifyStatusRules is checked only when no text rule matched.
var classifyStatusRules = []classifyRule{
	{status: http.StatusUnauthorized, disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{status: http.StatusPaymentRequired, disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{status: http.StatusForbidden, disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{status: http.StatusNotFound, disp: DispositionCooldown, cooldown: 2 * time.Minute},
	{status: http.StatusTooManyRequests, disp: DispositionRetryBackoff},
}

// Classify maps an upstream failure to a routing disposition, matching text rules
// against the (case-insensitive) body before falling back to status rules. The
// returned duration is the fixed cooldown for DispositionCooldown matches; it is
// zero for DispositionRetryBackoff, whose wait is computed by the caller from the
// existing per-attempt backoff formula. ok is false when nothing matched, in which
// case the caller keeps its current behavior (DispositionNone).
func Classify(status int, body string) (disp Disposition, cooldown time.Duration, ok bool) {
	lower := strings.ToLower(body)
	for _, rule := range classifyTextRules {
		if strings.Contains(lower, rule.text) {
			return rule.disp, rule.cooldown, true
		}
	}
	for _, rule := range classifyStatusRules {
		if rule.status == status {
			return rule.disp, rule.cooldown, true
		}
	}
	return DispositionNone, 0, false
}
