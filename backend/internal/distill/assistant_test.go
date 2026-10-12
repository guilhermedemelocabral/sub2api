package distill

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func contentString(t *testing.T, msg apicompat.ChatMessage) string {
	t.Helper()
	if len(msg.Content) == 0 {
		return ""
	}
	var s string
	require.NoError(t, json.Unmarshal(msg.Content, &s))
	return s
}

func TestTapAnthropicSSESplitChunks(t *testing.T) {
	tap := NewTap(ProtocolAnthropicMessages)
	lines := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
	}, "\n")
	// Split mid-line inside the second data JSON payload.
	splitAt := strings.Index(lines, `"tool_use"`) + len(`"tool_use"`)
	tap.Feed([]byte(lines[:splitAt]))
	tap.Feed([]byte(lines[splitAt:]))

	msg, id, ok := tap.Finish()
	require.True(t, ok)
	require.Equal(t, "msg_1", id)
	require.Equal(t, "assistant", msg.Role)
	require.Equal(t, "ok", contentString(t, msg))
	require.Len(t, msg.ToolCalls, 1)
	require.Equal(t, "toolu_1", msg.ToolCalls[0].ID)
	require.Equal(t, "function", msg.ToolCalls[0].Type)
	require.Equal(t, "bash", msg.ToolCalls[0].Function.Name)
	require.Equal(t, `{"cmd":"ls"}`, msg.ToolCalls[0].Function.Arguments)
}

func TestTapChatCompletionsJSON(t *testing.T) {
	tap := NewTap(ProtocolChatCompletions)
	body := `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`
	tap.Feed([]byte(body))

	msg, id, ok := tap.Finish()
	require.True(t, ok)
	require.Equal(t, "chatcmpl_1", id)
	require.Equal(t, "assistant", msg.Role)
	require.Equal(t, "hi", contentString(t, msg))
	require.Len(t, msg.ToolCalls, 1)
	require.Equal(t, "call_1", msg.ToolCalls[0].ID)
	require.Equal(t, "bash", msg.ToolCalls[0].Function.Name)
	require.Equal(t, `{"cmd":"ls"}`, msg.ToolCalls[0].Function.Arguments)
}

func TestTapResponsesSSE(t *testing.T) {
	tap := NewTap(ProtocolResponses)
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"bash","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		``,
	}, "\n")
	tap.Feed([]byte(sse))

	msg, id, ok := tap.Finish()
	require.True(t, ok)
	require.Equal(t, "resp_1", id)
	require.Equal(t, "assistant", msg.Role)
	require.Len(t, msg.ToolCalls, 1)
	require.Equal(t, "call_1", msg.ToolCalls[0].ID)
	require.Equal(t, "bash", msg.ToolCalls[0].Function.Name)
	require.Equal(t, `{}`, msg.ToolCalls[0].Function.Arguments)
}

func TestTapErrorJSONOkFalse(t *testing.T) {
	tap := NewTap(ProtocolAnthropicMessages)
	tap.Feed([]byte(`{"type":"error"}`))

	_, _, ok := tap.Finish()
	require.False(t, ok)
}
