package service

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// appendRequestTranslationErrorEvent records a fatal request-translation
// failure (JSON parse or protocol bridge) as an ops upstream event so the
// failure is visible per request with its cause. Translation errors return
// before any account is selected, so account may be nil. Tool-call failures
// (conflicts, lowering errors, malformed tool payloads) get Kind
// "tool_error"; other shape errors get Kind "request_error".
func appendRequestTranslationErrorEvent(c *gin.Context, account *Account, requestedModel string, err error) {
	if c == nil || err == nil {
		return
	}
	ev := OpsUpstreamErrorEvent{
		Kind:    "request_error",
		Stage:   "request_translation",
		Message: err.Error(),
	}
	if isToolRelatedTranslationError(err) {
		ev.Kind = "tool_error"
	}
	if account != nil {
		ev.Platform = account.Platform
		ev.AccountID = account.ID
		ev.AccountName = account.Name
	}
	if model := strings.TrimSpace(requestedModel); model != "" {
		ev.Detail = "model=" + model
	}
	appendOpsUpstreamError(c, ev)
}

func isToolRelatedTranslationError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"tool", "function_call", "function call", "mcp_"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
