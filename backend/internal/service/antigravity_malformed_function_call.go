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
	// Gemini usually emits a valid call on a fresh sample. Past this budget the
	// upstream answer is delivered as-is, so a schema the model cannot satisfy
	// does not loop across every account.
	antigravityMalformedRetryLimit = 2
)

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
// arrives before any client byte into a request-scoped retry. It returns nil
// once bytes were written or the per-request budget is spent.
func antigravityMalformedFunctionCallRetry(c *gin.Context, model string) error {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return nil
	}
	attempts := c.GetInt(antigravityMalformedRetriesKey)
	if attempts >= antigravityMalformedRetryLimit {
		return nil
	}
	c.Set(antigravityMalformedRetriesKey, attempts+1)
	logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] MALFORMED_FUNCTION_CALL before output, retrying: model=%s attempt=%d/%d", model, attempts+1, antigravityMalformedRetryLimit)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":{"type":"upstream_error","message":"Gemini returned MALFORMED_FUNCTION_CALL"}}`),
		RetryableOnSameAccount: true,
		RequestScopedTransient: true,
		SameAccountRetryMax:    1,
	}
}
