package distill

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

type contextKey struct{}

// ContextKey identifies the distill Tap stored on a gin context.
var ContextKey = contextKey{}

// Tap accumulates upstream response bytes and assembles an assistant message.
type Tap struct {
	protocol string
	lineBuf  strings.Builder
	rawBuf   bytes.Buffer

	responseID string
	content    strings.Builder
	reasoning  strings.Builder
	toolCalls  []apicompat.ChatToolCall
	toolArgs   map[int]*strings.Builder

	chatJSONDone bool
	chatMessage  apicompat.ChatMessage

	responsesAcc *apicompat.BufferedResponseAccumulator
	sawError     bool
}

// NewTap returns a Tap for the given distill protocol constant.
func NewTap(protocol string) *Tap {
	t := &Tap{
		protocol: protocol,
		toolArgs: make(map[int]*strings.Builder),
	}
	if protocol == ProtocolResponses {
		t.responsesAcc = apicompat.NewBufferedResponseAccumulator()
	}
	return t
}

// Feed appends client response bytes and parses complete SSE/JSON fragments.
func (t *Tap) Feed(b []byte) {
	t.rawBuf.Write(b)
	t.lineBuf.WriteString(string(b))
	for {
		s := t.lineBuf.String()
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			return
		}
		line := s[:idx]
		if idx+1 < len(s) {
			t.lineBuf.Reset()
			t.lineBuf.WriteString(s[idx+1:])
		} else {
			t.lineBuf.Reset()
		}
		t.processLine(line)
	}
}

// Finish flushes any partial line and returns the assembled assistant message.
func (t *Tap) Finish() (assistant apicompat.ChatMessage, responseID string, ok bool) {
	if t.lineBuf.Len() > 0 {
		t.processLine(t.lineBuf.String())
		t.lineBuf.Reset()
	}

	switch t.protocol {
	case ProtocolChatCompletions:
		if !t.chatJSONDone {
			t.tryParseChatJSONBody()
		}
		if t.chatJSONDone {
			msg := t.chatMessage
			msg.Role = "assistant"
			return msg, t.responseID, t.finishOK(msg)
		}
	case ProtocolResponses:
		if !t.chatJSONDone {
			t.tryParseResponsesJSONBody()
		}
		if t.chatJSONDone {
			msg := t.chatMessage
			msg.Role = "assistant"
			return msg, t.responseID, t.finishOK(msg)
		}
		if t.responsesAcc != nil && (t.responsesAcc.HasContent() || t.responseID != "") {
			resp := &apicompat.ResponsesResponse{
				ID:     t.responseID,
				Status: "completed",
				Output: t.responsesAcc.BuildOutput(),
			}
			conv := apicompat.ResponsesToChatCompletions(resp, "")
			if len(conv.Choices) > 0 {
				msg := conv.Choices[0].Message
				msg.Role = "assistant"
				id := t.responseID
				if id == "" {
					id = conv.ID
				}
				return msg, id, t.finishOK(msg)
			}
		}
		if t.sawError {
			return apicompat.ChatMessage{}, "", false
		}
	case ProtocolAnthropicMessages:
		if t.sawError {
			return apicompat.ChatMessage{}, "", false
		}
	}

	msg := t.buildAnthropicOrChatStreamMessage()
	msg.Role = "assistant"
	if t.sawError {
		return apicompat.ChatMessage{}, "", false
	}
	return msg, t.responseID, t.finishOK(msg)
}

func (t *Tap) finishOK(msg apicompat.ChatMessage) bool {
	if t.sawError {
		return false
	}
	if msg.ReasoningContent != "" {
		return true
	}
	if len(msg.ToolCalls) > 0 {
		return true
	}
	if len(msg.Content) > 0 {
		var s string
		if json.Unmarshal(msg.Content, &s) == nil && s != "" {
			return true
		}
		// Non-string JSON content still counts as content.
		if json.Unmarshal(msg.Content, &s) != nil {
			return true
		}
	}
	return t.content.Len() > 0 || len(t.toolCalls) > 0 || t.reasoning.Len() > 0
}

func (t *Tap) buildAnthropicOrChatStreamMessage() apicompat.ChatMessage {
	for idx, b := range t.toolArgs {
		args := b.String()
		if idx < len(t.toolCalls) {
			t.toolCalls[idx].Function.Arguments = args
		}
	}
	msg := apicompat.ChatMessage{Role: "assistant"}
	if t.reasoning.Len() > 0 {
		msg.ReasoningContent = t.reasoning.String()
	}
	if t.content.Len() > 0 {
		raw, _ := json.Marshal(t.content.String())
		msg.Content = raw
	}
	if len(t.toolCalls) > 0 {
		msg.ToolCalls = append([]apicompat.ChatToolCall(nil), t.toolCalls...)
	}
	return msg
}

func (t *Tap) processLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	if strings.HasPrefix(line, "data:") {
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			return
		}
		t.parseDataPayload(payload)
		return
	}

	if line[0] == '{' {
		t.parseDataPayload(line)
	}
}

func (t *Tap) parseDataPayload(payload string) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return
	}
	if probe.Type == "error" {
		t.sawError = true
		return
	}

	switch t.protocol {
	case ProtocolAnthropicMessages:
		t.parseAnthropicEvent(payload, probe.Type)
	case ProtocolChatCompletions:
		if t.isChatCompletionJSON(payload) {
			t.tryParseChatJSON(payload)
			return
		}
		t.parseChatStreamChunk(payload)
	case ProtocolResponses:
		t.parseResponsesEvent(payload)
	}
}

func (t *Tap) isChatCompletionJSON(payload string) bool {
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return false
	}
	return len(resp.Choices) > 0 && resp.Choices[0].Message.Role != ""
}

func (t *Tap) tryParseChatJSONBody() {
	body := strings.TrimSpace(t.rawBuf.String())
	if body == "" || body[0] != '{' {
		return
	}
	t.tryParseChatJSON(body)
}

func (t *Tap) tryParseResponsesJSONBody() {
	body := strings.TrimSpace(t.rawBuf.String())
	if body == "" || body[0] != '{' {
		return
	}
	var resp apicompat.ResponsesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || (resp.ID == "" && len(resp.Output) == 0) {
		return
	}
	conv := apicompat.ResponsesToChatCompletions(&resp, resp.Model)
	if len(conv.Choices) == 0 {
		return
	}
	t.chatJSONDone = true
	t.chatMessage = conv.Choices[0].Message
	t.responseID = resp.ID
}

func (t *Tap) tryParseChatJSON(payload string) {
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return
	}
	if len(resp.Choices) == 0 {
		return
	}
	t.chatJSONDone = true
	t.responseID = resp.ID
	t.chatMessage = resp.Choices[0].Message
}

func (t *Tap) parseAnthropicEvent(payload, typ string) {
	switch typ {
	case "message_start":
		var evt struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(payload), &evt) == nil && evt.Message.ID != "" {
			t.responseID = evt.Message.ID
		}
	case "content_block_start":
		var evt struct {
			Index         int `json:"index"`
			ContentBlock  struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(payload), &evt) != nil {
			return
		}
		switch evt.ContentBlock.Type {
		case "tool_use":
			for len(t.toolCalls) <= evt.Index {
				t.toolCalls = append(t.toolCalls, apicompat.ChatToolCall{})
			}
			t.toolCalls[evt.Index] = apicompat.ChatToolCall{
				ID:   evt.ContentBlock.ID,
				Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name: evt.ContentBlock.Name,
				},
			}
			if _, ok := t.toolArgs[evt.Index]; !ok {
				t.toolArgs[evt.Index] = &strings.Builder{}
			}
		}
	case "content_block_delta":
		var evt struct {
			Index int `json:"index"`
			Delta struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				PartialJSON  string `json:"partial_json"`
				Thinking     string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &evt) != nil {
			return
		}
		switch evt.Delta.Type {
		case "text_delta":
			t.content.WriteString(evt.Delta.Text)
		case "input_json_delta":
			if b, ok := t.toolArgs[evt.Index]; ok {
				b.WriteString(evt.Delta.PartialJSON)
			}
		case "thinking_delta":
			t.reasoning.WriteString(evt.Delta.Thinking)
		}
	}
}

func (t *Tap) parseChatStreamChunk(payload string) {
	var chunk apicompat.ChatCompletionsChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if chunk.ID != "" {
		t.responseID = chunk.ID
	}
	if len(chunk.Choices) == 0 {
		return
	}
	delta := chunk.Choices[0].Delta
	if delta.Content != nil {
		t.content.WriteString(*delta.Content)
	}
	if delta.ReasoningContent != nil {
		t.reasoning.WriteString(*delta.ReasoningContent)
	}
	for _, tc := range delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		for len(t.toolCalls) <= idx {
			t.toolCalls = append(t.toolCalls, apicompat.ChatToolCall{Type: "function"})
		}
		if tc.ID != "" {
			t.toolCalls[idx].ID = tc.ID
		}
		if tc.Type != "" {
			t.toolCalls[idx].Type = tc.Type
		}
		if tc.Function.Name != "" {
			t.toolCalls[idx].Function.Name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			if _, ok := t.toolArgs[idx]; !ok {
				t.toolArgs[idx] = &strings.Builder{}
			}
			t.toolArgs[idx].WriteString(tc.Function.Arguments)
		}
	}
}

func (t *Tap) parseResponsesEvent(payload string) {
	var evt apicompat.ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		return
	}
	switch evt.Type {
	case "response.created", "response.completed", "response.done":
		if evt.Response != nil && evt.Response.ID != "" {
			t.responseID = evt.Response.ID
		}
	case "error":
		t.sawError = true
	}
	if t.responsesAcc != nil {
		t.responsesAcc.ProcessEvent(&evt)
	}

	// Non-streaming Responses JSON body (single object).
	if evt.Type == "" && evt.Response == nil {
		var resp apicompat.ResponsesResponse
		if json.Unmarshal([]byte(payload), &resp) == nil && (resp.ID != "" || len(resp.Output) > 0) {
			conv := apicompat.ResponsesToChatCompletions(&resp, resp.Model)
			if len(conv.Choices) > 0 {
				t.chatJSONDone = true
				t.chatMessage = conv.Choices[0].Message
				t.responseID = resp.ID
			}
		}
	}
}

// tapWriter wraps gin.ResponseWriter and feeds written bytes to a Tap.
type tapWriter struct {
	gin.ResponseWriter
	tap *Tap
}

func (w *tapWriter) Write(b []byte) (int, error) {
	w.tap.Feed(b)
	return w.ResponseWriter.Write(b)
}

func (w *tapWriter) WriteString(s string) (int, error) {
	w.tap.Feed([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// Wrap returns a ResponseWriter that tees written bytes into the Tap.
func (t *Tap) Wrap(w gin.ResponseWriter) gin.ResponseWriter {
	return &tapWriter{ResponseWriter: w, tap: t}
}
