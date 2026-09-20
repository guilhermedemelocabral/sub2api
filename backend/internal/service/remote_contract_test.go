package service

import "testing"

func TestNormalizeStepfunEventDropsEmptyToolIdentity(t *testing.T) {
	event := map[string]any{
		"agent": "",
		"choices": []any{map[string]any{
			"finish_reason": "",
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "",
					"type":  "",
					"function": map[string]any{
						"name":      "",
						"arguments": "{\"x\":1}",
					},
				}},
			},
		}},
	}
	out := NormalizeRemoteEvent("stepfun", event)
	if _, ok := out["agent"]; ok {
		t.Fatal("empty agent must be dropped")
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != nil {
		t.Fatalf("empty finish_reason: %v", choice["finish_reason"])
	}
	call := choice["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if _, ok := call["id"]; ok {
		t.Fatal("empty id must be dropped")
	}
	if _, ok := call["type"]; ok {
		t.Fatal("empty type must be dropped")
	}
	fn := call["function"].(map[string]any)
	if _, ok := fn["name"]; ok {
		t.Fatal("empty name must be dropped")
	}
	if _, ok := call["index"]; !ok {
		t.Fatal("delta tool_calls must keep index")
	}
}

func TestNormalizeStepfunCompletedDropsIndex(t *testing.T) {
	event := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": 0, "id": "a", "type": "function", "function": map[string]any{"name": "multiply"}},
					map[string]any{"index": 0, "id": "b", "type": "function", "function": map[string]any{"name": "get_time"}},
				},
			},
		}},
	}
	out := NormalizeRemoteEvent("stepfun", event)
	calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("calls=%d", len(calls))
	}
	names := make([]string, 0, 2)
	for _, raw := range calls {
		call := raw.(map[string]any)
		if _, ok := call["index"]; ok {
			t.Fatal("completed tool_calls must drop index")
		}
		fn := call["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	if names[0] != "multiply" || names[1] != "get_time" {
		t.Fatalf("names=%v", names)
	}
}

func TestNormalizeRemoteEventUnknownPlatformIsNoop(t *testing.T) {
	event := map[string]any{"id": "x", "agent": ""}
	out := NormalizeRemoteEvent("gemini", event)
	if out["id"] != "x" {
		t.Fatal("gemini must be untouched")
	}
	agent, ok := out["agent"]
	if !ok || agent != "" {
		t.Fatal("gemini must keep empty agent")
	}
}
