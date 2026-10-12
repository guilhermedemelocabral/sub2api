package distill

import (
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolChatCompletions   = "openai_chat_completions"
	ProtocolResponses         = "openai_responses"
)

type Turn struct {
	At                 time.Time                `json:"at"`
	RequestID          string                   `json:"request_id"`
	SessionID          string                   `json:"session_id"`
	ResponseID         string                   `json:"response_id"`
	PreviousResponseID string                   `json:"previous_response_id"`
	APIKeyID           int64                    `json:"api_key_id"`
	Protocol           string                   `json:"protocol"`
	Model              string                   `json:"model"`
	Stream             bool                     `json:"stream"`
	Tools              []apicompat.ChatTool     `json:"tools"`
	Messages           []apicompat.ChatMessage  `json:"messages"`
	Assistant          apicompat.ChatMessage    `json:"assistant"`
}

func MarshalTurn(t Turn) ([]byte, error) {
	t.At = t.At.UTC()
	if t.Tools == nil {
		t.Tools = []apicompat.ChatTool{}
	}
	if t.Messages == nil {
		t.Messages = []apicompat.ChatMessage{}
	}
	body, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
