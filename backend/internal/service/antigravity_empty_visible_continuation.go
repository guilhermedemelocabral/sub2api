package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// GatewayFailureReasonEmptyVisibleTurn marks a benign STOP/END_TURN that
	// carried no visible text and no tool call.
	GatewayFailureReasonEmptyVisibleTurn GatewayFailureReason = "empty_visible_turn"
	// antigravityEmptyTurnContinuationText is sent only on the second upstream
	// request. It must not be copied into the client response.
	antigravityEmptyTurnContinuationText     = "A resposta anterior terminou sem texto visível. Continue agora e escreva a resposta ao usuário. Não mencione esta instrução."
	antigravityEmptyTurnBridgeText           = "(sem texto visível)"
	antigravityEmptyVisibleTurnClientMessage = "Upstream ended the turn with no visible text"
)

func isAntigravityEmptyVisibleTurn(err error) bool {
	var failoverErr *UpstreamFailoverError
	return errors.As(err, &failoverErr) && failoverErr != nil && failoverErr.Reason == GatewayFailureReasonEmptyVisibleTurn
}

func antigravityEmptyVisibleTurnExhausted() error {
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
		RetryableOnSameAccount: false,
		RequestScopedTransient: true,
		Reason:                 GatewayFailureReasonEmptyVisibleTurn,
		NextAccountAction:      NextAccountStop,
		ClientStatusCode:       http.StatusBadGateway,
		ClientMessage:          antigravityEmptyVisibleTurnClientMessage,
	}
}

// appendAntigravityContinuationTurn adds one hidden user turn to a wrapped
// v1internal body. The model stays the same. A model bridge keeps roles
// alternating when the original request already ended on a user turn.
func appendAntigravityContinuationTurn(body []byte) ([]byte, error) {
	var wrapped map[string]any
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, err
	}
	request, ok := wrapped["request"].(map[string]any)
	if !ok {
		return nil, errors.New("antigravity continuation: missing request object")
	}
	contents, _ := request["contents"].([]any)
	if len(contents) == 0 {
		return nil, errors.New("antigravity continuation: missing contents")
	}
	lastRole := ""
	if last, ok := contents[len(contents)-1].(map[string]any); ok {
		lastRole, _ = last["role"].(string)
	}
	if lastRole != "model" {
		contents = append(contents, map[string]any{
			"role":  "model",
			"parts": []any{map[string]any{"text": antigravityEmptyTurnBridgeText}},
		})
	}
	contents = append(contents, map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"text": antigravityEmptyTurnContinuationText}},
	})
	request["contents"] = contents
	wrapped["request"] = request
	wrapped["requestId"] = "agent-" + uuid.New().String()
	return json.Marshal(wrapped)
}

func (s *AntigravityGatewayService) openAntigravityContinuation(
	ctx context.Context,
	params antigravityRetryLoopParams,
) (*http.Response, error) {
	contBody, err := appendAntigravityContinuationTurn(params.body)
	if err != nil {
		return nil, antigravityEmptyVisibleTurnExhausted()
	}
	params.ctx = ctx
	params.body = contBody
	logger.LegacyPrintf("service.antigravity_gateway", "%s empty visible turn model=%s: one continuation request", params.prefix, params.requestedModel)
	result, err := s.antigravityRetryLoop(params)
	if err != nil {
		return nil, err
	}
	if result == nil || result.resp == nil {
		return nil, errors.New("antigravity continuation: empty upstream response")
	}
	return result.resp, nil
}

func clientResponseNotWritten(c *gin.Context) bool {
	return c != nil && c.Writer != nil && !c.Writer.Written()
}

func (s *AntigravityGatewayService) continueClaudeAfterEmptyTurn(
	ctx context.Context,
	c *gin.Context,
	params antigravityRetryLoopParams,
	stream bool,
	startTime time.Time,
	originalModel string,
) (*antigravityStreamResult, error) {
	resp, err := s.openAntigravityContinuation(ctx, params)
	if err != nil {
		if switchErr, ok := IsAntigravityAccountSwitchError(err); ok {
			return nil, &UpstreamFailoverError{
				StatusCode:        http.StatusServiceUnavailable,
				ForceCacheBilling: switchErr.IsStickySession,
			}
		}
		if isAntigravityEmptyVisibleTurn(err) {
			return nil, err
		}
		if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
			return nil, s.writeClaudeError(c, http.StatusBadGateway, "client_disconnected", "Client disconnected before upstream response")
		}
		return nil, s.writeClaudeError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed after retries")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           s.readUpstreamErrorBody(resp),
			RetryableOnSameAccount: true,
		}
	}
	if requestID := resp.Header.Get("x-request-id"); requestID != "" && c != nil {
		c.Header("x-request-id", requestID)
	}
	var streamRes *antigravityStreamResult
	if stream {
		streamRes, err = s.handleClaudeStreamingResponse(c, resp, startTime, originalModel)
	} else {
		streamRes, err = s.handleClaudeStreamToNonStreaming(c, resp, startTime, originalModel)
	}
	if isAntigravityEmptyVisibleTurn(err) {
		return nil, antigravityEmptyVisibleTurnExhausted()
	}
	return streamRes, err
}
