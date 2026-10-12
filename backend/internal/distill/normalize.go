package distill

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

type Input struct {
	At          time.Time
	RequestID   string
	SessionID   string
	ResponseID  string
	APIKeyID    int64
	Protocol    string
	Model       string
	Stream      bool
	RequestBody []byte
	Assistant   apicompat.ChatMessage
}

func Normalize(in Input) ([]byte, error) {
	chat, previous, err := toChatRequest(in.Protocol, in.RequestBody)
	if err != nil {
		return nil, err
	}
	messages := make([]apicompat.ChatMessage, len(chat.Messages))
	for i, msg := range chat.Messages {
		messages[i] = OmitMessage(msg)
	}
	tools := chat.Tools
	if tools == nil {
		tools = []apicompat.ChatTool{}
	}
	return MarshalTurn(Turn{
		At:                 in.At,
		RequestID:          in.RequestID,
		SessionID:          in.SessionID,
		ResponseID:         in.ResponseID,
		PreviousResponseID: previous,
		APIKeyID:           in.APIKeyID,
		Protocol:           in.Protocol,
		Model:              in.Model,
		Stream:             in.Stream,
		Tools:              tools,
		Messages:           messages,
		Assistant:          OmitMessage(in.Assistant),
	})
}

func toChatRequest(protocol string, body []byte) (*apicompat.ChatCompletionsRequest, string, error) {
	switch protocol {
	case ProtocolAnthropicMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, "", err
		}
		chat, err := apicompat.AnthropicToChatCompletionsRequest(&req)
		return chat, "", err
	case ProtocolChatCompletions:
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, "", err
		}
		return &req, "", nil
	case ProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, "", err
		}
		chat, err := apicompat.ResponsesToChatCompletionsRequest(&req)
		return chat, req.PreviousResponseID, err
	default:
		return nil, "", fmt.Errorf("distill: unknown protocol %q", protocol)
	}
}
