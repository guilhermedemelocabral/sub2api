package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// shouldApplyRemoteOpenAICompatContract gates the remote vendor contract to
// OpenAI-compat hops. Gemini/Antigravity (and Grok, which has its own path
// contract) are never rewritten here.
func shouldApplyRemoteOpenAICompatContract(account *Account) bool {
	if account == nil {
		return false
	}
	if DefaultGeminiPlatform(account.Platform) {
		return false
	}
	if account.Platform == PlatformGrok {
		return false
	}
	return true
}

func remoteContractPlatform(account *Account) string {
	if account == nil {
		return ""
	}
	// Explicit config wins: Credentials["vendor_family"] ("stepfun",
	// "betallm", ...). Legacy name-substring matching below stays as
	// fallback for accounts configured before the field existed.
	if family := strings.ToLower(strings.TrimSpace(account.GetCredential("vendor_family"))); family != "" {
		return family
	}
	name := strings.ToLower(strings.TrimSpace(account.Name))
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(name)
	switch {
	case compact == "betallm" || strings.Contains(compact, "betallm") || strings.Contains(name, "beta-llm"):
		return "betallm"
	case compact == "stepfun" || strings.Contains(compact, "stepfun") || strings.Contains(name, "step-plan") || name == "step":
		return "stepfun"
	default:
		return account.Platform
	}
}

func applyRemoteOpenAICompatRequest(account *Account, model string, body []byte) []byte {
	if !shouldApplyRemoteOpenAICompatContract(account) || len(body) == 0 {
		return body
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	platform := remoteContractPlatform(account)
	family := remoteVendorFamily(platform)
	_, hasCtx := parsed["context_length"]
	_, hasMax := parsed["max_tokens"]
	if !hasMax {
		_, hasMax = parsed["max_completion_tokens"]
	}
	if family == "" && !(hasCtx && hasMax) {
		return body
	}
	prepared := PrepareRemoteRequest(platform, model, parsed)
	out, err := json.Marshal(prepared)
	if err != nil {
		return body
	}
	return out
}

func applyRemoteNormalizeSSELine(account *Account, line string) string {
	if !shouldApplyRemoteOpenAICompatContract(account) {
		return line
	}
	if remoteVendorFamily(remoteContractPlatform(account)) != "step" {
		return line
	}
	payload, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line
	}
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" {
		return line
	}
	event := parseRemoteJSONMap([]byte(payload))
	if event == nil {
		return line
	}
	normalized := NormalizeRemoteEvent(remoteContractPlatform(account), event)
	out, err := json.Marshal(normalized)
	if err != nil {
		return line
	}
	prefixLen := len(line) - len(payload)
	if prefixLen < 0 {
		return line
	}
	return line[:prefixLen] + string(out)
}

func parseRemoteJSONMap(raw []byte) map[string]any {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil
	}
	return event
}

func firstRemoteChoice(event map[string]any) map[string]any {
	if event == nil {
		return nil
	}
	choices, _ := event["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	return choice
}

func firstChoiceFinishReason(event map[string]any) string {
	choice := firstRemoteChoice(event)
	if choice == nil {
		return ""
	}
	reason, _ := choice["finish_reason"].(string)
	return reason
}

func firstChoiceMessageContent(event map[string]any) string {
	choice := firstRemoteChoice(event)
	if choice == nil {
		return ""
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return ""
	}
	content, _ := message["content"].(string)
	return content
}

func firstChoiceHasToolCalls(event map[string]any) bool {
	choice := firstRemoteChoice(event)
	if choice == nil {
		return false
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return false
	}
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if fn, ok := message["function_call"].(map[string]any); ok && len(fn) > 0 {
		return true
	}
	return false
}

func setFirstChoiceMessageContent(event map[string]any, content string) {
	choice := firstRemoteChoice(event)
	if choice == nil {
		return
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{"role": "assistant"}
		choice["message"] = message
	}
	message["content"] = content
}

func setFirstChoiceFinishReason(event map[string]any, reason string) {
	choice := firstRemoteChoice(event)
	if choice == nil {
		return
	}
	choice["finish_reason"] = reason
}

func withAssistantPartial(body map[string]any, content string) map[string]any {
	next := make(map[string]any, len(body)+1)
	for k, v := range body {
		next[k] = v
	}
	var messages []any
	if raw, ok := body["messages"].([]any); ok {
		messages = append([]any(nil), raw...)
	}
	messages = append(messages, map[string]any{"role": "assistant", "content": content})
	next["messages"] = messages
	return next
}

func inferRemoteContinueHint(content string) ContinueHint {
	if strings.Count(content, "```")%2 == 1 {
		return ContinueHint{CodeFence: true}
	}
	if strings.TrimSpace(content) == "" {
		return ContinueHint{Reasoning: true}
	}
	return ContinueHint{}
}

// RunContinueLoop re-POSTs measured nudges while shouldContinueCompletion
// proceeds (finish_reason=length, or a blank stop). Max 3 shots including
// the first response already in event. Tool-call turns are never continued.
// A 4xx/5xx nudge returns the concatenated partial plus the error; the
// partial is not wiped.
func (s *OpenAIGatewayService) RunContinueLoop(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	platform string,
	first map[string]any,
	requestBody map[string]any,
	targetURL string,
	token string,
	customUA string,
	grokCacheIdentity string,
) (map[string]any, error) {
	if first == nil {
		return first, nil
	}
	var loopErr error
	accumulated := firstChoiceMessageContent(first)
	finish := firstChoiceFinishReason(first)
	hasTools := firstChoiceHasToolCalls(first)
	final, reason, _ := ContinueUntilStopEx(defaultContinueAttempts, func(n int) ContinueAttempt {
		if n == 1 {
			return ContinueAttempt{Content: accumulated, Finish: finish, HasToolCalls: hasTools}
		}
		logger.L().Debug("harness-continuation",
			zap.Int64("account_id", accountIDOrZero(account)),
			zap.String("platform", platform),
			zap.Int("attempt", n),
			zap.Int("max_attempts", defaultContinueAttempts),
			zap.String("finish_reason", finish),
		)
		hint := inferRemoteContinueHint(accumulated)
		nextReq := BuildContinueRequest(platform, withAssistantPartial(requestBody, accumulated), hint)
		payload, err := json.Marshal(nextReq)
		if err != nil {
			loopErr = err
			return ContinueAttempt{Finish: "stop"}
		}
		resp, err := s.sendCCUpstreamRequest(ctx, c, account, targetURL, payload, false, token, customUA, grokCacheIdentity)
		if err != nil {
			loopErr = err
			return ContinueAttempt{Finish: "stop"}
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			loopErr = err
			return ContinueAttempt{Finish: "stop"}
		}
		if resp.StatusCode >= 400 {
			loopErr = fmt.Errorf("remote continue nudge: status %d", resp.StatusCode)
			return ContinueAttempt{Finish: "stop"}
		}
		event := parseRemoteJSONMap(raw)
		if event == nil {
			loopErr = fmt.Errorf("remote continue nudge: invalid json")
			return ContinueAttempt{Finish: "stop"}
		}
		event = NormalizeRemoteEvent(platform, event)
		piece := firstChoiceMessageContent(event)
		accumulated += piece
		finish = firstChoiceFinishReason(event)
		hasTools = hasTools || firstChoiceHasToolCalls(event)
		return ContinueAttempt{Content: piece, Finish: finish, HasToolCalls: hasTools}
	})
	// final is the full concatenation across shots; rewrite the event once.
	setFirstChoiceMessageContent(first, final)
	setFirstChoiceFinishReason(first, reason)
	return first, loopErr
}

func accountIDOrZero(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

// ccStreamContinueConfig carries what a streaming CC forward needs to re-POST
// a continuation nudge mid-stream. A nil config disables streaming
// continuation and preserves byte-identical passthrough.
type ccStreamContinueConfig struct {
	ctx               context.Context
	c                 *gin.Context
	account           *Account
	platform          string
	requestBody       map[string]any
	targetURL         string
	token             string
	customUA          string
	grokCacheIdentity string
}

func newCCStreamContinueConfig(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamBody []byte,
	targetURL string,
	token string,
	customUA string,
	grokCacheIdentity string,
) *ccStreamContinueConfig {
	if !shouldApplyRemoteOpenAICompatContract(account) {
		return nil
	}
	req := parseRemoteJSONMap(upstreamBody)
	if req == nil {
		return nil
	}
	return &ccStreamContinueConfig{
		ctx:               ctx,
		c:                 c,
		account:           account,
		platform:          remoteContractPlatform(account),
		requestBody:       req,
		targetURL:         targetURL,
		token:             token,
		customUA:          customUA,
		grokCacheIdentity: grokCacheIdentity,
	}
}

// postCCContinueStream re-POSTs the nudge request with streaming enabled so
// the next shot's deltas continue the same client stream transparently.
func (s *OpenAIGatewayService) postCCContinueStream(cfg *ccStreamContinueConfig, accumulated string) (*http.Response, error) {
	hint := inferRemoteContinueHint(accumulated)
	nextReq := BuildContinueRequest(cfg.platform, withAssistantPartial(cfg.requestBody, accumulated), hint)
	payload, err := json.Marshal(nextReq)
	if err != nil {
		return nil, err
	}
	return s.sendCCUpstreamRequest(cfg.ctx, cfg.c, cfg.account, cfg.targetURL, payload, true, cfg.token, cfg.customUA, cfg.grokCacheIdentity)
}

// streamChunkContinueSignal extracts the per-chunk continuation signal from a
// CC stream data payload: the chunk finish_reason ("" when absent/null),
// whether the chunk carries tool calls, and the user-visible content delta.
func streamChunkContinueSignal(payload string) (finish string, hasToolCalls bool, contentDelta string) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" {
		return "", false, ""
	}
	choice := gjson.Get(trimmed, "choices.0")
	if !choice.Exists() {
		return "", false, ""
	}
	finish = choice.Get("finish_reason").String()
	delta := choice.Get("delta")
	if !delta.Exists() {
		return finish, false, ""
	}
	contentDelta = delta.Get("content").String()
	if toolCalls := delta.Get("tool_calls"); toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
		hasToolCalls = true
	}
	if delta.Get("function_call").IsObject() {
		hasToolCalls = true
	}
	return finish, hasToolCalls, contentDelta
}

func (s *OpenAIGatewayService) applyRemoteNonStreamContract(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	requestBody []byte,
	targetURL string,
	token string,
	customUA string,
	grokCacheIdentity string,
) (*http.Response, error) {
	if !shouldApplyRemoteOpenAICompatContract(account) || resp == nil {
		return resp, nil
	}
	raw, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	platform := remoteContractPlatform(account)
	needNormalize := remoteVendorFamily(platform) == "step"
	needContinue := remoteNonStreamNeedsContinue(raw)
	if !needNormalize && !needContinue {
		next := *resp
		next.Body = io.NopCloser(bytes.NewReader(raw))
		next.ContentLength = int64(len(raw))
		return &next, nil
	}
	event := parseRemoteJSONMap(raw)
	if event == nil {
		next := *resp
		next.Body = io.NopCloser(bytes.NewReader(raw))
		next.ContentLength = int64(len(raw))
		return &next, nil
	}
	if needNormalize {
		event = NormalizeRemoteEvent(platform, event)
	}
	var continueErr error
	if needContinue {
		var req map[string]any
		if unmarshalErr := json.Unmarshal(requestBody, &req); unmarshalErr == nil {
			event, continueErr = s.RunContinueLoop(ctx, c, account, platform, event, req, targetURL, token, customUA, grokCacheIdentity)
		}
	}
	out := raw
	if encoded, marshalErr := json.Marshal(event); marshalErr == nil {
		out = encoded
	}
	next := *resp
	next.Body = io.NopCloser(bytes.NewReader(out))
	next.ContentLength = int64(len(out))
	return &next, continueErr
}

func gjsonGetFinishReason(raw []byte) string {
	event := parseRemoteJSONMap(raw)
	return firstChoiceFinishReason(event)
}

// remoteNonStreamNeedsContinue reports whether a buffered non-stream CC
// response merits entering RunContinueLoop: finish_reason=length, or a
// blank stop without tool calls. The loop itself re-evaluates the full
// shouldContinueCompletion decision per shot.
func remoteNonStreamNeedsContinue(raw []byte) bool {
	event := parseRemoteJSONMap(raw)
	if event == nil {
		return false
	}
	proceed, _ := shouldContinueCompletion(
		firstChoiceFinishReason(event),
		1,
		defaultContinueAttempts,
		firstChoiceHasToolCalls(event),
		strings.TrimSpace(firstChoiceMessageContent(event)) == "",
	)
	return proceed
}
