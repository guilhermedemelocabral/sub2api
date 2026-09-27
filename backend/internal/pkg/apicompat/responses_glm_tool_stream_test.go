package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesOutputSummaryAcceptsStringForms(t *testing.T) {
	var item ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"reasoning","id":"rs_1","summary":["plan the call"]}`), &item))
	require.Equal(t, []ResponsesSummary{{Type: "summary_text", Text: "plan the call"}}, item.Summary)

	var bare ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"reasoning","id":"rs_2","summary":"just text"}`), &bare))
	require.Equal(t, "just text", bare.Summary[0].Text)

	var event ResponsesStreamEvent
	raw := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":["done"]}]}}`)
	require.NoError(t, json.Unmarshal(raw, &event))
	require.Equal(t, "done", event.Response.Output[0].Summary[0].Text)
}

// BetaLLM/GLM Responses streams put a reasoning item at output_index 0, then a
// function call at output_index 1, but argument deltas carry item_id and omit
// output_index. The zero value used to land on the closed thinking block, so
// Anthropic clients saw an empty tool_use.
func TestResponsesEventToAnthropic_GLMToolArgsFollowItemID(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.Model = "glm-5.3-flash"

	sequence := []*ResponsesStreamEvent{
		{Type: "response.created", Response: &ResponsesResponse{ID: "resp_1", Model: "glm-5.3-flash", Status: "in_progress"}},
		{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "reasoning", ID: "rs_1"}},
		{Type: "response.reasoning.delta", ItemID: "rs_1", Delta: "check the city"},
		{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "get_weather"}},
		{Type: "response.function_call_arguments.delta", ItemID: "fc_1", Delta: `{"city":"Sao Paulo"}`},
		{Type: "response.output_item.done", OutputIndex: 0, Item: &ResponsesOutput{Type: "reasoning", ID: "rs_1", Summary: []ResponsesSummary{{Type: "summary_text", Text: "check the city"}}}},
		{Type: "response.output_item.done", Item: &ResponsesOutput{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Sao Paulo"}`}},
		{Type: "response.completed", Response: &ResponsesResponse{ID: "resp_1", Status: "completed", Output: []ResponsesOutput{
			{Type: "reasoning", ID: "rs_1", Summary: []ResponsesSummary{{Type: "summary_text", Text: "check the city"}}},
			{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Sao Paulo"}`},
		}}},
	}

	var toolStart, toolDelta *int
	var partial string
	var stopReason string
	sawStop := false
	sawThinking := false
	for _, evt := range sequence {
		for _, out := range ResponsesEventToAnthropicEvents(evt, state) {
			switch out.Type {
			case "content_block_start":
				if out.ContentBlock != nil && out.ContentBlock.Type == "tool_use" && out.Index != nil {
					idx := *out.Index
					toolStart = &idx
				}
			case "content_block_delta":
				if out.Delta != nil && out.Delta.Type == "thinking_delta" && out.Delta.Thinking != "" {
					sawThinking = true
				}
				if out.Delta != nil && out.Delta.Type == "input_json_delta" && out.Index != nil {
					idx := *out.Index
					toolDelta = &idx
					partial += out.Delta.PartialJSON
				}
			case "message_delta":
				if out.Delta != nil {
					stopReason = out.Delta.StopReason
				}
			case "message_stop":
				sawStop = true
			}
		}
	}

	require.NotNil(t, toolStart)
	require.NotNil(t, toolDelta)
	require.Equal(t, *toolStart, *toolDelta)
	require.NotEqual(t, 0, *toolDelta)
	require.Equal(t, `{"city":"Sao Paulo"}`, partial)
	require.True(t, sawThinking)
	require.Equal(t, "tool_use", stopReason)
	require.True(t, sawStop)
}
