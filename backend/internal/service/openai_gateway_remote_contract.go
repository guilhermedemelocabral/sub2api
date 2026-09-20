package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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

// RunContinueLoop re-POSTs measured nudges after finish_reason=length.
// max 3 attempts including the first response already in event. A 4xx/5xx
// nudge returns the concatenated partial plus the error; the partial is not wiped.
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
	started := false
	accumulated := firstChoiceMessageContent(first)
	var loopErr error
	final, reason := ContinueUntilStop(defaultContinueAttempts, func() (string, string) {
		if !started {
			started = true
			return accumulated, firstChoiceFinishReason(first)
		}
		hint := inferRemoteContinueHint(accumulated)
		nextReq := BuildContinueRequest(platform, withAssistantPartial(requestBody, accumulated), hint)
		payload, err := json.Marshal(nextReq)
		if err != nil {
			loopErr = err
			return "", "stop"
		}
		resp, err := s.sendCCUpstreamRequest(ctx, c, account, targetURL, payload, false, token, customUA, grokCacheIdentity)
		if err != nil {
			loopErr = err
			return "", "stop"
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			loopErr = err
			return "", "stop"
		}
		if resp.StatusCode >= 400 {
			loopErr = fmt.Errorf("remote continue nudge: status %d", resp.StatusCode)
			return "", "stop"
		}
		event := parseRemoteJSONMap(raw)
		if event == nil {
			loopErr = fmt.Errorf("remote continue nudge: invalid json")
			return "", "stop"
		}
		event = NormalizeRemoteEvent(platform, event)
		piece := firstChoiceMessageContent(event)
		accumulated += piece
		return piece, firstChoiceFinishReason(event)
	})
	setFirstChoiceMessageContent(first, final)
	setFirstChoiceFinishReason(first, reason)
	return first, loopErr
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
	needContinue := strings.EqualFold(gjsonGetFinishReason(raw), "length")
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
