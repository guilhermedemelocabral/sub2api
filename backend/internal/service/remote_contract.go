package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NormalizeRemoteEvent sanitizes one Chat completion or chunk for a remote
// platform. Gemini and other unknown platforms are returned unchanged.
func NormalizeRemoteEvent(platform string, event map[string]any) map[string]any {
	switch platform {
	case "stepfun", "step-plan", "step":
		return normalizeStepfunEvent(event)
	default:
		return event
	}
}

func jsonArgumentString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case map[string]any, []any:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	default:
		return "", false
	}
}

func dropEmpty(container map[string]any, keys ...string) {
	for _, key := range keys {
		value, ok := container[key]
		if !ok {
			continue
		}
		if value == nil || value == "" {
			delete(container, key)
		}
	}
}

func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyJSON(val)
		}
		return out
	default:
		return v
	}
}

func sanitizeToolCallDelta(call map[string]any) map[string]any {
	result := deepCopyJSON(call).(map[string]any)
	dropEmpty(result, "id", "type")
	function, ok := result["function"].(map[string]any)
	if !ok {
		return result
	}
	function = deepCopyJSON(function).(map[string]any)
	dropEmpty(function, "name")
	if arguments, ok := jsonArgumentString(function["arguments"]); ok {
		function["arguments"] = arguments
	} else {
		delete(function, "arguments")
	}
	if len(function) > 0 {
		result["function"] = function
	} else {
		delete(result, "function")
	}
	return result
}

func canonicalizeCompletedToolCalls(calls any) []any {
	list, ok := calls.([]any)
	if !ok {
		return []any{}
	}
	cleaned := make([]any, 0, len(list))
	for _, raw := range list {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item := sanitizeToolCallDelta(call)
		delete(item, "index")
		function, _ := item["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name != "" {
			if typ, exists := item["type"]; !exists || typ == nil || typ == "" {
				item["type"] = "function"
			}
		}
		cleaned = append(cleaned, item)
	}
	return cleaned
}

func normalizeStepfunMessage(message map[string]any) {
	if tools, ok := message["tool_calls"].([]any); ok && len(tools) > 0 {
		message["tool_calls"] = canonicalizeCompletedToolCalls(tools)
	}
	dropEmpty(message, "function_call")
}

func normalizeStepfunChoice(choice map[string]any) {
	if finish, ok := choice["finish_reason"]; ok && finish == "" {
		choice["finish_reason"] = nil
	}
	if message, ok := choice["message"].(map[string]any); ok {
		normalizeStepfunMessage(message)
	}
	if delta, ok := choice["delta"].(map[string]any); ok {
		if tools, ok := delta["tool_calls"].([]any); ok {
			cleaned := make([]any, 0, len(tools))
			for _, raw := range tools {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				cleaned = append(cleaned, sanitizeToolCallDelta(call))
			}
			delta["tool_calls"] = cleaned
		}
	}
}

func normalizeStepfunEvent(event map[string]any) map[string]any {
	if agent, ok := event["agent"]; !ok || agent == nil || agent == "" {
		delete(event, "agent")
	}
	if choices, ok := event["choices"].([]any); ok {
		for _, raw := range choices {
			if choice, ok := raw.(map[string]any); ok {
				normalizeStepfunChoice(choice)
			}
		}
	}
	return event
}

// ContinueHint selects which measured nudge to append after a truncated completion.
type ContinueHint struct {
	Reasoning bool
	CodeFence bool
}

const (
	continueNudgeGeneric    = "Continue from the exact point the previous assistant message stopped. Do not restart or restate. Produce the next tokens of the same answer."
	continueNudgeReasoning  = "Your previous turn was cut off inside your private reasoning. The assistant message above is your own unfinished scratchpad, not an answer you gave. Continue reasoning from exactly where it stopped, close the reasoning block, then write the complete answer."
	continueNudgeCode       = "The previous assistant message stopped inside an unterminated code block. Resume from its exact last character. Do not repeat any line already written, do not reopen the code fence, do not add commentary before the code, and do not restate the task. Emit only the continuation."
	defaultContinueAttempts = 3
)

// BuildContinueRequest returns a copy of body with a user nudge appended.
// platform is accepted so the caller can route; these helpers stay generic
// and do not consult Gemini/DefaultGeminiPlatform.
func BuildContinueRequest(platform string, body map[string]any, hint ContinueHint) map[string]any {
	_ = platform
	next := make(map[string]any, len(body)+1)
	for k, v := range body {
		next[k] = v
	}
	var messages []any
	if raw, ok := body["messages"].([]any); ok {
		messages = append([]any(nil), raw...)
	}
	nudge := continueNudgeGeneric
	if hint.Reasoning {
		nudge = continueNudgeReasoning
	} else if hint.CodeFence {
		nudge = continueNudgeCode
	}
	messages = append(messages, map[string]any{"role": "user", "content": nudge})
	next["messages"] = messages
	return next
}

// ContinueAttempt is one generation shot inside a transparent continuation
// chain. Content is the user-visible text produced by the shot, Finish is
// its finish_reason, and HasToolCalls reports whether the shot emitted tool
// calls (which must never be continued: the client owns the tool turn).
type ContinueAttempt struct {
	Content      string
	Finish       string
	HasToolCalls bool
}

// shouldContinueCompletion mirrors LLMBoost's should_continue_completion:
// bounded multi-shot stitching behind one client request. attempt is
// 1-based; at most maxAttempts shots are produced in total. It returns the
// proceed decision plus a skip reason for turn-end telemetry ("" when
// proceeding, mirroring LLMBoost's "none").
func shouldContinueCompletion(finish string, attempt, maxAttempts int, hasToolCalls bool, contentEmpty bool) (bool, string) {
	if maxAttempts <= 0 {
		maxAttempts = defaultContinueAttempts
	}
	if attempt >= maxAttempts {
		return false, "max-attempts"
	}
	if hasToolCalls {
		return false, "tool-calls-present"
	}
	normalized := strings.ToLower(strings.TrimSpace(finish))
	if normalized == "length" {
		return true, ""
	}
	if normalized == "" {
		// No terminal event: the stream broke mid-turn. Broken-stream
		// salvage stays disabled (LLMBoost's continuation_retry_broken_stream
		// default); the existing truncation/failover path owns that case.
		return false, "broken-stream"
	}
	if normalized == "stop" && contentEmpty {
		// Blank stop: upstream ended cleanly but produced nothing
		// user-visible. One bounded nudge retries the same account instead
		// of handing the client an empty success.
		return true, ""
	}
	return false, "finish-reason-not-continuable:" + normalized
}

// ContinueUntilStopEx concatenates attempt content while
// shouldContinueCompletion proceeds. It returns the concatenated content,
// the final finish_reason, and the number of shots consumed.
func ContinueUntilStopEx(maxAttempts int, attempt func(attempt int) ContinueAttempt) (string, string, int) {
	if maxAttempts <= 0 {
		maxAttempts = defaultContinueAttempts
	}
	var final, reason string
	shots := 0
	for n := 1; n <= maxAttempts; n++ {
		got := attempt(n)
		final += got.Content
		reason = got.Finish
		shots = n
		proceed, _ := shouldContinueCompletion(got.Finish, n, maxAttempts, got.HasToolCalls, strings.TrimSpace(final) == "")
		if !proceed {
			break
		}
	}
	return final, reason, shots
}

// ContinueUntilStop concatenates attempt content until finish_reason is not
// "length" or maxAttempts is reached. A non-positive maxAttempts defaults to 3.
// It keeps the historical length-only contract; new call sites that observe
// tool calls or blank stops must use ContinueUntilStopEx instead.
func ContinueUntilStop(maxAttempts int, attempt func() (string, string)) (string, string) {
	if maxAttempts <= 0 {
		maxAttempts = defaultContinueAttempts
	}
	var final, reason string
	for i := 0; i < maxAttempts; i++ {
		content, finish := attempt()
		final += content
		reason = finish
		if finish != "length" {
			break
		}
	}
	return final, reason
}

// PrepareRemoteRequest rewrites an OpenAI-compat Chat Completions body for a
// remote vendor. Gemini/Antigravity (DefaultGeminiPlatform) are returned
// unchanged. BetaLLM cannot disable thinking via reasoning_effort=none/off;
// those values are replaced with thinking.type=disabled and the effort field
// is removed. Step cannot disable thinking: off/none is mapped to low.
//
// When body.context_length is known and max_tokens/max_completion_tokens is
// set, the completion budget is clamped so
// prompt_est + max_tokens + 1 <= context_length, where prompt_est is
// max(1, len(json(messages))). If the remaining window is <= 0 this layer
// cannot reject the request, so the clamp is skipped (do not invent a
// 1-token generation).
func PrepareRemoteRequest(platform, model string, body map[string]any) map[string]any {
	_ = model
	if body == nil || DefaultGeminiPlatform(platform) {
		return body
	}
	switch remoteVendorFamily(platform) {
	case "betallm":
		applyBetaLLMThinkingOff(body)
	case "step":
		applyStepOfficialModel(body)
		applyStepEffort(body)
		flattenStepTextContent(body)
	}
	clampRemoteMaxTokens(body)
	return body
}

// flattenStepTextContent collapses text-only content blocks into a plain
// string. Step's prefill validator rejects array content ("Input should be a
// valid string"), which breaks harnesses that always send blocks. Messages
// with non-text parts (images) are left untouched.
func flattenStepTextContent(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		texts := make([]string, 0, len(parts))
		textOnly := true
		for _, part := range parts {
			block, ok := part.(map[string]any)
			if !ok {
				textOnly = false
				break
			}
			blockType, _ := block["type"].(string)
			if blockType != "text" && blockType != "input_text" {
				textOnly = false
				break
			}
			text, _ := block["text"].(string)
			texts = append(texts, text)
		}
		if textOnly {
			message["content"] = strings.Join(texts, "")
		}
	}
}

func remoteVendorFamily(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "betallm", "beta-llm":
		return "betallm"
	case "stepfun", "step-plan", "step":
		return "step"
	default:
		return ""
	}
}

func isRemoteEffortOff(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "off":
		return true
	default:
		return false
	}
}

func applyBetaLLMThinkingOff(body map[string]any) {
	if !isRemoteEffortOff(body["reasoning_effort"]) {
		return
	}
	body["thinking"] = map[string]any{"type": "disabled"}
	delete(body, "reasoning_effort")
}

func applyStepEffortFloor(body map[string]any) {
	applyStepEffort(body)
}

// Official Step Plan ids from
// https://platform.stepfun.ai/docs/en/guides/models/step-5-preview
// and https://platform.stepfun.ai/docs/en/step-plan/integrations/reasoning-api
func officialStepModelID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || strings.HasPrefix(s, "commandcode/") {
		return ""
	}
	s = strings.TrimPrefix(s, "stepfun/")
	s = strings.TrimPrefix(s, "stepfun-")
	switch {
	case s == "step-5" || s == "step5-preview" || strings.HasPrefix(s, "step-5-preview"):
		return "step-5-preview"
	case strings.HasPrefix(s, "step-3.7-flash"):
		return "step-3.7-flash"
	case strings.HasPrefix(s, "step-3.5-flash-2603"):
		return "step-3.5-flash-2603"
	case strings.HasPrefix(s, "step-3.5-flash"):
		return "step-3.5-flash"
	case strings.HasPrefix(s, "step-router-v1"):
		return "step-router-v1"
	default:
		return ""
	}
}

func applyStepOfficialModel(body map[string]any) {
	raw, _ := body["model"].(string)
	if official := officialStepModelID(raw); official != "" {
		body["model"] = official
	}
}

func stepEffortWire(raw any) string {
	s := ""
	if v, ok := raw.(string); ok {
		s = strings.ToLower(strings.TrimSpace(v))
	}
	switch s {
	case "low", "minimal", "off", "none":
		return "low"
	case "medium", "med":
		return "medium"
	case "high", "xhigh", "max", "on", "auto":
		return "high"
	default:
		return ""
	}
}

// applyStepEffort maps client aliases onto the documented low|medium|high
// ladder. Omitted or unknown effort becomes high (operator default).
func applyStepEffort(body map[string]any) {
	raw := body["reasoning_effort"]
	if raw == nil {
		if reasoning, ok := body["reasoning"].(map[string]any); ok {
			raw = reasoning["effort"]
		}
	}
	wire := stepEffortWire(raw)
	if wire == "" {
		wire = "high"
	}
	body["reasoning_effort"] = wire
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(thinking["type"])), "disabled") {
			delete(body, "thinking")
		}
	}
}

func remoteNumericInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func estimatePromptTokens(body map[string]any) int {
	raw, err := json.Marshal(body["messages"])
	if err != nil {
		return 1
	}
	if len(raw) < 1 {
		return 1
	}
	return len(raw)
}

func clampRemoteMaxTokens(body map[string]any) {
	ctxLen, ok := remoteNumericInt(body["context_length"])
	if !ok || ctxLen <= 0 {
		return
	}
	remaining := ctxLen - estimatePromptTokens(body) - 1
	// Overflow: cannot reject here. Leave max_tokens as-is rather than
	// sending a dummy 1-token generation.
	if remaining <= 0 {
		return
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, exists := body[key]
		if !exists {
			continue
		}
		cur, ok := remoteNumericInt(raw)
		if !ok {
			continue
		}
		if cur > remaining {
			body[key] = remaining
		}
	}
}
