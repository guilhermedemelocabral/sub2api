package service

import (
	"encoding/json"
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

func TestPrepareRemoteRequestGeminiLeavesBodyUnchanged(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "none",
		"max_tokens":       128,
		"context_length":   16,
		"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		"keep":             true,
	}
	out := PrepareRemoteRequest("gemini", "gemini-2.5-flash", body)
	if out["reasoning_effort"] != "none" {
		t.Fatalf("gemini must keep reasoning_effort: %v", out["reasoning_effort"])
	}
	if _, ok := out["thinking"]; ok {
		t.Fatal("gemini must not inject thinking")
	}
	if out["max_tokens"] != 128 {
		t.Fatalf("gemini must not clamp max_tokens: %v", out["max_tokens"])
	}
	if out["keep"] != true {
		t.Fatal("gemini must return the same body fields")
	}
}

func TestPrepareRemoteRequestBetaLLMThinkingOffDisablesThinking(t *testing.T) {
	for _, platform := range []string{"betallm", "beta-llm"} {
		body := map[string]any{
			"reasoning_effort": "none",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		}
		out := PrepareRemoteRequest(platform, "glm", body)
		thinking, ok := out["thinking"].(map[string]any)
		if !ok || thinking["type"] != "disabled" {
			t.Fatalf("%s thinking-off: got %#v", platform, out["thinking"])
		}
		if _, exists := out["reasoning_effort"]; exists {
			t.Fatalf("%s must drop reasoning_effort=none (it still thinks upstream)", platform)
		}
	}
}

func TestPrepareRemoteRequestBetaLLMThinkingOffAlias(t *testing.T) {
	out := PrepareRemoteRequest("betallm", "glm", map[string]any{"reasoning_effort": "off"})
	thinking, _ := out["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking=%#v", out["thinking"])
	}
	if _, exists := out["reasoning_effort"]; exists {
		t.Fatal("reasoning_effort=off must be removed on BetaLLM")
	}
}

func TestPrepareRemoteRequestStepfunOffMapsToLow(t *testing.T) {
	for _, platform := range []string{"stepfun", "step-plan", "step"} {
		for _, effort := range []string{"off", "none"} {
			out := PrepareRemoteRequest(platform, "step-2", map[string]any{"reasoning_effort": effort})
			if out["reasoning_effort"] != "low" {
				t.Fatalf("%s effort %q: got %#v", platform, effort, out["reasoning_effort"])
			}
		}
	}
}

func TestPrepareRemoteRequestStepfunAliasesOfficialModelAndEffort(t *testing.T) {
	out := PrepareRemoteRequest("stepfun", "stepfun-step-5-preview", map[string]any{
		"model":            "stepfun-step-5-preview",
		"reasoning_effort": "xhigh",
	})
	if out["model"] != "step-5-preview" {
		t.Fatalf("model=%#v", out["model"])
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("effort=%#v", out["reasoning_effort"])
	}
}

func TestPrepareRemoteRequestStepfunDefaultEffortHigh(t *testing.T) {
	out := PrepareRemoteRequest("stepfun", "step-5-preview", map[string]any{"model": "step-5-preview"})
	if out["reasoning_effort"] != "high" {
		t.Fatalf("default effort=%#v", out["reasoning_effort"])
	}
}

func TestPrepareRemoteRequestStepfunReasoningEffortField(t *testing.T) {
	out := PrepareRemoteRequest("stepfun", "step-5-preview", map[string]any{
		"model":     "step-5-preview",
		"reasoning": map[string]any{"effort": "med"},
	})
	if out["reasoning_effort"] != "medium" {
		t.Fatalf("nested effort=%#v", out["reasoning_effort"])
	}
}

func TestPrepareRemoteRequestClampsMaxTokensToContext(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "hi"}}
	promptJSON, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	promptEst := len(promptJSON)
	if promptEst < 1 {
		promptEst = 1
	}
	// remaining = context - promptEst - 1 = 10
	body := map[string]any{
		"messages":       messages,
		"context_length": promptEst + 10 + 1,
		"max_tokens":     100,
	}
	out := PrepareRemoteRequest("betallm", "glm", body)
	got, ok := asRemoteInt(out["max_tokens"])
	if !ok || got != 10 {
		t.Fatalf("clamped max_tokens=%v (promptEst=%d)", out["max_tokens"], promptEst)
	}
}

func TestPrepareRemoteRequestSkipsClampWhenRemainingNonPositive(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "this-is-a-long-prompt-for-overflow"}}
	body := map[string]any{
		"messages":       messages,
		"context_length": 4,
		"max_tokens":     50,
	}
	out := PrepareRemoteRequest("stepfun", "step-2", body)
	got, ok := asRemoteInt(out["max_tokens"])
	if !ok || got != 50 {
		t.Fatalf("overflow must skip clamp, got %#v", out["max_tokens"])
	}
}

func asRemoteInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func TestShouldApplyRemoteOpenAICompatContractSkipsGemini(t *testing.T) {
	if shouldApplyRemoteOpenAICompatContract(&Account{Platform: "gemini"}) {
		t.Fatal("gemini must skip")
	}
	if shouldApplyRemoteOpenAICompatContract(&Account{Platform: "antigravity"}) {
		t.Fatal("antigravity must skip")
	}
	if shouldApplyRemoteOpenAICompatContract(&Account{Platform: "google"}) {
		t.Fatal("google must skip")
	}
	if !shouldApplyRemoteOpenAICompatContract(&Account{Platform: "openai", Name: "betallm"}) {
		t.Fatal("openai-compat must apply")
	}
}
