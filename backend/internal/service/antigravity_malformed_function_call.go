package service

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	antigravityMalformedRetriesKey = "antigravity_malformed_function_call_retries"
	// MALFORMED is usually deterministic for a given prompt+schema, so a single
	// cross-account sample is enough to rule out account/session skew. Budget 1
	// bounds added latency and avoids burning the pool on deterministic failures.
	antigravityMalformedRetryLimit = 1
)

// antigravityFinishReasonOfLine extracts candidates.0.finishReason from a raw
// Gemini SSE line (v1internal wrapper or direct shape). Returns "" when absent.
func antigravityFinishReasonOfLine(line string) string {
	payload := strings.TrimSpace(line)
	if !strings.Contains(payload, "finishReason") {
		return ""
	}
	payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
	if inner := gjson.Get(payload, "response"); inner.Exists() && inner.IsObject() {
		payload = inner.Raw
	}
	return gjson.Get(payload, "candidates.0.finishReason").String()
}

func isAntigravityMalformedFunctionCallLine(line string) bool {
	payload := strings.TrimSpace(line)
	if !strings.Contains(payload, "MALFORMED_FUNCTION_CALL") {
		return false
	}
	payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
	if inner := gjson.Get(payload, "response"); inner.Exists() && inner.IsObject() {
		payload = inner.Raw
	}
	return gjson.Get(payload, "candidates.0.finishReason").String() == "MALFORMED_FUNCTION_CALL"
}

// antigravityMalformedFunctionCallRetry turns a MALFORMED_FUNCTION_CALL that
// arrives before any client byte into an internal gateway failover across accounts.
func antigravityMalformedFunctionCallRetry(c *gin.Context, model string) error {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return nil
	}
	attempts := c.GetInt(antigravityMalformedRetriesKey)
	if attempts < antigravityMalformedRetryLimit {
		c.Set(antigravityMalformedRetriesKey, attempts+1)
		logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] MALFORMED_FUNCTION_CALL before output, retrying on fresh account: model=%s attempt=%d/%d", model, attempts+1, antigravityMalformedRetryLimit)
		return &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			ResponseBody:           []byte(`{"error":{"type":"upstream_error","message":"Gemini returned MALFORMED_FUNCTION_CALL"}}`),
			RetryableOnSameAccount: false,
			RequestScopedTransient: true,
			SameAccountRetryMax:    0,
		}
	}
	// Never return nil to deliver empty or corrupt payload to the client.
	// MALFORMED is deterministic for a given prompt+schema (the upstream fails
	// the same serialization on every account), so stop instead of burning the
	// whole pool through the failover loop.
	logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] MALFORMED_FUNCTION_CALL exhausted retries across accounts: model=%s", model)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":{"type":"api_error","message":"Gemini failed to format function call arguments after retries"}}`),
		RetryableOnSameAccount: false,
		RequestScopedTransient: false,
		NextAccountAction:      NextAccountStop,
	}
}

// UpstreamSafetyRefusalError builds the terminal error for content-safety
// refusals. It must NEVER trigger same-account retry or next-account
// failover: rotating accounts to dodge a content filter is a compliance
// violation. Propagates as-is with stop reason refusal.
func UpstreamSafetyRefusalError(model, finishReason string) *UpstreamFailoverError {
	logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] safety refusal propagated without retry: model=%s finish_reason=%s", model, finishReason)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusUnprocessableEntity,
		ResponseBody:           []byte(`{"error":{"type":"refusal","message":"Upstream refused the request on content policy"}}`),
		RetryableOnSameAccount: false,
		RequestScopedTransient: false,
		NextAccountAction:      NextAccountStop,
	}
}

// UpstreamUnknownFinishError builds the terminal error for unrecognized empty
// finish reasons. Fail-fast 502 without retry or account switch, with a
// distinct log line for SRE triage.
func UpstreamUnknownFinishError(model, finishReason string) *UpstreamFailoverError {
	logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] unknown empty finish reason, failing fast: model=%s finish_reason=%s", model, finishReason)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":{"type":"api_error","message":"Upstream returned an unrecognized empty finish reason"}}`),
		RetryableOnSameAccount: false,
		RequestScopedTransient: false,
		NextAccountAction:      NextAccountStop,
	}
}
