package distill

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestOmitMessageReplacesDataURI(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"veja"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`)
	got := OmitMessage(apicompat.ChatMessage{Role: "user", Content: raw})
	require.Contains(t, string(got.Content), `"text":"veja"`)
	require.NotContains(t, string(got.Content), "base64,AAAA")
	require.Contains(t, string(got.Content), `"omitted":true`)
	require.Contains(t, string(got.Content), `"bytes":`)
}

func TestOmitMessageKeepsToolArguments(t *testing.T) {
	msg := apicompat.ChatMessage{
		Role: "assistant",
		ToolCalls: []apicompat.ChatToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: apicompat.ChatFunctionCall{
				Name:      "bash",
				Arguments: `{"cmd":"ls"}`,
			},
		}},
	}
	got := OmitMessage(msg)
	require.Equal(t, `{"cmd":"ls"}`, got.ToolCalls[0].Function.Arguments)
}
