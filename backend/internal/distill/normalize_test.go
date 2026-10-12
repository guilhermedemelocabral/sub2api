package distill

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func parseTurnLine(t *testing.T, line []byte) Turn {
	t.Helper()
	require.NotEmpty(t, line)
	require.Equal(t, byte('\n'), line[len(line)-1])
	var got Turn
	require.NoError(t, json.Unmarshal(line[:len(line)-1], &got))
	return got
}

func TestNormalizeAnthropicRequest(t *testing.T) {
	assistant := apicompat.ChatMessage{
		Role: "assistant",
		ToolCalls: []apicompat.ChatToolCall{{
			ID:   "toolu_1",
			Type: "function",
			Function: apicompat.ChatFunctionCall{
				Name:      "bash",
				Arguments: `{"cmd":"ls"}`,
			},
		}},
	}
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"oi"}],"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`)
	line, err := Normalize(Input{
		At:          time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Protocol:    ProtocolAnthropicMessages,
		Model:       "claude-sonnet-5",
		RequestBody: body,
		Assistant:   assistant,
	})
	require.NoError(t, err)
	got := parseTurnLine(t, line)
	require.Len(t, got.Messages, 1)
	require.Equal(t, "user", got.Messages[0].Role)
	require.Len(t, got.Tools, 1)
	require.NotNil(t, got.Tools[0].Function)
	require.Equal(t, "bash", got.Tools[0].Function.Name)
	require.Len(t, got.Assistant.ToolCalls, 1)
	require.Equal(t, "bash", got.Assistant.ToolCalls[0].Function.Name)
	require.Equal(t, "", got.PreviousResponseID)
}

func TestNormalizeResponsesRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-5","previous_response_id":"resp_prev","input":[{"type":"message","role":"user","content":"oi"}]}`)
	line, err := Normalize(Input{
		Protocol:    ProtocolResponses,
		Model:       "gpt-5",
		RequestBody: body,
		Assistant:   apicompat.ChatMessage{Role: "assistant"},
	})
	require.NoError(t, err)
	got := parseTurnLine(t, line)
	require.Equal(t, "resp_prev", got.PreviousResponseID)
}

func TestNormalizeOmitsAnthropicImageBytes(t *testing.T) {
	assistant := apicompat.ChatMessage{
		Role: "assistant",
		ToolCalls: []apicompat.ChatToolCall{{
			ID:   "toolu_1",
			Type: "function",
			Function: apicompat.ChatFunctionCall{
				Name:      "bash",
				Arguments: `{"cmd":"ls"}`,
			},
		}},
	}
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	line, err := Normalize(Input{
		Protocol:    ProtocolAnthropicMessages,
		Model:       "claude-sonnet-5",
		RequestBody: body,
		Assistant:   assistant,
	})
	require.NoError(t, err)
	require.NotContains(t, string(line), "AAAA")
	require.Contains(t, string(line), `"omitted":true`)
	got := parseTurnLine(t, line)
	require.Equal(t, `{"cmd":"ls"}`, got.Assistant.ToolCalls[0].Function.Arguments)
}
