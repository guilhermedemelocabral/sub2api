package service

import (
	"strings"
	"testing"
)

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

func TestContinueNudgeForTruncatedAnswer(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "q"}, map[string]any{"role": "assistant", "content": "partial"}},
	}
	origLen := len(body["messages"].([]any))
	next := BuildContinueRequest("betallm", body, ContinueHint{Reasoning: false, CodeFence: false})
	if len(body["messages"].([]any)) != origLen {
		t.Fatal("BuildContinueRequest must not mutate the caller's messages")
	}
	msgs := next["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatal("nudge must be a user message")
	}
	if !strings.Contains(last["content"].(string), "Continue from the exact point") {
		t.Fatalf("generic nudge missing: %v", last["content"])
	}
}

func TestContinueNudgeForTruncatedReasoning(t *testing.T) {
	next := BuildContinueRequest("betallm", map[string]any{
		"messages": []any{map[string]any{"role": "assistant", "content": ""}},
	}, ContinueHint{Reasoning: true})
	last := next["messages"].([]any)[1].(map[string]any)
	if !strings.Contains(last["content"].(string), "private reasoning") {
		t.Fatalf("reasoning nudge missing: %v", last["content"])
	}
}

func TestContinueNudgeForOpenCodeFence(t *testing.T) {
	next := BuildContinueRequest("stepfun", map[string]any{
		"messages": []any{map[string]any{"role": "assistant", "content": "```python\nprint(1)"}},
	}, ContinueHint{CodeFence: true})
	last := next["messages"].([]any)[1].(map[string]any)
	if !strings.Contains(last["content"].(string), "unterminated code block") {
		t.Fatalf("code nudge missing: %v", last["content"])
	}
}

func TestFinishLengthLoopCapsAtThree(t *testing.T) {
	attempts := 0
	final, reason := ContinueUntilStop(3, func() (string, string) {
		attempts++
		return "tok", "length"
	})
	if attempts != 3 || reason != "length" || final != "toktoktok" {
		t.Fatalf("attempts=%d reason=%s final=%q", attempts, reason, final)
	}
}
