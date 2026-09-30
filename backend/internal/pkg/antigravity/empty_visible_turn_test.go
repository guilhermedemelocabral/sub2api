package antigravity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamingProcessor_EmptyStopDoesNotInventVisibleText(t *testing.T) {
	p := NewStreamingProcessor("gemini-3.8-flash-high")
	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0,"thoughtsTokenCount":40}}}`

	out := string(p.ProcessLine(line))

	require.True(t, p.EmptyVisibleTurn())
	require.NotContains(t, out, "turno sem saída")
	require.NotContains(t, out, "gateway:")
	require.NotContains(t, out, "text_delta")
}

func TestStreamingProcessor_VisibleTextIsNotAnEmptyTurn(t *testing.T) {
	p := NewStreamingProcessor("gemini-3.8-flash-high")
	line := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"resposta real"}]},"finishReason":"STOP"}]}}`

	out := string(p.ProcessLine(line))

	require.False(t, p.EmptyVisibleTurn())
	require.Contains(t, out, "resposta real")
	require.NotContains(t, out, "turno sem saída")
}

func TestNonStreamingProcessor_EmptyStopDoesNotInventVisibleText(t *testing.T) {
	p := NewNonStreamingProcessor()
	resp := &GeminiResponse{
		Candidates: []GeminiCandidate{{
			FinishReason: "STOP",
			Content: &GeminiContent{
				Role:  "model",
				Parts: []GeminiPart{{Thought: true, Text: "só pensamento"}},
			},
		}},
	}

	got := p.Process(resp, "msg_1", "gemini-3.8-flash-high")

	require.True(t, p.EmptyVisibleTurn())
	for _, block := range got.Content {
		require.NotEqual(t, "text", block.Type)
		require.False(t, strings.Contains(block.Text, "turno sem saída"))
		require.False(t, strings.Contains(block.Thinking, "turno sem saída"))
	}
}
