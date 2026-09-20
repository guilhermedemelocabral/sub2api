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
