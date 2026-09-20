package service

import "encoding/json"

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

// ContinueUntilStop concatenates attempt content until finish_reason is not
// "length" or maxAttempts is reached. A non-positive maxAttempts defaults to 3.
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
