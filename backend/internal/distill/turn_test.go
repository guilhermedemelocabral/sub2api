package distill

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestMarshalTurnKeepsEmptyIDs(t *testing.T) {
	line, err := MarshalTurn(Turn{
		At:        time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Protocol:  ProtocolAnthropicMessages,
		Model:     "claude-sonnet-5",
		Stream:    true,
		Assistant: apicompat.ChatMessage{Role: "assistant"},
	})
	require.NoError(t, err)
	require.Equal(t, byte('\n'), line[len(line)-1])
	var got map[string]any
	require.NoError(t, json.Unmarshal(line, &got))
	require.Equal(t, "2026-10-12T10:00:00Z", got["at"])
	require.Equal(t, "", got["session_id"])
	require.Equal(t, "", got["response_id"])
	require.Equal(t, "", got["previous_response_id"])
	require.Equal(t, "anthropic_messages", got["protocol"])
	require.Contains(t, got, "tools")
	require.Contains(t, got, "messages")
}
